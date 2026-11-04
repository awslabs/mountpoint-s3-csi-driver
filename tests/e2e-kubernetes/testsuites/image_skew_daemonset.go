package custom_testsuites

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	// csiNodeImageEnv is the s3-plugin env var the CSI node compares with the mounter's image.
	csiNodeImageEnv = "CSI_NODE_IMAGE"
	// imageMismatchMessage is the start of the warning the CSI node logs on a mismatch.
	imageMismatchMessage = "image mismatch on node"
	// credentialsRefreshedMessage is what the CSI node logs (at the chart's default -v=4) when a republish of a
	// mounted target refreshes its credentials. The warning above is rate-limited, so it can't show a republish.
	credentialsRefreshedMessage = "is already mounted, credentials refreshed"
	// republishTimeout covers the kubelet's periodic republish (requiresRepublish), about once a minute.
	republishTimeout = 5 * time.Minute
)

type s3CSIImageSkewDaemonsetTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3CSIImageSkewDaemonsetTestSuite checks that a workload still mounts while s3-csi-node and the mounter on its
// node run different images, the state a node is in after an upgrade until it is recycled, and that the CSI node
// warns about it.
func InitS3CSIImageSkewDaemonsetTestSuite() storageframework.TestSuite {
	return &s3CSIImageSkewDaemonsetTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "imageskewdaemonset",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIImageSkewDaemonsetTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIImageSkewDaemonsetTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIImageSkewDaemonsetTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"image-skew-ds", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.Describe("Image mismatch between s3-csi-node and the mounter (Daemonset Architecture)", func() {
		var config *storageframework.PerTestConfig

		ginkgo.BeforeEach(func(ctx context.Context) {
			if !isDaemonsetMounterMode(ctx, f) {
				ginkgo.Skip("the image check only exists in daemonset mounter mode")
			}
			if _, ok := csiNodeImageEnvValue(csiDriverDaemonSet(ctx, f)); !ok {
				ginkgo.Skip(fmt.Sprintf("the installed chart does not set %s on s3-plugin", csiNodeImageEnv))
			}
			config = driver.PrepareTest(ctx, f)
		})

		// Serial: it recreates the s3-csi-node pod on one node, which every mount there depends on.
		ginkgo.It("should mount while the images differ, and warn", ginkgo.Serial, func(ctx context.Context) {
			nodeName := config.ClientNodeSelection.Name // the driver's single node, if pinned
			if nodeName == "" {
				node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
				framework.ExpectNoError(err)
				nodeName = node.Name
			}

			ginkgo.By(fmt.Sprintf("Recreating only the s3-csi-node pod on %s with a different %s", nodeName, csiNodeImageEnv))
			ginkgo.DeferCleanup(skewCSINodeImage(ctx, f, nodeName))

			ginkgo.By("Mounting a volume in a workload on that node")
			vol := createVolumeResourceWithMountOptions(ctx, config, pattern, nil)
			ginkgo.DeferCleanup(vol.CleanupResource)
			pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name,
				e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": nodeName},
					[]*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, ""))
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })
			checkWriteToPathSucceedEventually(ctx, f, pod, "/mnt/volume1/during-skew.txt", 1024, time.Now().UTC().UnixNano())

			ginkgo.By("The CSI node logged the mismatch warning")
			gomega.Expect(verifyCSINodeLogs(ctx, f, nodeName, imageMismatchMessage)).To(gomega.BeTrue(),
				"s3-csi-node on %s should log %q while the images differ", nodeName, imageMismatchMessage)

			ginkgo.By("Credential refresh: the kubelet's republish of the mounted volume is served")
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				return csiNodeLogHasLine(ctx, f, nodeName, string(pod.UID), credentialsRefreshedMessage)
			}).WithTimeout(republishTimeout).WithPolling(10*time.Second).Should(gomega.BeTrue(),
				"s3-csi-node on %s should refresh the credentials of pod %s while the images differ", nodeName, pod.Name)
		})
	})
}

// csiNodeLogHasLine reports whether a line of the s3-plugin log on nodeName contains every one of parts.
func csiNodeLogHasLine(ctx context.Context, f *framework.Framework, nodeName string, parts ...string) bool {
	pod, err := podOnNode(ctx, f, csiNodePodLabel, nodeName)
	if err != nil {
		return false
	}
	logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, csiDriverDaemonSetNamespace, pod.Name, csiNodeContainerName)
	if err != nil {
		return false
	}
nextLine:
	for _, line := range strings.Split(logs, "\n") {
		for _, p := range parts {
			if !strings.Contains(line, p) {
				continue nextLine
			}
		}
		return true
	}
	return false
}

// skewCSINodeImage recreates only the s3-csi-node pod on nodeName with a CSI_NODE_IMAGE that differs
// from the mounter's image, as a CSI node recreated after an upgrade would see. The DaemonSet is
// switched to OnDelete first, so no other node's pod moves. The returned cleanup puts the template
// back, recreates that pod, then restores the update strategy.
func skewCSINodeImage(ctx context.Context, f *framework.Framework, nodeName string) func(context.Context) {
	ginkgo.GinkgoHelper()
	ds := csiDriverDaemonSet(ctx, f)
	originalStrategy := *ds.Spec.UpdateStrategy.DeepCopy()
	originalImage, _ := csiNodeImageEnvValue(ds)

	updateCSINodeDaemonSet(ctx, f, func(ds *appsv1.DaemonSet) {
		ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}
	})
	updateCSINodeDaemonSet(ctx, f, func(ds *appsv1.DaemonSet) { setCSINodeImageEnv(ds, originalImage+"-e2e-skew") })
	recreateCSINodePod(ctx, f, nodeName)

	cleanupF := teardownSafeFramework(f)
	return func(ctx context.Context) {
		updateCSINodeDaemonSet(ctx, cleanupF, func(ds *appsv1.DaemonSet) { setCSINodeImageEnv(ds, originalImage) })
		recreateCSINodePod(ctx, cleanupF, nodeName)
		updateCSINodeDaemonSet(ctx, cleanupF, func(ds *appsv1.DaemonSet) { ds.Spec.UpdateStrategy = originalStrategy })
	}
}

// csiNodeImageEnvValue returns CSI_NODE_IMAGE from the s3-plugin container of ds's template.
func csiNodeImageEnvValue(ds *appsv1.DaemonSet) (string, bool) {
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != csiNodeContainerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == csiNodeImageEnv {
				return e.Value, true
			}
		}
	}
	return "", false
}

// setCSINodeImageEnv sets CSI_NODE_IMAGE on the s3-plugin container of ds's template.
func setCSINodeImageEnv(ds *appsv1.DaemonSet, value string) {
	for i := range ds.Spec.Template.Spec.Containers {
		c := &ds.Spec.Template.Spec.Containers[i]
		if c.Name != csiNodeContainerName {
			continue
		}
		for j := range c.Env {
			if c.Env[j].Name == csiNodeImageEnv {
				c.Env[j].Value = value
				return
			}
		}
	}
	framework.Failf("%s not found on container %s of %s", csiNodeImageEnv, csiNodeContainerName, ds.Name)
}

// updateCSINodeDaemonSet applies mutate to the s3-csi-node DaemonSet, retrying on conflicts.
func updateCSINodeDaemonSet(ctx context.Context, f *framework.Framework, mutate func(*appsv1.DaemonSet)) {
	ginkgo.GinkgoHelper()
	client := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace)
	framework.ExpectNoError(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ds, err := client.Get(ctx, csiDriverDaemonSetName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		mutate(ds)
		_, err = client.Update(ctx, ds, metav1.UpdateOptions{})
		return err
	}), "updating DaemonSet %s", csiDriverDaemonSetName)
}

// recreateCSINodePod deletes the s3-csi-node pod on nodeName and waits for its replacement to be Ready.
func recreateCSINodePod(ctx context.Context, f *framework.Framework, nodeName string) {
	ginkgo.GinkgoHelper()
	killCSINodePodOnNode(ctx, f, nodeName)
	waitForCSINodePodReady(ctx, f, nodeName)
	waitForCSINodePodStable(ctx, f, nodeName)
}
