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
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	iamRoleS3FullAccess     = "arn:aws:iam::aws:policy/AmazonS3FullAccess"
	iamRoleS3ReadOnlyAccess = "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"
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

func (t *s3CSICredentialsTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSICredentialsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var l local

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"credentials", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})
	toWrite := 1024 // 1KB

	type writtenFile struct {
		path string
		seed int64
	}

	expectWriteToSucceed := func(pod *v1.Pod) writtenFile {
		seed := time.Now().UTC().UnixNano()
		path := "/mnt/volume1/file.txt"
		ginkgo.By(fmt.Sprintf("checking writing to %s", path))
		checkWriteToPath(f, pod, path, toWrite, seed)
		return writtenFile{path, seed}
	}

	expectReadToSucceed := func(pod *v1.Pod, file writtenFile) {
		ginkgo.By(fmt.Sprintf("checking reading from %s", file.path))
		checkReadFromPath(f, pod, file.path, toWrite, file.seed)
	}

	expectWriteToFail := func(pod *v1.Pod) {
		seed := time.Now().UTC().UnixNano()
		path := "/mnt/volume1/file.txt"
		ginkgo.By(fmt.Sprintf("checking if writing to %s fails", path))
		checkWriteToPathFails(f, pod, path, toWrite, seed)
	}

	ginkgo.Context("Driver level", func() {
		// TODO: Add tests for current functionality.
	})

	ginkgo.Context("Pod level", func() {
		ginkgo.Context("should use correct credentials", func() {
			ginkgo.It("full access role", func(ctx context.Context) {
				sa, deleteSA := createServiceAccount(ctx, f, "s3-csi-e2e-sa", annotateServiceAccountWithRole(iamRoleS3FullAccess))
				defer deleteSA(ctx)

				resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, t.GetTestSuiteInfo().SupportedSizeRange)
				l.resources = append(l.resources, resource)

				pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, sa.Name)
				framework.ExpectNoError(err)
				defer func() {
					framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
				}()

				writtenFile := expectWriteToSucceed(pod)
				expectReadToSucceed(pod, writtenFile)
			})

			ginkgo.It("read-only role", func(ctx context.Context) {
				sa, deleteSA := createServiceAccount(ctx, f, "s3-csi-e2e-sa", annotateServiceAccountWithRole(iamRoleS3ReadOnlyAccess))
				defer deleteSA(ctx)

				resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, t.GetTestSuiteInfo().SupportedSizeRange)
				l.resources = append(l.resources, resource)

				pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, sa.Name)
				framework.ExpectNoError(err)
				defer func() {
					framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
				}()

				expectWriteToFail(pod)
			})
		})

		ginkgo.It("should refresh credentials after receiving new tokens", func(ctx context.Context) {
			// TODO:
			// 1. Trigger a manual `TokenRequest` or wait for it's own lifecylce
			// 2. Assert new token file is written to the Pod
		})

		ginkgo.It("should use up to date role associated with service account", func(ctx context.Context) {
			// Create a SA with full access role
			sa, deleteSA := createServiceAccount(ctx, f, "s3-csi-e2e-sa", annotateServiceAccountWithRole(iamRoleS3FullAccess))
			defer deleteSA(ctx)

			resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, t.GetTestSuiteInfo().SupportedSizeRange)
			l.resources = append(l.resources, resource)

			pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, sa.Name)
			framework.ExpectNoError(err)

			writtenFile := expectWriteToSucceed(pod)
			expectReadToSucceed(pod, writtenFile)

			// Associate SA with read-only access role
			saClient := f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name)
			annotateServiceAccountWithRole(iamRoleS3ReadOnlyAccess)(sa)
			_, err = saClient.Update(ctx, sa, metav1.UpdateOptions{})
			framework.ExpectNoError(err)

			// Re-create the pod
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			pod, err = createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, sa.Name)
			framework.ExpectNoError(err)
			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			}()

			// The pod should only have a read-only access now
			expectReadToSucceed(pod, writtenFile)
			expectWriteToFail(pod)
		})

		ginkgo.It("should fail if service account does not have an associated role", func(ctx context.Context) {
			// TODO: How this should fail?
		})

		ginkgo.It("should not use csi driver's service account tokens", func(ctx context.Context) {
			driverSA := getCSIDriverServiceAccount(ctx, f)
			restoreDriverSA := overrideServiceAccountRole(ctx, f, driverSA, iamRoleS3FullAccess)
			defer restoreDriverSA(ctx)

			sa, deleteSA := createServiceAccount(ctx, f, "s3-csi-e2e-sa", annotateServiceAccountWithRole(iamRoleS3ReadOnlyAccess))
			defer deleteSA(ctx)

			resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, t.GetTestSuiteInfo().SupportedSizeRange)
			l.resources = append(l.resources, resource)

			pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, sa.Name)
			framework.ExpectNoError(err)
			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			}()

			expectWriteToFail(pod)
		})

		ginkgo.It("should not use mix different pod's service account tokens", func(ctx context.Context) {
			saFullAccess, deleteSAFullAccess := createServiceAccount(ctx, f, "s3-csi-e2e-sa", annotateServiceAccountWithRole(iamRoleS3FullAccess))
			defer deleteSAFullAccess(ctx)

			saReadOnlyAccess, deleteSAReadOnlyAccess := createServiceAccount(ctx, f, "s3-csi-e2e-read-only-sa", annotateServiceAccountWithRole(iamRoleS3ReadOnlyAccess))
			defer deleteSAReadOnlyAccess(ctx)

			resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, t.GetTestSuiteInfo().SupportedSizeRange)
			l.resources = append(l.resources, resource)

			podFullAccess, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, saFullAccess.Name)
			framework.ExpectNoError(err)
			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, podFullAccess))
			}()

			podReadOnlyAccess, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, saReadOnlyAccess.Name)
			framework.ExpectNoError(err)
			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, podReadOnlyAccess))
			}()

			writtenFile := expectWriteToSucceed(podFullAccess)
			expectReadToSucceed(podFullAccess, writtenFile)

			expectWriteToFail(podReadOnlyAccess)
		})

		ginkgo.It("should not use pod's service account role if 'authenticationSource' is 'driver'", func(ctx context.Context) {
			// TODO:
			// 1. Associate Pod's service account with S3 Full Access role
			// 2. Associate driver's service account with S3 Read-only Access role
			// 3. Assert write fails but read succeeds
		})

		ginkgo.It("should not use pod's service account role if 'authenticationSource' is not explicitly set to 'pod'", func(ctx context.Context) {
			// TODO:
			// 1. Associate Pod's service account with S3 Full Access role
			// 2. Associate driver's service account with S3 Read-only Access role
			// 3. Assert write fails but read succeeds
		})
	})
}
