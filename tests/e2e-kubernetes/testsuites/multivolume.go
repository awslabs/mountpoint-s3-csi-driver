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
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

type s3CSIMultiVolumeTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIMultiVolumeTestSuite() storageframework.TestSuite {
	return &s3CSIMultiVolumeTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "multivolume",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIMultiVolumeTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIMultiVolumeTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIMultiVolumeTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"multivolume", storageframework.GetDriverTimeouts(driver))
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
	testTwoPodsSameVolume := func(ctx context.Context, pvc *v1.PersistentVolumeClaim, requiresSameNode bool) {
		var pods []*v1.Pod
		node := l.config.ClientNodeSelection
		// Create each pod with pvc
		for i := 0; i < 2; i++ {
			index := i + 1
			ginkgo.By(fmt.Sprintf("Creating pod%d with a volume on %+v", index, node))
			pod, err := e2epod.CreatePod(ctx, f.ClientSet, f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{pvc}, admissionapi.LevelBaseline, "")
			framework.ExpectNoError(err)
			// The pod must get deleted before this function returns because the caller may try to
			// delete volumes as part of the tests. Keeping the pod running would block that.
			// If the test times out, then the namespace deletion will take care of it.
			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			}()
			pods = append(pods, pod)
			actualNodeName := pod.Spec.NodeName
			// Set affinity for the next pod depending on requiresSameNode
			if requiresSameNode {
				e2epod.SetAffinity(&node, actualNodeName)
			} else {
				e2epod.SetAntiAffinity(&node, actualNodeName)
			}
		}

		path := "/mnt/volume1"

		pod1WritesTo := filepath.Join(path, "file1.txt")
		seed := time.Now().UTC().UnixNano()
		checkWriteToPath(f, pods[0], pod1WritesTo, toWrite, seed)
		checkReadFromPath(f, pods[1], pod1WritesTo, toWrite, seed)

		pod2WritesTo := filepath.Join(path, "file2.txt")
		seed = time.Now().UTC().UnixNano()
		checkWriteToPath(f, pods[1], pod2WritesTo, toWrite, seed)
		checkReadFromPath(f, pods[0], pod2WritesTo, toWrite, seed)
	}

	testOnePodTwoVolumes := func(ctx context.Context, pvcs []*v1.PersistentVolumeClaim, seed int64, doWrite bool) {
		node := l.config.ClientNodeSelection
		ginkgo.By(fmt.Sprintf("Creating pod with a volume on %+v", node))
		pod, err := e2epod.CreatePod(ctx, f.ClientSet, f.Namespace.Name, nil, pvcs, admissionapi.LevelBaseline, "")
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()
		for i := 0; i < len(pvcs); i++ {
			fileInVol := fmt.Sprintf("/mnt/volume%d/file.txt", i+1)
			volSeed := seed + int64(i)
			if doWrite {
				ginkgo.By(fmt.Sprintf("Checking write to volume #%d", i))
				checkWriteToPath(f, pod, fileInVol, toWrite, volSeed)
			}
			ginkgo.By(fmt.Sprintf("Checking read from volume #%d", i))
			checkReadFromPath(f, pod, fileInVol, toWrite, volSeed)
		}
	}

	// This tests below configuration:
	// [pod1] [pod2]
	// [   node1   ]
	//   \      /
	//   [volume1]
	ginkgo.It("should concurrently access the single volume from pods on the same node", func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		testTwoPodsSameVolume(ctx, resource.Pvc, true)
	})

	// This tests below configuration:
	//        [pod1] [pod2]
	// [   node1   ] [   node2   ]
	//         \      /
	//         [volume1]
	ginkgo.It("should concurrently access the single volume from pods on different node", func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		testTwoPodsSameVolume(ctx, resource.Pvc, false)
	})

	// This tests below configuration:
	//          [pod1]                            same node       [pod2]
	//      [   node1   ]                           ==>        [   node1   ]
	//          /    \      <- same volume mode                   /    \
	//   [volume1]  [volume2]                              [volume1]  [volume2]
	//		/				\								/				\
	// 	[bucket1]		[bucket2]						[bucket1]		[bucket2]
	ginkgo.It("should access to two volumes with the same volume mode and retain data across pod recreation on the same node", func(ctx context.Context) {
		var pvcs []*v1.PersistentVolumeClaim
		numVols := 2

		for i := 0; i < numVols; i++ {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)
			pvcs = append(pvcs, resource.Pvc)
		}
		seed := time.Now().UTC().UnixNano()
		ginkgo.By("Checking read/write works with empty buckets")
		testOnePodTwoVolumes(ctx, pvcs, seed, true /* doWrite */)
		ginkgo.By("Checking read works from non-empty buckets after pod recreation")
		testOnePodTwoVolumes(ctx, pvcs, seed, false /* doWrite */)
	})
}
