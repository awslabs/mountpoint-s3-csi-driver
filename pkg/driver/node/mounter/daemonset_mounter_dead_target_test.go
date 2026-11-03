package mounter_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// TestMount_DeadTargetLifecycle walks the "mounter pod restarted under a live workload" case end to end:
// a healthy republish still succeeds, a republish after the target's Mountpoint died fails with Aborted
// naming the workload pod, and NodeUnpublishVolume of the dead target still succeeds and cleans up.
func TestMount_DeadTargetLifecycle(t *testing.T) {
	testCtx := setupDM(t)
	target := testCtx.targetPath(testCtx.podUID)

	// Absent target: a fresh mount goes all the way through.
	testCtx.mountVolume()
	uid := uint32(testCtx.ownerOf(filepath.Join(testCtx.commDir, testCtx.volumeID))[0])
	assert.Equals(t, true, testCtx.dm.UIDInUseForTesting(uid))

	provideCtx := credentialprovider.ProvideContext{
		WorkloadPodID: testCtx.podUID,
		VolumeID:      testCtx.volumeID,
		PodNamespace:  "team-a",
	}
	volumeCtx := map[string]string{"csi.storage.k8s.io/pod.name": "trainer-0"}

	// Healthy target: a republish (requiresRepublish) refreshes credentials and returns nil.
	err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, provideCtx, volumeCtx, mountpoint.ParseArgs(nil), "", nil)
	assert.NoError(t, err)

	// The mounter pod restarts: every Mountpoint on the node exits and the target reports ENOTCONN.
	testCtx.mount.MountCheckErrors = map[string]error{target: healthPathErr(syscall.ENOTCONN)}

	err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, provideCtx, volumeCtx, mountpoint.ParseArgs(nil), "", nil)
	assert.Equals(t, codes.Aborted, status.Code(err))
	msg := status.Convert(err).Message()
	for _, want := range []string{testCtx.volumeID, "team-a/trainer-0", testCtx.podUID, "recreate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
	}
	for _, r := range msg {
		if r > 0x7e || (r < 0x20 && r != '\t') {
			t.Errorf("error message %q is not printable ASCII (rune %q)", msg, r)
			break
		}
	}
	// The failed republish did not tear down the mount: the UID is still held for NodeUnpublishVolume.
	assert.Equals(t, true, testCtx.dm.UIDInUseForTesting(uid))

	// NodeUnpublishVolume on pod deletion: the dead target is unmounted and the mount released.
	err = testCtx.dm.Unmount(testCtx.ctx, target, credentialprovider.CleanupContext{
		PodID:    testCtx.podUID,
		VolumeID: testCtx.volumeID,
	})
	assert.NoError(t, err)
	testCtx.assertUnmounted(target)
	assert.Equals(t, false, testCtx.dm.UIDInUseForTesting(uid))
}

// TestMount_DeadTargetWithoutPodName checks the message still identifies the pod when kubelet sent no
// pod name or namespace (podInfoOnMount off, or an older CSIDriver object).
func TestMount_DeadTargetWithoutPodName(t *testing.T) {
	kubeletPath := t.TempDir()
	const volumeID = "pv-no-pod-info"
	targetDir := filepath.Join(kubeletPath, "pods", "pod-uid-789", "volumes", "kubernetes.io~csi", volumeID, "mount")
	assert.NoError(t, os.MkdirAll(targetDir, 0750))

	dm := newHealthDM(&fakeHealthMount{isMountPointErr: healthPathErr(syscall.ENOTCONN)})
	err := dm.Mount(context.Background(), "test-bucket", targetDir,
		credentialprovider.ProvideContext{VolumeID: "handle-1", WorkloadPodID: "pod-uid-789"},
		nil, mountpoint.Args{}, "", nil)

	assert.Equals(t, codes.Aborted, status.Code(err))
	msg := status.Convert(err).Message()
	for _, want := range []string{volumeID, "pod-uid-789"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
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
