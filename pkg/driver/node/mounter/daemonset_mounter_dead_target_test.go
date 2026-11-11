package mounter_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// isMounted reports whether path is in the fake kernel mount table.
func (testCtx *dmTestCtx) isMounted(path string) bool {
	mounts, err := testCtx.mount.List()
	assert.NoError(testCtx.t, err)
	for _, mp := range mounts {
		if mp.Path == path {
			return true
		}
	}
	return false
}

// killAllMounts makes the source and the given targets report ENOTCONN while they stay in the mount
// table, as they do after the mounter pod restarts and every Mountpoint on the node exits.
func (testCtx *dmTestCtx) killAllMounts(targets ...string) {
	testCtx.mount.MountCheckErrors = map[string]error{
		mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID): healthPathErr(syscall.ENOTCONN),
	}
	for _, target := range targets {
		testCtx.mount.MountCheckErrors[target] = healthPathErr(syscall.ENOTCONN)
	}
}

// republish runs kubelet's NodePublishVolume for an already-published pod. With serveMount it also plays
// the mounter: it receives the mount options and mounts the source, as a fresh FUSE mount needs.
func (testCtx *dmTestCtx) republish(podUID string, volumeCtx map[string]string, serveMount bool) error {
	res := make(chan error, 1)
	go func() {
		res <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(podUID),
			credentialprovider.ProvideContext{WorkloadPodID: podUID, VolumeID: testCtx.volumeID},
			volumeCtx, mountpoint.ParseArgs(nil), "", nil)
	}()
	if serveMount {
		testCtx.receiveMountOptions()
		testCtx.mount.Mount("mountpoint-s3", mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID), "fuse", nil)
	}
	return <-res
}

func podInfo(podUID string) map[string]string {
	return map[string]string{
		"csi.storage.k8s.io/pod.namespace": "team-a",
		"csi.storage.k8s.io/pod.name":      "trainer-0",
		"csi.storage.k8s.io/pod.uid":       podUID,
	}
}

// TestMount_DeadTargetRemount walks the "mounter pod restarted under a live workload" case: the restart
// kills the shared source and the pod's bind mount alike, and kubelet's next republish of the dead
// target unmounts it, mounts the source again, binds it, and reports Aborted once so kubelet shows a
// FailedMount Event. The republish may come before or after the periodic cleanup reaps the dead source.
func TestMount_DeadTargetRemount(t *testing.T) {
	for _, cleanupFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("cleanup ran first: %t", cleanupFirst), func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath(testCtx.podUID)
			source := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
			volumeCtx := podInfo(testCtx.podUID)

			testCtx.mountVolume()
			// Healthy target: a republish refreshes credentials and returns nil.
			assert.NoError(t, testCtx.republish(testCtx.podUID, volumeCtx, false))

			testCtx.killAllMounts(target)
			if cleanupFirst {
				testCtx.dm.CleanupOrphans()
				assert.Equals(t, false, testCtx.isMounted(source))
				assert.Equals(t, true, testCtx.isMounted(target)) // cleanup never touches workload targets
			}

			logs := testutil.CaptureKlog(t)
			err := testCtx.republish(testCtx.podUID, volumeCtx, true)
			assert.Equals(t, codes.Aborted, status.Code(err))
			assertRemountMessage(t, status.Convert(err).Message(), testCtx.volumeID, "team-a/trainer-0 ("+testCtx.podUID+")")
			// The gRPC interceptor (driver.go) already logs the returned error; Mount adds no line of its own.
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.HasPrefix(line, "E") || strings.HasPrefix(line, "W") {
					t.Errorf("dead-target remount logged at Warning/Error level: %q", line)
				}
			}

			// The dead target was unmounted and bound again to a freshly mounted source.
			testCtx.assertUnmounted(target)
			assert.Equals(t, true, testCtx.isMounted(source))
			assert.Equals(t, true, testCtx.isMounted(target))
			uid := uint32(testCtx.ownerOf(filepath.Join(testCtx.commDir, testCtx.volumeID))[0])
			assert.Equals(t, true, testCtx.dm.UIDInUseForTesting(uid))

			// The next republish finds a healthy target and succeeds.
			assert.NoError(t, testCtx.republish(testCtx.podUID, volumeCtx, false))

			// NodeUnpublishVolume on pod deletion releases the new mount like any other.
			assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, target,
				credentialprovider.CleanupContext{PodID: testCtx.podUID, VolumeID: testCtx.volumeID}))
			assert.Equals(t, false, testCtx.isMounted(target))
			assert.Equals(t, false, testCtx.isMounted(source))
			assert.Equals(t, false, testCtx.dm.UIDInUseForTesting(uid))
		})
	}
}

// TestMount_DeadTargetRemountShared checks two pods sharing a PV: the first republish mounts the source
// again, the second binds to it, and the source stays until the last pod is unpublished.
func TestMount_DeadTargetRemountShared(t *testing.T) {
	testCtx := setupDM(t)
	podA, podB := testCtx.podUID, uuid.New().String()
	targetA, targetB := testCtx.targetPath(podA), testCtx.targetPath(podB)
	source := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)

	testCtx.mountVolume()
	assert.NoError(t, testCtx.republish(podB, podInfo(podB), false)) // pod B shares the running source

	testCtx.killAllMounts(targetA, targetB)
	testCtx.dm.CleanupOrphans()

	errA := testCtx.republish(podA, podInfo(podA), true)
	assert.Equals(t, codes.Aborted, status.Code(errA))
	errB := testCtx.republish(podB, podInfo(podB), false) // shares the new source, no new FUSE mount
	assert.Equals(t, codes.Aborted, status.Code(errB))
	assertRemountMessage(t, status.Convert(errB).Message(), testCtx.volumeID, "team-a/trainer-0 ("+podB+")")
	assert.Equals(t, true, testCtx.isMounted(targetA))
	assert.Equals(t, true, testCtx.isMounted(targetB))

	cleanupCtx := func(pod string) credentialprovider.CleanupContext {
		return credentialprovider.CleanupContext{PodID: pod, VolumeID: testCtx.volumeID}
	}
	assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, targetA, cleanupCtx(podA)))
	assert.Equals(t, true, testCtx.isMounted(source)) // pod B still uses it
	assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, targetB, cleanupCtx(podB)))
	assert.Equals(t, false, testCtx.isMounted(source))
}

// TestMount_DeadTargetUnmountFails checks that a dead target the unmount cannot remove is reported as a
// retryable (non-final) error, and nothing is mounted over it.
func TestMount_DeadTargetUnmountFails(t *testing.T) {
	testCtx := setupDM(t)
	target := testCtx.targetPath(testCtx.podUID)
	source := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)

	testCtx.mountVolume()
	testCtx.killAllMounts(target)
	testCtx.dm.CleanupOrphans()
	testCtx.mount.UnmountFunc = func(path string) error {
		if path == target {
			return errors.New("device or resource busy")
		}
		return nil
	}

	err := testCtx.republish(testCtx.podUID, podInfo(testCtx.podUID), false)
	assert.Equals(t, codes.Aborted, status.Code(err))
	if !strings.Contains(err.Error(), "failed to unmount dead target") {
		t.Errorf("error %q is not the dead-target unmount failure", err)
	}
	assert.Equals(t, true, testCtx.isMounted(target))
	assert.Equals(t, false, testCtx.isMounted(source))
}

// assertRemountMessage checks the message: printable ASCII, the action first (so a truncated Event keeps
// it), then the volume and pod identity.
func assertRemountMessage(t *testing.T, msg, pvName, pod string) {
	t.Helper()
	const action = "restart the containers using it, or delete and recreate this pod"
	for _, want := range []string{action, "Transport endpoint is not connected", "Volume " + pvName, "pod " + pod} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
	}
	if a, v := strings.Index(msg, action), strings.Index(msg, "Volume "+pvName); a < 0 || v < 0 || a > v {
		t.Errorf("error message %q does not put the action before the identifiers", msg)
	}
	if strings.Contains(msg, "s3-csi-daemonset-mounter") {
		t.Errorf("error message %q names the V3 mounter pod, which is wrong for legacy V1/V2 targets", msg)
	}
	for _, r := range msg {
		if r > 0x7e || r < 0x20 {
			t.Errorf("error message %q is not printable ASCII (rune %q)", msg, r)
			break
		}
	}
}

// TestMount_DeadTargetPodIdentity checks the pod identity in the message for every combination of
// podInfoOnMount keys kubelet may send (podInfoOnMount off, or an older CSIDriver object, sends none).
func TestMount_DeadTargetPodIdentity(t *testing.T) {
	tests := []struct {
		name    string
		keys    []string
		wantPod func(uid string) string
	}{
		{"all pod info", []string{"namespace", "name", "uid"}, func(uid string) string { return "team-a/trainer-0 (" + uid + ")" }},
		{"namespace without name keeps the namespace", []string{"namespace", "uid"}, func(uid string) string { return "with UID " + uid + " in namespace team-a" }},
		{"name without namespace keeps the name", []string{"name", "uid"}, func(uid string) string { return "trainer-0 (" + uid + ")" }},
		{"no pod info uses the workload pod UID", nil, func(uid string) string { return "with UID " + uid }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCtx := setupDM(t)
			all := podInfo(testCtx.podUID)
			volumeCtx := map[string]string{}
			for _, k := range tt.keys {
				volumeCtx["csi.storage.k8s.io/pod."+k] = all["csi.storage.k8s.io/pod."+k]
			}
			testCtx.mountVolume()
			testCtx.killAllMounts(testCtx.targetPath(testCtx.podUID))

			err := testCtx.republish(testCtx.podUID, volumeCtx, true)
			assert.Equals(t, codes.Aborted, status.Code(err))
			assertRemountMessage(t, status.Convert(err).Message(), testCtx.volumeID, tt.wantPod(testCtx.podUID))
		})
	}
}

// TestMount_UnknownTargetStateIsRetryable checks that a target whose health cannot be determined still
// fails with the plain retryable error (Internal at the gRPC boundary), not the dead-target Aborted.
func TestMount_UnknownTargetStateIsRetryable(t *testing.T) {
	kubeletPath := t.TempDir()
	const volumeID = "pv-unknown-target"
	targetDir := filepath.Join(kubeletPath, "pods", "pod-uid-321", "volumes", "kubernetes.io~csi", volumeID, "mount")
	assert.NoError(t, os.MkdirAll(targetDir, 0750))

	dm := mounter.NewDaemonsetMounter(nil, "test-node",
		mpmounter.NewWithMount(&fakeHealthMount{isMountPointErr: healthPathErr(syscall.EINTR)}),
		nil, nil, nil, nil, "", cluster.DefaultKubernetes)
	err := dm.Mount(context.Background(), "test-bucket", targetDir,
		credentialprovider.ProvideContext{VolumeID: volumeID, WorkloadPodID: "pod-uid-321"},
		nil, mountpoint.Args{}, "", nil)

	if err == nil {
		t.Fatal("expected an error for an unknown target state")
	}
	assert.Equals(t, codes.Unknown, status.Code(err))
	if !strings.Contains(err.Error(), "will retry") {
		t.Errorf("error %q is not the retryable unknown-state error", err)
	}
}
