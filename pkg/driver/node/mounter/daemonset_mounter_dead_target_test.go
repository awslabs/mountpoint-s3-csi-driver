package mounter_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/golang/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mountutils "k8s.io/mount-utils"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// strictUnmountMounter fails Unmount of a path that is not in the mount table, the way umount(8)
// exits 32 ("not mounted"). FakeMounter alone returns nil there, which would hide a missed unmount.
type strictUnmountMounter struct {
	*mountutils.FakeMounter
}

func (m *strictUnmountMounter) Unmount(target string) error {
	mounts, err := m.FakeMounter.List()
	if err != nil {
		return err
	}
	for _, mp := range mounts {
		if mp.Path == target {
			return m.FakeMounter.Unmount(target)
		}
	}
	return fmt.Errorf("unmount failed: exit status 32: umount: %s: not mounted", target)
}

// useStrictUnmount rebuilds the mounter around a strictUnmountMounter. The rest of the wiring matches setupDM.
func (testCtx *dmTestCtx) useStrictUnmount() {
	strict := &strictUnmountMounter{testCtx.mount}
	credProvider := mock_credentialprovider.NewMockProviderInterface(gomock.NewController(testCtx.t))
	credProvider.EXPECT().Provide(gomock.Any(), gomock.Any()).
		Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil).AnyTimes()
	credProvider.EXPECT().Cleanup(gomock.Any()).Return(nil).AnyTimes()
	testCtx.dm = mounter.NewDaemonsetMounter(testCtx.client, testCtx.nodeName,
		mpmounter.NewWithMount(strict), credProvider,
		func(target string, opts mpmounter.MountOptions) (int, error) {
			testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
			fd, err := syscall.Dup(int(mountertest.OpenDevNull(testCtx.t).Fd()))
			assert.NoError(testCtx.t, err)
			return fd, nil
		},
		func(source, target string) error {
			return testCtx.mount.Mount(source, target, "bind", []string{"bind"})
		},
		func() ([]mountutils.MountInfo, error) {
			var infos []mountutils.MountInfo
			for _, mp := range testCtx.mount.MountPoints {
				infos = append(infos, mountutils.MountInfo{MountPoint: mp.Path})
			}
			return infos, nil
		}, testK8sVersion, cluster.DefaultKubernetes)
	testCtx.installOwnershipRecorders()
	assert.NoError(testCtx.t, testCtx.dm.DiscoverMounter(testCtx.ctx))
}

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

// TestMount_DeadTargetLifecycle walks the "mounter pod restarted under a live workload" case as it
// happens in production: the restart kills the shared source and the pod's bind mount alike, the
// periodic cleanup tears the dead source down and drops the mount map entry, kubelet's republish of
// the dead target fails with Aborted naming the pod, and NodeUnpublishVolume then unmounts the dead
// target through the untracked best-effort path.
func TestMount_DeadTargetLifecycle(t *testing.T) {
	testCtx := setupDM(t)
	testCtx.useStrictUnmount()
	target := testCtx.targetPath(testCtx.podUID)
	source := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)

	// Absent target: a fresh mount goes all the way through.
	testCtx.mountVolume()
	uid := uint32(testCtx.ownerOf(filepath.Join(testCtx.commDir, testCtx.volumeID))[0])
	assert.Equals(t, true, testCtx.dm.UIDInUseForTesting(uid))

	provideCtx := credentialprovider.ProvideContext{WorkloadPodID: testCtx.podUID, VolumeID: testCtx.volumeID}
	volumeCtx := map[string]string{
		"csi.storage.k8s.io/pod.namespace": "team-a",
		"csi.storage.k8s.io/pod.name":      "trainer-0",
		"csi.storage.k8s.io/pod.uid":       testCtx.podUID,
	}

	// Healthy target: a republish (requiresRepublish) refreshes credentials and returns nil.
	err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, provideCtx, volumeCtx, mountpoint.ParseArgs(nil), "", nil)
	assert.NoError(t, err)

	// The mounter pod restarts: every Mountpoint on the node exits, so the source and the target both
	// report ENOTCONN while staying in the mount table.
	testCtx.mount.MountCheckErrors = map[string]error{
		source: healthPathErr(syscall.ENOTCONN),
		target: healthPathErr(syscall.ENOTCONN),
	}

	// The periodic cleanup finds the dead source, unmounts it and forgets the mount.
	testCtx.dm.CleanupOrphans()
	assert.Equals(t, false, testCtx.isMounted(source))
	assert.Equals(t, false, testCtx.dm.UIDInUseForTesting(uid))
	assert.Equals(t, true, testCtx.isMounted(target)) // cleanup never touches workload targets

	// kubelet's republish of the dead target fails with Aborted naming the pod.
	err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, provideCtx, volumeCtx, mountpoint.ParseArgs(nil), "", nil)
	assert.Equals(t, codes.Aborted, status.Code(err))
	assertDeadTargetMessage(t, status.Convert(err).Message(), testCtx.volumeID, "team-a/trainer-0 ("+testCtx.podUID+")")
	// The failed republish touched nothing: no new source, no UID taken.
	assert.Equals(t, false, testCtx.isMounted(source))
	assert.Equals(t, false, testCtx.dm.UIDInUseForTesting(uid))

	// NodeUnpublishVolume on pod deletion: no map entry is left, so the best-effort path unmounts the
	// dead target. With strict umount semantics this only succeeds if the target really was mounted.
	cleanupCtx := credentialprovider.CleanupContext{PodID: testCtx.podUID, VolumeID: testCtx.volumeID}
	assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, target, cleanupCtx))
	assert.Equals(t, false, testCtx.isMounted(target))

	// A kubelet retry of NodeUnpublishVolume after success is still a no-op success.
	assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, target, cleanupCtx))
}

// assertDeadTargetMessage checks the dead-target message: printable ASCII, the action first (so a
// truncated Event keeps it), then the volume and pod identity.
func assertDeadTargetMessage(t *testing.T, msg, pvName, pod string) {
	t.Helper()
	const action = "delete and recreate this pod to remount the volume"
	for _, want := range []string{action, "Volume " + pvName, "pod " + pod} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
	}
	if a, v := strings.Index(msg, action), strings.Index(msg, pvName); a < 0 || v < 0 || a > v {
		t.Errorf("error message %q does not put the action before the identifiers", msg)
	}
	if strings.Contains(msg, "s3-csi-daemonset-mounter") {
		t.Errorf("error message %q names the V3 mounter pod, which is wrong for legacy V1/V2 targets", msg)
	}
	for _, r := range msg {
		if r > 0x7e || (r < 0x20 && r != '\t') {
			t.Errorf("error message %q is not printable ASCII (rune %q)", msg, r)
			break
		}
	}
}

// TestMount_DeadTargetPodIdentity checks the pod identity in the message for every combination of
// podInfoOnMount keys kubelet may send (podInfoOnMount off, or an older CSIDriver object, sends none).
func TestMount_DeadTargetPodIdentity(t *testing.T) {
	const volumeID = "pv-pod-info"
	tests := []struct {
		name      string
		volumeCtx map[string]string
		wantPod   string
	}{
		{"all pod info", map[string]string{
			"csi.storage.k8s.io/pod.namespace": "team-a", "csi.storage.k8s.io/pod.name": "trainer-0", "csi.storage.k8s.io/pod.uid": "pod-uid-789",
		}, "team-a/trainer-0 (pod-uid-789)"},
		{"namespace without name keeps the namespace", map[string]string{
			"csi.storage.k8s.io/pod.namespace": "team-a", "csi.storage.k8s.io/pod.uid": "pod-uid-789",
		}, "with UID pod-uid-789 in namespace team-a"},
		{"no pod info falls back to the target path UID", nil, "with UID pod-uid-789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetDir := filepath.Join(t.TempDir(), "pods", "pod-uid-789", "volumes", "kubernetes.io~csi", volumeID, "mount")
			assert.NoError(t, os.MkdirAll(targetDir, 0750))

			dm := newHealthDM(&fakeHealthMount{isMountPointErr: healthPathErr(syscall.ENOTCONN)})
			err := dm.Mount(context.Background(), "test-bucket", targetDir,
				credentialprovider.ProvideContext{VolumeID: "handle-1", WorkloadPodID: "pod-uid-789"},
				tt.volumeCtx, mountpoint.Args{}, "", nil)

			assert.Equals(t, codes.Aborted, status.Code(err))
			assertDeadTargetMessage(t, status.Convert(err).Message(), volumeID, tt.wantPod)
		})
	}
}

// TestMount_DeadTargetLogsNoError checks the dead-target path adds no Error line of its own: the gRPC
// interceptor (driver.go) already logs every returned error, and kubelet retries for as long as the pod lives.
func TestMount_DeadTargetLogsNoError(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "pods", "pod-uid-555", "volumes", "kubernetes.io~csi", "pv-log", "mount")
	assert.NoError(t, os.MkdirAll(targetDir, 0750))

	dm := newHealthDM(&fakeHealthMount{isMountPointErr: healthPathErr(syscall.ENOTCONN)})
	logs := testutil.CaptureKlog(t) // after construction, which warns about CSI_NODE_IMAGE
	err := dm.Mount(context.Background(), "test-bucket", targetDir,
		credentialprovider.ProvideContext{VolumeID: "pv-log", WorkloadPodID: "pod-uid-555"}, nil, mountpoint.Args{}, "", nil)

	assert.Equals(t, codes.Aborted, status.Code(err))
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.HasPrefix(line, "E") || strings.HasPrefix(line, "W") {
			t.Errorf("dead-target Mount logged at Warning/Error level: %q", line)
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
