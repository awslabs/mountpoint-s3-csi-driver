// This file implements a performance test suite, which validates the S3 CSI driver's I/O
// performance characteristics by running FIO benchmarks against S3 volumes mounted in
// Kubernetes pods.
// This test suite is disabled by default and can be enabled with the --performance flag.
package customsuites

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
	OutputPath    = "test-results/output.json"
	FioCfgPodFile = "/c.fio"
	// For now using AWS recommended Ubuntu image, this might change in the future to
	// use a custom image with FIO pre-installed.
	ubuntuImage = "public.ecr.aws/docker/library/ubuntu:22.04"
)

// s3CSIPerformanceTestSuite implements a test suite for measuring I/O performance
// with the S3 CSI driver using FIO benchmarks. It measures basic read/write throughput
// and validates that concurrent access from multiple pods maintains expected performance.
type s3CSIPerformanceTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3PerformanceTestSuite initializes and returns a test suite for measuring
// I/O performance characteristics of the S3 CSI driver.
//
// This suite runs FIO (Flexible I/O Tester) benchmarks to:
// - Measure sequential read and write throughput
// - Measure random read performance
// - Test performance with concurrent pods accessing the same volume
// - Calculate minimum throughput under load
//
// Results are stored in a JSON output file for later analysis.
func InitS3PerformanceTestSuite() storageframework.TestSuite {
	return &s3CSIPerformanceTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "performance",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

// GetTestSuiteInfo returns information about the test suite.
func (t *s3CSIPerformanceTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

// SkipUnsupportedTests allows test suites to skip certain tests based on driver capabilities.
// For S3 performance tests, all tests should be supported, so this is a no-op.
func (t *s3CSIPerformanceTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

// DefineTests defines all test cases for this test suite.
// It creates the necessary pods with volume mounts and runs FIO benchmarks on them.
func (t *s3CSIPerformanceTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)
	f := framework.NewFrameworkWithCustomTimeouts("performance", storageframework.GetDriverTimeouts(driver))
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

	// getFioCfgNames returns a list of FIO configuration names by reading the directory
	getFioCfgNames := func() []string {
		entries, err := os.ReadDir(FioCfgHostDir)
		framework.ExpectNoError(err)
		names := []string{}
		for _, entry := range entries {
			names = append(names, strings.Replace(entry.Name(), ".fio", "", 1))
		}
		return names
	}

	// writeOutput writes benchmark results to the output file
	writeOutput := func(output []benchmarkEntry) {
		// Create directory if it doesn't exist
		err := os.MkdirAll("test-results", 0755)
		framework.ExpectNoError(err)

		data, err := json.Marshal(output)
		framework.ExpectNoError(err)
		err = os.WriteFile(OutputPath, data, 0644)
		framework.ExpectNoError(err)
	}

	// Test measures baseline I/O throughput from multiple concurrent pods on the same node
	ginkgo.It("should reach baseline io throughput from N=3 concurrent pods on the same node", func(ctx context.Context) {
		// testVolumeSizeRange := t.GetTestSuiteInfo().SupportedSizeRange
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{})
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

// Define FIO result structures
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

// Define benchmark output structures
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
