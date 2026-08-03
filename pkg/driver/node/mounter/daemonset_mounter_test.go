package mounter_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	mountutils "k8s.io/mount-utils"

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
	mount        *mountutils.FakeMounter
	mountSyscall func(target string, opts mpmounter.MountOptions) (int, error)

	nodeName      string
	bucketName    string
	volumeID      string
	podUID        string
	mounterPodUID string
	kubeletPath   string
	commDir       string
}

// targetPath returns a valid kubelet-style target path that targetpath.Parse can parse.
// Format: <kubeletPath>/pods/<podUID>/volumes/kubernetes.io~csi/<volumeID>/mount
func (testCtx *dmTestCtx) targetPath(podUID string) string {
	return filepath.Join(testCtx.kubeletPath, "pods", podUID, "volumes", "kubernetes.io~csi", testCtx.volumeID, "mount")
}

func setupDM(t *testing.T) *dmTestCtx {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	t.Setenv("MOUNTER_NAMESPACE", "kube-system")

	kubeletPath := t.TempDir()
	// Eval symlinks on `kubeletPath` as `mountutils.NewFakeMounter` also does that and we rely on
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
	fakeMounter := mountutils.NewFakeMounter(nil)

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

	dm := mounter.NewDaemonsetMounter(client, nodeName, mpmounter.NewWithMount(fakeMounter), mockCredProvider, mountSyscall, func(source, target string) error {
		return fakeMounter.Mount(source, target, "bind", []string{"bind"})
	}, func() ([]mountutils.MountInfo, error) {
		// Return mount info entries matching what FakeMounter has registered.
		var infos []mountutils.MountInfo
		for _, mp := range fakeMounter.MountPoints {
			infos = append(infos, mountutils.MountInfo{MountPoint: mp.Path})
		}
		return infos, nil
	})
	err = dm.DiscoverCommDir(ctx)
	assert.NoError(t, err)

	testCtx.dm = dm
	return testCtx
}

func TestDaemonsetMounter(t *testing.T) {
	t.Run("Mounting", func(t *testing.T) {
		t.Run("Correctly passes mount options", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath(testCtx.podUID)

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
			sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
			testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

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
				VolumeId:   testCtx.volumeID,
			}, got)
		})

		t.Run("Does not duplicate mounts if target is already mounted and refreshes credentials", func(t *testing.T) {
			mockCtl := gomock.NewController(t)
			mockCredProvider := mock_credentialprovider.NewMockProviderInterface(mockCtl)

			testCtx := setupDM(t)
			target := testCtx.targetPath(testCtx.podUID)

			// volumeID is the PV name extracted from target path — used as credential dir name
			expectedWritePath := filepath.Join(testCtx.commDir, testCtx.volumeID)
			expectedEnvPath := filepath.Join("/comm", testCtx.volumeID)

			mockCredProvider.EXPECT().Provide(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, provideCtx credentialprovider.ProvideContext) (envprovider.Environment, credentialprovider.AuthenticationSource, error) {
					assert.Equals(t, expectedWritePath, provideCtx.WritePath)
					assert.Equals(t, expectedEnvPath, provideCtx.EnvPath)
					assert.Equals(t, credentialprovider.MountKindDaemonset, provideCtx.MountKind)
					return envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil
				})

			// Replace DM with one using the custom mock credential provider
			testCtx.dm = mounter.NewDaemonsetMounter(testCtx.client, testCtx.nodeName,
				mpmounter.NewWithMount(testCtx.mount), mockCredProvider,
				func(tgt string, opts mpmounter.MountOptions) (int, error) {
					return 0, nil
				}, func(source, target string) error {
					return testCtx.mount.Mount(source, target, "bind", []string{"bind"})
				}, nil)
			err := testCtx.dm.DiscoverCommDir(testCtx.ctx)
			assert.NoError(t, err)

			err = os.MkdirAll(target, 0755)
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

		t.Run("Credentials cleaned up on mount failure", func(t *testing.T) {
			mockCtl := gomock.NewController(t)
			mockCredProvider := mock_credentialprovider.NewMockProviderInterface(mockCtl)

			testCtx := setupDM(t)
			target := testCtx.targetPath(testCtx.podUID)

			expectedWritePath := filepath.Join(testCtx.commDir, testCtx.volumeID)
			expectedEnvPath := filepath.Join("/comm", testCtx.volumeID)

			provideCall := mockCredProvider.EXPECT().Provide(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, provideCtx credentialprovider.ProvideContext) (envprovider.Environment, credentialprovider.AuthenticationSource, error) {
					assert.Equals(t, expectedWritePath, provideCtx.WritePath)
					assert.Equals(t, expectedEnvPath, provideCtx.EnvPath)
					assert.Equals(t, credentialprovider.MountKindDaemonset, provideCtx.MountKind)
					return envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil
				})

			mockCredProvider.EXPECT().Cleanup(gomock.Any()).After(provideCall).
				DoAndReturn(func(cleanupCtx credentialprovider.CleanupContext) error {
					assert.Equals(t, expectedWritePath, cleanupCtx.WritePath)
					assert.Equals(t, credentialprovider.MountKindDaemonset, cleanupCtx.MountKind)
					return nil
				})

			// Replace DM with one using the custom mock credential provider
			mountErr := fmt.Errorf("simulated mount failure")
			testCtx.dm = mounter.NewDaemonsetMounter(testCtx.client, testCtx.nodeName,
				mpmounter.NewWithMount(testCtx.mount), mockCredProvider,
				func(tgt string, opts mpmounter.MountOptions) (int, error) {
					return 0, mountErr
				}, func(source, target string) error {
					return testCtx.mount.Mount(source, target, "bind", []string{"bind"})
				}, nil)
			err := testCtx.dm.DiscoverCommDir(testCtx.ctx)
			assert.NoError(t, err)

			err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("mount should fail")
			}
			assert.Contains(t, err.Error(), "simulated mount failure")
		})

		t.Run("Unmounts source if mounter does not receive mount options", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath(testCtx.podUID)

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

			// In sharing mode, FUSE mount goes to source path, not target.
			// After failure, source should be unmounted and cleaned up (directory removed).
			sourcePath := filepath.Join(testCtx.kubeletPath, "plugins", "s3.csi.aws.com", "mnt", testCtx.volumeID)
			mounted, err := testCtx.dm.IsMountPoint(sourcePath)
			// ErrNotExist is expected: cleanupMount removes the source directory after unmounting.
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("unexpected error checking mount point: %v", err)
			}
			if mounted {
				t.Error("it should unmount source if mounter does not receive the mount options")
			}
			testCtx.assertUnmounted(sourcePath)
		})

		t.Run("Unmounts source if Mountpoint fails to start with error file", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath(testCtx.podUID)

			// Skip fakeMounter's CheckMountpoint match (don't register as "mountpoint-s3")
			// but register the path in the mount table so unmountIfMounted can find it.
			testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
				testCtx.mount.Mount("fuse-pending", tgt, "fuse", nil)
				fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			// Construct error file path
			mountId := testCtx.volumeID
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
			// verify Unmount was called on source path via FakeMounter log.
			sourcePath := filepath.Join(testCtx.kubeletPath, "plugins", "s3.csi.aws.com", "mnt", testCtx.volumeID)
			testCtx.assertUnmounted(sourcePath)
		})
	})

	t.Run("Unmounting", func(t *testing.T) {
		t.Run("Removes mount from target", func(t *testing.T) {
			testCtx := setupDM(t)
			target := testCtx.targetPath(testCtx.podUID)

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
			sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
			testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)
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
					dm := mounter.NewDaemonsetMounter(client, "test-node", mpmounter.NewWithMount(mountutils.NewFakeMounter(nil)), nil, nil, nil, nil)

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
			target := testCtx.targetPath(testCtx.podUID)

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
				nil,
				nil,
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
			target := testCtx.targetPath("pod-timeout")

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
				WorkloadPodID: "pod-timeout",
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
			target := testCtx.targetPath("pod-cancel")

			testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
				testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
				fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := testCtx.dm.Mount(ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: "pod-cancel",
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

func TestDaemonsetMounter_PodSharing(t *testing.T) {
	t.Run("Second pod shares existing mount without new FUSE mount", func(t *testing.T) {
		testCtx := setupDM(t)
		target1 := testCtx.targetPath("pod-a-uid")
		target2 := testCtx.targetPath("pod-b-uid")

		fuseMountCount := 0
		testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
			fuseMountCount++
			testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
			fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
			assert.NoError(t, err)
			return fd, nil
		}

		// Mount first pod
		mountRes := make(chan error)
		go func() {
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target1, credentialprovider.ProvideContext{
				WorkloadPodID:        "pod-a-uid",
				VolumeID:             testCtx.volumeID,
				AuthenticationSource: "driver",
				ServiceAccountName:   "default",
				PodNamespace:         "default",
			}, mountpoint.ParseArgs(nil), "", nil)
		}()

		// Receive and complete the first mount
		testCtx.receiveMountOptions()
		sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
		testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

		err := <-mountRes
		assert.NoError(t, err)
		assert.Equals(t, 1, fuseMountCount)

		// Mount second pod — same volume, same params → should share via bind mount
		err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target2, credentialprovider.ProvideContext{
			WorkloadPodID:        "pod-b-uid",
			VolumeID:             testCtx.volumeID,
			AuthenticationSource: "driver",
			ServiceAccountName:   "default",
			PodNamespace:         "default",
		}, mountpoint.ParseArgs(nil), "", nil)
		assert.NoError(t, err)

		// FUSE mount should NOT have been called again — only bind mount
		assert.Equals(t, 1, fuseMountCount)
	})

	t.Run("Second pod rejected with different service account (pod auth) without overwriting credentials", func(t *testing.T) {
		mockCtl := gomock.NewController(t)
		mockCredProvider := mock_credentialprovider.NewMockProviderInterface(mockCtl)

		testCtx := setupDM(t)
		target1 := testCtx.targetPath("pod-a-uid")
		target2 := testCtx.targetPath("pod-b-uid")

		// Expect Provide to be called exactly once (for the first pod only).
		// The second pod should be rejected BEFORE provideCredentials is reached.
		mockCredProvider.EXPECT().Provide(gomock.Any(), gomock.Any()).
			Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil).
			Times(1)
		mockCredProvider.EXPECT().Cleanup(gomock.Any()).Return(nil).AnyTimes()

		testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
			testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
			fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
			assert.NoError(t, err)
			return fd, nil
		}

		// Replace DM with one using the strict mock
		testCtx.dm = mounter.NewDaemonsetMounter(testCtx.client, testCtx.nodeName,
			mpmounter.NewWithMount(testCtx.mount), mockCredProvider,
			testCtx.mountSyscall, func(source, target string) error {
				return testCtx.mount.Mount(source, target, "bind", []string{"bind"})
			}, nil)
		err := testCtx.dm.DiscoverCommDir(testCtx.ctx)
		assert.NoError(t, err)

		// Mount first pod with sa-a using pod auth
		mountRes := make(chan error)
		go func() {
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target1, credentialprovider.ProvideContext{
				WorkloadPodID:        "pod-a-uid",
				VolumeID:             testCtx.volumeID,
				AuthenticationSource: "pod",
				ServiceAccountName:   "sa-a",
				PodNamespace:         "default",
			}, mountpoint.ParseArgs(nil), "", nil)
		}()

		testCtx.receiveMountOptions()
		sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
		testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

		err = <-mountRes
		assert.NoError(t, err)

		// Second pod with sa-b — should be rejected because pod auth enforces SA match.
		// Provide must NOT be called again (gomock Times(1) will fail if it is).
		err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target2, credentialprovider.ProvideContext{
			WorkloadPodID:        "pod-b-uid",
			VolumeID:             testCtx.volumeID,
			AuthenticationSource: "pod",
			ServiceAccountName:   "sa-b",
			PodNamespace:         "default",
		}, mountpoint.ParseArgs(nil), "", nil)
		if err == nil {
			t.Fatal("expected error for mismatched service account with pod auth")
		}
		assert.Contains(t, err.Error(), "serviceAccountName mismatch")
	})

	t.Run("Second pod with different service account allowed with driver auth", func(t *testing.T) {
		testCtx := setupDM(t)
		target1 := testCtx.targetPath("pod-a-uid")
		target2 := testCtx.targetPath("pod-b-uid")

		fuseMountCount := 0
		testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
			fuseMountCount++
			testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
			fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
			assert.NoError(t, err)
			return fd, nil
		}

		// Mount first pod with sa-a using driver auth
		mountRes := make(chan error)
		go func() {
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target1, credentialprovider.ProvideContext{
				WorkloadPodID:        "pod-a-uid",
				VolumeID:             testCtx.volumeID,
				AuthenticationSource: "driver",
				ServiceAccountName:   "sa-a",
				PodNamespace:         "default",
			}, mountpoint.ParseArgs(nil), "", nil)
		}()

		testCtx.receiveMountOptions()
		sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
		testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

		err := <-mountRes
		assert.NoError(t, err)

		// Second pod with sa-b — should SUCCEED because driver auth doesn't enforce SA match
		err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target2, credentialprovider.ProvideContext{
			WorkloadPodID:        "pod-b-uid",
			VolumeID:             testCtx.volumeID,
			AuthenticationSource: "driver",
			ServiceAccountName:   "sa-b",
			PodNamespace:         "default",
		}, mountpoint.ParseArgs(nil), "", nil)
		assert.NoError(t, err)

		// Only 1 FUSE mount — second pod shared via bind mount
		assert.Equals(t, 1, fuseMountCount)
	})

	t.Run("Unmount last consumer resets entry, next mount with diff params succeeds", func(t *testing.T) {
		testCtx := setupDM(t)
		target1 := testCtx.targetPath("pod-a-uid")
		target2 := testCtx.targetPath("pod-b-uid")

		testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
			testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
			fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
			assert.NoError(t, err)
			return fd, nil
		}

		// Mount with sa-a
		mountRes := make(chan error)
		go func() {
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target1, credentialprovider.ProvideContext{
				WorkloadPodID:        "pod-a-uid",
				VolumeID:             testCtx.volumeID,
				AuthenticationSource: "driver",
				ServiceAccountName:   "sa-a",
				PodNamespace:         "default",
			}, mountpoint.ParseArgs(nil), "", nil)
		}()

		testCtx.receiveMountOptions()
		sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
		testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

		err := <-mountRes
		assert.NoError(t, err)

		// Unmount the only consumer
		err = testCtx.dm.Unmount(testCtx.ctx, target1, credentialprovider.CleanupContext{
			VolumeID: testCtx.volumeID,
			PodID:    "pod-a-uid",
		})
		assert.NoError(t, err)

		// Now mount with sa-b — should succeed because entry was reset
		go func() {
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target2, credentialprovider.ProvideContext{
				WorkloadPodID:        "pod-b-uid",
				VolumeID:             testCtx.volumeID,
				AuthenticationSource: "driver",
				ServiceAccountName:   "sa-b",
				PodNamespace:         "default",
			}, mountpoint.ParseArgs(nil), "", nil)
		}()

		testCtx.receiveMountOptions()
		testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

		err = <-mountRes
		assert.NoError(t, err)
	})

	t.Run("Concurrent mounts same volume same params no race", func(t *testing.T) {
		testCtx := setupDM(t)

		fuseMountCount := 0
		testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
			fuseMountCount++
			testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
			fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
			assert.NoError(t, err)
			return fd, nil
		}

		// First mount to establish the source
		target1 := testCtx.targetPath("pod-first")
		mountRes := make(chan error)
		go func() {
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target1, credentialprovider.ProvideContext{
				WorkloadPodID:        "pod-first",
				VolumeID:             testCtx.volumeID,
				AuthenticationSource: "driver",
				ServiceAccountName:   "default",
				PodNamespace:         "default",
			}, mountpoint.ParseArgs(nil), "", nil)
		}()

		testCtx.receiveMountOptions()
		sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
		testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

		err := <-mountRes
		assert.NoError(t, err)

		// Now fire 5 concurrent mounts — all should share, no new FUSE mount
		var results [5]error
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				podUID := "pod-" + string(rune('a'+idx))
				target := testCtx.targetPath(podUID)
				results[idx] = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
					WorkloadPodID:        podUID,
					VolumeID:             testCtx.volumeID,
					AuthenticationSource: "driver",
					ServiceAccountName:   "default",
					PodNamespace:         "default",
				}, mountpoint.ParseArgs(nil), "", nil)
			}(i)
		}
		wg.Wait()

		for i, err := range results {
			if err != nil {
				t.Errorf("concurrent mount %d failed: %v", i, err)
			}
		}

		// Only 1 FUSE mount should have happened (the initial one)
		assert.Equals(t, 1, fuseMountCount)
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
		if action.Action == mountutils.FakeActionUnmount && action.Target == target {
			return
		}
	}
	testCtx.t.Errorf("expected Unmount to be called on %s, FakeMounter log: %v", target, testCtx.mount.GetLog())
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
