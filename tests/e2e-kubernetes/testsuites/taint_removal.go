package custom_testsuites

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"slices"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/errors"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	agentNotReadyTaintKey = "s3.csi.aws.com/agent-not-ready"
	mounterDaemonSetName  = "s3-csi-daemonset-mounter"
)

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

		checkWriteToPathSucceed(ctx, f, pod, path, testWriteSize, seed)
		checkReadFromPathSucceed(ctx, f, pod, path, testWriteSize, seed)
		checkListingPathWithEntries(ctx, f, pod, volPath, []string{filename})
		checkDeletingPathSucceed(ctx, f, pod, path)
		checkListingPathWithEntries(ctx, f, pod, volPath, []string{})
	}

	// Since we're modifying cluster-wide resources in credential tests,
	// we shouldn't run them in parallel with other tests.
	//                          |
	//                        ------
	Describe("Taint Removal", Serial, func() {
		It("should remove agent-not-ready taint once the driver is ready and allow workload scheduling", func(ctx context.Context) {
			// 1. Get a node where CSI driver is running
			node := getCSIDriverNode(ctx, f)
			framework.Logf("Selected node %s for taint removal test", node.Name)

			// v3 only removes the taint once the mounter is running, so start with no mounter on the node
			v3 := isDaemonsetMounterMode(ctx, f)
			var restoreMounter func(context.Context) error
			if v3 {
				waitForNoS3VolumesOnNode(ctx, f, node.Name, driver.GetDriverInfo().Name)
				restoreMounter = removeMounterFromNode(ctx, f, node.Name)
				// Restart the node pod so no taint watcher from its previous start is running
				killCSIDriverPods(ctx, f)
			}

			// 2. Apply the taint to the node
			err := applyAgentNotReadyTaint(ctx, f.ClientSet, node.Name)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error {
				return removeAgentNotReadyTaint(ctx, f.ClientSet, node.Name)
			})

			// 3. Create volume resource
			vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol.CleanupResource)

			// 4. Restart CSI driver to trigger taint watcher
			framework.Logf("Restarting CSI driver pods to trigger taint watcher")
			killCSIDriverPods(ctx, f)

			if v3 {
				// Without a mounter the node pod never removes the taint
				gomega.Consistently(ctx, hasAgentNotReadyTaint(f.ClientSet, node.Name)).WithTimeout(90 * time.Second).WithPolling(10 * time.Second).Should(gomega.BeTrue())
				framework.ExpectNoError(restoreMounter(ctx))
			}

			// Wait for CSI driver pods to be ready again
			waitForCSIDriverReady(ctx, f)
			gomega.Eventually(ctx, hasAgentNotReadyTaint(f.ClientSet, node.Name)).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(gomega.BeFalse())

			// 5. Create and verify pod scheduling on the previously tainted node
			framework.Logf("Creating pod on previously tainted node %s", node.Name)
			pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": node.Name},
				[]*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
			pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

			// 6. Test basic file operations
			framework.Logf("Testing file operations on pod %s", pod.Name)
			checkBasicFileOperations(ctx, pod, e2epod.VolumeMountPath1)
		})

		It("should evict a workload on a NoExecute taint but keep the driver pods serving it", func(ctx context.Context) {
			if !isDaemonsetMounterMode(ctx, f) {
				Skip("the shared mounter only exists in daemonset mode")
			}
			node := getCSIDriverNode(ctx, f)
			mounter, err := podOnNode(ctx, f, mounterPodLabel, node.Name)
			framework.ExpectNoError(err)
			csiNode, err := podOnNode(ctx, f, csiNodePodLabel, node.Name)
			framework.ExpectNoError(err)

			// 1. Run two workloads sharing a live mount on the node; only the second tolerates the taint below
			taint := v1.Taint{Key: "s3.csi.aws.com/e2e-evict", Effect: v1.TaintEffectNoExecute}
			vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol.CleanupResource)
			runPod := func(tolerations ...v1.Toleration) *v1.Pod {
				pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": node.Name},
					[]*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
				pod.Spec.Tolerations = tolerations
				pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
				framework.ExpectNoError(err)
				deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })
				return pod
			}
			pod := runPod()
			tolerant := runPod(v1.Toleration{Key: taint.Key, Operator: v1.TolerationOpExists, Effect: taint.Effect})

			// 2. Taint the node with a key the first workload doesn't tolerate (the driver tolerates every taint by default)
			e2enode.AddOrUpdateTaintOnNode(ctx, f.ClientSet, node.Name, taint)
			deferCleanup(func(ctx context.Context) error {
				e2enode.RemoveTaintOffNode(ctx, f.ClientSet, node.Name, taint)
				return nil
			})

			// 3. The workload is evicted, while the driver pods serving its mount are neither evicted nor restarted
			framework.ExpectNoError(e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name, f.Timeouts.PodDelete))
			for _, driverPod := range []*v1.Pod{mounter, csiNode} {
				p, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).Get(ctx, driverPod.Name, metav1.GetOptions{})
				framework.ExpectNoError(err, "driver pod %s was removed", driverPod.Name)
				gomega.Expect(p.DeletionTimestamp).To(gomega.BeNil(), "driver pod %s is being evicted", driverPod.Name)
				for i, status := range p.Status.ContainerStatuses {
					gomega.Expect(status.RestartCount).To(gomega.Equal(driverPod.Status.ContainerStatuses[i].RestartCount), "container %s in driver pod %s restarted", status.Name, driverPod.Name)
				}
			}

			// 4. The shared mount still serves the workload that stayed
			checkBasicFileOperations(ctx, tolerant, e2epod.VolumeMountPath1)
		})
	})
}

// removeMounterFromNode keeps the mounter DaemonSet off nodeName until the returned function (also run on cleanup) puts it back.
func removeMounterFromNode(ctx context.Context, f *framework.Framework, nodeName string) func(context.Context) error {
	ds, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).Get(ctx, mounterDaemonSetName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	original := ds.Spec.Template.Spec.Affinity
	framework.ExpectNoError(patchMounterAffinity(ctx, f, excludeNode(original, nodeName)))

	restore := func(ctx context.Context) error {
		if err := patchMounterAffinity(ctx, f, original); err != nil {
			return err
		}
		waitForMounterPodReady(ctx, f, nodeName)
		// Restart the node pod so it doesn't wait out its crash-loop backoff
		killCSIDriverPods(ctx, f)
		waitForCSIDriverReady(ctx, f)
		return nil
	}
	DeferCleanup(restore)

	waitForNoMounterPod(ctx, f, nodeName)
	return restore
}

// excludeNode returns a copy of affinity that also keeps pods off nodeName.
func excludeNode(affinity *v1.Affinity, nodeName string) *v1.Affinity {
	affinity = affinity.DeepCopy()
	if affinity == nil {
		affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{{}}}}}
	}
	// Required node selector terms are ORed, so the node has to be excluded in every term
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for i := range terms {
		terms[i].MatchFields = append(terms[i].MatchFields, v1.NodeSelectorRequirement{Key: "metadata.name", Operator: v1.NodeSelectorOpNotIn, Values: []string{nodeName}})
	}
	return affinity
}

// patchMounterAffinity sets the affinity on the mounter DaemonSet's pod template.
func patchMounterAffinity(ctx context.Context, f *framework.Framework, affinity *v1.Affinity) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"affinity": affinity}}}})
	if err != nil {
		return err
	}
	_, err = f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).Patch(ctx, mounterDaemonSetName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// waitForNoMounterPod waits until no mounter pod is left on nodeName. A terminating mounter pod still reports Running.
func waitForNoMounterPod(ctx context.Context, f *framework.Framework, nodeName string) {
	gomega.Eventually(ctx, func(ctx context.Context) ([]v1.Pod, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-daemonset-mounter",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		return pods.Items, err
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(gomega.BeEmpty())
}

// hasAgentNotReadyTaint returns a poll function reporting whether nodeName has the agent-not-ready taint.
func hasAgentNotReadyTaint(client clientset.Interface, nodeName string) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(node.Spec.Taints, func(t v1.Taint) bool { return t.Key == agentNotReadyTaintKey }), nil
	}
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
