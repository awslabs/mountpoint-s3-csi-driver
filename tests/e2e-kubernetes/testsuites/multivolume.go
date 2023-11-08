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
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
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

	f := framework.NewFrameworkWithCustomTimeouts("multivolume", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	init := func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
	}
	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
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

	testConcurrentAccessToSingleVolume := func(ctx context.Context, pvcs []*v1.PersistentVolumeClaim, seed int64, doWrite bool) {
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
		init(ctx)
		ginkgo.DeferCleanup(cleanup)

		testVolumeSizeRange := t.GetTestSuiteInfo().SupportedSizeRange
		resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, testVolumeSizeRange)
		l.resources = append(l.resources, resource)
		testTwoPodsSameVolume(ctx, resource.Pvc, true)
	})

	// This tests below configuration:
	//        [pod1] [pod2]
	// [   node1   ] [   node2   ]
	//         \      /
	//         [volume1]
	ginkgo.It("should concurrently access the single volume from pods on different node", func(ctx context.Context) {
		init(ctx)
		ginkgo.DeferCleanup(cleanup)

		testVolumeSizeRange := t.GetTestSuiteInfo().SupportedSizeRange
		resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, testVolumeSizeRange)
		l.resources = append(l.resources, resource)
		testTwoPodsSameVolume(ctx, resource.Pvc, true)
	})

	// This tests below configuration:
	//          [pod1]                            same node       [pod2]
	//      [   node1   ]                           ==>        [   node1   ]
	//          /    \						                      /    \
	//   [volume1]  [volume2]                              [volume1]  [volume2]
	//		/				\								/				\
	// 	[bucket1]		[bucket2]						[bucket1]		[bucket2]
	ginkgo.It("should access to two volumes with the same volume mode and retain data across pod recreation on the same node", func(ctx context.Context) {
		init(ctx)
		ginkgo.DeferCleanup(cleanup)

		var pvcs []*v1.PersistentVolumeClaim
		numVols := 2

		for i := 0; i < numVols; i++ {
			testVolumeSizeRange := t.GetTestSuiteInfo().SupportedSizeRange
			resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, testVolumeSizeRange)
			l.resources = append(l.resources, resource)
			pvcs = append(pvcs, resource.Pvc)
		}
		seed := time.Now().UTC().UnixNano()
		ginkgo.By("Checking read/write works with empty buckets")
		testConcurrentAccessToSingleVolume(ctx, pvcs, seed, true /* doWrite */)
		ginkgo.By("Checking read works from non-empty buckets after pod recreation")
		testConcurrentAccessToSingleVolume(ctx, pvcs, seed, false /* doWrite */)
	})
}

// genBinDataFromSeed generate binData with random seed
func genBinDataFromSeed(len int, seed int64) []byte {
	binData := make([]byte, len)
	rand.Seed(seed)

	_, err := rand.Read(binData)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}

	return binData
}

func checkWriteToPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	data := genBinDataFromSeed(toWrite, seed)
	encoded := base64.StdEncoding.EncodeToString(data)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | sha256sum", encoded))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | dd of=%s bs=%d count=1", encoded, path, toWrite))
	ginkgo.By(fmt.Sprintf("written data with sha: %x", sha256.Sum256(data)))
}

func checkReadFromPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	sum := sha256.Sum256(genBinDataFromSeed(toWrite, seed))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum", path, toWrite))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum | grep -Fq %x", path, toWrite, sum))
}
