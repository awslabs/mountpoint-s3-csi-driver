// Tests that workloads using S3 volumes get scheduled and mounted on the nodes an autoscaler provisions for them.
// The pod mounter (V2) only created a Mountpoint Pod once its workload was scheduled, so autoscalers didn't account for
// it, and the node might have no room left for it (https://github.com/awslabs/mountpoint-s3-csi-driver/issues/543).
// The daemonset mounter runs on every node, so autoscalers account for it. But a node can only mount as many volumes
// as the driver advertises, which autoscalers don't know for a node they haven't launched yet.
//
// They only run with `--run-scheduling-tests`, on a cluster whose autoscaler provisions the nodes of a pool for them
// (see `scripts/profiles`). The pool's nodes have the `e2e.s3.csi.aws.com/pool=default` label and NoSchedule taint,
// so nothing else runs on them, and the `s3.csi.aws.com/agent-not-ready` startup taint.

package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	// Key and value of the label and the NoSchedule taint on the nodes of the pool.
	nodePoolKey   = "e2e.s3.csi.aws.com/pool"
	nodePoolValue = "default"

	// How long the autoscaler gets to provision a node, and the driver to get ready on it.
	nodeProvisionTimeout = 8 * time.Minute
	// How long the autoscaler gets to remove the pool's nodes once they're empty.
	nodeRemovalTimeout = 15 * time.Minute
)

type s3CSISchedulingTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSISchedulingTestSuite() storageframework.TestSuite {
	return &s3CSISchedulingTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "scheduling",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSISchedulingTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSISchedulingTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSISchedulingTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"scheduling", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	type local struct {
		config    *storageframework.PerTestConfig
		resources []*storageframework.VolumeResource
	}
	var l local

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

	f.It("should scale out workloads beyond a node's volume limit without overcommitting any node", f.WithSerial(), func(ctx context.Context) {
		ginkgo.By("Waiting for the pool to have no nodes")
		waitForEmptyNodePool(ctx, f)
		// Runs after the workloads are deleted, so that the nodes are gone before the driver gets uninstalled, and the
		// next run starts from an empty pool again.
		ginkgo.DeferCleanup(waitForEmptyNodePool, f)

		driverName := driver.GetDriverInfo().Name
		limit, err := getCSINodeLimits(ctx, f, getCSIDriverNode(ctx, f).Name, driverName)
		framework.ExpectNoError(err)
		workloads := 2 * limit

		ginkgo.By(fmt.Sprintf("Creating %d workloads with a volume each, twice as many as a node can mount", workloads))
		var pods []*v1.Pod
		for range workloads {
			vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, vol)
			pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
			targetNodePool(&pod.Spec)
			pod, err := createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(e2epod.DeletePodWithWait, f.ClientSet, pod)
			pods = append(pods, pod)
		}
		start := time.Now()

		ginkgo.By("Waiting for the autoscaler to provision nodes for all of them")
		// Autoscalers might only learn a node's volume limit once it has joined, and provision one node at a time.
		pods = waitForPodsRunning(ctx, f, pods, time.Duration(workloads/limit)*nodeProvisionTimeout)
		nodes := podNodes(pods)
		framework.Logf("All %d workloads run after %v, on %d node(s)", workloads, time.Since(start).Round(time.Second), len(nodes))

		// Also checks the startup taint: without it, the workloads would land on the first node before the driver
		// registered its volume limit there.
		ginkgo.By("Checking that no node mounts more volumes than its limit, and that the mounter is ready on each")
		for _, node := range nodes {
			nodeLimit, err := getCSINodeLimits(ctx, f, node, driverName)
			framework.ExpectNoError(err)
			inUse, err := countS3VolumesOnNode(ctx, f, node, driverName)
			framework.ExpectNoError(err)
			gomega.Expect(inUse).To(gomega.BeNumerically("<=", nodeLimit),
				"node %s mounts %d volumes, more than its limit of %d", node, inUse, nodeLimit)
			waitForMounterPodReady(ctx, f, node)
		}

		ginkgo.By("Checking that the volumes work")
		for _, pod := range pods {
			seed := time.Now().UTC().UnixNano()
			path := filepath.Join(e2epod.VolumeMountPath1, fmt.Sprintf("test-%d", seed))
			checkWriteToPathSucceed(ctx, f, pod, path, 1024, seed)
			checkReadFromPathSucceed(ctx, f, pod, path, 1024, seed)
		}
	})
}

// targetNodePool makes a pod run on the pool's nodes.
func targetNodePool(spec *v1.PodSpec) {
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	spec.NodeSelector[nodePoolKey] = nodePoolValue
	spec.Tolerations = append(spec.Tolerations, v1.Toleration{
		Key:      nodePoolKey,
		Operator: v1.TolerationOpEqual,
		Value:    nodePoolValue,
		Effect:   v1.TaintEffectNoSchedule,
	})
}

// waitForEmptyNodePool waits for the autoscaler to remove the pool's nodes, which it does once they're empty.
func waitForEmptyNodePool(ctx context.Context, f *framework.Framework) {
	gomega.Eventually(ctx, func(ctx context.Context) ([]string, error) {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: nodePoolKey + "=" + nodePoolValue})
		if err != nil {
			return nil, err
		}
		var names []string
		for _, node := range nodes.Items {
			names = append(names, node.Name)
		}
		return names, nil
	}).WithTimeout(nodeRemovalTimeout).WithPolling(10*time.Second).Should(gomega.BeEmpty(), "the autoscaler didn't remove the pool's nodes")
}

// waitForPodsRunning waits for all pods to be running, and returns them up to date.
func waitForPodsRunning(ctx context.Context, f *framework.Framework, pods []*v1.Pod, timeout time.Duration) []*v1.Pod {
	var running []*v1.Pod
	gomega.Eventually(ctx, func(ctx context.Context) ([]string, error) {
		running = nil
		var notRunning []string
		for _, p := range pods {
			pod, err := f.ClientSet.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil, gomega.StopTrying(fmt.Sprintf("pod %s is gone, e.g. evicted to consolidate its node", p.Name))
			}
			if err != nil {
				return nil, err
			}
			switch pod.Status.Phase {
			case v1.PodRunning:
				running = append(running, pod)
			case v1.PodFailed, v1.PodSucceeded:
				return nil, gomega.StopTrying(fmt.Sprintf("pod %s is %s", pod.Name, pod.Status.Phase))
			default:
				notRunning = append(notRunning, fmt.Sprintf("%s (%s)", pod.Name, podWaitingReason(pod)))
			}
		}
		return notRunning, nil
	}).WithTimeout(timeout).WithPolling(10*time.Second).Should(gomega.BeEmpty(), "pods didn't run")
	return running
}

// podWaitingReason tells why a pod isn't running yet.
func podWaitingReason(pod *v1.Pod) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodScheduled && condition.Status != v1.ConditionTrue {
			return fmt.Sprintf("not scheduled: %s", condition.Message)
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			return fmt.Sprintf("on node %s: %s", pod.Spec.NodeName, status.State.Waiting.Reason)
		}
	}
	return fmt.Sprintf("on node %s: %s", pod.Spec.NodeName, pod.Status.Phase)
}

func podNodes(pods []*v1.Pod) []string {
	var nodes []string
	for _, pod := range pods {
		if !slices.Contains(nodes, pod.Spec.NodeName) {
			nodes = append(nodes, pod.Spec.NodeName)
		}
	}
	return nodes
}
