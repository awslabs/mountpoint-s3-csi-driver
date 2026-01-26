package custom_testsuites

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	// Test configuration
	evictionOrderTestName = "evictionorder"
	volumeNameAnnotation  = "s3.csi.aws.com/volume-name"
	transportErrorString  = "Transport endpoint is not connected"

	// Pod configuration
	workloadPodCount              = 5
	terminationGracePeriodSeconds = 120
	cleanupWaitDuration           = 30 * time.Second

	// Tracking configuration
	deletionTrackingTimeout = 650 * time.Second // 600s grace period + 50s buffer
	logCheckInterval        = 1 * time.Second

	// Container configuration
	containerName  = "app"
	containerImage = "ubuntu"
	volumeName     = "persistent-storage"
	mountPath      = "/data"
)

// workloadScript is the shell script that runs in workload pods
// It traps SIGTERM and continues writing to test graceful shutdown behavior
const workloadScript = `trap 'echo "[SIGTERM] Received, continuing to write..."' TERM
while true; do
  ERR=$(echo 'Hello from the container!' >> /data/$(hostname)-$(date -u).txt 2>&1)
  [ $? -ne 0 ] && echo "WRITE FAILED: $ERR"
  sleep 1
done`

// deletionTracker holds the state for tracking pod deletions
type deletionTracker struct {
	timestamps      map[string]time.Time
	transportErrors map[string]bool
	lastLogCheck    map[string]time.Time
}

func newDeletionTracker() *deletionTracker {
	return &deletionTracker{
		timestamps:      make(map[string]time.Time),
		transportErrors: make(map[string]bool),
		lastLogCheck:    make(map[string]time.Time),
	}
}

// testResult holds the results of the SIGTERM test verification
type testResult struct {
	mpDeletedLast      bool
	hasTransportErrors bool
	failedPods         []string
}

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
		defer cleanupVolume(ctx, vol)

		// Deploy workload pods that continuously write to the volume
		workloadPods, err := deployWorkloadPods(ctx, f.ClientSet, f.Namespace.Name, vol.Pvc.Name, workloadPodCount)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to deploy workload pods")
		gomega.Expect(workloadPods).To(gomega.HaveLen(workloadPodCount))

		// Find the Mountpoint pod associated with our volume
		mpPod, err := findMountpointPod(ctx, f.ClientSet, vol.Pv.Name, workloadPods[0].Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to find Mountpoint pod")
		framework.Logf("Found Mountpoint pod: %s", mpPod.Name)

		// Trigger pod deletions
		deletePods(ctx, f.ClientSet, f.Namespace.Name, workloadPods, mpPod)

		// Track deletions and check for errors
		tracker := trackDeletions(ctx, f.ClientSet, f.Namespace.Name, workloadPods, mpPod)

		// Verify test results
		result := verifyTestResults(tracker, workloadPods, mpPod)
		logTestResults(result)

		gomega.Expect(result.mpDeletedLast).To(gomega.BeTrue(),
			"Mountpoint pod should be deleted after all workload pods")
		gomega.Expect(result.hasTransportErrors).To(gomega.BeFalse(),
			"No transport endpoint errors should occur: failed pods: %v", result.failedPods)
	})
}

// deployWorkloadPods creates the specified number of workload pods
func deployWorkloadPods(ctx context.Context, cs clientset.Interface, namespace, pvcName string, count int) ([]*v1.Pod, error) {
	pods := make([]*v1.Pod, 0, count)

	for i := 0; i < count; i++ {
		podSpec := buildWorkloadPodSpec(pvcName)
		pod, err := createPod(ctx, cs, namespace, podSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to create workload pod %d: %w", i, err)
		}
		pods = append(pods, pod)
	}

	return pods, nil
}

// buildWorkloadPodSpec creates the pod specification for a workload pod
func buildWorkloadPodSpec(pvcName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "s3-test-workload-",
		},
		Spec: v1.PodSpec{
			TerminationGracePeriodSeconds: int64Ptr(terminationGracePeriodSeconds),
			SecurityContext: &v1.PodSecurityContext{
				RunAsUser:  int64Ptr(0),
				RunAsGroup: int64Ptr(0),
			},
			Containers: []v1.Container{
				{
					Name:    containerName,
					Image:   containerImage,
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{workloadScript},
					SecurityContext: &v1.SecurityContext{
						RunAsUser:  int64Ptr(0),
						RunAsGroup: int64Ptr(0),
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName,
							MountPath: mountPath,
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
}

// findMountpointPod locates the Mountpoint pod for a specific volume on a node
func findMountpointPod(ctx context.Context, cs clientset.Interface, volumeName, nodeName string) (*v1.Pod, error) {
	pods, err := cs.CoreV1().Pods(mountpointNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in %s namespace: %w", mountpointNamespace, err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[volumeNameAnnotation] == volumeName && pod.Spec.NodeName == nodeName {
			return pod, nil
		}
	}

	return nil, fmt.Errorf("no Mountpoint pod found for volume %s on node %s", volumeName, nodeName)
}

// deletePods initiates deletion of all pods (mountpoint and workload)
func deletePods(ctx context.Context, cs clientset.Interface, namespace string, workloadPods []*v1.Pod, mpPod *v1.Pod) {
	// Delete Mountpoint pod first to test graceful handling
	if err := cs.CoreV1().Pods(mountpointNamespace).Delete(ctx, mpPod.Name, metav1.DeleteOptions{}); err != nil {
		framework.Logf("Warning: failed to delete Mountpoint pod %s: %v", mpPod.Name, err)
	}

	// Delete all workload pods
	for _, pod := range workloadPods {
		if err := cs.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			framework.Logf("Warning: failed to delete workload pod %s: %v", pod.Name, err)
		}
	}
}

// trackDeletions monitors pod deletions and checks for transport errors
func trackDeletions(ctx context.Context, cs clientset.Interface, namespace string, workloadPods []*v1.Pod, mpPod *v1.Pod) *deletionTracker {
	tracker := newDeletionTracker()
	deadline := time.Now().Add(deletionTrackingTimeout)
	expectedDeletions := len(workloadPods) + 1 // workload pods + mountpoint pod

	for time.Now().Before(deadline) {
		trackWorkloadPods(ctx, cs, namespace, workloadPods, tracker)
		trackMountpointPod(ctx, cs, mpPod, tracker)

		if len(tracker.timestamps) == expectedDeletions {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	return tracker
}

// trackWorkloadPods checks the status of workload pods and records deletions/errors
func trackWorkloadPods(ctx context.Context, cs clientset.Interface, namespace string, pods []*v1.Pod, tracker *deletionTracker) {
	for _, pod := range pods {
		if _, tracked := tracker.timestamps[pod.Name]; tracked {
			continue
		}

		p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			// Pod not found - it's been deleted
			tracker.timestamps[pod.Name] = time.Now()
			framework.Logf("Pod %s deleted at %s", pod.Name, tracker.timestamps[pod.Name])
			continue
		}

		if isPodTerminated(p) {
			tracker.timestamps[pod.Name] = time.Now()
			framework.Logf("Pod %s terminated with phase %s at %s", pod.Name, p.Status.Phase, tracker.timestamps[pod.Name])
			continue
		}

		// Check for transport errors if not already found
		if !tracker.transportErrors[pod.Name] && shouldCheckLogs(tracker.lastLogCheck[pod.Name]) {
			if hasTransportError(ctx, cs, namespace, pod.Name, tracker.lastLogCheck[pod.Name]) {
				tracker.transportErrors[pod.Name] = true
				framework.Logf("Transport error detected in pod %s", pod.Name)
			}
			tracker.lastLogCheck[pod.Name] = time.Now()
		}
	}
}

// trackMountpointPod checks if the mountpoint pod has been deleted
func trackMountpointPod(ctx context.Context, cs clientset.Interface, mpPod *v1.Pod, tracker *deletionTracker) {
	if _, tracked := tracker.timestamps[mpPod.Name]; tracked {
		return
	}

	_, err := cs.CoreV1().Pods(mountpointNamespace).Get(ctx, mpPod.Name, metav1.GetOptions{})
	if err != nil {
		tracker.timestamps[mpPod.Name] = time.Now()
		framework.Logf("Mountpoint pod %s deleted at %s", mpPod.Name, tracker.timestamps[mpPod.Name])
	}
}

// isPodTerminated checks if a pod has reached a terminal state
func isPodTerminated(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded
}

// shouldCheckLogs determines if enough time has passed to check logs again
func shouldCheckLogs(lastCheck time.Time) bool {
	return time.Since(lastCheck) > logCheckInterval
}

// hasTransportError checks pod logs for transport endpoint errors
func hasTransportError(ctx context.Context, cs clientset.Interface, namespace, podName string, sinceTime time.Time) bool {
	opts := &v1.PodLogOptions{}
	if !sinceTime.IsZero() {
		opts.SinceTime = &metav1.Time{Time: sinceTime}
	}

	req := cs.CoreV1().Pods(namespace).GetLogs(podName, opts)
	logs, err := req.Stream(ctx)
	if err != nil {
		return false
	}
	defer logs.Close()

	logBytes, err := io.ReadAll(logs)
	if err != nil || len(logBytes) == 0 {
		return false
	}

	return strings.Contains(string(logBytes), transportErrorString)
}

// verifyTestResults analyzes the tracking data to determine test success/failure
func verifyTestResults(tracker *deletionTracker, workloadPods []*v1.Pod, mpPod *v1.Pod) testResult {
	result := testResult{
		mpDeletedLast: true,
		failedPods:    make([]string, 0),
	}

	mpDeletionTime, mpTracked := tracker.timestamps[mpPod.Name]
	if !mpTracked {
		result.mpDeletedLast = false
		framework.Logf("WARNING: Mountpoint pod deletion was not tracked")
	}

	for _, pod := range workloadPods {
		// Only check pods on the same node as the Mountpoint pod
		if pod.Spec.NodeName != mpPod.Spec.NodeName {
			continue
		}

		// Check deletion order
		if mpTracked {
			if workloadDeletionTime, ok := tracker.timestamps[pod.Name]; ok {
				if mpDeletionTime.Before(workloadDeletionTime) {
					result.mpDeletedLast = false
					framework.Logf("FAILED: Mountpoint pod deleted before workload pod %s", pod.Name)
				}
			}
		}

		// Check for transport errors
		if tracker.transportErrors[pod.Name] {
			result.hasTransportErrors = true
			result.failedPods = append(result.failedPods, pod.Name)
		}
	}

	return result
}

// logTestResults outputs the final test results
func logTestResults(result testResult) {
	if result.mpDeletedLast {
		framework.Logf("SUCCESS: Mountpoint pod deleted last")
	} else {
		framework.Logf("FAILED: Mountpoint pod was not deleted last")
	}

	if !result.hasTransportErrors {
		framework.Logf("SUCCESS: No transport errors detected")
	} else {
		framework.Logf("FAILED: Transport errors detected in pods: %v", result.failedPods)
	}
}

// cleanupVolume waits for cleanup and removes the volume resource
func cleanupVolume(ctx context.Context, vol *storageframework.VolumeResource) {
	time.Sleep(cleanupWaitDuration)
	vol.CleanupResource(ctx)
}

// int64Ptr returns a pointer to an int64 value
func int64Ptr(i int64) *int64 {
	return &i
}
