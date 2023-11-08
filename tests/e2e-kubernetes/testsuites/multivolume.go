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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	testTwoPodsSameVolume := func(ctx context.Context, pvc *v1.PersistentVolumeClaim, requiresSameNode bool) {
		var pods []*v1.Pod
		node := l.config.ClientNodeSelection
		cs := f.ClientSet
		// Create each pod with pvc
		for i := 0; i < 2; i++ {
			index := i + 1
			ginkgo.By(fmt.Sprintf("Creating pod%d with a volume on %+v", index, node))
			pod, err := e2epod.CreatePod(ctx, cs, f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{pvc}, admissionapi.LevelBaseline, "")
			framework.ExpectNoError(err)
			// The pod must get deleted before this function returns because the caller may try to
			// delete volumes as part of the tests. Keeping the pod running would block that.
			// If the test times out, then the namespace deletion will take care of it.
			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, cs, pod))
			}()
			pod, err = cs.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			pods = append(pods, pod)
			framework.ExpectNoError(err, fmt.Sprintf("get pod%d", index))
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
		pod2WritesTo := filepath.Join(path, "file2.txt")
		pod1Wrote := checkWriteToPath(f, pods[0], pod1WritesTo)
		checkReadFromPath(f, pods[1], pod1WritesTo, pod1Wrote)
		pod2Wrote := checkWriteToPath(f, pods[1], pod2WritesTo)
		checkReadFromPath(f, pods[0], pod2WritesTo, pod2Wrote)
	}

	// This tests below configuration:
	// [pod1] [pod2]
	// [   node1   ]
	//   \      /     <- same volume mode
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
	//         \      /     <- same volume mode
	//         [volume1]
	ginkgo.It("should concurrently access the single volume from pods on different node", func(ctx context.Context) {
		init(ctx)
		ginkgo.DeferCleanup(cleanup)

		testVolumeSizeRange := t.GetTestSuiteInfo().SupportedSizeRange
		resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, testVolumeSizeRange)
		l.resources = append(l.resources, resource)
		testTwoPodsSameVolume(ctx, resource.Pvc, true)
	})
}

// genBinDataFromSeed generate binData
func genBinDataFromSeed(len int) []byte {
	binData := make([]byte, len)

	_, err := rand.Read(binData)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}

	return binData
}

func checkWriteToPath(f *framework.Framework, pod *v1.Pod, path string) []byte {
	toWrite := 1024 // 1KB
	data := genBinDataFromSeed(toWrite)
	encoded := base64.StdEncoding.EncodeToString(data)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | sha256sum", encoded))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | dd of=%s bs=%d count=1", encoded, path, len(data)))
	return data
}

func checkReadFromPath(f *framework.Framework, pod *v1.Pod, path string, data []byte) {
	sum := sha256.Sum256(data)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum", path, len(data)))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum | grep -Fq %x", path, len(data), sum))
}
