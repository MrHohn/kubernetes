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

package network

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/manifest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	numIngressesSmall  = 5
	numIngressesMedium = 20
	numIngressesLarge  = 100

	scaleTestIngressNamePrefix = "ing-scale"
	scaleTestBackendName       = "echoheaders-scale"
	scaleTestSecretName        = "tls-secret"
	scaleTestHostname          = "scale.ingress.com"
	scaleTestNumBackends       = 10

	maxConccurentIngressCreations  = 20
	ingressCreateLatencyThreshhold = 15 * time.Minute
	ingressUpdateLatencyThreshhold = 10 * time.Minute
	ingressesCleanupTimeout        = 60 * time.Minute
)

var (
	scaleTestLabels = map[string]string{
		"app": scaleTestBackendName,
	}
)

// Limit the number of parallel ingress creation.
var threadLimit = make(chan struct{}, maxConccurentIngressCreations)

func acquireThread() {
	threadLimit <- struct{}{}
}

func releaseThread() {
	<-threadLimit
}

var _ = SIGDescribe("Loadbalancing: L7 Scalability", func() {
	defer GinkgoRecover()
	var (
		ns string
	)
	f := framework.NewDefaultFramework("ingress-scale")

	BeforeEach(func() {
		ns = f.Namespace.Name
	})

	Describe("GCE [Slow] [Serial] [Feature:IngressScale]", func() {
		var (
			jig           *framework.IngressTestJig
			gceController *framework.GCEIngressController
			scaleTestSvcs []*v1.Service
			scaleTestIngs []*extensions.Ingress
		)

		BeforeEach(func() {
			framework.SkipUnlessProviderIs("gce", "gke")

			By("Initializing ingress test suite and gce controller")
			jig = framework.NewIngressTestJig(f.ClientSet)
			gceController = &framework.GCEIngressController{
				Ns:     ns,
				Client: jig.Client,
				Cloud:  framework.TestContext.CloudConfig,
			}
			gceController.Init()

			scaleTestSvcs = []*v1.Service{}
			scaleTestIngs = []*extensions.Ingress{}
		})

		AfterEach(func() {
			By("Cleaning up services...")
			for _, svc := range scaleTestSvcs {
				if svc != nil {
					jig.TryDeleteGivenService(svc)
				}
			}
			By("Cleaning up ingresses...")
			for _, ing := range scaleTestIngs {
				if ing != nil {
					jig.TryDeleteGivenIngress(ing)
				}
			}

			By("Cleaning up cloud resources...")
			framework.CleanupGCEIngressControllerWithTimeout(gceController, ingressesCleanupTimeout)
		})

		It("Create and update ingress should happen promptly with small/medium/large amount of ingresses", func() {
			By(fmt.Sprintf("Bringing up %d backend servers...", scaleTestNumBackends))
			testDeploy := generateScaleTestBackendDeploymentSpec(scaleTestNumBackends)
			_, err := f.ClientSet.ExtensionsV1beta1().Deployments(ns).Create(testDeploy)
			defer func() {
				By(fmt.Sprintf("Deleting deployment %s...", testDeploy.Name))
				if err := f.ClientSet.ExtensionsV1beta1().Deployments(ns).Delete(testDeploy.Name, nil); err != nil {
					framework.Logf("Failed to delete deployment %s: %v", testDeploy.Name, err)
				}
			}()
			framework.ExpectNoError(err)

			testSecret := generateScaleTestTLSSecretSpec()
			By(fmt.Sprintf("Creating TLS secret %s...", testSecret.Name))
			_, err = f.ClientSet.CoreV1().Secrets(ns).Create(testSecret)
			defer func() {
				By(fmt.Sprintf("Deleting TLS secret %s...", testSecret.Name))
				if err := f.ClientSet.CoreV1().Secrets(ns).Delete(testSecret.Name, nil); err != nil {
					framework.Logf("Failed to delete TLS secret %s: %v", testSecret.Name, err)
				}
			}()
			framework.ExpectNoError(err)

			currentNum := new(int)

			prepareIngsFunc := func(goalNum int) {
				var errs []error
				var ingWg sync.WaitGroup
				numToCreate := goalNum - *currentNum
				ingWg.Add(numToCreate)
				start := time.Now()
				for ; *currentNum < goalNum; *currentNum++ {
					suffix := fmt.Sprintf("%d", *currentNum)
					go func() {
						defer ingWg.Done()
						acquireThread()
						defer releaseThread()

						start := time.Now()
						svcCreated, ingCreated, err := createScaleTestServiceIngress(f.ClientSet, ns, suffix)
						scaleTestSvcs = append(scaleTestSvcs, svcCreated)
						scaleTestIngs = append(scaleTestIngs, ingCreated)
						if err != nil {
							framework.Logf("Failed to create test service and ingress: %v", err)
							errs = append(errs, err)
							return
						}
						framework.Logf("Waiting for ingress %s to come up...", ingCreated.Name)
						jig.WaitForGivenIngressWithTimeout(ingCreated, false, ingressCreateLatencyThreshhold)
						elapsed := time.Since(start)
						framework.Logf("Spent %s for ingress %s to come up", elapsed, ingCreated.Name)
					}()
				}

				ingWg.Wait()
				Expect(len(errs)).To(Equal(0), "Expect no error for while services and ingresses")
				elapsed := time.Since(start)
				framework.Logf("Spent %s for %d ingresses to come up", elapsed, numToCreate)
			}

			measureCreatUpdateFunc := func() {
				framework.Logf("Create one more ingress and wait for it to come up")
				start := time.Now()
				svcCreated, ingCreated, err := createScaleTestServiceIngress(f.ClientSet, ns, fmt.Sprintf("%d", *currentNum))
				*currentNum = *currentNum + 1
				scaleTestSvcs = append(scaleTestSvcs, svcCreated)
				scaleTestIngs = append(scaleTestIngs, ingCreated)
				framework.ExpectNoError(err)
				framework.Logf("Waiting for ingress %s to come up...", ingCreated.Name)
				jig.WaitForGivenIngressWithTimeout(ingCreated, false, ingressCreateLatencyThreshhold)
				elapsed := time.Since(start)
				framework.Logf("Spent %s for ingress %s to come up", elapsed, ingCreated.Name)

				framework.Logf("Updating ingress and wait for change to take effect")
				ingToUpdate, err := f.ClientSet.ExtensionsV1beta1().Ingresses(ns).Get(ingCreated.Name, metav1.GetOptions{})
				framework.ExpectNoError(err)
				addTestPathToIngress(ingToUpdate)
				start = time.Now()
				ingToUpdate, err = f.ClientSet.ExtensionsV1beta1().Ingresses(ns).Update(ingToUpdate)
				framework.ExpectNoError(err)
				jig.WaitForGivenIngressWithTimeout(ingToUpdate, false, ingressUpdateLatencyThreshhold)
				elapsed = time.Since(start)
				framework.Logf("Spent %s for updating ingress %s", elapsed, ingToUpdate.Name)
			}

			By(fmt.Sprintf("Create small amount (%d) ingresses and wait for them to come up", numIngressesSmall))
			prepareIngsFunc(numIngressesSmall)
			By(fmt.Sprintf("Measure create and update latency with small amount (%d) ingresses", numIngressesSmall))
			measureCreatUpdateFunc()

			By(fmt.Sprintf("Create medium amount (%d) ingresses and wait for them to come up", numIngressesMedium))
			prepareIngsFunc(numIngressesMedium)
			By(fmt.Sprintf("Measure create and update latency with medium amount (%d) ingresses", numIngressesMedium))
			measureCreatUpdateFunc()

			By(fmt.Sprintf("Create large amount (%d) ingresses and wait for them to come up", numIngressesLarge))
			prepareIngsFunc(numIngressesLarge)
			By(fmt.Sprintf("Measure create and update latency with large amount (%d) ingresses", numIngressesLarge))
			measureCreatUpdateFunc()

		})
	})
})

func addTestPathToIngress(ing *extensions.Ingress) {
	ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths = append(
		ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths,
		extensions.HTTPIngressPath{
			Path:    "/test",
			Backend: ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].Backend,
		})
}

func createScaleTestServiceIngress(cs clientset.Interface, namespace, suffix string) (*v1.Service, *extensions.Ingress, error) {
	svcCreated, err := cs.CoreV1().Services(namespace).Create(generateScaleTestServiceSpec(suffix))
	if err != nil {
		return nil, nil, err
	}
	ingCreated, err := cs.ExtensionsV1beta1().Ingresses(namespace).Create(generateScaleTestIngressSpec(suffix))
	if err != nil {
		return nil, nil, err
	}
	return svcCreated, ingCreated, nil
}

func generateScaleTestIngressSpec(suffix string) *extensions.Ingress {
	return &extensions.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", scaleTestIngressNamePrefix, suffix),
		},
		Spec: extensions.IngressSpec{
			TLS: []extensions.IngressTLS{
				{SecretName: scaleTestSecretName},
			},
			Rules: []extensions.IngressRule{
				{
					Host: scaleTestHostname,
					IngressRuleValue: extensions.IngressRuleValue{
						HTTP: &extensions.HTTPIngressRuleValue{
							Paths: []extensions.HTTPIngressPath{
								{
									Path: "/scale",
									Backend: extensions.IngressBackend{
										ServiceName: fmt.Sprintf("%s-%s", scaleTestBackendName, suffix),
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
}

func generateScaleTestServiceSpec(suffix string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("%s-%s", scaleTestBackendName, suffix),
			Labels: scaleTestLabels,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       "http",
				Protocol:   v1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromInt(8080),
			}},
			Selector: scaleTestLabels,
			Type:     v1.ServiceTypeNodePort,
		},
	}
}

func generateScaleTestBackendDeploymentSpec(numReplicas int32) *extensions.Deployment {
	return &extensions.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: scaleTestBackendName,
		},
		Spec: extensions.DeploymentSpec{
			Replicas: &numReplicas,
			Selector: &metav1.LabelSelector{MatchLabels: scaleTestLabels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: scaleTestLabels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  scaleTestBackendName,
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

func generateScaleTestTLSSecretSpec() *v1.Secret {
	secret, err := manifest.SecretFromManifest(filepath.Join(framework.IngressManifestPath, "static-ip/secret.yaml"))
	Expect(err).NotTo(HaveOccurred())
	return secret
}
