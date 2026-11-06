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
		assert.Equals(t, int64(5), volumeLimit(t, dm))
	})

	t.Run("is no limit from a mounter labeled 0", func(t *testing.T) {
		dm := limitDM(t, withLimit(runningMounterPod("mounter"), "0"))
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, int64(0), volumeLimit(t, dm))
	})

	t.Run("is none from a mounter with no valid label", func(t *testing.T) {
		dm := limitDM(t, runningMounterPod("mounter"))
		assert.NoError(t, dm.DiscoverMounter(ctx))
		assert.Equals(t, noMounterVolumeLimit, volumeLimit(t, dm))
	})

	t.Run("is none with no mounter, once the startup wait is over", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(100*time.Millisecond, time.Minute)

		start := time.Now()
		assert.Equals(t, noMounterVolumeLimit, volumeLimit(t, dm))
		if waited := time.Since(start); waited < 90*time.Millisecond {
			t.Fatalf("returned after %v, before the startup wait was over", waited)
		}

		start = time.Now()
		assert.Equals(t, noMounterVolumeLimit, volumeLimit(t, dm))
		if waited := time.Since(start); waited > 50*time.Millisecond {
			t.Fatalf("waited %v again after the startup wait", waited)
		}
	})

	t.Run("waits at startup for a mounter that starts", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(30*time.Second, time.Minute)
		stopCh := make(chan struct{})
		defer close(stopCh)
		go dm.StartCommDirWatch(stopCh)
		go func() {
			time.Sleep(100 * time.Millisecond)
			_, _ = dm.clientset.CoreV1().Pods(mounterNamespace).Create(ctx, withLimit(runningMounterPod("mounter"), "3"), metav1.CreateOptions{})
		}()

		start := time.Now()
		assert.Equals(t, int64(3), volumeLimit(t, dm))
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
		dm.SetVolumeLimitTimingForTesting(30*time.Second, time.Minute)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		limit, err := dm.VolumeLimit(cancelled)
		assert.NoError(t, err)
		assert.Equals(t, noMounterVolumeLimit, limit)
		// Kubelet never got that answer, so it doesn't count as the limit kubelet has.
		assert.Equals(t, true, dm.limit.advertisedAt.IsZero())
	})

	t.Run("keeps the limit of a mounter that went away until another one runs", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)
		dm.setMounter(&mounterPod{commDir: "old", maxVolumes: 3})
		dm.setMounter(nil)
		assert.Equals(t, int64(3), volumeLimit(t, dm))

		dm.setMounter(&mounterPod{commDir: "new", maxVolumes: 6})
		assert.Equals(t, int64(6), volumeLimit(t, dm))
	})

	t.Run("keeps only the limit of the mounter that just went away", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)
		dm.setMounter(&mounterPod{commDir: "a", maxVolumes: 5})
		dm.setMounter(nil)
		// The next mounter has no valid label, so it has no limit to keep.
		dm.setMounter(&mounterPod{commDir: "b", maxVolumes: -1})
		dm.setMounter(nil)

		assert.Equals(t, noMounterVolumeLimit, volumeLimit(t, dm))
	})

	t.Run("logs a change once", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)
		logs := testutil.CaptureKlog(t)

		dm.VolumeLimit(ctx)
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 3})
		dm.VolumeLimit(ctx)
		dm.VolumeLimit(ctx)

		assert.Equals(t, 1, strings.Count(logs.String(), "advertising no volume limit for node test-node: no s3-csi-daemonset-mounter runs on this node"))
		assert.Equals(t, 1, strings.Count(logs.String(), "advertising a volume limit of 3 for node test-node"))
	})

	t.Run("logs a new reason for the same limit", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)
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
		dm.SetVolumeLimitTimingForTesting(0, 0)
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
		dm.SetVolumeLimitTimingForTesting(0, 0)
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
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)
		dm.VolumeLimit(ctx)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		// Kubelet asked less than staleVolumeLimitWarnAfter ago.
		dm.warnIfVolumeLimitStale()
		// Kubelet has the current limit, however long ago it asked.
		dm.VolumeLimit(ctx)
		dm.SetVolumeLimitTimingForTesting(0, 0)
		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})

	t.Run("doesn't warn before kubelet first asks", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})

	t.Run("doesn't warn when kubelet keeps the limit of a mounter that went away", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetVolumeLimitTimingForTesting(0, 0)
		dm.setMounter(&mounterPod{maxVolumes: 4})
		dm.VolumeLimit(ctx)
		dm.setMounter(nil)
		logs := testutil.CaptureKlog(t)

		dm.warnIfVolumeLimitStale()

		assert.Equals(t, false, strings.Contains(logs.String(), "the volume limit kubelet has"))
	})
}

func TestVolumeLimitOnNodeWithStartupTaint(t *testing.T) {
	ctx := context.Background()
	taint := corev1.Taint{Key: "s3.csi.aws.com/agent-not-ready", Effect: corev1.TaintEffectNoExecute}

	t.Run("fails at every call until a mounter runs", func(t *testing.T) {
		dm := limitDM(t, testNode(taint))
		dm.SetStartupTaintKey(taint.Key)
		dm.SetVolumeLimitTimingForTesting(100*time.Millisecond, time.Minute)

		for range 2 {
			start := time.Now()
			_, err := dm.VolumeLimit(ctx)
			if err == nil {
				t.Fatal("VolumeLimit succeeded with no mounter on a node with the startup taint")
			}
			assert.Contains(t, err.Error(), "no s3-csi-daemonset-mounter runs on node test-node yet")
			if waited := time.Since(start); waited < 90*time.Millisecond {
				t.Fatalf("failed after %v, before waiting for a mounter", waited)
			}
		}

		dm.setMounter(&mounterPod{maxVolumes: 3})
		assert.Equals(t, int64(3), volumeLimit(t, dm))
	})

	t.Run("waits for a mounter that starts", func(t *testing.T) {
		dm := limitDM(t, testNode(taint))
		dm.SetStartupTaintKey(taint.Key)
		dm.SetVolumeLimitTimingForTesting(30*time.Second, time.Minute)
		go func() {
			time.Sleep(100 * time.Millisecond)
			dm.setMounter(&mounterPod{maxVolumes: 3})
		}()

		assert.Equals(t, int64(3), volumeLimit(t, dm))
	})

	t.Run("keeps the limit of a mounter that went away", func(t *testing.T) {
		dm := limitDM(t, testNode(taint))
		dm.SetStartupTaintKey(taint.Key)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)
		dm.setMounter(&mounterPod{maxVolumes: 3})
		dm.setMounter(nil)

		assert.Equals(t, int64(3), volumeLimit(t, dm))
	})

	t.Run("is none on a node without it", func(t *testing.T) {
		dm := limitDM(t, testNode())
		dm.SetStartupTaintKey(taint.Key)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)

		assert.Equals(t, noMounterVolumeLimit, volumeLimit(t, dm))
	})

	t.Run("is none when the node can't be read", func(t *testing.T) {
		dm := limitDM(t)
		dm.SetStartupTaintKey(taint.Key)
		dm.SetVolumeLimitTimingForTesting(0, time.Minute)

		assert.Equals(t, noMounterVolumeLimit, volumeLimit(t, dm))
	})
}

func TestRunningMounterVolumeLimit(t *testing.T) {
	dm := limitDM(t)
	_, ok := dm.RunningMounterVolumeLimit()
	assert.Equals(t, false, ok)

	dm.setMounter(&mounterPod{maxVolumes: 4})
	limit, ok := dm.RunningMounterVolumeLimit()
	assert.Equals(t, true, ok)
	assert.Equals(t, int64(4), limit)

	// The limit a mounter that went away leaves isn't one a running mounter takes.
	dm.setMounter(nil)
	_, ok = dm.RunningMounterVolumeLimit()
	assert.Equals(t, false, ok)
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

// testNode is the test-node Node, with taints.
func testNode(taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}, Spec: corev1.NodeSpec{Taints: taints}}
}

// withLimit sets the mounter pod's MounterMaxVolumesLabel.
func withLimit(pod *corev1.Pod, limit string) *corev1.Pod {
	pod.Labels[MounterMaxVolumesLabel] = limit
	return pod
}
