package mounter

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, int64(5), dm.VolumeLimit(ctx))
	})

	t.Run("is no limit from a mounter labeled 0", func(t *testing.T) {
		dm := limitDM(t, withLimit(runningMounterPod("mounter"), "0"))
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, int64(0), dm.VolumeLimit(ctx))
	})

	t.Run("is none from a mounter with no valid label", func(t *testing.T) {
		dm := limitDM(t, runningMounterPod("mounter"))
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, noMounterVolumeLimit, dm.VolumeLimit(ctx))
	})

	t.Run("is none with no mounter, once the startup wait is over", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(100*time.Millisecond, time.Minute, time.Minute)

		start := time.Now()
		assert.Equals(t, noMounterVolumeLimit, dm.VolumeLimit(ctx))
		if waited := time.Since(start); waited < 90*time.Millisecond {
			t.Fatalf("returned after %v, before the startup wait was over", waited)
		}

		start = time.Now()
		assert.Equals(t, noMounterVolumeLimit, dm.VolumeLimit(ctx))
		if waited := time.Since(start); waited > 50*time.Millisecond {
			t.Fatalf("waited %v again after the startup wait", waited)
		}
	})

	t.Run("waits at startup for a mounter that starts", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(30*time.Second, time.Minute, time.Minute)
		stopCh := make(chan struct{})
		defer close(stopCh)
		go dm.StartCommDirWatch(stopCh)
		go func() {
			time.Sleep(100 * time.Millisecond)
			_, _ = dm.clientset.CoreV1().Pods(mounterNamespace).Create(ctx, withLimit(runningMounterPod("mounter"), "3"), metav1.CreateOptions{})
		}()

		start := time.Now()
		assert.Equals(t, int64(3), dm.VolumeLimit(ctx))
		if waited := time.Since(start); waited > 10*time.Second {
			t.Fatalf("returned after %v, not when the mounter was found", waited)
		}
		select {
		case <-dm.MounterFound():
		default:
			t.Fatal("MounterFound is not closed after the mounter was found")
		}
	})

	t.Run("stops waiting when kubelet gives up", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(30*time.Second, time.Minute, time.Minute)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		assert.Equals(t, noMounterVolumeLimit, dm.VolumeLimit(cancelled))
		// Kubelet never got that answer, so it doesn't count as the limit kubelet has.
		assert.Equals(t, true, dm.limit.advertisedAt.IsZero())
	})

	t.Run("keeps the limit of a mounter that went away while it is replaced, then is none", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 200*time.Millisecond, time.Minute)
		dm.setMounter(&mounterPod{commDir: "old", maxVolumes: 3})
		dm.setMounter(nil)
		assert.Equals(t, int64(3), dm.VolumeLimit(ctx))

		time.Sleep(250 * time.Millisecond)
		assert.Equals(t, noMounterVolumeLimit, dm.VolumeLimit(ctx))

		dm.setMounter(&mounterPod{commDir: "new", maxVolumes: 6})
		assert.Equals(t, int64(6), dm.VolumeLimit(ctx))
	})

	t.Run("keeps only the limit of the mounter that just went away", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute, time.Minute)
		dm.setMounter(&mounterPod{commDir: "a", maxVolumes: 5})
		dm.setMounter(nil)
		// The next mounter has no valid label, so it has no limit for the grace to keep.
		dm.setMounter(&mounterPod{commDir: "b", maxVolumes: -1})
		dm.setMounter(nil)

		assert.Equals(t, noMounterVolumeLimit, dm.VolumeLimit(ctx))
	})

	t.Run("logs a change once", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0, time.Minute)
		dm.setMounter(&mounterPod{maxVolumes: 3})
		logs := testutil.CaptureKlog(t)

		dm.VolumeLimit(ctx)
		dm.VolumeLimit(ctx)
		dm.setMounter(nil)
		dm.VolumeLimit(ctx)

		assert.Equals(t, 1, strings.Count(logs.String(), "advertising a volume limit of 3 for node test-node"))
		assert.Equals(t, 1, strings.Count(logs.String(), "advertising no volume limit for node test-node: no s3-csi-daemonset-mounter runs on this node"))
	})

	t.Run("logs a new reason for the same limit", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0, time.Minute)
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
		dm.SetVolumeLimitTimingForTesting(0, 0, 0)
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

	t.Run("warns again when the same limit goes stale again after kubelet caught up", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0, 0)
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		dm.warnIfVolumeLimitStale()
		// Kubelet catches up, the mounter goes away and kubelet reads that too, then the mounter is back.
		dm.VolumeLimit(ctx)
		dm.setMounter(nil)
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Contains(t, logs.String(), stale)
	})

	t.Run("doesn't warn while kubelet re-reads the limit", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0, time.Minute)
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		// Kubelet asked less than staleVolumeLimitWarnAfter ago.
		dm.warnIfVolumeLimitStale()
		// Kubelet has the current limit, however long ago it asked.
		dm.VolumeLimit(ctx)
		dm.SetVolumeLimitTimingForTesting(0, 0, 0)
		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})

	t.Run("doesn't warn before kubelet first asks", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0, 0)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})

	t.Run("doesn't warn when kubelet keeps the limit of a mounter that went away", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0, 0)
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

// limitDM is a DaemonsetMounter on test-node, built as the driver builds it, whose API server holds pods.
func limitDM(t *testing.T, pods ...*corev1.Pod) *DaemonsetMounter {
	t.Helper()
	var objects []runtime.Object
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	dm := NewDaemonsetMounter(fake.NewSimpleClientset(objects...), "test-node", mpmounter.NewWithMount(mountutils.NewFakeMounter(nil)),
		nil, nil, nil, nil, "", cluster.DefaultKubernetes)
	dm.kubeletPath = t.TempDir()
	return dm
}

// withLimit sets the mounter pod's MounterMaxVolumesLabel.
func withLimit(pod *corev1.Pod, limit string) *corev1.Pod {
	pod.Labels[MounterMaxVolumesLabel] = limit
	return pod
}
