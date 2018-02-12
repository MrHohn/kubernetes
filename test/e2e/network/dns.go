/*
Copyright 2015 The Kubernetes Authors.

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

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

const dnsTestPodHostName = "dns-querier-1"
const dnsTestServiceName = "dns-test-service"

var _ = SIGDescribe("DNS", func() {
	f := framework.NewDefaultFramework("dns")

	/*
	   Testname: dns-for-clusters
	   Description: Make sure that DNS can resolve the names of clusters.
	*/
	framework.ConformanceIt("should provide DNS for the cluster ", func() {
		// All the names we need to be able to resolve.
		// TODO: Spin up a separate test service and test that dns works for that service.
		namesToResolve := []string{
			"kubernetes.default",
			"kubernetes.default.svc",
			"kubernetes.default.svc.cluster.local",
		}
		// Added due to #8512. This is critical for GCE and GKE deployments.
		if framework.ProviderIs("gce", "gke") {
			namesToResolve = append(namesToResolve, "google.com")
			namesToResolve = append(namesToResolve, "metadata")
		}
		hostFQDN := fmt.Sprintf("%s.%s.%s.svc.cluster.local", dnsTestPodHostName, dnsTestServiceName, f.Namespace.Name)
		hostEntries := []string{hostFQDN, dnsTestPodHostName}
		wheezyProbeCmd, wheezyFileNames := createProbeCommand(namesToResolve, hostEntries, "", "wheezy", f.Namespace.Name)
		jessieProbeCmd, jessieFileNames := createProbeCommand(namesToResolve, hostEntries, "", "jessie", f.Namespace.Name)
		By("Running these commands on wheezy: " + wheezyProbeCmd + "\n")
		By("Running these commands on jessie: " + jessieProbeCmd + "\n")

		// Run a pod which probes DNS and exposes the results by HTTP.
		By("creating a pod to probe DNS")
		pod := createDNSPod(f.Namespace.Name, wheezyProbeCmd, jessieProbeCmd, dnsTestPodHostName, dnsTestServiceName)
		validateDNSResults(f, pod, append(wheezyFileNames, jessieFileNames...))
	})

	/*
	   Testname: dns-for-services
	   Description: Make sure that DNS can resolve the names of services.
	*/
	framework.ConformanceIt("should provide DNS for services ", func() {
		// Create a test headless service.
		By("Creating a test headless service")
		testServiceSelector := map[string]string{
			"dns-test": "true",
		}
		headlessService := framework.CreateServiceSpec(dnsTestServiceName, "", true, testServiceSelector)
		_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(headlessService)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			By("deleting the test headless service")
			defer GinkgoRecover()
			f.ClientSet.CoreV1().Services(f.Namespace.Name).Delete(headlessService.Name, nil)
		}()

		regularService := framework.CreateServiceSpec("test-service-2", "", false, testServiceSelector)
		regularService, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(regularService)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			By("deleting the test service")
			defer GinkgoRecover()
			f.ClientSet.CoreV1().Services(f.Namespace.Name).Delete(regularService.Name, nil)
		}()

		// All the names we need to be able to resolve.
		// TODO: Create more endpoints and ensure that multiple A records are returned
		// for headless service.
		namesToResolve := []string{
			fmt.Sprintf("%s", headlessService.Name),
			fmt.Sprintf("%s.%s", headlessService.Name, f.Namespace.Name),
			fmt.Sprintf("%s.%s.svc", headlessService.Name, f.Namespace.Name),
			fmt.Sprintf("_http._tcp.%s.%s.svc", headlessService.Name, f.Namespace.Name),
			fmt.Sprintf("_http._tcp.%s.%s.svc", regularService.Name, f.Namespace.Name),
		}

		wheezyProbeCmd, wheezyFileNames := createProbeCommand(namesToResolve, nil, regularService.Spec.ClusterIP, "wheezy", f.Namespace.Name)
		jessieProbeCmd, jessieFileNames := createProbeCommand(namesToResolve, nil, regularService.Spec.ClusterIP, "jessie", f.Namespace.Name)
		By("Running these commands on wheezy: " + wheezyProbeCmd + "\n")
		By("Running these commands on jessie: " + jessieProbeCmd + "\n")

		// Run a pod which probes DNS and exposes the results by HTTP.
		By("creating a pod to probe DNS")
		pod := createDNSPod(f.Namespace.Name, wheezyProbeCmd, jessieProbeCmd, dnsTestPodHostName, dnsTestServiceName)
		pod.ObjectMeta.Labels = testServiceSelector

		validateDNSResults(f, pod, append(wheezyFileNames, jessieFileNames...))
	})

	It("should provide DNS for pods for Hostname and Subdomain", func() {
		// Create a test headless service.
		By("Creating a test headless service")
		testServiceSelector := map[string]string{
			"dns-test-hostname-attribute": "true",
		}
		serviceName := "dns-test-service-2"
		podHostname := "dns-querier-2"
		headlessService := framework.CreateServiceSpec(serviceName, "", true, testServiceSelector)
		_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(headlessService)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			By("deleting the test headless service")
			defer GinkgoRecover()
			f.ClientSet.CoreV1().Services(f.Namespace.Name).Delete(headlessService.Name, nil)
		}()

		hostFQDN := fmt.Sprintf("%s.%s.%s.svc.cluster.local", podHostname, serviceName, f.Namespace.Name)
		hostNames := []string{hostFQDN, podHostname}
		namesToResolve := []string{hostFQDN}
		wheezyProbeCmd, wheezyFileNames := createProbeCommand(namesToResolve, hostNames, "", "wheezy", f.Namespace.Name)
		jessieProbeCmd, jessieFileNames := createProbeCommand(namesToResolve, hostNames, "", "jessie", f.Namespace.Name)
		By("Running these commands on wheezy: " + wheezyProbeCmd + "\n")
		By("Running these commands on jessie: " + jessieProbeCmd + "\n")

		// Run a pod which probes DNS and exposes the results by HTTP.
		By("creating a pod to probe DNS")
		pod1 := createDNSPod(f.Namespace.Name, wheezyProbeCmd, jessieProbeCmd, dnsTestPodHostName, dnsTestServiceName)
		pod1.ObjectMeta.Labels = testServiceSelector
		pod1.Spec.Hostname = podHostname
		pod1.Spec.Subdomain = serviceName

		validateDNSResults(f, pod1, append(wheezyFileNames, jessieFileNames...))
	})

	It("should provide DNS for ExternalName services", func() {
		// Create a test ExternalName service.
		By("Creating a test externalName service")
		serviceName := "dns-test-service-3"
		externalNameService := framework.CreateServiceSpec(serviceName, "foo.example.com", false, nil)
		_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(externalNameService)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			By("deleting the test externalName service")
			defer GinkgoRecover()
			f.ClientSet.CoreV1().Services(f.Namespace.Name).Delete(externalNameService.Name, nil)
		}()

		hostFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, f.Namespace.Name)
		wheezyProbeCmd, wheezyFileName := createTargetedProbeCommand(hostFQDN, "CNAME", "wheezy")
		jessieProbeCmd, jessieFileName := createTargetedProbeCommand(hostFQDN, "CNAME", "jessie")
		By("Running these commands on wheezy: " + wheezyProbeCmd + "\n")
		By("Running these commands on jessie: " + jessieProbeCmd + "\n")

		// Run a pod which probes DNS and exposes the results by HTTP.
		By("creating a pod to probe DNS")
		pod1 := createDNSPod(f.Namespace.Name, wheezyProbeCmd, jessieProbeCmd, dnsTestPodHostName, dnsTestServiceName)

		validateTargetedProbeOutput(f, pod1, []string{wheezyFileName, jessieFileName}, "foo.example.com.")

		// Test changing the externalName field
		By("changing the externalName to bar.example.com")
		_, err = framework.UpdateService(f.ClientSet, f.Namespace.Name, serviceName, func(s *v1.Service) {
			s.Spec.ExternalName = "bar.example.com"
		})
		Expect(err).NotTo(HaveOccurred())
		wheezyProbeCmd, wheezyFileName = createTargetedProbeCommand(hostFQDN, "CNAME", "wheezy")
		jessieProbeCmd, jessieFileName = createTargetedProbeCommand(hostFQDN, "CNAME", "jessie")
		By("Running these commands on wheezy: " + wheezyProbeCmd + "\n")
		By("Running these commands on jessie: " + jessieProbeCmd + "\n")

		// Run a pod which probes DNS and exposes the results by HTTP.
		By("creating a second pod to probe DNS")
		pod2 := createDNSPod(f.Namespace.Name, wheezyProbeCmd, jessieProbeCmd, dnsTestPodHostName, dnsTestServiceName)

		validateTargetedProbeOutput(f, pod2, []string{wheezyFileName, jessieFileName}, "bar.example.com.")

		// Test changing type from ExternalName to ClusterIP
		By("changing the service to type=ClusterIP")
		_, err = framework.UpdateService(f.ClientSet, f.Namespace.Name, serviceName, func(s *v1.Service) {
			s.Spec.Type = v1.ServiceTypeClusterIP
			s.Spec.Ports = []v1.ServicePort{
				{Port: 80, Name: "http", Protocol: "TCP"},
			}
		})
		Expect(err).NotTo(HaveOccurred())
		wheezyProbeCmd, wheezyFileName = createTargetedProbeCommand(hostFQDN, "A", "wheezy")
		jessieProbeCmd, jessieFileName = createTargetedProbeCommand(hostFQDN, "A", "jessie")
		By("Running these commands on wheezy: " + wheezyProbeCmd + "\n")
		By("Running these commands on jessie: " + jessieProbeCmd + "\n")

		// Run a pod which probes DNS and exposes the results by HTTP.
		By("creating a third pod to probe DNS")
		pod3 := createDNSPod(f.Namespace.Name, wheezyProbeCmd, jessieProbeCmd, dnsTestPodHostName, dnsTestServiceName)

		svc, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Get(externalNameService.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		validateTargetedProbeOutput(f, pod3, []string{wheezyFileName, jessieFileName}, svc.Spec.ClusterIP)
	})

	It("should support configurable pod resolv.conf", func() {
		testNdotsValue := "2"
		testTimeoutValue := "3"
		testDNSConfig := &v1.PodDNSConfig{
			Nameservers: []string{"1.2.3.4"},
			Searches:    []string{"test.search.path"},
			Options: []v1.PodDNSConfigOption{
				{
					Name:  "ndots",
					Value: &testNdotsValue,
				},
				{
					Name:  "timeout",
					Value: &testTimeoutValue,
				},
			},
		}
		catResolvConfCmd := "cat /etc/resolv.conf"

		By("Preparing auto-generated pod DNS resolver configs.")
		verifier := &resolvConfVerifier{}
		// Create a pod with dnsPolicy=ClusterFirst for comparison.
		dnsClusterFirstPodName := framework.CreateExecPodOrFail(f.ClientSet, f.Namespace.Name, "dns-cluster-first-", func(pod *v1.Pod) {
			pod.Spec.DNSPolicy = v1.DNSClusterFirst
		})
		clusterFirstConfigStr, err := framework.RunHostCmd(f.Namespace.Name, dnsClusterFirstPodName, catResolvConfCmd)
		Expect(err).NotTo(HaveOccurred())
		framework.Logf("dnsClusterFirstPod has resolv.conf:\n%s", clusterFirstConfigStr)
		// Create a pod with dnsPolicy=Default for comparison.
		dnsDefaultPodName := framework.CreateExecPodOrFail(f.ClientSet, f.Namespace.Name, "dns-default-", func(pod *v1.Pod) {
			pod.Spec.DNSPolicy = v1.DNSDefault
		})
		defaultConfigStr, err := framework.RunHostCmd(f.Namespace.Name, dnsDefaultPodName, catResolvConfCmd)
		Expect(err).NotTo(HaveOccurred())
		framework.Logf("dnsDefaultPod has resolv.conf:\n%s", defaultConfigStr)
		err = verifier.init(clusterFirstConfigStr, defaultConfigStr)
		Expect(err).NotTo(HaveOccurred(), "failed to initialize resolv.conf verifier")
		framework.DeletePodOrFail(f.ClientSet, f.Namespace.Name, dnsClusterFirstPodName)
		framework.DeletePodOrFail(f.ClientSet, f.Namespace.Name, dnsDefaultPodName)

		testCases := []struct {
			desc          string
			podNamePrefix string
			tweakPodFunc  func(pod *v1.Pod)
		}{
			{
				desc:          "using dnsConfig with dnsPolicy=None",
				podNamePrefix: "dns-none-",
				tweakPodFunc: func(pod *v1.Pod) {
					pod.Spec.DNSPolicy = v1.DNSNone
					pod.Spec.DNSConfig = testDNSConfig
				},
			},
			{
				desc:          "using dnsConfig with dnsPolicy=ClusterFirst",
				podNamePrefix: "dns-cluster-first-custom-",
				tweakPodFunc: func(pod *v1.Pod) {
					pod.Spec.DNSPolicy = v1.DNSClusterFirst
					pod.Spec.DNSConfig = testDNSConfig
				},
			},
			{
				desc:          "using dnsConfig with dnsPolicy=ClusterFirstWithHostNet",
				podNamePrefix: "dns-cluster-first-host-net-custom-",
				tweakPodFunc: func(pod *v1.Pod) {
					pod.Spec.DNSPolicy = v1.DNSClusterFirstWithHostNet
					pod.Spec.DNSConfig = testDNSConfig
					pod.Spec.HostNetwork = true
				},
			},
			{
				desc:          "using dnsConfig with dnsPolicy=Default",
				podNamePrefix: "dns-default-custom-",
				tweakPodFunc: func(pod *v1.Pod) {
					pod.Spec.DNSPolicy = v1.DNSDefault
					pod.Spec.DNSConfig = testDNSConfig
				},
			},
		}

		for _, tc := range testCases {
			By(fmt.Sprintf("Verifying resolv.conf on a pod %s", tc.desc))
			var testPod *v1.Pod
			testPodName := framework.CreateExecPodOrFail(f.ClientSet, f.Namespace.Name, tc.podNamePrefix, func(pod *v1.Pod) {
				tc.tweakPodFunc(pod)
				testPod = pod
			})
			confStr, err := framework.RunHostCmd(f.Namespace.Name, testPodName, catResolvConfCmd)
			Expect(err).NotTo(HaveOccurred())
			framework.Logf("Pod %q has resolv.conf:\n%s", testPodName, confStr)
			framework.DeletePodOrFail(f.ClientSet, f.Namespace.Name, testPodName)
			err = verifier.verify(testPod, confStr)
			Expect(err).NotTo(HaveOccurred(), "failed to verify pod resolv.conf")
		}
	})
})
