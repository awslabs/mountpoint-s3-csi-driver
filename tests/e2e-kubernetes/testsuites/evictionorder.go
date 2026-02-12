package custom_testsuites

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	evictionOrderTestName = "evictionorder"
	volumeNameAnnotation  = "s3.csi.aws.com/volume-name"
	workloadPodCount      = 2
	readCheckAfterSeconds = 10
)

type s3CSIEvictionOrderTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3CSIEvictionOrderTestSuite initializes the eviction order test suite
func InitS3CSIEvictionOrderTestSuite() storageframework.TestSuite {
	return &s3CSIEvictionOrderTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: evictionOrderTestName,
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIEvictionOrderTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIEvictionOrderTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIEvictionOrderTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(
		NamespacePrefix+evictionOrderTestName,
		storageframework.GetDriverTimeouts(driver),
	)
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.It("should handle SIGTERM correctly during pod eviction", func(ctx context.Context) {
		config := driver.PrepareTest(ctx, f)
		vol := createVolumeResourceWithAccessMode(ctx, config, pattern, v1.ReadWriteMany)
		defer func(ctx context.Context, vol *storageframework.VolumeResource) {
			vol.CleanupResource(ctx)
		}(ctx, vol)

		// Deploy workload pods that do nothing
		workloadPods := make([]*v1.Pod, 0, workloadPodCount)
		for range workloadPodCount {
			pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
			pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			defer func() {
				_ = e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
			}()
			workloadPods = append(workloadPods, pod)
		}

		// Write a file to the bucket with some content (later we'll test reading it)
		seed := time.Now().UTC().UnixNano()
		toWrite := 1024
		filePath := path.Join(e2epod.VolumeMountPath1, "file.txt")
		err := checkWriteToPath(ctx, f, workloadPods[0], filePath, toWrite, seed)
		framework.ExpectNoError(err)

		// Find the Mountpoint pods associated with our volume
		mpPods, err := findMountpointPods(ctx, f.ClientSet, vol.Pv.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to find Mountpoint pods")

		// Trigger MP pod deletions
		for _, mpPod := range mpPods {
			if err := f.ClientSet.CoreV1().Pods(mountpointNamespace).Delete(ctx, mpPod.Name, metav1.DeleteOptions{}); err != nil {
				framework.ExpectNoError(err)
			}
		}

		// Wait briefly to ensure MP pod gracefully handles SIGTERM
		time.Sleep(readCheckAfterSeconds * time.Second)

		// Check we can still use the mount insde the workload
		for _, pod := range workloadPods {
			err = checkReadFromPath(ctx, f, pod, filePath, toWrite, seed)
			framework.ExpectNoError(err)
		}
	})
}

// findMountpointPods locates all Mountpoint pods for a specific volume on a node
func findMountpointPods(ctx context.Context, cs clientset.Interface, volumeName string) ([]*v1.Pod, error) {
	pods, err := cs.CoreV1().Pods(mountpointNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in %s namespace: %w", mountpointNamespace, err)
	}

	var matchingPods []*v1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[volumeNameAnnotation] == volumeName {
			matchingPods = append(matchingPods, pod)
		}
	}

	if len(matchingPods) == 0 {
		return nil, fmt.Errorf("no Mountpoint pods found for volume %s", volumeName)
	}

	return matchingPods, nil
}
