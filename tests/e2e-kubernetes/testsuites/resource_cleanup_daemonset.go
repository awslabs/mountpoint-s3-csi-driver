package custom_testsuites

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
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
}
