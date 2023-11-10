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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/pointer"
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
	ginkgo.It("should access volume as a non-root user", func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"uid=1000", "gid=2000", "allow-other"})
		l.resources = append(l.resources, resource)
		ginkgo.By("Creating pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		pod.Spec.SecurityContext.RunAsGroup = pointer.Int64(2000)
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
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '644 2000 1000'", fileInVol))
		ginkgo.By("Checking dir group owner")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '755 2000 1000'", volPath))
		ginkgo.By("Checking pod identity")
		e2evolume.VerifyExecInPodSucceed(f, pod, "id | grep 'uid=1000 gid=2000 groups=2000'")
	})
}
