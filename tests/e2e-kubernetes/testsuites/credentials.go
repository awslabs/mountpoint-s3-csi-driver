/*
Copyright 2019 The Kubernetes Authors.

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

package custom_testsuites

import (
	"context"
	"fmt"
	ginkgo "github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"time"
)

type s3CSICredentialsTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSICredentialsTestSuite() storageframework.TestSuite {
	return &s3CSICredentialsTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "credentials",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSICredentialsTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSICredentialsTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSICredentialsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)

	namespace := NamespacePrefix + "credentials"
	f := framework.NewFrameworkWithCustomTimeouts(namespace, storageframework.GetDriverTimeouts(driver))
	f.SkipNamespaceCreation = true
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		namespaceObj, err := getNamespace(f.ClientSet, namespace)
		framework.ExpectNoError(err)
		f.Namespace = namespaceObj

		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})
	toWrite := 1024 // 1KB

	testPodLevelIdentity := func(ctx context.Context, pvc *v1.PersistentVolumeClaim) {
		node := l.config.ClientNodeSelection

		ginkgo.By(fmt.Sprintf("Creating readWritePod with a volume on %+v", node))
		readWritePod, err := createPodWithSA(ctx, f.ClientSet, namespace, []*v1.PersistentVolumeClaim{pvc}, "s3-csi-e2e-sa")
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, readWritePod))
		}()

		ginkgo.By(fmt.Sprintf("Creating readOnlyPod with a volume on %+v", node))
		readOnlyPod, err := createPodWithSA(ctx, f.ClientSet, namespace, []*v1.PersistentVolumeClaim{pvc}, "s3-csi-e2e-read-only-sa")
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, readOnlyPod))
		}()

		seed := time.Now().UTC().UnixNano()

		fileToWrite := fmt.Sprintf("/mnt/volume1/file.txt")
		fileToFail := fmt.Sprintf("/mnt/volume1/file_to_fail.txt")

		ginkgo.By(fmt.Sprintf("Checking write from readWritePod"))
		checkWriteToPath(f, readWritePod, fileToWrite, toWrite, seed)
		ginkgo.By(fmt.Sprintf("Checking read from readWritePod"))
		checkReadFromPath(f, readWritePod, fileToWrite, toWrite, seed)
		ginkgo.By(fmt.Sprintf("Checking write fails from readOnlyPod"))
		checkWriteToPathFails(f, readOnlyPod, fileToFail, toWrite, seed)
		ginkgo.By(fmt.Sprintf("Checking read from readOnlyPod"))
		checkReadFromPath(f, readOnlyPod, fileToWrite, toWrite, seed)
	}

	ginkgo.It("should use pod level credentials", func(ctx context.Context) {
		testVolumeSizeRange := t.GetTestSuiteInfo().SupportedSizeRange
		resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, testVolumeSizeRange)
		l.resources = append(l.resources, resource)
		testPodLevelIdentity(ctx, resource.Pvc)
	})
}

func createPodWithSA(ctx context.Context, client clientset.Interface, namespace string, pvclaims []*v1.PersistentVolumeClaim, serviceAccountName string) (*v1.Pod, error) {
	pod := e2epod.MakePod(namespace, nil, pvclaims, admissionapi.LevelBaseline, "")
	pod.Spec.ServiceAccountName = serviceAccountName
	return createPod(ctx, client, namespace, pod)
}
