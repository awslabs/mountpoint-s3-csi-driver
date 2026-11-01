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
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	// csiNodeImageEnv is the s3-plugin env var the CSI node compares with the mounter's image.
	csiNodeImageEnv = "CSI_NODE_IMAGE"
	// imageMismatchMessage is the start of the error the CSI node returns on a mismatch.
	imageMismatchMessage = "image mismatch on node"
	// republishTimeout covers the kubelet's periodic republish (requiresRepublish), which follows its
	// pod sync, about once a minute.
	republishTimeout = 5 * time.Minute
	// remountTimeout covers the kubelet's mount retry backoff once the skew is gone.
	remountTimeout = 5 * time.Minute
)

type s3CSIImageSkewDaemonsetTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3CSIImageSkewDaemonsetTestSuite checks what a workload sees while s3-csi-node and the mounter on
// its node run different images: a new mount and the credential refresh of a running one are refused
// with a FailedMount event, and both recover once the two pods match again.
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
		ginkgo.It("should refuse a new mount and credential refresh while the images differ, and recover once they match", ginkgo.Serial, func(ctx context.Context) {
			ginkgo.By("Mounting a volume in a workload while the images match")
			running := createVolumeResourceWithMountOptions(ctx, config, pattern, nil)
			ginkgo.DeferCleanup(running.CleanupResource)
			runningPod, err := createPod(ctx, f.ClientSet, f.Namespace.Name,
				e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{running.Pvc}, admissionapi.LevelBaseline, ""))
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, runningPod) })
			nodeName := runningPod.Spec.NodeName

			const toWrite = 1024
			file := "/mnt/volume1/before-skew.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceed(ctx, f, runningPod, file, toWrite, seed)

			ginkgo.By(fmt.Sprintf("Recreating only the s3-csi-node pod on %s with a different %s", nodeName, csiNodeImageEnv))
			restore := skewCSINodeImage(ctx, f, nodeName)
			ginkgo.DeferCleanup(restore)

			ginkgo.By("Credential refresh: waiting for a FailedMount event on the running workload")
			logMismatchEvent(ctx, f, runningPod, republishTimeout)

			ginkgo.By("The running workload still serves its files: the mount itself is untouched")
			pod, err := f.ClientSet.CoreV1().Pods(runningPod.Namespace).Get(ctx, runningPod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodRunning))
			checkReadFromPathSucceedEventually(ctx, f, runningPod, file, toWrite, seed)

			ginkgo.By("New mount: creating a workload with another volume on the same node")
			fresh := createVolumeResourceWithMountOptions(ctx, config, pattern, nil)
			ginkgo.DeferCleanup(fresh.CleanupResource)
			freshPod, err := createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name,
				e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": nodeName},
					[]*v1.PersistentVolumeClaim{fresh.Pvc}, admissionapi.LevelBaseline, ""))
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, freshPod) })

			ginkgo.By("New mount: waiting for a FailedMount event on the new workload")
			logMismatchEvent(ctx, f, freshPod, 2*time.Minute)
			pod, err = f.ClientSet.CoreV1().Pods(freshPod.Namespace).Get(ctx, freshPod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodPending), "the new workload must not start while the images differ")

			ginkgo.By(fmt.Sprintf("Recreating the s3-csi-node pod on %s with the original %s", nodeName, csiNodeImageEnv))
			restore(ctx)

			ginkgo.By("The new workload mounts once the images match")
			framework.ExpectNoError(e2epod.WaitTimeoutForPodRunningInNamespace(ctx, f.ClientSet, freshPod.Name, freshPod.Namespace, remountTimeout))
			freshFile := "/mnt/volume1/after-skew.txt"
			freshSeed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, freshPod, freshFile, toWrite, freshSeed)
			checkReadFromPathSucceedEventually(ctx, f, runningPod, file, toWrite, seed)
		})
	})
}

// skewCSINodeImage recreates only the s3-csi-node pod on nodeName with a CSI_NODE_IMAGE that differs
// from the mounter's image, as a CSI node recreated after an upgrade would see. The DaemonSet is
// switched to OnDelete first, so no other node's pod moves. The returned restore (idempotent, also
// fine as a cleanup) puts the template back, recreates that pod, then restores the update strategy.
func skewCSINodeImage(ctx context.Context, f *framework.Framework, nodeName string) func(context.Context) {
	ginkgo.GinkgoHelper()
	ds := csiDriverDaemonSet(ctx, f)
	originalStrategy := *ds.Spec.UpdateStrategy.DeepCopy()
	originalImage, _ := csiNodeImageEnvValue(ds)
	skewedImage := originalImage + "-e2e-skew"

	updateCSINodeDaemonSet(ctx, f, func(ds *appsv1.DaemonSet) {
		ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}
	})
	updateCSINodeDaemonSet(ctx, f, func(ds *appsv1.DaemonSet) { setCSINodeImageEnv(ds, skewedImage) })
	recreateCSINodePod(ctx, f, nodeName)
	expectCSINodePodImageEnv(ctx, f, nodeName, skewedImage)
	framework.Logf("s3-csi-node on %s now has %s=%q; the mounter runs %q", nodeName, csiNodeImageEnv, skewedImage, originalImage)

	cleanupF := teardownSafeFramework(f)
	restored := false
	return func(ctx context.Context) {
		if restored {
			return
		}
		restored = true
		// The template matches the original again, so the other nodes' pods stay up to date.
		updateCSINodeDaemonSet(ctx, cleanupF, func(ds *appsv1.DaemonSet) { setCSINodeImageEnv(ds, originalImage) })
		recreateCSINodePod(ctx, cleanupF, nodeName)
		expectCSINodePodImageEnv(ctx, cleanupF, nodeName, originalImage)
		updateCSINodeDaemonSet(ctx, cleanupF, func(ds *appsv1.DaemonSet) { ds.Spec.UpdateStrategy = originalStrategy })
	}
}

// logMismatchEvent waits for a FailedMount event on pod naming the image mismatch, and logs every
// FailedMount event the pod has, so the run shows exactly what a user sees.
func logMismatchEvent(ctx context.Context, f *framework.Framework, pod *v1.Pod, timeout time.Duration) {
	ginkgo.GinkgoHelper()
	var found *v1.Event
	gomega.Eventually(ctx, func(ctx context.Context) (bool, error) {
		events, err := f.ClientSet.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + pod.Name,
		})
		if err != nil {
			return false, err
		}
		for i := range events.Items {
			if events.Items[i].Reason == "FailedMount" && strings.Contains(events.Items[i].Message, imageMismatchMessage) {
				found = &events.Items[i]
				return true, nil
			}
		}
		return false, nil
	}).WithTimeout(timeout).WithPolling(5*time.Second).Should(gomega.BeTrue(),
		"no FailedMount event containing %q on pod %s", imageMismatchMessage, pod.Name)

	framework.Logf("Event on pod %s: type=%s reason=%s count=%d\n  message: %s",
		pod.Name, found.Type, found.Reason, found.Count, found.Message)
	gomega.Expect(found.Message).To(gomega.ContainSubstring("FailedPrecondition"))
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

// expectCSINodePodImageEnv checks the s3-csi-node pod on nodeName was created with CSI_NODE_IMAGE=want.
func expectCSINodePodImageEnv(ctx context.Context, f *framework.Framework, nodeName, want string) {
	ginkgo.GinkgoHelper()
	pod, err := podOnNode(ctx, f, csiNodePodLabel, nodeName)
	framework.ExpectNoError(err)
	for _, c := range pod.Spec.Containers {
		if c.Name != csiNodeContainerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == csiNodeImageEnv {
				gomega.Expect(e.Value).To(gomega.Equal(want), "s3-csi-node pod %s on %s", pod.Name, nodeName)
				return
			}
		}
	}
	framework.Failf("s3-csi-node pod %s on %s has no %s", pod.Name, nodeName, csiNodeImageEnv)
}
