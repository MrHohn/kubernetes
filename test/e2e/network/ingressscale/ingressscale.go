/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ingressscale

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	NumIngressesSmall  = 5
	NumIngressesMedium = 20
	NumIngressesLarge  = 100

	ScaleTestIngressNamePrefix = "ing-scale"
	ScaleTestBackendName       = "echoheaders-scale"
	ScaleTestSecretName        = "tls-secret-scale"
	ScaleTestHostname          = "scale.ingress.com"
	ScaleTestNumBackends       = 10

	maxConcurrentIngressCreations = 20
	// We don't expect waitForIngress to take longer
	// than WaitForIngressMaxTimeout.
	WaitForIngressMaxTimeout = 60 * time.Minute
	IngressesCleanupTimeout  = 60 * time.Minute
)

var (
	ScaleTestLabels = map[string]string{
		"app": ScaleTestBackendName,
	}
)

// Limit the number of parallel ingress creations.
var threadLimit = make(chan struct{}, maxConcurrentIngressCreations)

func acquireThread() {
	threadLimit <- struct{}{}
}

func releaseThread() {
	<-threadLimit
}

type IngressScaleFramework struct {
	Clientset     clientset.Interface
	Jig           *framework.IngressTestJig
	GCEController *framework.GCEIngressController
	CloudConfig   framework.CloudConfig
	Logger        framework.TestLogger

	Namespace        string
	EnableTLS        bool
	NumIngressesTest []int

	ScaleTestSvcs []*v1.Service
	ScaleTestIngs []*extensions.Ingress

	CreateLatencies     []time.Duration
	StepLatencies       []time.Duration
	StepCreateLatencies []time.Duration
	StepUpdateLatencies []time.Duration
}

func NewIngressScaleFramework(cs clientset.Interface, ns string, cloudConfig framework.CloudConfig) *IngressScaleFramework {
	return &IngressScaleFramework{
		Namespace:   ns,
		Clientset:   cs,
		CloudConfig: cloudConfig,
		Logger:      &framework.E2ELogger{},
		EnableTLS:   true,
		NumIngressesTest: []int{
			NumIngressesSmall,
			NumIngressesMedium,
			NumIngressesLarge,
		},
	}
}

func (f *IngressScaleFramework) PrepareScaleTest() error {
	f.Logger.Infof("Initializing ingress test suite and gce controller...")
	f.Jig = framework.NewIngressTestJig(f.Clientset)
	f.Jig.Logger = f.Logger
	f.GCEController = &framework.GCEIngressController{
		Client: f.Clientset,
		Cloud:  f.CloudConfig,
	}
	if err := f.GCEController.Init(); err != nil {
		return fmt.Errorf("Failed to initialize GCE controller: %v", err)
	}

	f.ScaleTestSvcs = []*v1.Service{}
	f.ScaleTestIngs = []*extensions.Ingress{}

	return nil
}

func (f *IngressScaleFramework) CleanupScaleTest() []error {
	var errs []error

	f.Logger.Infof("Cleaning up services...")
	for _, svc := range f.ScaleTestSvcs {
		if svc != nil {
			err := f.Clientset.CoreV1().Services(svc.Namespace).Delete(svc.Name, nil)
			if err != nil {
				errs = append(errs, fmt.Errorf("Error while deleting the service %v/%v: %v", svc.Namespace, svc.Name, err))
			}
		}
	}
	f.Logger.Infof("Cleaning up ingresses...")
	for _, ing := range f.ScaleTestIngs {
		if ing != nil {
			err := f.Clientset.ExtensionsV1beta1().Ingresses(ing.Namespace).Delete(ing.Name, nil)
			if err != nil {
				errs = append(errs, fmt.Errorf("Error while deleting the ingress %v/%v: %v", ing.Namespace, ing.Name, err))
			}
		}
	}

	f.Logger.Infof("Cleaning up cloud resources...")
	if err := f.GCEController.CleanupGCEIngressControllerWithTimeout(IngressesCleanupTimeout); err != nil {
		errs = append(errs, err)
	}

	return errs
}

func (f *IngressScaleFramework) RunScaleTest() []error {
	var errs []error

	testDeploy := GenerateScaleTestBackendDeploymentSpec(ScaleTestNumBackends)
	f.Logger.Infof("Creating deployment %s...", testDeploy.Name)
	_, err := f.Jig.Client.ExtensionsV1beta1().Deployments(f.Namespace).Create(testDeploy)
	defer func() {
		f.Logger.Infof("Deleting deployment %s...", testDeploy.Name)
		if err := f.Jig.Client.ExtensionsV1beta1().Deployments(f.Namespace).Delete(testDeploy.Name, nil); err != nil {
			errs = append(errs, fmt.Errorf("Failed to delete deployment %s: %v", testDeploy.Name, err))
		}
	}()
	if err != nil {
		errs = append(errs, fmt.Errorf("Failed to create deployment %s: %v", testDeploy.Name, err))
		return errs
	}

	if f.EnableTLS {
		f.Logger.Infof("Ensuring TLS secret %s...", ScaleTestSecretName)
		if err := f.Jig.PrepareTLSSecret(f.Namespace, ScaleTestSecretName, ScaleTestHostname); err != nil {
			errs = append(errs, fmt.Errorf("Failed to prepare TLS secret %s: %v", ScaleTestSecretName, err))
			return errs
		}
	}

	// currentNum keeps track of how many ingresses have been created.
	currentNum := new(int)

	prepareIngsFunc := func(goalNum int) {
		var ingWg sync.WaitGroup
		numToCreate := goalNum - *currentNum
		ingWg.Add(numToCreate)
		errQueue := make(chan error, numToCreate)
		latencyQueue := make(chan time.Duration, numToCreate)
		start := time.Now()
		for ; *currentNum < goalNum; *currentNum++ {
			suffix := fmt.Sprintf("%d", *currentNum)
			go func() {
				defer ingWg.Done()
				// Throttling parallel ingress creations.
				acquireThread()
				defer releaseThread()

				start := time.Now()
				svcCreated, ingCreated, err := f.CreateScaleTestServiceIngress(suffix, f.EnableTLS)
				f.ScaleTestSvcs = append(f.ScaleTestSvcs, svcCreated)
				f.ScaleTestIngs = append(f.ScaleTestIngs, ingCreated)
				if err != nil {
					errQueue <- err
					return
				}
				f.Logger.Infof("Waiting for ingress %s to come up...", ingCreated.Name)
				if err := f.Jig.WaitForGivenIngressWithTimeout(ingCreated, false, WaitForIngressMaxTimeout); err != nil {
					errQueue <- err
					return
				}
				elapsed := time.Since(start)
				f.Logger.Infof("Spent %s for ingress %s to come up", elapsed, ingCreated.Name)
				latencyQueue <- elapsed
			}()
		}

		// Wait until all ingress creations are complete.
		ingWg.Wait()
		elapsed := time.Since(start)
		for latency := range latencyQueue {
			f.CreateLatencies = append(f.CreateLatencies, latency)
		}
		if len(errQueue) != 0 {
			f.Logger.Errorf("Failed while creating services and ingresses, spent %v", elapsed)
			for err := range errQueue {
				errs = append(errs, err)
			}
			return
		}
		f.Logger.Infof("Spent %s for %d ingresses to come up", elapsed, numToCreate)
		f.StepLatencies = append(f.StepLatencies, elapsed)
	}

	measureCreateUpdateFunc := func() {
		f.Logger.Infof("Create one more ingress and wait for it to come up")
		start := time.Now()
		svcCreated, ingCreated, err := f.CreateScaleTestServiceIngress(fmt.Sprintf("%d", *currentNum), f.EnableTLS)
		*currentNum = *currentNum + 1
		f.ScaleTestSvcs = append(f.ScaleTestSvcs, svcCreated)
		f.ScaleTestIngs = append(f.ScaleTestIngs, ingCreated)
		if err != nil {
			errs = append(errs, err)
			return
		}

		f.Logger.Infof("Waiting for ingress %s to come up...", ingCreated.Name)
		if err := f.Jig.WaitForGivenIngressWithTimeout(ingCreated, false, WaitForIngressMaxTimeout); err != nil {
			errs = append(errs, err)
			return
		}
		elapsed := time.Since(start)
		f.Logger.Infof("Spent %s for ingress %s to come up", elapsed, ingCreated.Name)
		f.StepCreateLatencies = append(f.StepCreateLatencies)

		f.Logger.Infof("Updating ingress and wait for change to take effect")
		ingToUpdate, err := f.Clientset.ExtensionsV1beta1().Ingresses(f.Namespace).Get(ingCreated.Name, metav1.GetOptions{})
		if err != nil {
			errs = append(errs, err)
			return
		}
		AddTestPathToIngress(ingToUpdate)
		start = time.Now()
		ingToUpdate, err = f.Clientset.ExtensionsV1beta1().Ingresses(f.Namespace).Update(ingToUpdate)
		if err != nil {
			errs = append(errs, err)
			return
		}

		if err := f.Jig.WaitForGivenIngressWithTimeout(ingToUpdate, false, WaitForIngressMaxTimeout); err != nil {
			errs = append(errs, err)
			return
		}
		elapsed = time.Since(start)
		f.Logger.Infof("Spent %s for updating ingress %s", elapsed, ingToUpdate.Name)
		f.StepUpdateLatencies = append(f.StepUpdateLatencies)
	}

	defer f.dumpLatencies()

	for _, num := range f.NumIngressesTest {
		f.Logger.Infof("Create %d ingresses and wait for them to come up", num)
		prepareIngsFunc(num)
		f.Logger.Infof("Measure create and update latency with %d ingresses", num)
		measureCreateUpdateFunc()

		if len(errs) != 0 {
			return errs
		}
	}

	return errs
}

// TODO: Need a better way/format for scale data output.
func (f *IngressScaleFramework) dumpLatencies() {
	f.Logger.Infof("Dumping scale test latencies...")
	for _, latency := range f.CreateLatencies {
		f.Logger.Infof("Ingress parallel creation latencies:")
		f.Logger.Infof("%v", latency)
	}
	for i, latency := range f.StepLatencies {
		f.Logger.Infof("Total time for completing %d ingress creations: %v", f.NumIngressesTest[i], latency)
	}
	for i, latency := range f.StepCreateLatencies {
		f.Logger.Infof("Ingress creation latency under %d ingresses: %v", f.NumIngressesTest[i], latency)
	}
	for i, latency := range f.StepUpdateLatencies {
		f.Logger.Infof("Ingress update latency under %d ingresses: %v", f.NumIngressesTest[i], latency)
	}
}

func AddTestPathToIngress(ing *extensions.Ingress) {
	ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths = append(
		ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths,
		extensions.HTTPIngressPath{
			Path:    "/test",
			Backend: ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].Backend,
		})
}

func (f *IngressScaleFramework) CreateScaleTestServiceIngress(suffix string, enableTLS bool) (*v1.Service, *extensions.Ingress, error) {
	svcCreated, err := f.Clientset.CoreV1().Services(f.Namespace).Create(GenerateScaleTestServiceSpec(suffix))
	if err != nil {
		return nil, nil, err
	}
	ingCreated, err := f.Clientset.ExtensionsV1beta1().Ingresses(f.Namespace).Create(GenerateScaleTestIngressSpec(suffix, enableTLS))
	if err != nil {
		return nil, nil, err
	}
	return svcCreated, ingCreated, nil
}

func GenerateScaleTestIngressSpec(suffix string, enableTLS bool) *extensions.Ingress {
	ing := &extensions.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", ScaleTestIngressNamePrefix, suffix),
		},
		Spec: extensions.IngressSpec{
			TLS: []extensions.IngressTLS{
				{SecretName: ScaleTestSecretName},
			},
			Rules: []extensions.IngressRule{
				{
					Host: ScaleTestHostname,
					IngressRuleValue: extensions.IngressRuleValue{
						HTTP: &extensions.HTTPIngressRuleValue{
							Paths: []extensions.HTTPIngressPath{
								{
									Path: "/scale",
									Backend: extensions.IngressBackend{
										ServiceName: fmt.Sprintf("%s-%s", ScaleTestBackendName, suffix),
										ServicePort: intstr.IntOrString{
											Type:   intstr.Int,
											IntVal: 80,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if enableTLS {
		ing.Spec.TLS = []extensions.IngressTLS{
			{SecretName: ScaleTestSecretName},
		}
	}
	return ing
}

func GenerateScaleTestServiceSpec(suffix string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("%s-%s", ScaleTestBackendName, suffix),
			Labels: ScaleTestLabels,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       "http",
				Protocol:   v1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromInt(8080),
			}},
			Selector: ScaleTestLabels,
			Type:     v1.ServiceTypeNodePort,
		},
	}
}

func GenerateScaleTestBackendDeploymentSpec(numReplicas int32) *extensions.Deployment {
	return &extensions.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: ScaleTestBackendName,
		},
		Spec: extensions.DeploymentSpec{
			Replicas: &numReplicas,
			Selector: &metav1.LabelSelector{MatchLabels: ScaleTestLabels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ScaleTestLabels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  ScaleTestBackendName,
							Image: "gcr.io/google_containers/echoserver:1.6",
							Ports: []v1.ContainerPort{{ContainerPort: 8080}},
							ReadinessProbe: &v1.Probe{
								Handler: v1.Handler{
									HTTPGet: &v1.HTTPGetAction{
										Port: intstr.FromInt(8080),
										Path: "/healthz",
									},
								},
								FailureThreshold: 10,
								PeriodSeconds:    1,
								SuccessThreshold: 1,
								TimeoutSeconds:   1,
							},
						},
					},
				},
			},
		},
	}
}
