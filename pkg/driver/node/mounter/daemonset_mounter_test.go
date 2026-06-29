package mounter_test

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"

	"github.com/golang/mock/gomock"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

type dmTestCtx struct {
	t   *testing.T
	ctx context.Context

	dm           *mounter.DaemonsetMounter
	client       *fake.Clientset
	mount        *mount.FakeMounter
	mountSyscall func(target string, opts mpmounter.MountOptions) (int, error)

	nodeName      string
	bucketName    string
	volumeID      string
	podUID        string
	mounterPodUID string
	kubeletPath   string
	commDir       string
	targetPath    string
}

func setupDM(t *testing.T) *dmTestCtx {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	t.Setenv("MOUNTER_NAMESPACE", "kube-system")

	kubeletPath := t.TempDir()
	// Eval symlinks on `kubeletPath` as `mount.NewFakeMounter` also does that and we rely on
	// `mount.List()` to compare mount points and they need to be the same.
	parentDir, err := filepath.EvalSymlinks(filepath.Dir(kubeletPath))
	assert.NoError(t, err)
	kubeletPath = filepath.Join(parentDir, filepath.Base(kubeletPath))

	// Chdir to `kubeletPath` so `mountoptions.{Recv, Send}` can use relative paths to Unix sockets
	// to overcome `bind: invalid argument` (108 character limit, https://github.com/golang/go/issues/6895).
	t.Chdir(kubeletPath)

	bucketName := "test-bucket"
	podUID := uuid.New().String()
	volumeID := "s3-csi-driver-volume"
	nodeName := "test-node"
	mounterPodUID := uuid.New().String()

	commDir := filepath.Join(kubeletPath, "pods", mounterPodUID, "volumes", "kubernetes.io~empty-dir", mounter.CommVolumeName)
	err = os.MkdirAll(commDir, 0750)
	assert.NoError(t, err)

	// Add s3-csi-daemonset-mounter label for commDir tests
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3-csi-daemonset-mounter-abcde",
			Namespace: "kube-system",
			UID:       types.UID(mounterPodUID),
			Labels:    map[string]string{"app": "s3-csi-daemonset-mounter"},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	client := fake.NewSimpleClientset(pod)
	fakeMounter := mount.NewFakeMounter(nil)

	targetPath := filepath.Join(kubeletPath, "pods", podUID, "volumes", "kubernetes.io~csi", volumeID, "mount")

	testCtx := &dmTestCtx{
		t:             t,
		ctx:           ctx,
		client:        client,
		mount:         fakeMounter,
		nodeName:      nodeName,
		bucketName:    bucketName,
		volumeID:      volumeID,
		podUID:        podUID,
		mounterPodUID: mounterPodUID,
		kubeletPath:   kubeletPath,
		commDir:       commDir,
		targetPath:    targetPath,
	}

	mountSyscall := func(target string, opts mpmounter.MountOptions) (int, error) {
		if testCtx.mountSyscall != nil {
			return testCtx.mountSyscall(target, opts)
		}
		fakeMounter.Mount("mountpoint-s3", target, "fuse", nil)
		fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
		assert.NoError(t, err)
		return fd, nil
	}

	t.Setenv("CONTAINER_KUBELET_PATH", kubeletPath)
	mockCtl := gomock.NewController(t)
	mockCredProvider := mock_credentialprovider.NewMockProviderInterface(mockCtl)
	mockCredProvider.EXPECT().Provide(gomock.Any(), gomock.Any()).
		Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil).
		AnyTimes()
	mockCredProvider.EXPECT().Cleanup(gomock.Any()).Return(nil).AnyTimes()

	dm := mounter.NewDaemonsetMounter(client, nodeName, mpmounter.NewWithMount(fakeMounter), mockCredProvider, mountSyscall)
	err = dm.DiscoverCommDir(ctx)
	assert.NoError(t, err)

	testCtx.dm = dm
	return testCtx
}

func TestDaemonsetMounter(t *testing.T) {
	t.Run("Mounting", func(t *testing.T) {
		t.Run("Correctly passes mount options", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath

			devNull := mountertest.OpenDevNull(t)
			testCtx.mountSyscall = func(target string, opts mpmounter.MountOptions) (int, error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err := syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
				assert.Equals(t, true, opts.ReadOnly)
				return fd, nil
			}

			args := mountpoint.ParseArgs([]string{"--read-only", "--prefix=data/"})

			mountRes := make(chan error)
			go func() {
				err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
					WorkloadPodID: testCtx.podUID,
					VolumeID:      testCtx.volumeID,
				}, args, "", nil)
				if err != nil {
					log.Println("Mount failed", err)
				}
				mountRes <- err
			}()

			got := testCtx.receiveMountOptions()
			testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)

			err := <-mountRes
			assert.NoError(t, err)

			gotFile := os.NewFile(uintptr(got.Fd), "fd")
			t.Cleanup(func() { gotFile.Close() })
			mountertest.AssertSameFile(t, devNull, gotFile)

			// Reset fd as they might be different in different ends.
			got.Fd = 0

			env := envprovider.Default()
			assert.Equals(t, mountoptions.Options{
				BucketName: testCtx.bucketName,
				Args:       []string{"--prefix=data/"},
				Env:        env.List(),
				VolumeId:   mustGetMountId(t, testCtx.targetPath),
			}, got)
		})

		t.Run("Does not duplicate mounts if target is already mounted", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath

			err := os.MkdirAll(target, 0755)
			assert.NoError(t, err)
			testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)

			mountSyscallCalled := false
			testCtx.mountSyscall = func(target string, opts mpmounter.MountOptions) (int, error) {
				mountSyscallCalled = true
				return 0, nil
			}

			err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, mountpoint.ParseArgs(nil), "", nil)
			assert.NoError(t, err)

			if mountSyscallCalled {
				t.Error("mountSyscall should not be called for already-mounted target")
			}
		})

		t.Run("Unmounts source if mounter does not receive mount options", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath

			// Create socket but don't listen so no one receives mount options.
			// mount_options.go Send -> dialWithRetry will retry until context deadline.
			sockPath := filepath.Join(testCtx.commDir, mounter.MountSockName)
			_, err := os.Create(sockPath)
			assert.NoError(t, err)

			shortCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()

			err = testCtx.dm.Mount(shortCtx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("mount should fail if mounter does not receive the mount options")
			}
			assert.Contains(t, err.Error(), "failed to send mount options")

			// Expect false, nil output from mount.go CheckMountpoint
			mounted, err := testCtx.dm.IsMountPoint(target)
			assert.NoError(t, err)
			if mounted {
				t.Error("it should unmount target if mounter does not receive the mount options")
			}
			testCtx.assertUnmounted(target)
		})

		t.Run("Unmounts source if Mountpoint fails to start with error file", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath

			// Skip fakeMounter - it caused waitForMount's CheckMountpoint poll to win the race over .error file poll
			testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
				fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			// Construct error file path
			mountId := mustGetMountId(t, testCtx.targetPath)
			errFilePath := filepath.Join(testCtx.commDir, mounter.GetErrorFileName(mountId))

			mountRes := make(chan error)
			go func() {
				mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
					WorkloadPodID: testCtx.podUID,
					VolumeID:      testCtx.volumeID,
				}, mountpoint.ParseArgs(nil), "", nil)
			}()

			testCtx.receiveMountOptions()

			// Do not register mount - simulates Mountpoint receiving fd but fails to start serving.

			// Write error file to simulate Mountpoint crash
			mountError := "mount-s3 exited with code 1"
			err := os.WriteFile(errFilePath, []byte(mountError), 0644)
			assert.NoError(t, err)

			err = <-mountRes
			if err == nil {
				t.Fatal("mount should fail if Mountpoint fails to start")
			}
			assert.Contains(t, err.Error(), mountError)

			// Can't use IsMountpoint/CheckMountpoint (didn't register mount), so we
			// verify Unmount was called via FakeMounter log.
			testCtx.assertUnmounted(target)
		})
	})

	t.Run("Unmounting", func(t *testing.T) {
		t.Run("Removes mount from target", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath

			mountRes := make(chan error)
			go func() {
				err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
					WorkloadPodID: testCtx.podUID,
					VolumeID:      testCtx.volumeID,
				}, mountpoint.ParseArgs(nil), "", nil)
				if err != nil {
					log.Println("Mount failed", err)
				}
				mountRes <- err
			}()

			testCtx.receiveMountOptions()
			testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
			err := <-mountRes
			assert.NoError(t, err)

			mounted, err := testCtx.dm.IsMountPoint(target)
			assert.NoError(t, err)
			if !mounted {
				t.Fatal("target should be mounted after Mount")
			}

			err = testCtx.dm.Unmount(testCtx.ctx, target, credentialprovider.CleanupContext{
				PodID:    testCtx.podUID,
				VolumeID: testCtx.volumeID,
			})
			assert.NoError(t, err)

			mounted, err = testCtx.dm.IsMountPoint(target)
			assert.NoError(t, err)
			if mounted {
				t.Error("target should not be mounted after Unmount")
			}
		})
	})

	t.Run("Comm dir lifecycle", func(t *testing.T) {
		t.Run("DiscoverCommDir rejects invalid pod states", func(t *testing.T) {
			// DiscoverCommDir -> tryDiscoverCommDir should reject invalid pod states
			tests := []struct {
				name    string
				pods    []runtime.Object
				wantErr error
			}{
				{"no pods", nil, mounter.ErrNoRunningMounterPod},
				{"multiple running pods", []runtime.Object{
					mounterPod("mounter-aaa", corev1.PodRunning),
					mounterPod("mounter-bbb", corev1.PodRunning),
				}, mounter.ErrMultipleMounterPods},
				{"non-running pod", []runtime.Object{
					mounterPod("mounter-pending", corev1.PodPending),
				}, mounter.ErrNoRunningMounterPod},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Setenv("CONTAINER_KUBELET_PATH", t.TempDir())

					client := fake.NewSimpleClientset(tt.pods...)
					dm := mounter.NewDaemonsetMounter(client, "test-node", mpmounter.NewWithMount(mount.NewFakeMounter(nil)), nil, nil)

					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
					defer cancel()

					err := dm.DiscoverCommDir(ctx)
					t.Logf("%v", err)
					if err == nil {
						t.Fatal("expected error from DiscoverCommDir")
					}
					assert.ErrorIs(t, err, tt.wantErr)
					assert.ErrorIs(t, err, mounter.ErrCommDirDiscoveryFailed)
				})
			}
		})

		t.Run("StartCommDirWatch stops on channel close", func(t *testing.T) {
			testCtx := setupDM(t)

			stopCh := make(chan struct{})
			done := make(chan struct{})
			go func() {
				testCtx.dm.StartCommDirWatch(stopCh)
				close(done)
			}()

			close(stopCh)

			select {
			case <-done:
			case <-time.After(1 * time.Second):
				t.Fatal("StartCommDirWatch did not stop after stopCh was closed")
			}
		})

		t.Run("Mount fails fast when commDir not discovered", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath

			// Create a fresh DM which has not discovered commDir (setupDM called dm.DiscoverCommDir(ctx))
			// and has no StartCommDirWatch process to populate it.
			mountSyscallCalled := false
			testCtx.dm = mounter.NewDaemonsetMounter(
				testCtx.client, testCtx.nodeName,
				mpmounter.NewWithMount(testCtx.mount),
				nil,
				func(target string, opts mpmounter.MountOptions) (int, error) {
					mountSyscallCalled = true
					return 0, nil
				},
			)

			err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("expected error when commDir is not discovered")
			}
			assert.ErrorIs(t, err, mounter.ErrCommDirNotReady)
			if mountSyscallCalled {
				t.Error("mountSyscall should not be called when commDir is not available")
			}
		})

		t.Run("Mount nils commDir on staleness (socket not found)", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath

			testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
				testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
				fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()

			// No receiveMountOptions (socket does not exist). Send -> dialWithRetry will retry
			// until context timeout (DeadlineExceeded) which should nil commDir on staleness
			err := testCtx.dm.Mount(ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("expected error on send timeout")
			}
			assert.Contains(t, err.Error(), "failed to send mount options")

			// Verify commDir was nilled by the staleness detection
			_, err = testCtx.dm.GetCommDir()
			assert.ErrorIs(t, err, mounter.ErrCommDirNotReady)
		})

		t.Run("Cancelled context does not cause stale commDir", func(t *testing.T) {
			// Kubelet cancels NodePublishVolume when workload pod deleted mid-mount. If
			// it incorrectly nils commDir, all subsequent mounts fail with "mounter pod
			// not available" until the watcher re-discovers.
			testCtx := setupDM(t)
			target := testCtx.targetPath

			testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
				testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
				fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := testCtx.dm.Mount(ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("expected error on cancelled context")
			}
			assert.Contains(t, err.Error(), "failed to send mount options")

			// Verify commDir was NOT nilled by the cancelled context
			_, err = testCtx.dm.GetCommDir()
			assert.NoError(t, err)
		})
	})
}

func (testCtx *dmTestCtx) receiveMountOptions() mountoptions.Options {
	testCtx.t.Helper()
	sockPath := filepath.Join(testCtx.commDir, mounter.MountSockName)
	options, err := mountoptions.Recv(testCtx.ctx, sockPath)
	assert.NoError(testCtx.t, err)
	return options
}

func (testCtx *dmTestCtx) assertUnmounted(target string) {
	testCtx.t.Helper()
	for _, action := range testCtx.mount.GetLog() {
		if action.Action == mount.FakeActionUnmount && action.Target == target {
			return
		}
	}
	testCtx.t.Errorf("expected Unmount to be called on %s, FakeMounter log: %v", target, testCtx.mount.GetLog())
}

func mustGetMountId(t *testing.T, target string) string {
	t.Helper()
	id, err := mounter.GetMountId(target)
	assert.NoError(t, err)
	return id
}

func mounterPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kube-system",
			UID:    types.UID(uuid.New().String()),
			Labels: map[string]string{"app": "s3-csi-daemonset-mounter"},
		},
		Spec:   corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{Phase: phase},
	}
}
