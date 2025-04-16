/*
Copyright 2023 Scality, Inc.

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

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	clientset *kubernetes.Clientset
	namespace string
)

// setupKubernetesClient initializes the Kubernetes client
func setupKubernetesClient() {
	var kubeconfig string

	// Use the current context in kubeconfig
	if os.Getenv("KUBECONFIG") != "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	} else if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	} else {
		Fail("Unable to locate kubeconfig")
	}

	// Build config from kubeconfig file
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to build config from kubeconfig")

	// Create clientset
	clientset, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Kubernetes client")

	// Set namespace
	namespace = os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "mount-s3"
	}
	fmt.Printf("Using namespace: %s\n", namespace)
}

// verifyClusterAccess checks if we can access the Kubernetes cluster
func verifyClusterAccess() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "Failed to access Kubernetes API")
}

func TestE2EScality(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scality S3 CSI Driver E2E Suite")
}

var _ = BeforeSuite(func() {
	// Set up Kubernetes client
	setupKubernetesClient()

	// Verify cluster access
	verifyClusterAccess()

	// Ensure namespace exists
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		_, err = clientset.CoreV1().Namespaces().Create(
			ctx,
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			},
			metav1.CreateOptions{},
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace %s", namespace)
		fmt.Printf("Created namespace: %s\n", namespace)
	}
})

var _ = AfterSuite(func() {
	// Clean up resources if needed
	fmt.Println("Cleaning up test resources...")
})

// This is a package main, but we're not actually running anything from this file
// Tests are defined in csi_driver_test.go and are run with "go test"
func main() {
	fmt.Println("This file contains test setup code for the Scality S3 CSI Driver E2E tests.")
	fmt.Println("Run the tests with: go test -v -tags=e2e")
}
