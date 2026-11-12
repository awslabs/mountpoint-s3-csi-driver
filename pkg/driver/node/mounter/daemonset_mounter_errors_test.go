package mounter_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// failMountWithErrorFile runs Mount, plays a mounter whose Mountpoint fails to start by writing
// `content` to the error file, and returns Mount's error.
func (testCtx *dmTestCtx) failMountWithErrorFile(content string) error {
	testCtx.t.Helper()
	// Register the FUSE mount under another device so waitForMount never sees Mountpoint serving.
	testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
		testCtx.mount.Mount("fuse-pending", tgt, "fuse", nil)
		fd, err := syscall.Dup(int(mountertest.OpenDevNull(testCtx.t).Fd()))
		assert.NoError(testCtx.t, err)
		return fd, nil
	}
	mountRes := make(chan error, 1)
	go func() {
		mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(testCtx.podUID), credentialprovider.ProvideContext{
			WorkloadPodID: testCtx.podUID,
			VolumeID:      testCtx.volumeID,
		}, nil, mountpoint.ParseArgs(nil), "", nil)
	}()
	testCtx.receiveMountOptions()
	errFile := filepath.Join(testCtx.commDir, mounter.GetErrorFileName(testCtx.volumeID))
	// Atomically, as the mounter writes it (a plain WriteFile could be polled while still empty).
	assert.NoError(testCtx.t, util.WriteFileOwned(errFile, []byte(content), 0600, 0))
	err := <-mountRes
	if err == nil {
		testCtx.t.Fatal("Mount should fail when Mountpoint fails to start")
	}
	testCtx.mountSyscall = nil
	return err
}

func TestMountError_NamesThisNodesMounterPodAndLogPrefix(t *testing.T) {
	testCtx := setupDM(t)
	err := testCtx.failMountWithErrorFile("Mountpoint exited with code 1: Error: Failed to create S3 client")

	msg := err.Error()
	assert.Contains(t, msg, "Error: Failed to create S3 client")
	// setupDM's mounter pod on this node, not a label selector spanning every node's mounter.
	assert.Contains(t, msg, "Logs: `kubectl logs")
	assert.Contains(t, msg, "s3-csi-daemonset-mounter-abcde -c mounter")
	assert.Contains(t, msg, "grep -F '["+testCtx.volumeID+"]'")
	assertNotContainsStr(t, msg, "-l app=s3-csi-daemonset-mounter`")
}

func TestMountError_OversizedErrorFileIsCapped(t *testing.T) {
	testCtx := setupDM(t)
	// An older mounter writes up to 1 MiB of raw stderr.
	err := testCtx.failMountWithErrorFile("Error: first\n" + strings.Repeat("noise \x1b[0m line\n", 64*1024))

	msg := err.Error()
	if len(msg) > 1024 {
		t.Fatalf("NodePublishVolume error is %d bytes, want at most 1024; it starts %.200q", len(msg), msg)
	}
	assert.Contains(t, msg, "Error: first")
	assertNotContainsStr(t, msg, "\x1b")
	assertNotContainsStr(t, msg, "\n")
}

func TestMountError_MounterNotDiscovered_HintIsNodeScoped(t *testing.T) {
	kubeletPath := t.TempDir()
	const volumeID = "pv-no-mounter"
	target := filepath.Join(kubeletPath, "pods", "pod-uid-1", "volumes", "kubernetes.io~csi", volumeID, "mount")
	dm := mounter.NewDaemonsetMounter(nil, "node-a", mpmounter.NewWithMount(&fakeHealthMount{}),
		nil, nil, nil, nil, "", cluster.DefaultKubernetes)

	err := dm.Mount(context.Background(), "bucket", target,
		credentialprovider.ProvideContext{VolumeID: volumeID, WorkloadPodID: "pod-uid-1"}, nil, mountpoint.Args{}, "", nil)
	assert.ErrorIs(t, err, mounter.ErrCommDirNotReady)
	assert.Contains(t, err.Error(), "--field-selector spec.nodeName=node-a")
}

func assertNotContainsStr(t *testing.T, got, unwanted string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Fatalf("%.300q contains %q", got, unwanted)
	}
}

// An error file whose content formats to nothing still fails the mount at once, with a readable message.
func TestMountError_EmptyErrorFileFailsFastWithAMessage(t *testing.T) {
	testCtx := setupDM(t)
	start := time.Now()
	err := testCtx.failMountWithErrorFile("\x1b[0m\n")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v; an empty error file must not wait for the mount timeout", time.Since(start))
	}
	assert.Contains(t, err.Error(), "the mounter reported no reason. Logs: ")
	assertNotContainsStr(t, err.Error(), "failed: .")
}

func TestMountError_CredentialFailure_PointsAtTheNodePluginLog(t *testing.T) {
	testCtx := setupDM(t)
	mockCtl := gomock.NewController(t)
	provider := mock_credentialprovider.NewMockProviderInterface(mockCtl)
	provider.EXPECT().Provide(gomock.Any(), gomock.Any()).Return(nil, credentialprovider.AuthenticationSourceDriver, errors.New("no token")).AnyTimes()
	provider.EXPECT().Cleanup(gomock.Any()).Return(nil).AnyTimes()
	testCtx.useCredProvider(provider)

	err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(testCtx.podUID), testCtx.driverProvideCtx(testCtx.podUID),
		nil, mountpoint.ParseArgs(nil), "", nil)
	assert.Contains(t, err.Error(), "-l app=s3-csi-node --field-selector spec.nodeName="+testCtx.nodeName)
	assert.Contains(t, err.Error(), "-c s3-plugin")
}

func TestMountError_NoDoublePeriodBeforeTheHint(t *testing.T) {
	testCtx := setupDM(t)
	err := testCtx.failMountWithErrorFile("Mountpoint exited with code 1: Error: x Caused by: 0: A nonrecoverable failure occurred.")
	assertNotContainsStr(t, err.Error(), "..")
	assert.Contains(t, err.Error(), "failure occurred. Logs: ")
}

// A Mountpoint that dies after its mount succeeded writes an error file nobody polls; the periodic
// cleanup logs its reason, with the log hint, before removing it.
func TestPeriodicCleanup_LogsTheReasonOfAMountpointThatDied(t *testing.T) {
	var buf bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&buf)
	t.Cleanup(func() { klog.SetOutput(os.Stderr); klog.LogToStderr(true) })

	testCtx := setupDM(t)
	testCtx.mountVolume()
	errFile := filepath.Join(testCtx.commDir, mounter.GetErrorFileName(testCtx.volumeID))
	assert.NoError(t, util.WriteFileOwned(errFile, []byte("Mountpoint was killed by signal SIGKILL"), 0600, 0))
	assert.NoError(t, testCtx.mount.Unmount(mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)))

	testCtx.dm.CleanupOrphans()
	klog.Flush()

	assert.FileNotExists(t, errFile)
	assert.Contains(t, buf.String(), "DaemonsetMounter: volume "+testCtx.volumeID+": Mountpoint was killed by signal SIGKILL. Logs: ")
}
