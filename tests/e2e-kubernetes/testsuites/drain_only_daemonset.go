package custom_testsuites

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/test/e2e/framework"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
)

// Drain-only cleaner timing (mirrors cmd/aws-s3-csi-controller/csicontroller/drain_stale_attachment_cleaner.go).
// The cleaner runs every drainCleanupInterval and only prunes attachments older than
// drainStaleAttachmentThreshold. We stamp the synthetic attachment well in the past so it is
// immediately eligible, then wait out (interval + threshold + slack).
const (
	drainCleanerInterval  = 2 * time.Minute
	drainStaleThreshold   = 2 * time.Minute
	drainAssertionTimeout = 8 * time.Minute
	drainAssertionPolling = 15 * time.Second
)

type s3CSIDrainOnlyDaemonsetTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3CSIDrainOnlyDaemonsetTestSuite exercises the drain-only controller (DrainStaleAttachmentCleaner)
// that runs in daemonset (V3) mode. It does NOT require a real V2 mount: it fabricates a stale
// MountpointS3PodAttachment (S3PA) referencing a non-existent workload pod, then asserts the cleaner
// prunes it end-to-end. This rides along in parallel with the rest of the e2e suite — its only cost is
// an Eventually wait bounded by the cleaner's cadence, which overlaps other specs.
func InitS3CSIDrainOnlyDaemonsetTestSuite() storageframework.TestSuite {
	return &s3CSIDrainOnlyDaemonsetTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "drainonlydaemonset",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIDrainOnlyDaemonsetTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIDrainOnlyDaemonsetTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIDrainOnlyDaemonsetTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"drainonly-ds", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.Describe("Drain-Only Controller (Daemonset Architecture)", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			// Only meaningful in daemonset mode, where the controller runs the drain-only cleaner.
			if !isDaemonsetMounterMode(ctx, f) {
				ginkgo.Skip("drain-only controller test requires daemonset mounter mode")
			}
			driver.PrepareTest(ctx, f)
		})

		ginkgo.It("should drain a stale MountpointS3PodAttachment whose workload no longer exists", func(ctx context.Context) {
			// A node in the cluster — the S3PA's spec.nodeName just needs to be a real node so the
			// controller's node-scoped listing (if any) can see it; any schedulable node works.
			nodeName := anyNodeName(ctx, f)

			// Unique names so parallel specs / retries don't collide.
			suffix := uuid.New().String()[:8]
			mpPodName := "mp-drain-e2e-" + suffix
			s3paName := "s3pa-drain-e2e-" + suffix
			staleWorkloadUID := "workload-does-not-exist-" + suffix

			// 1. Create a dummy Mountpoint Pod in the Mountpoint namespace. The cleaner annotates this
			//    with needs-unmount when its attachment list empties, then deletes it once Succeeded.
			//    We use a long-sleeping container so the pod stays Running until the driver acts on it.
			mpPod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mpPodName,
					Namespace: mountpointNamespace,
					Labels:    map[string]string{"app": "mp-drain-e2e"},
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					// The Mountpoint namespace enforces the "restricted" Pod Security Standard,
					// so the dummy pod must set a compliant security context.
					SecurityContext: &v1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(int64(1000)),
						SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []v1.Container{{
						Name:    "pause",
						Image:   "public.ecr.aws/docker/library/busybox:stable-musl",
						Command: []string{"/bin/sh", "-c", "sleep 3600"},
						SecurityContext: &v1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &v1.Capabilities{Drop: []v1.Capability{"ALL"}},
						},
					}},
				},
			}
			_, err := f.ClientSet.CoreV1().Pods(mountpointNamespace).Create(ctx, mpPod, metav1.CreateOptions{})
			framework.ExpectNoError(err, "creating synthetic Mountpoint Pod")
			ginkgo.DeferCleanup(func(ctx context.Context) {
				_ = f.ClientSet.CoreV1().Pods(mountpointNamespace).Delete(ctx, mpPodName, metav1.DeleteOptions{})
			})

			// 2. Create a stale S3PA that references the dummy Mountpoint Pod and a workload UID that
			//    does not exist. AttachmentTime is stamped in the past so it is immediately past the
			//    staleness threshold.
			staleTime := metav1.NewTime(time.Now().Add(-(drainStaleThreshold + time.Minute)).UTC())
			s3pa := &crdv2.MountpointS3PodAttachment{
				ObjectMeta: metav1.ObjectMeta{Name: s3paName},
				Spec: crdv2.MountpointS3PodAttachmentSpec{
					NodeName:             nodeName,
					PersistentVolumeName: "pv-drain-e2e-" + suffix,
					VolumeID:             "vol-drain-e2e-" + suffix,
					MountOptions:         "",
					AuthenticationSource: "driver",
					WorkloadFSGroup:      "",
					MountpointS3PodAttachments: map[string][]crdv2.WorkloadAttachment{
						mpPodName: {{WorkloadPodUID: staleWorkloadUID, AttachmentTime: staleTime}},
					},
				},
			}
			createS3PodAttachment(ctx, f, s3pa)
			ginkgo.DeferCleanup(func(ctx context.Context) {
				_ = f.DynamicClient.Resource(s3paGVR).Delete(ctx, s3paName, metav1.DeleteOptions{})
			})

			framework.Logf("Created synthetic stale S3PA %q -> MP pod %q (workload UID %q). Waiting for drain-only cleaner...",
				s3paName, mpPodName, staleWorkloadUID)

			// 3. The cleaner should annotate the Mountpoint Pod with needs-unmount as it empties the
			//    attachment map. (Best-effort observation; the terminal assertion below is S3PA deletion.)
			gomega.Eventually(ctx, func(ctx context.Context) (bool, error) {
				pod, err := f.ClientSet.CoreV1().Pods(mountpointNamespace).Get(ctx, mpPodName, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					// Pod already cleaned up — annotation step definitely happened.
					return true, nil
				}
				if err != nil {
					return false, err
				}
				return pod.Annotations[mppod.AnnotationNeedsUnmount] == "true", nil
			}).WithTimeout(drainAssertionTimeout).WithPolling(drainAssertionPolling).Should(gomega.BeTrue(),
				"expected drain-only cleaner to annotate Mountpoint Pod with needs-unmount (or delete it)")

			// 4. Terminal assertion: the emptied S3PA is deleted by the cleaner.
			gomega.Eventually(ctx, func(ctx context.Context) (bool, error) {
				_, err := f.DynamicClient.Resource(s3paGVR).Get(ctx, s3paName, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				return false, nil
			}).WithTimeout(drainAssertionTimeout).WithPolling(drainAssertionPolling).Should(gomega.BeTrue(),
				"expected drain-only cleaner to delete the emptied MountpointS3PodAttachment")

			framework.Logf("Drain-only cleaner successfully drained stale S3PA %q", s3paName)
		})

		ginkgo.It("should delete a leftover Headroom Pod whose workload no longer exists", func(ctx context.Context) {
			// The cleaner identifies a Headroom Pod by the "hr-" name prefix and reads the workload
			// UID from the LabelHeadroomForPod label. If that workload pod does not exist, the Headroom
			// Pod is deleted. We fabricate exactly that: an orphaned Headroom Pod referencing a
			// non-existent workload UID.
			suffix := uuid.New().String()[:8]
			// Name MUST start with "hr-" for mppod.IsHeadroomPod to recognize it.
			hrPodName := "hr-drain-e2e-" + suffix
			nonExistentWorkloadUID := "workload-does-not-exist-" + suffix

			hrPod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hrPodName,
					Namespace: mountpointNamespace,
					Labels: map[string]string{
						// The cleaner reads this label to find the referenced workload.
						mppod.LabelHeadroomForPod: nonExistentWorkloadUID,
					},
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					SecurityContext: &v1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(int64(1000)),
						SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []v1.Container{{
						Name:    "pause",
						Image:   "public.ecr.aws/docker/library/busybox:stable-musl",
						Command: []string{"/bin/sh", "-c", "sleep 3600"},
						SecurityContext: &v1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &v1.Capabilities{Drop: []v1.Capability{"ALL"}},
						},
					}},
				},
			}
			_, err := f.ClientSet.CoreV1().Pods(mountpointNamespace).Create(ctx, hrPod, metav1.CreateOptions{})
			framework.ExpectNoError(err, "creating synthetic Headroom Pod")
			ginkgo.DeferCleanup(func(ctx context.Context) {
				_ = f.ClientSet.CoreV1().Pods(mountpointNamespace).Delete(ctx, hrPodName, metav1.DeleteOptions{})
			})

			framework.Logf("Created orphaned Headroom Pod %q (workload UID %q). Waiting for drain-only cleaner to delete it...",
				hrPodName, nonExistentWorkloadUID)

			gomega.Eventually(ctx, func(ctx context.Context) (bool, error) {
				_, err := f.ClientSet.CoreV1().Pods(mountpointNamespace).Get(ctx, hrPodName, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				return false, nil
			}).WithTimeout(drainAssertionTimeout).WithPolling(drainAssertionPolling).Should(gomega.BeTrue(),
				"expected drain-only cleaner to delete the orphaned Headroom Pod")

			framework.Logf("Drain-only cleaner successfully deleted orphaned Headroom Pod %q", hrPodName)
		})
	})
}

// anyNodeName returns the name of any node in the cluster (fails the test if none exist).
func anyNodeName(ctx context.Context, f *framework.Framework) string {
	nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "listing nodes")
	gomega.Expect(nodes.Items).ToNot(gomega.BeEmpty(), "cluster has no nodes")
	return nodes.Items[0].Name
}

// createS3PodAttachment creates the given S3PA via the dynamic client (converting to unstructured).
func createS3PodAttachment(ctx context.Context, f *framework.Framework, s3pa *crdv2.MountpointS3PodAttachment) {
	s3pa.APIVersion = crdv2.GroupVersion.String()
	s3pa.Kind = "MountpointS3PodAttachment"
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s3pa)
	framework.ExpectNoError(err, "converting S3PA to unstructured")
	_, err = f.DynamicClient.Resource(s3paGVR).Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
	framework.ExpectNoError(err, "creating S3PA")
}
