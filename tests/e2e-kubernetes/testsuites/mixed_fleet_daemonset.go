package custom_testsuites

import (
	"context"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	// The label the chart puts on each mounter pod and the node pod advertises; spelled out as the contract between the two.
	mounterMaxVolumesLabel = "s3.csi.aws.com/max-volumes-per-node"
)

type s3CSIMixedFleetDaemonsetTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIMixedFleetDaemonsetTestSuite() storageframework.TestSuite {
	return &s3CSIMixedFleetDaemonsetTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name:         "daemonset-mixed-fleet",
			TestPatterns: []storageframework.TestPattern{storageframework.DefaultFsPreprovisionedPV},
		},
	}
}

func (t *s3CSIMixedFleetDaemonsetTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIMixedFleetDaemonsetTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIMixedFleetDaemonsetTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"daemonset-mixed-fleet", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	// Serial and Ordered: each Context reinstalls the mounters via Helm, which restarts every Mountpoint on every node.
	Describe("A mounter entry per kind of node", Ordered, ContinueOnFailure, Serial, func() {
		var (
			config     *storageframework.PerTestConfig
			release    *driverRelease
			nodeA      string
			driverName = driver.GetDriverInfo().Name
		)

		BeforeAll(func(ctx context.Context) {
			if !isDaemonsetMounterMode(ctx, f) {
				Skip("mounter entries are DaemonSets, which only exist in daemonset mode")
			}
			nodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				Skip("two entries need two nodes")
			}
			nodeA = nodes.Items[0].Name
			release = setUpDriverRelease(ctx, f)
		})

		BeforeEach(func(ctx context.Context) {
			config = driver.PrepareTest(ctx, f)
		})

		It("runs one mounter per node and advertises its limit, as a default install has", func(ctx context.Context) {
			expectOneMounterPerNode(ctx, f, driverName, func(string) string { return mounterDaemonSetName })
		})

		// Node A's entries request 2Gi, not the chart's 4Gi, so two of their mounters fit on one node during the overlap.
		nodeAResources := map[string]any{"requests": map[string]any{"memory": "2Gi"}}
		first := func() map[string]any {
			return map[string]any{"name": "first", "nodeSelector": map[string]any{"kubernetes.io/hostname": nodeA}, "maxVolumesPerNode": 3,
				"resources": nodeAResources}
		}
		rest := func() map[string]any {
			return map[string]any{"name": "rest", "maxVolumesPerNode": 5,
				"matchExpressions": []any{map[string]any{"key": "kubernetes.io/hostname", "operator": "NotIn", "values": []any{nodeA}}}}
		}

		Context("with two entries", func() {
			BeforeAll(func(ctx context.Context) {
				release.upgrade(ctx, f, withMounterEntries(release, first(), rest()))
			})

			It("runs each node's own entry's mounter and advertises its limit", func(ctx context.Context) {
				expectOneMounterPerNode(ctx, f, driverName, func(node string) string {
					if node == nodeA {
						return mounterDaemonSetName + "-first"
					}
					return mounterDaemonSetName + "-rest"
				})
				Expect(getCSINodeLimits(ctx, f, nodeA, driverName)).To(Equal(3))
				Expect(getCSINodeLimits(ctx, f, otherNode(ctx, f, nodeA), driverName)).To(Equal(5))
			})

			It("serves a workload on each kind of node", func(ctx context.Context) {
				for _, node := range []string{nodeA, otherNode(ctx, f, nodeA)} {
					pod, _ := writerOnNode(ctx, f, config, pattern, node)
					checkWriteAndRead(ctx, f, pod)
				}
			})
		})

		Context("with a third entry that also matches node A", func() {
			BeforeAll(func(ctx context.Context) {
				// A different rule from first's, as two identical selections are rejected at install, and first's limit, so node A
				// advertises 3 whichever mounter it picks.
				extra := map[string]any{"name": "extra", "maxVolumesPerNode": 3, "resources": nodeAResources,
					"matchExpressions": []any{map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": []any{nodeA}}}}
				// The restart starts node A's node pod with two mounters running, so it has to pick one.
				release.upgrade(ctx, f, withMounterEntries(release, first(), rest(), extra))
			})

			It("keeps node A's node pod running and registered, logging the overlap", func(ctx context.Context) {
				// The upgrade already waited for every node pod to be Ready, so node A's did not crash-loop on two mounters.
				Expect(mountersOn(ctx, f, nodeA)).To(HaveLen(2))
				Expect(getCSINodeLimits(ctx, f, nodeA, driverName)).To(Equal(3))
				Eventually(ctx, func(ctx context.Context) bool {
					return verifyCSINodeLogs(ctx, f, nodeA, "multiple running mounter pods found")
				}).WithTimeout(90 * time.Second).WithPolling(5 * time.Second).Should(BeTrue())
			})

			It("sends every mount on node A to the same mounter", func(ctx context.Context) {
				running, runningPV := writerOnNode(ctx, f, config, pattern, nodeA)
				servedBy := servingMounter(ctx, f, nodeA, runningPV)

				newPod, newPV := writerOnNode(ctx, f, config, pattern, nodeA)
				Expect(servingMounter(ctx, f, nodeA, newPV)).To(Equal(servedBy))
				checkWriteAndRead(ctx, f, running)
				checkWriteAndRead(ctx, f, newPod)
			})
		})
	})
}

// withMounterEntries returns the installed values with one complete daemonsetMounters element per entry.
func withMounterEntries(r *driverRelease, entries ...map[string]any) map[string]any {
	// Helm replaces a list whole, so each element starts from the chart's: a partial one renders an absent logLevel as `--v=`.
	chartMounter := r.chart.Values["daemonsetMounters"].([]any)[0].(map[string]any)
	var mounters []any
	for _, entry := range entries {
		mounter := maps.Clone(chartMounter)
		if installed, _ := r.installed["daemonsetMounters"].([]any); len(installed) > 0 {
			maps.Copy(mounter, installed[0].(map[string]any))
		}
		maps.Copy(mounter, entry)
		mounters = append(mounters, mounter)
	}
	values := maps.Clone(r.installed)
	values["daemonsetMounters"] = mounters
	return values
}

// expectOneMounterPerNode checks each node runs one mounter, from daemonSetFor's DaemonSet, and advertises its limit label.
func expectOneMounterPerNode(ctx context.Context, f *framework.Framework, driverName string, daemonSetFor func(node string) string) {
	GinkgoHelper()
	nodePods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{LabelSelector: csiNodePodLabel})
	framework.ExpectNoError(err)
	for _, nodePod := range nodePods.Items {
		node := nodePod.Spec.NodeName
		mounters := mountersOn(ctx, f, node)
		Expect(mounters).To(HaveLen(1), "mounter pods on node %s", node)
		Expect(mounters[0].OwnerReferences[0].Name).To(Equal(daemonSetFor(node)), "the mounter on node %s", node)
		limit, err := strconv.Atoi(mounters[0].Labels[mounterMaxVolumesLabel])
		framework.ExpectNoError(err, "the mounter on node %s has no %s label", node, mounterMaxVolumesLabel)
		Expect(getCSINodeLimits(ctx, f, node, driverName)).To(Equal(limit), "the limit node %s advertises", node)
	}
}

// mountersOn returns the mounter pods on node.
func mountersOn(ctx context.Context, f *framework.Framework, node string) []v1.Pod {
	GinkgoHelper()
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: mounterPodLabel,
		FieldSelector: "spec.nodeName=" + node,
	})
	framework.ExpectNoError(err)
	return pods.Items
}

// otherNode returns a node with a node pod other than node.
func otherNode(ctx context.Context, f *framework.Framework, node string) string {
	GinkgoHelper()
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{LabelSelector: csiNodePodLabel})
	framework.ExpectNoError(err)
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != node {
			return pod.Spec.NodeName
		}
	}
	Fail("no node pod on a node other than " + node)
	return ""
}

// writerOnNode creates a volume and a workload using it on node, registers their cleanup, and returns the workload and its PV's name.
func writerOnNode(ctx context.Context, f *framework.Framework, config *storageframework.PerTestConfig,
	pattern storageframework.TestPattern, node string) (*v1.Pod, string) {
	GinkgoHelper()
	vol := createVolumeResourceWithMountOptions(ctx, config, pattern, nil)
	DeferCleanup(vol.CleanupResource)
	pod := createPodOnNode(ctx, f, node, vol)[0]
	DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })
	return pod, vol.Pv.Name
}

// checkWriteAndRead writes a file through the workload's volume and reads it back.
func checkWriteAndRead(ctx context.Context, f *framework.Framework, pod *v1.Pod) {
	GinkgoHelper()
	path := filepath.Join(e2epod.VolumeMountPath1, "mixed-fleet.txt")
	seed := time.Now().UTC().UnixNano()
	checkWriteToPathSucceed(ctx, f, pod, path, 1024, seed)
	checkReadFromPathSucceed(ctx, f, pod, path, 1024, seed)
}

// servingMounter returns the mounter pod on node that received the mount request for pvName.
func servingMounter(ctx context.Context, f *framework.Framework, node, pvName string) string {
	GinkgoHelper()
	var served []string
	for _, mounter := range mountersOn(ctx, f, node) {
		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, mounter.Namespace, mounter.Name, mounterContainerName)
		framework.ExpectNoError(err)
		if strings.Contains(logs, "Received mount request for mount "+pvName) {
			served = append(served, mounter.Name)
		}
	}
	Expect(served).To(HaveLen(1), "mounters on node %s that received %s", node, pvName)
	return served[0]
}
