package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// A cleanup pass runs every 2m (cleanupInterval). We watch for one full cycle plus
// buffer so at least one pass is guaranteed to run within the window.
const cleanupObserveTimeout = 3 * time.Minute
const cleanupObservePoll = 5 * time.Second

type s3CSIResourceCleanupDaemonsetTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3CSIResourceCleanupDaemonsetTestSuite tests the periodic cleanup job in
// daemonset mode. It verifies the job's central safety property: a cleanup pass
// runs and leaves a healthy, in-use mount completely untouched.
func InitS3CSIResourceCleanupDaemonsetTestSuite() storageframework.TestSuite {
	return &s3CSIResourceCleanupDaemonsetTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "resourcecleanupdaemonset",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIResourceCleanupDaemonsetTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIResourceCleanupDaemonsetTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIResourceCleanupDaemonsetTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var l local

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"cleanup-ds", storageframework.GetDriverTimeouts(driver))
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

	// The core safety property of the periodic cleanup job: when it runs, it must
	// leave a healthy, in-use mount untouched. We first wait for a cleanup pass to
	// log that it left this volume intact (proving the job actually ran, not just
	// that time passed), then confirm the source mount and data are still there.
	ginkgo.It("should run a cleanup pass without disturbing a healthy in-use mount", ginkgo.Serial, func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		pvName := resource.Pv.Name

		targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
		defer deletePodsInOrder(ctx, f, pods)

		toWrite := 1024
		seed := time.Now().UTC().UnixNano()
		checkWriteToPathSucceedEventually(ctx, f, pods[0], "/mnt/volume1/keep.txt", toWrite, seed)

		// Wait for a cleanup pass to run and log that it left THIS volume intact.
		// The PV name is a fresh UUID, so this line can only come from a cleanup pass
		// over this test's volume during this run — proving the job actually ran.
		leftIntactMarker := fmt.Sprintf("volume %s healthy", pvName)
		ginkgo.By("Waiting for a cleanup pass to run and leave the healthy mount intact")
		gomega.Eventually(ctx, func(ctx context.Context) (bool, error) {
			return verifyCSINodeLogs(ctx, f, targetNode, leftIntactMarker), nil
		}).WithTimeout(cleanupObserveTimeout).WithPolling(cleanupObservePoll).Should(gomega.BeTrue())

		// The pass ran and left it alone: source mount still present, data still readable.
		gomega.Expect(countFuseMountsForVolume(ctx, f, targetNode, pvName)).To(gomega.Equal(1),
			"cleanup must not remove a healthy in-use source mount for volume %s", pvName)
		checkReadFromPathSucceedEventually(ctx, f, pods[0], "/mnt/volume1/keep.txt", toWrite, seed)
	})

	// After a volume's source is reclaimed (last consumer gone), the same PV must
	// mount cleanly again — reclaim leaves no stale state that blocks a fresh mount.
	ginkgo.It("should allow the same PV to mount again after its source is reclaimed", ginkgo.Serial, func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		pvName := resource.Pv.Name

		// First pod: mount, write, confirm the source exists.
		targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
		toWrite := 1024
		seed := time.Now().UTC().UnixNano()
		checkWriteToPathSucceedEventually(ctx, f, pods[0], "/mnt/volume1/remount.txt", toWrite, seed)
		gomega.Expect(countFuseMountsForVolume(ctx, f, targetNode, pvName)).To(gomega.Equal(1))

		// Delete the pod: last consumer gone, so the source is reclaimed.
		ginkgo.By("Deleting the pod and waiting for the source mount to be reclaimed")
		deletePodsInOrder(ctx, f, pods)
		gomega.Eventually(ctx, func(ctx context.Context) (int, error) {
			return countFuseMountsForVolume(ctx, f, targetNode, pvName), nil
		}).WithTimeout(cleanupObserveTimeout).WithPolling(cleanupObservePoll).Should(gomega.Equal(0))

		// Pin the new pod to the reclaimed node to confirm the reclaim left no stale state
		// that blocks a remount there. The pod is Running once created, so the source mount
		// already exists and a single count is enough.
		ginkgo.By("Recreating a pod on the reclaimed node and confirming a fresh source mounts")
		pods2 := createPodOnNode(ctx, f, targetNode, resource)
		defer deletePodsInOrder(ctx, f, pods2)
		gomega.Expect(countFuseMountsForVolume(ctx, f, targetNode, pvName)).To(gomega.Equal(1),
			"expected a fresh source mount for volume %s after remount on the reclaimed node", pvName)
		checkReadFromPathSucceedEventually(ctx, f, pods2[0], "/mnt/volume1/remount.txt", toWrite, seed)
	})

	// Kill the mounter so the source goes dead, but keep the workload running so no inline
	// unmount fires — that way only the periodic cleanup job can reclaim the source.
	ginkgo.It("should reclaim a dead source after the mounter pod is killed", ginkgo.Serial, func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		pvName := resource.Pv.Name

		targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
		defer deletePodsInOrder(ctx, f, pods)
		toWrite := 1024
		seed := time.Now().UTC().UnixNano()
		checkWriteToPathSucceedEventually(ctx, f, pods[0], "/mnt/volume1/dead.txt", toWrite, seed)
		gomega.Expect(countFuseMountsForVolume(ctx, f, targetNode, pvName)).To(gomega.Equal(1))

		// mount-s3 dies with the mounter pod; the source goes dead (ENOTCONN) but stays listed.
		ginkgo.By("Killing the mounter pod to make the source dead")
		killMounterPodOnNode(ctx, f, targetNode)

		// The workload is still running, so no inline unmount fires — only the periodic job
		// can reap the dead source. Wait for it to be torn down.
		ginkgo.By("Waiting for the cleanup job to reclaim the dead source")
		gomega.Eventually(ctx, func(ctx context.Context) (int, error) {
			return countFuseMountsForVolume(ctx, f, targetNode, pvName), nil
		}).WithTimeout(cleanupObserveTimeout).WithPolling(cleanupObservePoll).Should(gomega.Equal(0))
	})

	// With requiresRepublish, a container restart in a pod whose Mountpoint died must still see an I/O
	// error, never an empty directory that would silently hide the lost mount (writes would land in the
	// node's disk). Only this mount's Mountpoint is killed, so the mounter pod and its /comm survive, and
	// the cleanup job's reclaim of the dead source is observable there.
	// Not Serial: it kills only its own mount's Mountpoint, which no parallel spec shares.
	ginkgo.It("should keep failing I/O, not show an empty directory, when a workload container restarts after its Mountpoint dies", func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		pvName := resource.Pv.Name

		// Exits 1 once the marker appears, so the kubelet restarts the container (restartPolicy OnFailure).
		// The marker is in the container's own filesystem, so the restarted container starts without it.
		// The delay lets the exec that creates the marker return before the container goes away, as an exec
		// into a container that exits under it can hang.
		const restartMarker = "/tmp/restart-me"
		ginkgo.By("Creating a workload whose container exits when asked to")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline,
			fmt.Sprintf("while [ ! -e %s ]; do sleep 1; done; sleep 3; exit 1", restartMarker))
		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer deletePodsInOrder(ctx, f, []*v1.Pod{pod})
		targetNode := pod.Spec.NodeName

		checkWriteToPathSucceedEventually(ctx, f, pod, "/mnt/volume1/before.txt", 1024, time.Now().UTC().UnixNano())
		uid := statPath(ctx, f, targetNode, filepath.Join(commDirHostPath(ctx, f, targetNode), pvName)).uid

		ginkgo.By(fmt.Sprintf("Killing only this mount's Mountpoint (UID %d)", uid))
		pid := mountpointProcessRunningAs(ctx, f, targetNode, uid).pid
		_, stderr, err := execInMounterPod(ctx, f, targetNode, "kill -9 "+pid)
		framework.ExpectNoError(err, "killing Mountpoint pid %s: %s", pid, stderr)
		assertPodIOFails(ctx, f, pod, e2epod.VolumeMountPath1)

		ginkgo.By("Making the workload container exit, so the kubelet restarts it over the dead mount")
		triggerContainerExit(ctx, f, pod, restartMarker)
		restartedAt := time.Now()

		// One loop for both waits, to keep the spec short: the kubelet's restart attempts (with back-off) are
		// checked on every poll while the periodic cleanup reclaims the dead source. Each attempt, and the
		// requiresRepublish calls meanwhile, is a chance for a remount over the target to race it.
		ginkgo.By("Watching the restarts until the cleanup job reclaims the dead source: each restart must be refused or see failing I/O, never an empty directory")
		refused, startedWithFailingIO := 0, 0
		reclaimed := false
		deadline := restartedAt.Add(cleanupObserveTimeout)
		for !(reclaimed && time.Since(restartedAt) >= minRestartObservation) {
			if time.Now().After(deadline) {
				ginkgo.Fail(fmt.Sprintf("within %v, reclaimed=%v and restarts observed for %v (refused %d, started %d)",
					cleanupObserveTimeout, reclaimed, time.Since(restartedAt).Round(time.Second), refused, startedWithFailingIO))
			}
			current, err := f.ClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			state := current.Status.ContainerStatuses[0].State
			switch {
			case state.Running != nil:
				// Had the dead mount been replaced by an empty directory, the container would start and list it.
				assertPodIOFails(ctx, f, current, e2epod.VolumeMountPath1)
				startedWithFailingIO++
			case state.Waiting != nil && state.Waiting.Reason == "CreateContainerError":
				// The kubelet cannot bind-mount a dead FUSE mount into a new container, so it does not start.
				gomega.Expect(state.Waiting.Message).To(gomega.ContainSubstring("transport endpoint is not connected"),
					"the container must be refused because its volume's mount is dead, got: %s", state.Waiting.Message)
				refused++
			}
			if !reclaimed {
				reclaimed = countFuseMountsForVolume(ctx, f, targetNode, pvName) == 0
			}
			time.Sleep(restartObservePoll)
		}
		framework.Logf("Over %v: %d observation(s) of a refused restart, %d of a started container with failing I/O",
			time.Since(restartedAt).Round(time.Second), refused, startedWithFailingIO)
		gomega.Expect(refused+startedWithFailingIO).To(gomega.BeNumerically(">", 0),
			"observed no restart outcome for pod %s, so nothing was checked", pod.Name)

		// The workload still runs, so no unpublish fires: only the periodic cleanup reclaimed the dead source,
		// and it must have said so and removed the mount's credentials.
		ginkgo.By("Checking the cleanup job logged the reclaim and removed the mount's credentials")
		gomega.Eventually(ctx, func(ctx context.Context) bool {
			return verifyCSINodeLogs(ctx, f, targetNode, fmt.Sprintf("for volume %s is dead, cleaning up", pvName)) ||
				verifyCSINodeLogs(ctx, f, targetNode, fmt.Sprintf("for volume %s not in mount table, cleaning up", pvName))
		}).WithTimeout(time.Minute).WithPolling(cleanupObservePoll).Should(gomega.BeTrue(),
			"csi-node must log the reclaim of volume %s's dead source", pvName)
		credDir := filepath.Join(commDirHostPath(ctx, f, targetNode), pvName)
		_, err = execInCSINodePod(ctx, f, targetNode, fmt.Sprintf("test ! -e %q", credDir))
		framework.ExpectNoError(err, "the reclaimed mount's credential directory %s must be removed", credDir)
	})
}

// The restart watch runs at least minRestartObservation, which spans several of the kubelet's restart attempts,
// and until the cleanup job has reclaimed the dead source (at most one cleanupInterval).
const (
	minRestartObservation = time.Minute
	restartObservePoll    = 5 * time.Second
)

// triggerContainerExit makes the pod's container exit by creating `marker`, which its command waits for, and
// returns once the kubelet has recorded the exit.
func triggerContainerExit(ctx context.Context, f *framework.Framework, pod *v1.Pod, marker string) {
	triggered := time.Now().Add(-time.Second)
	// The container exits right after, so the exec may report an error; the recorded exit is the check.
	_, _, _ = e2epod.ExecCommandInContainerWithFullOutput(f, pod.Name, pod.Spec.Containers[0].Name, "/bin/sh", "-c", "touch "+marker)

	gomega.Eventually(ctx, func(ctx context.Context) error {
		current, err := f.ClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		last := current.Status.ContainerStatuses[0].LastTerminationState.Terminated
		if last == nil || last.FinishedAt.Time.Before(triggered) {
			return fmt.Errorf("container of pod %s has not exited since it was asked to", pod.Name)
		}
		return nil
	}).WithTimeout(f.Timeouts.PodStart).WithPolling(2 * time.Second).Should(gomega.Succeed())
}
