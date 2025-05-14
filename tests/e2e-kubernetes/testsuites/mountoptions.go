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

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	S3ExpressTestIdentifier = "express"
)

type s3CSIMountOptionsTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3MountOptionsTestSuite() storageframework.TestSuite {
	return &s3CSIMountOptionsTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "mountoptions",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIMountOptionsTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIMountOptionsTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIMountOptionsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)

	f := framework.NewFrameworkWithCustomTimeouts("mountoptions", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelRestricted

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

	ginkgo.It("should add debug MountOptions in createVolumeResource", func(ctx context.Context) {
		resource := createVolumeResource(ctx, l.config, pattern, v1.ReadWriteMany, []string{
			"allow-other",
		})
		l.resources = append(l.resources, resource)
		expectedMountOptions := []string{
			"--allow-other",
			"--debug",
			"--debug-crt",
		}
		gomega.Expect(resource.Pv.Spec.MountOptions).To(gomega.Equal(expectedMountOptions))

		resource = createVolumeResource(ctx, l.config, pattern, v1.ReadWriteMany, []string{
			"allow-other",
			"--debug",
			"--debug-crt",
		})
		l.resources = append(l.resources, resource)
		expectedMountOptions = []string{
			"--allow-other",
			"--debug",
			"--debug-crt",
		}
		gomega.Expect(resource.Pv.Spec.MountOptions).To(gomega.Equal(expectedMountOptions))

		resource = createVolumeResource(ctx, l.config, pattern, v1.ReadWriteMany, []string{
			"allow-other",
			"debug",
			"debug-crt",
		})
		l.resources = append(l.resources, resource)
		expectedMountOptions = []string{
			"--allow-other",
			"--debug",
			"--debug-crt",
		}
		gomega.Expect(resource.Pv.Spec.MountOptions).To(gomega.Equal(expectedMountOptions))
	})

	validateWriteToVolume := func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", defaultNonRootUser),
			fmt.Sprintf("gid=%d", defaultNonRootGroup),
			"allow-other",
			"debug",
			"debug-crt",
		})
		l.resources = append(l.resources, resource)
		ginkgo.By("Creating pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()
		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/file.txt", volPath)
		seed := time.Now().UTC().UnixNano()
		toWrite := 1024 // 1KB
		ginkgo.By("Checking write to a volume")
		checkWriteToPath(f, pod, fileInVol, toWrite, seed)
		ginkgo.By("Checking read from a volume")
		checkReadFromPath(f, pod, fileInVol, toWrite, seed)
		ginkgo.By("Checking file group owner")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '644 %d %d'", fileInVol, defaultNonRootGroup, defaultNonRootUser))
		ginkgo.By("Checking dir group owner")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '755 %d %d'", volPath, defaultNonRootGroup, defaultNonRootUser))
		ginkgo.By("Checking pod identity")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("id | grep 'uid=%d gid=%d groups=%d'", defaultNonRootUser, defaultNonRootGroup, defaultNonRootGroup))
	}
	ginkgo.It("should access volume as a non-root user", func(ctx context.Context) {
		validateWriteToVolume(ctx)
	})
	ginkgo.It("S3 express -- should access volume as a non-root user", func(ctx context.Context) {
		l.config.Prefix = S3ExpressTestIdentifier
		validateWriteToVolume(ctx)
	})

	accessVolAsNonRootUser := func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{})
		l.resources = append(l.resources, resource)
		ginkgo.By("Creating pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()
		volPath := "/mnt/volume1"
		ginkgo.By("Checking file group owner")
		_, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("ls %s", volPath))
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(stderr).To(gomega.ContainSubstring("Permission denied"))
	}
	ginkgo.It("should not be able to access volume as a non-root user", func(ctx context.Context) {
		accessVolAsNonRootUser(ctx)
	})
	ginkgo.It("S3 express -- should not be able to access volume as a non-root user", func(ctx context.Context) {
		l.config.Prefix = S3ExpressTestIdentifier
		accessVolAsNonRootUser(ctx)
	})
}
