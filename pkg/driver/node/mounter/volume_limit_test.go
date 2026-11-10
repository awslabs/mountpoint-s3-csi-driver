package mounter

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	mountutils "k8s.io/mount-utils"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// Some of these tests capture klog's process-global output, so none may call t.Parallel.

func TestVolumeLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("is the running mounter's label", func(t *testing.T) {
		dm := limitDM(t, withLimit(runningMounterPod("mounter"), "5"))
		dm.SetFallbackVolumeLimit(2)
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, int64(5), volumeLimit(t, dm))
	})

	t.Run("is no limit from a mounter labeled 0", func(t *testing.T) {
		dm := limitDM(t, withLimit(runningMounterPod("mounter"), "0"))
		dm.SetFallbackVolumeLimit(2)
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, int64(0), volumeLimit(t, dm))
	})

	t.Run("is the fallback from a mounter with no valid label", func(t *testing.T) {
		dm := limitDM(t, runningMounterPod("mounter"))
		dm.SetFallbackVolumeLimit(2)
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, int64(2), volumeLimit(t, dm))
	})

	t.Run("is the fallback right away with no mounter", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetFallbackVolumeLimit(4)
		start := time.Now()
		assert.Equals(t, int64(4), volumeLimit(t, dm))
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("took %v, so it waited for a mounter", took)
		}
	})

	t.Run("is none with no mounter and no fallback", func(t *testing.T) {
		dm := limitDM(t)
		assert.Equals(t, int64(0), volumeLimit(t, dm))
	})

	t.Run("is the label of a running mounter that discovery hasn't found yet", func(t *testing.T) {
		dm := limitDM(t, withLimit(runningMounterPod("mounter"), "3"))
		dm.SetFallbackVolumeLimit(8)

		assert.Equals(t, int64(3), volumeLimit(t, dm))
		select {
		case <-dm.rediscoverCh:
		default:
			t.Fatal("VolumeLimit didn't ask StartCommDirWatch to look for the mounter")
		}
	})

	t.Run("is the fallback while more than one mounter runs", func(t *testing.T) {
		dm := limitDM(t, withLimit(runningMounterPod("a"), "4"), withLimit(runningMounterPod("b"), "32"))
		dm.SetFallbackVolumeLimit(2)
		assert.Equals(t, int64(2), volumeLimit(t, dm))
	})

	t.Run("fails when it can't tell whether a mounter runs", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetFallbackVolumeLimit(4)
		dm.clientset.(*fake.Clientset).PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("API server unavailable")
		})

		_, err := dm.VolumeLimit(ctx)

		assert.Contains(t, err.Error(), "can't tell whether an s3-csi-daemonset-mounter runs on node test-node")
		// Kubelet never got a limit, so there's none it has.
		assert.Equals(t, true, dm.limit.advertisedAt.IsZero())
	})

	t.Run("follows the mounter as it comes and goes", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetFallbackVolumeLimit(2)
		assert.Equals(t, int64(2), volumeLimit(t, dm))

		dm.setMounter(&mounterPod{maxVolumes: 6})
		assert.Equals(t, int64(6), volumeLimit(t, dm))

		dm.setMounter(nil)
		assert.Equals(t, int64(2), volumeLimit(t, dm))
	})

	t.Run("logs a change once", func(t *testing.T) {
		dm := limitDM(t)
		logs := testutil.CaptureKlog(t)

		dm.VolumeLimit(ctx)
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 3})
		dm.VolumeLimit(ctx)
		dm.VolumeLimit(ctx)

		assert.Equals(t, 1, strings.Count(logs.String(), "advertising no volume limit for node test-node: no s3-csi-daemonset-mounter runs on this node yet"))
		assert.Equals(t, 1, strings.Count(logs.String(), "advertising a volume limit of 3 for node test-node"))
	})

	t.Run("logs a new reason for the same limit", func(t *testing.T) {
		dm := limitDM(t)
		logs := testutil.CaptureKlog(t)

		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 0})
		dm.VolumeLimit(ctx)

		assert.Contains(t, logs.String(), "advertising no volume limit for node test-node: its s3-csi-daemonset-mounter's "+MounterMaxVolumesLabel+" label is 0")
	})
}

func TestWarnIfVolumeLimitStale(t *testing.T) {
	ctx := context.Background()
	const stale = "the volume limit kubelet has for node test-node is none, but its s3-csi-daemonset-mounter's is 4"

	t.Run("warns once when kubelet keeps a limit that no longer holds", func(t *testing.T) {
		dm := limitDM(t)
		dm.staleVolumeLimitWarnAfter = 0
		// Kubelet registered the node pod before its mounter ran, and never asks again.
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()
		assert.Contains(t, logs.String(), stale)
		assert.Contains(t, logs.String(), "kubectl delete pod -n "+mounterNamespace+" -l app=s3-csi-node --field-selector spec.nodeName=test-node")

		warned := logs.Len()
		dm.warnIfVolumeLimitStale()
		assert.Equals(t, warned, logs.Len())
	})

	t.Run("warns when kubelet keeps a fallback the mounter's limit differs from", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetFallbackVolumeLimit(2)
		dm.staleVolumeLimitWarnAfter = 0
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Contains(t, logs.String(), "the volume limit kubelet has for node test-node is 2, but its s3-csi-daemonset-mounter's is 4")
	})

	t.Run("warns again when the same limit goes stale again after kubelet caught up", func(t *testing.T) {
		dm := limitDM(t)
		dm.staleVolumeLimitWarnAfter = 0
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		dm.warnIfVolumeLimitStale()
		// Kubelet catches up, then reads no limit from a replacement mounter labeled 0, then the mounter labeled 4 is back.
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 0})
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Contains(t, logs.String(), stale)
	})

	t.Run("doesn't warn while kubelet re-reads the limit", func(t *testing.T) {
		dm := limitDM(t)
		dm.staleVolumeLimitWarnAfter = time.Minute
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		// Kubelet asked less than staleVolumeLimitWarnAfter ago.
		dm.warnIfVolumeLimitStale()
		// Kubelet has the current limit, however long ago it asked.
		dm.VolumeLimit(ctx)
		dm.staleVolumeLimitWarnAfter = 0
		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})

	t.Run("doesn't warn before kubelet first asks", func(t *testing.T) {
		dm := limitDM(t)
		dm.staleVolumeLimitWarnAfter = 0
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})

	t.Run("doesn't warn while no mounter runs", func(t *testing.T) {
		dm := limitDM(t)
		dm.staleVolumeLimitWarnAfter = 0
		dm.setMounter(&mounterPod{maxVolumes: 4})
		dm.VolumeLimit(ctx)
		dm.setMounter(nil)
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})
}

func TestStaleCheckInterval(t *testing.T) {
	// Every second while a restarting mounter is back within its restart time, then less often.
	assert.Equals(t, commDirStaleCheckInterval, staleCheckInterval(0))
	assert.Equals(t, commDirStaleCheckInterval, staleCheckInterval(commDirFastStaleCheckWindow-time.Second))
	assert.Equals(t, commDirSlowStaleCheckInterval, staleCheckInterval(commDirFastStaleCheckWindow))
	assert.Equals(t, commDirSlowStaleCheckInterval, staleCheckInterval(time.Hour))
}

func TestMountWithNoMounterLooksForOneRightAway(t *testing.T) {
	dm := limitDM(t)
	target := filepath.Join(dm.kubeletPath, "pods", "workload-uid", "volumes", "kubernetes.io~csi", "pv", "mount")

	err := dm.Mount(context.Background(), "bucket", target, credentialprovider.ProvideContext{VolumeID: "pv"},
		nil, mountpoint.ParseArgs(nil), "", nil)

	assert.ErrorIs(t, err, ErrCommDirNotReady)
	assert.Contains(t, err.Error(), "no s3-csi-daemonset-mounter is running on node test-node yet")
	select {
	case <-dm.rediscoverCh:
	default:
		t.Fatal("Mount didn't ask StartCommDirWatch to look for a mounter")
	}
}

// limitDM is a DaemonsetMounter on test-node, built as the driver builds it, whose API server holds objects.
func limitDM(t *testing.T, objects ...runtime.Object) *DaemonsetMounter {
	t.Helper()
	dm := NewDaemonsetMounter(fake.NewSimpleClientset(objects...), "test-node", mpmounter.NewWithMount(mountutils.NewFakeMounter(nil)),
		nil, nil, nil, nil, "", cluster.DefaultKubernetes)
	dm.kubeletPath = t.TempDir()
	return dm
}

// volumeLimit returns dm's volume limit, failing the test if VolumeLimit fails.
func volumeLimit(t *testing.T, dm *DaemonsetMounter) int64 {
	t.Helper()
	limit, err := dm.VolumeLimit(context.Background())
	assert.NoError(t, err)
	return limit
}

// withLimit sets the mounter pod's MounterMaxVolumesLabel.
func withLimit(pod *corev1.Pod, limit string) *corev1.Pod {
	pod.Labels[MounterMaxVolumesLabel] = limit
	return pod
}
