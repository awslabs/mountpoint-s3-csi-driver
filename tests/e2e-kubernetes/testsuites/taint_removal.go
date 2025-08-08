package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"slices"

	. "github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const agentNotReadyTaintKey = "s3.csi.aws.com/agent-not-ready"

type s3CSITaintRemovalTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3TaintRemovalTestSuite() storageframework.TestSuite {
	return &s3CSITaintRemovalTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "taint-removal",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSITaintRemovalTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSITaintRemovalTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSITaintRemovalTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"taint-removal", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	type local struct {
		config *storageframework.PerTestConfig

		// A list of cleanup functions to be called after each test to clean resources created during the test.
		cleanup []func(context.Context) error
	}

	var l local

	deferCleanup := func(f func(context.Context) error) {
		l.cleanup = append(l.cleanup, f)
	}

	cleanup := func(ctx context.Context) {
		var errs []error
		slices.Reverse(l.cleanup) // clean items in reverse order similar to how `defer` works
		for _, f := range l.cleanup {
			errs = append(errs, f(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}

	BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		DeferCleanup(cleanup)
	})

	checkBasicFileOperations := func(ctx context.Context, pod *v1.Pod, volPath string) {
		seed := time.Now().UTC().UnixNano()
		filename := fmt.Sprintf("test-%d.txt", seed)
		path := filepath.Join(volPath, filename)
		testWriteSize := 1024 // 1KB

		checkWriteToPath(ctx, f, pod, path, testWriteSize, seed)
		checkReadFromPath(ctx, f, pod, path, testWriteSize, seed)
		checkListingPathWithEntries(ctx, f, pod, volPath, []string{filename})
		checkDeletingPath(ctx, f, pod, path)
		checkListingPathWithEntries(ctx, f, pod, volPath, []string{})
	}

	Describe("Taint Removal", Ordered, func() {
		BeforeEach(func(ctx context.Context) {
			framework.Logf("Waiting 1 minute for any existing taint watchers to timeout")
			time.Sleep(1 * time.Minute)
		})

		It("should remove agent-not-ready taint and allow workload scheduling", func(ctx context.Context) {
			// 1. Get a node where CSI driver is running
			node := getCSIDriverNode(ctx, f)
			framework.Logf("Selected node %s for taint removal test", node.Name)

			// 2. Apply the taint to the node
			err := applyAgentNotReadyTaint(ctx, f.ClientSet, node.Name)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error {
				return removeAgentNotReadyTaint(ctx, f.ClientSet, node.Name)
			})

			// 3. Verify taint was actually applied
			framework.Logf("Verifying taint was applied to node %s", node.Name)
			err = verifyTaintExists(ctx, f.ClientSet, node.Name, 30*time.Second)
			framework.ExpectNoError(err)

			// 4. Create volume resource
			vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol.CleanupResource)

			// 5. Restart CSI driver to trigger taint watcher
			framework.Logf("Restarting CSI driver pods to trigger taint watcher")
			killCSIDriverPods(ctx, f)

			// Wait for CSI driver pods to be ready again
			waitForCSIDriverReady(ctx, f)

			// 6. Wait for taint removal (should happen within 1 minute)
			framework.Logf("Waiting for taint removal from node %s", node.Name)
			err = waitForTaintRemoval(ctx, f.ClientSet, node.Name, 2*time.Minute)
			framework.ExpectNoError(err)

			// 7. Create and verify pod scheduling on the previously tainted node
			framework.Logf("Creating pod on previously tainted node %s", node.Name)
			pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": node.Name},
				[]*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
			pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

			// 8. Test basic file operations
			framework.Logf("Testing file operations on pod %s", pod.Name)
			checkBasicFileOperations(ctx, pod, e2epod.VolumeMountPath1)
		})

		It("should schedule pending workload after taint removal", func(ctx context.Context) {
			// 1. Get a node and apply taint
			node := getCSIDriverNode(ctx, f)
			framework.Logf("Selected node %s for pending workload test", node.Name)

			err := applyAgentNotReadyTaint(ctx, f.ClientSet, node.Name)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error {
				return removeAgentNotReadyTaint(ctx, f.ClientSet, node.Name)
			})

			// 2. Verify taint was actually applied
			framework.Logf("Verifying taint was applied to node %s", node.Name)
			err = verifyTaintExists(ctx, f.ClientSet, node.Name, 30*time.Second)
			framework.ExpectNoError(err)

			// 3. Create volume resource
			vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol.CleanupResource)

			// 4. Create pod that should be pending due to taint
			framework.Logf("Creating pod that should be pending due to taint on node %s", node.Name)
			pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": node.Name},
				[]*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

			// 5. Verify pod is pending due to taint
			framework.Logf("Verifying pod %s is pending due to taint", pod.Name)
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			if pod.Status.Phase != v1.PodPending {
				framework.Failf("Expected pod %s to be pending due to taint, but it's in phase %s", pod.Name, pod.Status.Phase)
			}

			// 6. Restart CSI driver to trigger taint removal
			framework.Logf("Restarting CSI driver pods to trigger taint removal")
			killCSIDriverPods(ctx, f)

			// Wait for CSI driver pods to be ready again
			waitForCSIDriverReady(ctx, f)

			// 7. Wait for taint removal and pod to become running
			framework.Logf("Waiting for taint removal and pod scheduling")
			err = waitForTaintRemoval(ctx, f.ClientSet, node.Name, 2*time.Minute)
			framework.ExpectNoError(err)

			err = e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
			framework.ExpectNoError(err)

			// 8. Test volume functionality
			framework.Logf("Testing volume functionality on scheduled pod %s", pod.Name)
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			checkBasicFileOperations(ctx, pod, e2epod.VolumeMountPath1)
		})
	})
}

// getCSIDriverNode returns a node where the CSI driver is running
func getCSIDriverNode(ctx context.Context, f *framework.Framework) *v1.Node {
	ds := csiDriverDaemonSet(ctx, f)
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(ds.Spec.Selector),
	})
	framework.ExpectNoError(err)
	if len(pods.Items) == 0 {
		framework.Failf("No CSI driver pods found")
	}

	nodeName := pods.Items[0].Spec.NodeName
	node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	return node
}

// applyAgentNotReadyTaint applies the agent-not-ready taint to the specified node
func applyAgentNotReadyTaint(ctx context.Context, client clientset.Interface, nodeName string) error {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Check if taint already exists
	for _, taint := range node.Spec.Taints {
		if taint.Key == agentNotReadyTaintKey {
			framework.Logf("Taint %s already exists on node %s", agentNotReadyTaintKey, nodeName)
			return nil // Already exists
		}
	}

	// Add the taint
	newTaint := v1.Taint{
		Key:    agentNotReadyTaintKey,
		Effect: v1.TaintEffectNoExecute,
	}
	node.Spec.Taints = append(node.Spec.Taints, newTaint)

	_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to apply taint to node %s: %w", nodeName, err)
	}

	framework.Logf("Applied taint %s to node %s", agentNotReadyTaintKey, nodeName)
	return nil
}

// removeAgentNotReadyTaint removes the agent-not-ready taint from the specified node
func removeAgentNotReadyTaint(ctx context.Context, client clientset.Interface, nodeName string) error {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Filter out the agent-not-ready taint
	var taintsToKeep []v1.Taint
	taintRemoved := false
	for _, taint := range node.Spec.Taints {
		if taint.Key != agentNotReadyTaintKey {
			taintsToKeep = append(taintsToKeep, taint)
		} else {
			taintRemoved = true
		}
	}

	if !taintRemoved {
		framework.Logf("Taint %s not found on node %s, nothing to remove", agentNotReadyTaintKey, nodeName)
		return nil
	}

	node.Spec.Taints = taintsToKeep
	_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove taint from node %s: %w", nodeName, err)
	}

	framework.Logf("Removed taint %s from node %s", agentNotReadyTaintKey, nodeName)
	return nil
}

// waitForTaintRemoval waits for the agent-not-ready taint to be removed from the specified node
func waitForTaintRemoval(ctx context.Context, client clientset.Interface, nodeName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		taintFound := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == agentNotReadyTaintKey {
				taintFound = true
				break
			}
		}

		if !taintFound {
			framework.Logf("Taint %s successfully removed from node %s", agentNotReadyTaintKey, nodeName)
			return nil // Taint removed
		}

		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("timeout waiting for taint %s to be removed from node %s", agentNotReadyTaintKey, nodeName)
}

// verifyTaintExists verifies that the agent-not-ready taint exists on the specified node with retry logic
func verifyTaintExists(ctx context.Context, client clientset.Interface, nodeName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		for _, taint := range node.Spec.Taints {
			if taint.Key == agentNotReadyTaintKey {
				framework.Logf("Verified taint %s exists on node %s", agentNotReadyTaintKey, nodeName)
				return nil
			}
		}

		framework.Logf("Taint %s not yet found on node %s, retrying...", agentNotReadyTaintKey, nodeName)
		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("timeout waiting for taint %s to appear on node %s", agentNotReadyTaintKey, nodeName)
}

// waitForCSIDriverReady waits for CSI driver pods to be ready after restart
func waitForCSIDriverReady(ctx context.Context, f *framework.Framework) {
	framework.Logf("Waiting for CSI driver pods to be ready")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		ds := csiDriverDaemonSet(ctx, f)
		if ds.Status.NumberReady == ds.Status.DesiredNumberScheduled {
			framework.Logf("CSI driver pods are ready")
			return
		}
		time.Sleep(5 * time.Second)
	}
	framework.Failf("Timeout waiting for CSI driver pods to be ready")
}
