package mounter_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"

	crdv2beta "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2beta"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const mountpointPodNamespace = "mount-s3"
const dummyIMDSRegion = "us-west-2"
const testK8sVersion = "v1.28.0"

type testCtx struct {
	t   *testing.T
	ctx context.Context

	podMounter *mounter.PodMounter

	client           *fake.Clientset
	mount            *mount.FakeMounter
	mockCredProvider *mock_credentialprovider.MockProviderInterface
	s3paCache        *mounter.FakeCache
	mountSyscall     func(target string, args mountpoint.Args) (fd int, err error)
	mountBindSyscall func(source, target string) (err error)

	bucketName  string
	kubeletPath string
	sourcePath  string
	targetPath  string
	podUID      string
	volumeID    string
	pvName      string
	nodeName    string
	fsGroup     string
	mpPodName   string
	mpPodUID    string
}

func setup(t *testing.T) *testCtx {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	mockCtl := gomock.NewController(t)
	mockCredProvider := mock_credentialprovider.NewMockProviderInterface(mockCtl)

	kubeletPath := t.TempDir()
	// Eval symlinks on `kubeletPath` as `mount.NewFakeMounter` also does that and we rely on
	// `mount.List()` to compare mount points and they need to be the same.
	parentDir, err := filepath.EvalSymlinks(filepath.Dir(kubeletPath))
	assert.NoError(t, err)
	kubeletPath = filepath.Join(parentDir, filepath.Base(kubeletPath))
	t.Setenv("KUBELET_PATH", kubeletPath)

	// Chdir to `kubeletPath` so `mountoptions.{Recv, Send}` can use relative paths to Unix sockets
	// to overcome `bind: invalid argument`.
	t.Chdir(kubeletPath)

	bucketName := "test-bucket"
	podUID := uuid.New().String()
	mpPodName := "test-mppod"
	mpPodUID := uuid.New().String()
	volumeID := "s3-csi-driver-volume"
	pvName := "s3-csi-driver-pv"
	nodeName := "test-node"
	fsGroup := "1000"
	s3paCache := &mounter.FakeCache{}
	targetPath := filepath.Join(
		kubeletPath,
		fmt.Sprintf("pods/%s/volumes/kubernetes.io~csi/%s/mount", podUID, pvName),
	)
	sourceMountDir := mounter.SourceMountDir(kubeletPath)
	sourcePath := filepath.Join(sourceMountDir, mpPodName)

	// Same behaviour as Kubernetes, see https://github.com/kubernetes/kubernetes/blob/8f8c94a04d00e59d286fe4387197bc62c6a4f374/pkg/volume/csi/csi_mounter.go#L211-L215
	err = os.MkdirAll(filepath.Dir(targetPath), 0750)
	assert.NoError(t, err)

	client := fake.NewClientset()
	fakeMounter := mount.NewFakeMounter(nil)

	testCtx := &testCtx{
		t:                t,
		ctx:              ctx,
		client:           client,
		mount:            fakeMounter,
		mockCredProvider: mockCredProvider,
		bucketName:       bucketName,
		kubeletPath:      kubeletPath,
		targetPath:       targetPath,
		podUID:           podUID,
		volumeID:         volumeID,
		pvName:           pvName,
		nodeName:         nodeName,
		fsGroup:          fsGroup,
		s3paCache:        s3paCache,
		mpPodName:        mpPodName,
		mpPodUID:         mpPodUID,
		sourcePath:       sourcePath,
	}

	testCrd := crdv2beta.MountpointS3PodAttachment{
		Spec: crdv2beta.MountpointS3PodAttachmentSpec{
			NodeName:             testCtx.nodeName,
			PersistentVolumeName: testCtx.pvName,
			VolumeID:             testCtx.volumeID,
			WorkloadFSGroup:      testCtx.fsGroup,
			MountpointS3PodAttachments: map[string][]crdv2beta.WorkloadAttachment{
				testCtx.mpPodName: {{WorkloadPodUID: testCtx.podUID}},
			},
		},
	}
	testCtx.s3paCache.TestItems = []crdv2beta.MountpointS3PodAttachment{testCrd}

	mountSyscall := func(target string, args mountpoint.Args) (fd int, err error) {
		if testCtx.mountSyscall != nil {
			return testCtx.mountSyscall(target, args)
		}

		fakeMounter.Mount("mountpoint-s3", target, "fuse", nil)
		return int(mountertest.OpenDevNull(t).Fd()), nil
	}

	mountBindSyscall := func(source, target string) (err error) {
		if testCtx.mountBindSyscall != nil {
			return testCtx.mountBindSyscall(source, target)
		}

		fakeMounter.Mount(source, target, "fuse", []string{"bind"})
		return nil
	}

	podWatcher := watcher.New(client, mountpointPodNamespace, nodeName, 10*time.Second)
	stopCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
	})
	err = podWatcher.Start(stopCh)
	assert.NoError(t, err)

	podMounter, err := mounter.NewPodMounter(podWatcher, s3paCache, mockCredProvider, mpmounter.NewWithMount(fakeMounter), mountSyscall,
		mountBindSyscall, testK8sVersion, nodeName)
	assert.NoError(t, err)

	testCtx.podMounter = podMounter

	return testCtx
}

func TestPodMounter(t *testing.T) {
	t.Run("Mounting", func(t *testing.T) {
		t.Run("Correctly passes mount options", func(t *testing.T) {
			testCtx := setup(t)

			devNull := mountertest.OpenDevNull(t)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)

				// Since `PodMounter.Mount` closes the file descriptor once it passes it to Mountpoint,
				// we should duplicate our file descriptor to ensure underlying file description won't
				// closed once the file descriptor passed to `PodMounter.Mount` closed.
				fd, err = syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)

				return fd, nil
			}
			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

			args := mountpoint.ParseArgs([]string{mountpoint.ArgReadOnly})

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					WorkloadPodID:        testCtx.podUID,
				}, args, testCtx.fsGroup)
				if err != nil {
					log.Println("Mount failed", err)
				}
				mountRes <- err
			}()

			mpPod := createMountpointPod(testCtx)
			mpPod.run()

			got := mpPod.receiveMountOptions(testCtx.ctx)

			err := <-mountRes
			assert.NoError(t, err)

			gotFile := os.NewFile(uintptr(got.Fd), "fd")
			mountertest.AssertSameFile(t, devNull, gotFile)
			// Reset fd as they might be different in different ends.
			// To verify underlying objects are the same, we need to compare "dev" and "ino" from "fstat" syscall, which we do with `AssertSameFile`.
			got.Fd = 0

			assert.Equals(t, mountoptions.Options{
				BucketName: testCtx.bucketName,
				Args: []string{
					"--user-agent-prefix=" + mounter.UserAgent(credentialprovider.AuthenticationSourceDriver, testK8sVersion),
				},
				Env: envprovider.Default().List(),
			}, got)
		})

		t.Run("Waits for Mountpoint Pod", func(t *testing.T) {
			testCtx := setup(t)
			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

			go func() {
				// Add delays to each Mountpoint Pod step
				time.Sleep(100 * time.Millisecond)
				mpPod := createMountpointPod(testCtx)
				time.Sleep(100 * time.Millisecond)
				mpPod.run()
				time.Sleep(100 * time.Millisecond)
				mpPod.receiveMountOptions(testCtx.ctx)
			}()

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID:      testCtx.volumeID,
				WorkloadPodID: testCtx.podUID,
			}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
			assert.NoError(t, err)
		})

		t.Run("Creates credential directory with group access", func(t *testing.T) {
			testCtx := setup(t)
			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

			args := mountpoint.ParseArgs([]string{mountpoint.ArgReadOnly})
			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					WorkloadPodID:        testCtx.podUID,
				}, args, testCtx.fsGroup)
				if err != nil {
					log.Println("Mount failed", err)
				}
				mountRes <- err
			}()

			mpPod := createMountpointPod(testCtx)
			mpPod.run()
			mpPod.receiveMountOptions(testCtx.ctx)
			err := <-mountRes

			assert.NoError(t, err)
			credDirInfo, err := os.Stat(mppod.PathOnHost(mpPod.podPath, mppod.KnownPathCredentials))
			assert.NoError(t, err)
			assert.Equals(t, true, credDirInfo.IsDir())
			assert.Equals(t, credentialprovider.CredentialDirPerm, credDirInfo.Mode().Perm())
		})
		t.Run("Does not duplicate mounts if target is already mounted", func(t *testing.T) {
			testCtx := setup(t)

			var mountCount atomic.Int32
			var bindMountCount atomic.Int32

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				mountCount.Add(1)
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				return int(mountertest.OpenDevNull(t).Fd()), nil
			}

			testCtx.mountBindSyscall = func(source, target string) (err error) {
				bindMountCount.Add(1)
				testCtx.mount.Mount(source, target, "fuse", []string{"bind"})
				return nil
			}
			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil).
				Times(5)

			go func() {
				mpPod := createMountpointPod(testCtx)
				mpPod.run()
				mpPod.receiveMountOptions(testCtx.ctx)
			}()

			for range 5 {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath,
					credentialprovider.ProvideContext{
						VolumeID:      testCtx.volumeID,
						WorkloadPodID: testCtx.podUID,
					}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
				assert.NoError(t, err)
			}

			assert.Equals(t, int32(1), mountCount.Load())
			assert.Equals(t, int32(1), bindMountCount.Load())
		})

		t.Run("Re-uses the same source mount for different targets if they share same Mountpoint Pod", func(t *testing.T) {
			// First Pod
			testCtx := setup(t)

			ok, _ := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
			assert.Equals(t, false, ok)

			var mountCount atomic.Int32
			var bindMountCount atomic.Int32

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				mountCount.Add(1)
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				return int(mountertest.OpenDevNull(t).Fd()), nil
			}
			testCtx.mountBindSyscall = func(source, target string) (err error) {
				bindMountCount.Add(1)
				testCtx.mount.Mount(source, target, "fuse", []string{"bind"})
				return nil
			}

			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil).
				Times(2)

			go func() {
				mpPod := createMountpointPod(testCtx)
				mpPod.run()
				mpPod.receiveMountOptions(testCtx.ctx)
			}()

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID:      testCtx.volumeID,
				WorkloadPodID: testCtx.podUID,
			}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
			assert.NoError(t, err)

			ok, err = testCtx.podMounter.IsMountPoint(testCtx.sourcePath)
			assert.NoError(t, err)
			assert.Equals(t, true, ok)
			ok, err = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			assert.Equals(t, true, ok)

			// Second Pod
			testCtx.podUID = uuid.New().String()
			targetPath2 := filepath.Join(
				testCtx.kubeletPath,
				fmt.Sprintf("pods/%s/volumes/kubernetes.io~csi/%s/mount", testCtx.podUID, testCtx.pvName),
			)
			err = os.MkdirAll(filepath.Dir(targetPath2), 0750)
			assert.NoError(t, err)
			parentDir, err := filepath.EvalSymlinks(filepath.Dir(targetPath2))
			assert.NoError(t, err)
			targetPath2 = filepath.Join(parentDir, filepath.Base(targetPath2))
			testCtx.targetPath = targetPath2
			testCrd2 := crdv2beta.MountpointS3PodAttachment{
				Spec: crdv2beta.MountpointS3PodAttachmentSpec{
					NodeName:             testCtx.nodeName,
					PersistentVolumeName: testCtx.pvName,
					VolumeID:             testCtx.volumeID,
					WorkloadFSGroup:      testCtx.fsGroup,
					MountpointS3PodAttachments: map[string][]crdv2beta.WorkloadAttachment{
						testCtx.mpPodName: {{WorkloadPodUID: testCtx.podUID}},
					},
				},
			}
			testCtx.s3paCache.TestItems = []crdv2beta.MountpointS3PodAttachment{testCrd2}

			err = testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID:      testCtx.volumeID,
				WorkloadPodID: testCtx.podUID,
			}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
			assert.NoError(t, err)

			ok, err = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			assert.Equals(t, true, ok)

			assert.Equals(t, int32(1), mountCount.Load())
			assert.Equals(t, int32(2), bindMountCount.Load())
		})

		t.Run("Updates credentials for existing SystemD mounts", func(t *testing.T) {
			testCtx := setup(t)
			t.Setenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS", "true")

			ok, _ := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
			assert.Equals(t, false, ok)

			// Simulate SystemD mount
			err := os.MkdirAll(testCtx.targetPath, 0750)
			assert.NoError(t, err)
			testCtx.mount.Mount("mountpoint-s3", testCtx.targetPath, "fuse", nil)

			ok, _ = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
			assert.Equals(t, true, ok)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				t.Errorf("unexpected mount syscall")
				return int(mountertest.OpenDevNull(t).Fd()), nil
			}
			testCtx.mountBindSyscall = func(source, target string) (err error) {
				t.Errorf("unexpected bind mount syscall")
				return nil
			}

			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				DoAndReturn(func(ctx context.Context, provideCtx credentialprovider.ProvideContext) (envprovider.Environment, credentialprovider.AuthenticationSource, error) {
					// Assert credential context was set correctly
					assert.Equals(t, credentialprovider.MountKindSystemd, provideCtx.MountKind)
					assert.Equals(t, "/var/lib/kubelet/plugins/s3.csi.aws.com/", provideCtx.WritePath)
					assert.Equals(t, "/var/lib/kubelet/plugins/s3.csi.aws.com/", provideCtx.EnvPath)
					return nil, credentialprovider.AuthenticationSourceDriver, nil
				})

			err = testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID:      testCtx.volumeID,
				WorkloadPodID: testCtx.podUID,
			}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
			assert.NoError(t, err)
		})

		t.Run("Unmounts source if Mountpoint Pod does not receive mount options", func(t *testing.T) {
			testCtx := setup(t)
			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

			go func() {
				mpPod := createMountpointPod(testCtx)
				mpPod.run()

				// Create the `mount.sock` but does not receive anything from it
				mountSock := mppod.PathOnHost(mpPod.podPath, mppod.KnownPathMountSock)
				_, err := os.Create(mountSock)
				assert.NoError(t, err)
			}()

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID:      testCtx.volumeID,
				WorkloadPodID: testCtx.podUID,
			}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
			if err == nil {
				t.Errorf("mount shouldn't succeeded if Mountpoint does not receive the mount options")
			}

			ok, err := testCtx.mount.IsMountPoint(testCtx.sourcePath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should unmount the source path if Mountpoint does not receive the mount options")
			}
			ok, err = testCtx.mount.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should not bind mount the target path if Mountpoint does not receive the mount options")
			}
		})

		t.Run("Unmounts source if Mountpoint Pod fails to start", func(t *testing.T) {
			testCtx := setup(t)
			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				// Does not do real mounting
				return int(mountertest.OpenDevNull(t).Fd()), nil
			}

			go func() {
				mpPod := createMountpointPod(testCtx)
				mpPod.run()
				mpPod.receiveMountOptions(testCtx.ctx)

				// Emulate that Mountpoint failed to mount
				mountErrorPath := mppod.PathOnHost(mpPod.podPath, mppod.KnownPathMountError)
				err := os.WriteFile(mountErrorPath, []byte("mount failed"), 0777)
				assert.NoError(t, err)
			}()

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID:      testCtx.volumeID,
				WorkloadPodID: testCtx.podUID,
			}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
			if err == nil {
				t.Errorf("mount shouldn't succeeded if Mountpoint fails to start")
			}

			ok, err := testCtx.mount.IsMountPoint(testCtx.sourcePath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should unmount the source path if Mountpoint fails to start")
			}
			ok, err = testCtx.mount.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should not bind mount the target path if Mountpoint fails to start")
			}
		})

		t.Run("Adds a help message to see Mountpoint logs if Mountpoint Pod fails to start", func(t *testing.T) {
			testCtx := setup(t)
			testCtx.mockCredProvider.EXPECT().
				Provide(testCtx.ctx, gomock.Any()).
				Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				// Does not do real mounting
				return int(mountertest.OpenDevNull(t).Fd()), nil
			}

			mpPod := createMountpointPod(testCtx)

			go func() {
				mpPod.run()
				mpPod.receiveMountOptions(testCtx.ctx)

				// Emulate that Mountpoint failed to mount
				mountErrorPath := mppod.PathOnHost(mpPod.podPath, mppod.KnownPathMountError)
				err := os.WriteFile(mountErrorPath, []byte("mount failed"), 0777)
				assert.NoError(t, err)
			}()

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID:      testCtx.volumeID,
				WorkloadPodID: testCtx.podUID,
			}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
			if err == nil {
				t.Errorf("mount shouldn't succeeded if Mountpoint fails to start")
			}

			mpLogsCmd := fmt.Sprintf("kubectl logs -n %s %s", mountpointPodNamespace, mpPod.pod.Name)
			if !strings.Contains(err.Error(), mpLogsCmd) {
				t.Errorf("Expected error message to contain a help message to get Mountpoint logs %s, but got: %s", mpLogsCmd, err.Error())
			}

			ok, err := testCtx.mount.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should unmount the target path if Mountpoint fails to start")
			}
		})
	})

	t.Run("Checking if target is a mount point", func(t *testing.T) {
		testCtx := setup(t)
		testCtx.mockCredProvider.EXPECT().
			Provide(testCtx.ctx, gomock.Any()).
			Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

		ok, _ := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.Equals(t, false, ok)

		go func() {
			mpPod := createMountpointPod(testCtx)
			mpPod.run()
			mpPod.receiveMountOptions(testCtx.ctx)
		}()

		err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
			VolumeID:      testCtx.volumeID,
			WorkloadPodID: testCtx.podUID,
		}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
		assert.NoError(t, err)

		ok, err = testCtx.podMounter.IsMountPoint(testCtx.sourcePath)
		assert.NoError(t, err)
		assert.Equals(t, true, ok)
		ok, err = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.NoError(t, err)
		assert.Equals(t, true, ok)
	})

	t.Run("Unmounting", func(t *testing.T) {
		testCtx := setup(t)
		testCtx.mockCredProvider.EXPECT().
			Provide(testCtx.ctx, gomock.Any()).
			Return(envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil)

		go func() {
			mpPod := createMountpointPod(testCtx)
			mpPod.run()
			mpPod.receiveMountOptions(testCtx.ctx)
		}()

		err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
			VolumeID:      testCtx.volumeID,
			WorkloadPodID: testCtx.podUID,
		}, mountpoint.ParseArgs(nil), testCtx.fsGroup)
		assert.NoError(t, err)

		ok, err := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.NoError(t, err)
		assert.Equals(t, true, ok)

		err = testCtx.podMounter.Unmount(testCtx.ctx, testCtx.targetPath, credentialprovider.CleanupContext{
			VolumeID: testCtx.volumeID,
			PodID:    testCtx.podUID,
		})
		assert.NoError(t, err)

		ok, err = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.NoError(t, err)
		assert.Equals(t, false, ok)
	})

	t.Run("Unmounting SystemD mounts", func(t *testing.T) {
		testCtx := setup(t)
		t.Setenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS", "true")

		ok, _ := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.Equals(t, false, ok)

		// Simulate SystemD mount
		err := os.MkdirAll(testCtx.targetPath, 0750)
		assert.NoError(t, err)
		testCtx.mount.Mount("mountpoint-s3", testCtx.targetPath, "fuse", nil)

		ok, _ = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.Equals(t, true, ok)

		testCtx.mockCredProvider.EXPECT().
			Cleanup(gomock.Any()).
			DoAndReturn(func(provideCtx credentialprovider.CleanupContext) error {
				// Assert credential context was set correctly
				assert.Equals(t, "/var/lib/kubelet/plugins/s3.csi.aws.com/", provideCtx.WritePath)
				assert.Equals(t, testCtx.volumeID, provideCtx.VolumeID)
				assert.Equals(t, testCtx.podUID, provideCtx.PodID)
				return nil
			})

		err = testCtx.podMounter.Unmount(testCtx.ctx, testCtx.targetPath, credentialprovider.CleanupContext{
			VolumeID: testCtx.volumeID,
			PodID:    testCtx.podUID,
		})
		assert.NoError(t, err)

		ok, err = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.NoError(t, err)
		assert.Equals(t, false, ok)
	})
}

type mountpointPod struct {
	testCtx *testCtx
	pod     *corev1.Pod
	podPath string
}

func createMountpointPod(testCtx *testCtx) *mountpointPod {
	t := testCtx.t
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:  types.UID(testCtx.mpPodUID),
			Name: testCtx.mpPodName,
		},
	}
	pod, err := testCtx.client.CoreV1().Pods(mountpointPodNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	assert.NoError(t, err)

	podPath := filepath.Join(testCtx.kubeletPath, "pods", string(pod.UID))
	// same with `emptyDir` volume, https://github.com/kubernetes/kubernetes/blob/8f8c94a04d00e59d286fe4387197bc62c6a4f374/pkg/volume/emptydir/empty_dir.go#L43-L48
	err = os.MkdirAll(mppod.PathOnHost(podPath), 0777)
	assert.NoError(t, err)

	return &mountpointPod{testCtx: testCtx, pod: pod, podPath: podPath}
}

func (mp *mountpointPod) run() {
	mp.testCtx.t.Helper()
	mp.pod.Status.Phase = corev1.PodRunning
	var err error
	mp.pod, err = mp.testCtx.client.CoreV1().Pods(mountpointPodNamespace).UpdateStatus(context.Background(), mp.pod, metav1.UpdateOptions{})
	assert.NoError(mp.testCtx.t, err)
}

// receiveMountOptions will receive mount options sent to the Mountpoint Pod.
// This operation will block in place, and ideally should be called from a separate goroutine.
func (mp *mountpointPod) receiveMountOptions(ctx context.Context) mountoptions.Options {
	mp.testCtx.t.Helper()
	mountSock := mppod.PathOnHost(mp.podPath, mppod.KnownPathMountSock)
	options, err := mountoptions.Recv(ctx, mountSock)
	assert.NoError(mp.testCtx.t, err)
	return options
}
