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
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

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
	FioCfgHostDir = "fio/"
	OutputPath    = "csi-test-artifacts/output.json"
	FioCfgPodFile = "/c.fio"
	ubuntuImage   = "public.ecr.aws/docker/library/ubuntu:22.04"
)

type s3CSIPerformanceTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIPerformanceTestSuite() storageframework.TestSuite {
	return &s3CSIPerformanceTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "performance",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIPerformanceTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIPerformanceTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIPerformanceTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"performance", storageframework.GetDriverTimeouts(driver))
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

	getFioCfgNames := func() []string {
		entries, err := os.ReadDir(FioCfgHostDir)
		framework.ExpectNoError(err)
		names := []string{}
		for _, entry := range entries {
			names = append(names, strings.Replace(entry.Name(), ".fio", "", 1))
		}
		return names
	}

	writeOutput := func(output []benchmarkEntry) {
		data, err := json.Marshal(output)
		framework.ExpectNoError(err)
		err = os.WriteFile(OutputPath, data, 0644)
		framework.ExpectNoError(err)
	}

	ginkgo.It("should reach baseline io throughput from N=3 concurrent pods on the same node", func(ctx context.Context) {
		testVolumeSizeRange := t.GetTestSuiteInfo().SupportedSizeRange
		resource := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, testVolumeSizeRange)
		l.resources = append(l.resources, resource)

		const podsNum = 3
		var pods []*v1.Pod
		nodeName := ""
		for i := 0; i < podsNum; i++ {
			index := i + 1
			ginkgo.By(fmt.Sprintf("Creating pod%d with a volume on %+v", index, nodeName))
			nodeSelector := make(map[string]string)
			if nodeName != "" {
				nodeSelector["kubernetes.io/hostname"] = nodeName
			}
			pod := e2epod.MakePod(f.Namespace.Name, nodeSelector, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod.Spec.Containers[0].Image = ubuntuImage
			var err error
			pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			pods = append(pods, pod)
			if nodeName == "" {
				nodeName = pod.Spec.NodeName
			} else {
				gomega.Expect(nodeName).To(gomega.Equal(pod.Spec.NodeName))
			}
		}

		ginkgo.By("Installing fio in pods")
		var wg sync.WaitGroup
		wg.Add(podsNum)
		for i := 0; i < podsNum; i++ {
			go func(podId int) {
				defer ginkgo.GinkgoRecover()
				defer wg.Done()
				e2evolume.VerifyExecInPodSucceed(f, pods[podId], "apt-get update && apt-get install fio -y")
			}(i)
		}
		wg.Wait()

		var output []benchmarkEntry
		for _, cfgName := range getFioCfgNames() {
			ginkgo.By(fmt.Sprintf("Running benchmark with config: %s", cfgName))
			for i := 0; i < podsNum; i++ {
				copySmallFileToPod(ctx, f, pods[i], FioCfgHostDir+cfgName+".fio", FioCfgPodFile)
			}
			throughputs := make([]float32, podsNum)
			var wg sync.WaitGroup
			wg.Add(podsNum)
			for i := 0; i < podsNum; i++ {
				go func(podId int) {
					defer ginkgo.GinkgoRecover()
					defer wg.Done()
					stdout, stderr, err := e2evolume.PodExec(f, pods[podId], fmt.Sprintf("FILENAME=/mnt/volume1/%s_%d fio %s --output-format=json", cfgName, podId, FioCfgPodFile))
					if err != nil {
						fmt.Printf("pod%d: [%s] [%s] [%s] [%v]", podId, cfgName, stdout, stderr, err)
					}
					framework.ExpectNoError(err)
					var fioResult fioResult
					framework.ExpectNoError(json.Unmarshal([]byte(stdout), &fioResult))
					var throughputMB float32
					if strings.Contains(cfgName, "read") {
						throughputMB = fioResult.Jobs[0].ReadMetric.BwBytes / 1024 / 1024
					} else {
						throughputMB = fioResult.Jobs[0].WriteMetric.BwBytes / 1024 / 1024
					}
					throughputs[podId] = throughputMB
				}(i)
			}
			wg.Wait()
			output = append(output, newBenchmarkEntry(cfgName, slices.Min(throughputs)))
		}
		writeOutput(output)
	})
}

type metrics struct {
	BwBytes float32 `json:"bw_bytes"`
}

type fioJob struct {
	ReadMetric  metrics `json:"read"`
	WriteMetric metrics `json:"write"`
}

type fioResult struct {
	Jobs []fioJob `json:"jobs"`
}

type benchmarkEntry struct {
	Name  string `json:"name"`
	Unit  string `json:"unit"`
	Value string `json:"value"`
}

func newBenchmarkEntry(name string, value float32) benchmarkEntry {
	return benchmarkEntry{
		Name:  name,
		Unit:  "MiB/s",
		Value: fmt.Sprintf("%f", value),
	}
}
