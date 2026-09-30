package mounter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	mountutils "k8s.io/mount-utils"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
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
	mounterDir    string
	commDir       string
	cacheDir      string
}

// targetPath returns a valid kubelet-style target path that targetpath.Parse can parse.
// Format: <kubeletPath>/pods/<podUID>/volumes/kubernetes.io~csi/<volumeID>/mount
func (testCtx *dmTestCtx) targetPath(podUID string) string {
	return testCtx.targetPathFor(podUID, testCtx.volumeID)
}

// targetPathFor is targetPath for a PV name other than the CSI volume ID, which the driver caches under.
func (testCtx *dmTestCtx) targetPathFor(podUID, pvName string) string {
	return filepath.Join(testCtx.kubeletPath, "pods", podUID, "volumes", "kubernetes.io~csi", pvName, "mount")
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

	// No cache volume: only setupDMWithCache creates one, so every other test exercises the
	// caching-disabled path.
	mounterDir := filepath.Join(kubeletPath, "pods", mounterPodUID)
	commDir := filepath.Join(mounterDir, "volumes", "kubernetes.io~empty-dir", mounter.CommVolumeName)
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
			Volumes: []corev1.Volume{{
				Name:         mounter.CommVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
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
		mounterDir:    mounterDir,
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
	}, testK8sVersion, cluster.DefaultKubernetes)
	err = dm.DiscoverMounter(ctx)
	assert.NoError(t, err)

	testCtx.dm = dm
	return testCtx
}

// setupDMWithCache is setupDM with a mounter pod of the given cache volume kind, plus the on-disk volume.
// Both together because production assumes they agree; testCtx.cacheDir is the created volume.
func setupDMWithCache(t *testing.T, cacheKind string) *dmTestCtx {
	t.Helper()

	testCtx := setupDM(t)

	volume := corev1.Volume{Name: mounter.CacheVolumeName}
	cacheDir := filepath.Join(testCtx.mounterDir, "volumes", "kubernetes.io~empty-dir", mounter.CacheVolumeName)
	switch cacheKind {
	case volumecontext.CacheTypeEmptyDir:
		volume.EmptyDir = &corev1.EmptyDirVolumeSource{}
	case volumecontext.CacheTypeEphemeral:
		volume.Ephemeral = &corev1.EphemeralVolumeSource{}
		// The bound PV's name is generated, so the driver reads it off the PVC, always named <pod>-<volume>.
		pvName := "pvc-" + uuid.New().String()
		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "s3-csi-daemonset-mounter-abcde-" + mounter.CacheVolumeName,
				Namespace: "kube-system",
			},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		}
		_, err := testCtx.client.CoreV1().PersistentVolumeClaims("kube-system").Create(testCtx.ctx, claim, metav1.CreateOptions{})
		assert.NoError(t, err)
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-cache"},
			}},
		}
		_, err = testCtx.client.CoreV1().PersistentVolumes().Create(testCtx.ctx, pv, metav1.CreateOptions{})
		assert.NoError(t, err)
		cacheDir = filepath.Join(testCtx.mounterDir, "volumes", "kubernetes.io~csi", pvName, "mount")
	default:
		t.Fatalf("setupDMWithCache: %q is not a cache kind", cacheKind)
	}

	assert.NoError(t, os.MkdirAll(cacheDir, 0770))
	testCtx.cacheDir = cacheDir
	testCtx.setMounterPodCacheVolume(&volume)
	return testCtx
}

// setMounterPodCacheVolume sets the mounter pod's cache volume, or removes it when nil, then
// re-runs discovery - the only thing that reads the cache type, so without it the old value stands.
func (testCtx *dmTestCtx) setMounterPodCacheVolume(volume *corev1.Volume) {
	testCtx.t.Helper()

	pods := testCtx.client.CoreV1().Pods("kube-system")
	list, err := pods.List(testCtx.ctx, metav1.ListOptions{})
	assert.NoError(testCtx.t, err)
	assert.Equals(testCtx.t, 1, len(list.Items))

	pod := &list.Items[0]
	var volumes []corev1.Volume
	for _, v := range pod.Spec.Volumes {
		if v.Name != mounter.CacheVolumeName {
			volumes = append(volumes, v)
		}
	}
	if volume != nil {
		volumes = append(volumes, *volume)
	}
	pod.Spec.Volumes = volumes

	_, err = pods.Update(testCtx.ctx, pod, metav1.UpdateOptions{})
	assert.NoError(testCtx.t, err)
	assert.NoError(testCtx.t, testCtx.dm.DiscoverMounter(testCtx.ctx))
}

// mountAndServe runs Mount to completion for a workload pod and PV, playing the mounter's part by
// receiving the options, and returns the target plus what the mounter got. Only usable with
// setupDM's default mountSyscall, which is what registers the source mount waitForMount polls for.
func (testCtx *dmTestCtx) mountAndServe(podUID, pvName string, volumeCtx map[string]string, mountOptions ...string) (string, mountoptions.Options) {
	testCtx.t.Helper()

	target := testCtx.targetPathFor(podUID, pvName)
	// Buffered: a failed assertion below exits that goroutine, and an unbuffered send would deadlock.
	mountRes := make(chan error, 1)
	go func() {
		mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, testCtx.driverProvideCtx(podUID),
			volumeCtx, mountpoint.ParseArgs(mountOptions), "", nil)
	}()

	got := testCtx.receiveMountOptions()
	assert.NoError(testCtx.t, <-mountRes)
	return target, got
}

// driverProvideCtx is the context a driver-authenticated workload pod mounts with. Two pods sharing
// one mount must agree on it, so a sharing test passes the same one to both.
func (testCtx *dmTestCtx) driverProvideCtx(podUID string) credentialprovider.ProvideContext {
	return credentialprovider.ProvideContext{
		WorkloadPodID:        podUID,
		VolumeID:             testCtx.volumeID,
		AuthenticationSource: "driver",
		ServiceAccountName:   "default",
		PodNamespace:         "default",
	}
}

// cacheArg returns the value of the `--cache` argument the mounter received, or "" when it has none.
func cacheArg(options mountoptions.Options) string {
	args := mountpoint.ParseArgs(options.Args)
	value, _ := args.Value(mountpoint.ArgCache)
	return value
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
				}, nil, args, "", nil)
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

			// Dup, so this File does not become a second owner of the fd receiveMountOptions closes -
			// whichever owner is collected first closes an fd the other still believes it holds.
			fd, err := syscall.Dup(got.Fd)
			assert.NoError(t, err)
			mountertest.AssertSameFile(t, devNull, os.NewFile(uintptr(fd), "fd"))

			// Reset fd as they might be different in different ends.
			got.Fd = 0

			env := envprovider.Default()
			// The mount must carry the user-agent, built from the same inputs setupDM used.
			expectedUserAgent := "--user-agent-prefix=" + mounter.UserAgent(credentialprovider.AuthenticationSourceDriver, testK8sVersion, cluster.DefaultKubernetes)
			assert.Equals(t, mountoptions.Options{
				BucketName: testCtx.bucketName,
				Args:       []string{"--prefix=data/", expectedUserAgent},
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
				}, nil, "", cluster.DefaultKubernetes)
			err := testCtx.dm.DiscoverMounter(testCtx.ctx)
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
			}, nil, mountpoint.ParseArgs(nil), "", nil)
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
				}, nil, "", cluster.DefaultKubernetes)
			err := testCtx.dm.DiscoverMounter(testCtx.ctx)
			assert.NoError(t, err)

			err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, nil, mountpoint.ParseArgs(nil), "", nil)
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
			}, nil, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("mount should fail if mounter does not receive the mount options")
			}
			assert.Contains(t, err.Error(), "failed to send mount options")

			// In sharing mode, FUSE mount goes to source path, not target.
			// After failure, source should be unmounted and cleaned up (directory removed).
			sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
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
				}, nil, mountpoint.ParseArgs(nil), "", nil)
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
			sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
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
				}, nil, mountpoint.ParseArgs(nil), "", nil)
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
		t.Run("Discovery resolves the mounter pod's comm directory", func(t *testing.T) {
			testCtx := setupDM(t)

			commDir, err := testCtx.dm.GetCommDir()
			assert.NoError(t, err)
			assert.Equals(t, testCtx.commDir, commDir)

			// Also assert against literals to test accuracy of commDirForMounterDir function as well.
			assert.Equals(t, filepath.Join(testCtx.kubeletPath, "pods", testCtx.mounterPodUID, "volumes", "kubernetes.io~empty-dir", "comm"), commDir)
		})

		t.Run("DiscoverMounter rejects invalid pod states", func(t *testing.T) {
			// DiscoverMounter -> tryDiscoverMounter should reject invalid pod states
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
					dm := mounter.NewDaemonsetMounter(client, "test-node", mpmounter.NewWithMount(mountutils.NewFakeMounter(nil)), nil, nil, nil, nil, "", cluster.DefaultKubernetes)

					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
					defer cancel()

					err := dm.DiscoverMounter(ctx)
					t.Logf("%v", err)
					if err == nil {
						t.Fatal("expected error from DiscoverMounter")
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

			// Create a fresh DM which has not discovered the mounter pod (setupDM called dm.DiscoverMounter(ctx))
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
				"",
				cluster.DefaultKubernetes,
			)

			err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: testCtx.podUID,
				VolumeID:      testCtx.volumeID,
			}, nil, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("expected error when the mounter pod is not discovered")
			}
			assert.ErrorIs(t, err, mounter.ErrCommDirNotReady)
			if mountSyscallCalled {
				t.Error("mountSyscall should not be called when the mounter pod is not available")
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
			// until context timeout (DeadlineExceeded) which should nil the mounter pod on staleness
			err := testCtx.dm.Mount(ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: "pod-timeout",
				VolumeID:      testCtx.volumeID,
			}, nil, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("expected error on send timeout")
			}
			assert.Contains(t, err.Error(), "failed to send mount options")

			// Verify the mounter pod was nilled by the staleness detection
			_, err = testCtx.dm.GetCommDir()
			assert.ErrorIs(t, err, mounter.ErrCommDirNotReady)
		})

		t.Run("Cancelled context does not cause stale commDir", func(t *testing.T) {
			// Kubelet cancels NodePublishVolume when workload pod deleted mid-mount. If
			// it incorrectly nils the mounter pod, all subsequent mounts fail with "mounter pod
			// not available" until the watcher re-discovers.
			testCtx := setupDM(t)
			target := testCtx.targetPath("pod-cancel")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
				testCtx.mount.Mount("mountpoint-s3", tgt, "fuse", nil)
				fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
				assert.NoError(t, err)
				cancel()
				return fd, nil
			}

			err := testCtx.dm.Mount(ctx, testCtx.bucketName, target, credentialprovider.ProvideContext{
				WorkloadPodID: "pod-cancel",
				VolumeID:      testCtx.volumeID,
			}, nil, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("expected error on cancelled context")
			}
			errMsg := err.Error()
			if !strings.Contains(errMsg, "failed to send mount options") && !strings.Contains(errMsg, "context canceled") {
				t.Fatalf("expected error containing \"failed to send mount options\" or \"context canceled\", got: %q", errMsg)
			}

			// Verify the mounter pod was NOT nilled by the cancelled context
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
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target1,
				testCtx.driverProvideCtx("pod-a-uid"), nil, mountpoint.ParseArgs(nil), "", nil)
		}()

		// Receive and complete the first mount
		testCtx.receiveMountOptions()
		sourcePath := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
		testCtx.mount.Mount("mountpoint-s3", sourcePath, "fuse", nil)

		err := <-mountRes
		assert.NoError(t, err)
		assert.Equals(t, 1, fuseMountCount)

		// Mount second pod — same volume, same params → should share via bind mount
		err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target2,
			testCtx.driverProvideCtx("pod-b-uid"), nil, mountpoint.ParseArgs(nil), "", nil)
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
			}, nil, "", cluster.DefaultKubernetes)
		err := testCtx.dm.DiscoverMounter(testCtx.ctx)
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
			}, nil, mountpoint.ParseArgs(nil), "", nil)
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
		}, nil, mountpoint.ParseArgs(nil), "", nil)
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
			}, nil, mountpoint.ParseArgs(nil), "", nil)
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
		}, nil, mountpoint.ParseArgs(nil), "", nil)
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
			}, nil, mountpoint.ParseArgs(nil), "", nil)
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
			}, nil, mountpoint.ParseArgs(nil), "", nil)
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
			mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target1,
				testCtx.driverProvideCtx("pod-first"), nil, mountpoint.ParseArgs(nil), "", nil)
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
				results[idx] = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target,
					testCtx.driverProvideCtx(podUID), nil, mountpoint.ParseArgs(nil), "", nil)
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

func TestDaemonsetMounter_Cache(t *testing.T) {
	// The PV name the driver caches under, deliberately not the CSI volume ID setupDM uses.
	const pvName = "s3-pv-name-differs-from-handle"

	cacheRequest := map[string]string{volumecontext.Cache: volumecontext.CacheEnabled}

	// mountWithoutServing runs a Mount that is expected to fail, so it does not play the
	// mounter's part; a request that reaches the socket would hang here rather than fail.
	mountWithoutServing := func(testCtx *dmTestCtx, ctx context.Context, podUID string, volumeCtx map[string]string) error {
		return testCtx.dm.Mount(ctx, testCtx.bucketName, testCtx.targetPathFor(podUID, pvName),
			credentialprovider.ProvideContext{WorkloadPodID: podUID, VolumeID: testCtx.volumeID},
			volumeCtx, mountpoint.ParseArgs(nil), "", nil)
	}

	// Discovery
	t.Run("A mounter whose ephemeral cache PVC cannot be read is not discovered", func(t *testing.T) {
		testCtx := setupDM(t)

		pods := testCtx.client.CoreV1().Pods("kube-system")
		pod, err := pods.Get(testCtx.ctx, "s3-csi-daemonset-mounter-abcde", metav1.GetOptions{})
		assert.NoError(t, err)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         mounter.CacheVolumeName,
			VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}},
		})
		_, err = pods.Update(testCtx.ctx, pod, metav1.UpdateOptions{})
		assert.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		err = testCtx.dm.DiscoverMounter(ctx)
		if err == nil {
			t.Fatal("expected discovery to fail when the cache PVC cannot be read")
		}
		// Unresolvable cache directory: reject every cached PV on the node.
		assert.Contains(t, err.Error(), "cache volume PVC")
	})

	// Accepting and rejecting a PV's request
	t.Run("An ephemeral mounter resolves its cache under the CSI volume the kubelet gave it", func(t *testing.T) {
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEphemeral)

		_, got := testCtx.mountAndServe("pod-a-uid", pvName, cacheRequest)
		assert.Equals(t, "/cache/"+pvName, cacheArg(got))

		// The test globs because the PV name is generated; under the test, the driver lookups PVC
		// landing on the directory the kubelet created.
		matches, err := filepath.Glob(filepath.Join(testCtx.mounterDir, "volumes", "kubernetes.io~csi", "*", "mount", pvName))
		assert.NoError(t, err)
		assert.Equals(t, []string{filepath.Join(testCtx.cacheDir, pvName)}, matches)
	})

	t.Run("A PV that asks for no cache gets none, even on a mounter that has one", func(t *testing.T) {
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEmptyDir)

		_, got := testCtx.mountAndServe("pod-a-uid", pvName, nil)

		assert.Equals(t, "", cacheArg(got))
		assert.FileNotExists(t, filepath.Join(testCtx.cacheDir, pvName))
	})

	t.Run("A v1 cache mount option's path is replaced with the mount's own directory", func(t *testing.T) {
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEmptyDir)

		_, got := testCtx.mountAndServe("pod-a-uid", pvName, nil, "cache /tmp/customer-path")

		assert.Equals(t, "/cache/"+pvName, cacheArg(got))
	})

	t.Run("A fresh mount over a dead cached source that no longer asks for a cache records none", func(t *testing.T) {
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEmptyDir)
		testCtx.mountAndServe("pod-a-uid", pvName, cacheRequest)

		// Unmounting the source makes it dead, so the next Mount takes the fresh-mount path and reuses the entry.
		assert.NoError(t, testCtx.mount.Unmount(mounter.SourceMountPath(testCtx.kubeletPath, pvName)))
		_, got := testCtx.mountAndServe("pod-b-uid", pvName, nil)

		assert.Equals(t, "", cacheArg(got))
		raw, err := os.ReadFile(mounter.MetaFileName(testCtx.kubeletPath, pvName))
		assert.NoError(t, err)
		var meta mounter.MountMeta
		assert.NoError(t, json.Unmarshal(raw, &meta))
		assert.Equals(t, "", meta.CacheDir)
	})

	t.Run("A cache request on a mounter with no cache volume is rejected before anything is created", func(t *testing.T) {
		testCtx := setupDM(t)

		err := mountWithoutServing(testCtx, testCtx.ctx, "pod-rejected", cacheRequest)
		assert.Equals(t, codes.InvalidArgument, status.Code(err))

		assert.FileNotExists(t, mounter.MetaFileName(testCtx.kubeletPath, pvName))
		assert.FileNotExists(t, mounter.SourceMountPath(testCtx.kubeletPath, pvName))
	})

	// Creating and removing the mount's directory
	t.Run("A cache directory that cannot be created fails the mount, keeping the meta file", func(t *testing.T) {
		// Assert, not skip: CI is unprivileged, so a root run must fail loudly rather than lose this case. (Considered skipping if root run)
		assert.Equals(t, false, os.Geteuid() == 0)
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEmptyDir)

		// Make cache volume read-only
		assert.NoError(t, os.Chmod(testCtx.cacheDir, 0500))
		t.Cleanup(func() { os.Chmod(testCtx.cacheDir, 0770) })

		err := mountWithoutServing(testCtx, testCtx.ctx, "pod-a-uid", cacheRequest)
		if err == nil {
			t.Fatal("expected the mount to fail when its cache directory cannot be created")
		}
		assert.Contains(t, err.Error(), "failed to create cache directory")

		// The meta file is written before the directory, so the cleanup that retries has the path.
		_, statErr := os.Stat(mounter.MetaFileName(testCtx.kubeletPath, pvName))
		assert.NoError(t, statErr)
	})

	t.Run("A cache that cannot be removed keeps the mount's meta file for a later retry", func(t *testing.T) {
		// Assert, not skip: CI is unprivileged, so a root run must fail loudly rather than lose this case. (Considered skipping if root run)
		assert.Equals(t, false, os.Geteuid() == 0)
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEmptyDir)

		target, _ := testCtx.mountAndServe("pod-a-uid", pvName, cacheRequest)
		mountCacheDir := filepath.Join(testCtx.cacheDir, pvName)

		// Make cache volume read-only
		assert.NoError(t, os.Chmod(testCtx.cacheDir, 0500))
		t.Cleanup(func() { os.Chmod(testCtx.cacheDir, 0770) })

		// TODO: do we want this to error instead? See TODO in daemonset_mounter.go's releaseTarget function.
		assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, target, credentialprovider.CleanupContext{VolumeID: pvName}))

		_, err := os.Stat(mountCacheDir)
		assert.NoError(t, err)
		_, err = os.Stat(mounter.MetaFileName(testCtx.kubeletPath, pvName))
		assert.NoError(t, err)

		// Use CleanupOrphans to see if MountMap entry is retained.
		assert.NoError(t, os.Chmod(testCtx.cacheDir, 0770))
		testCtx.dm.CleanupOrphans()

		assert.FileNotExists(t, mounter.MetaFileName(testCtx.kubeletPath, pvName))
		assert.FileNotExists(t, mountCacheDir)
	})

	// Rediscovery
	t.Run("Staleness drops the cache dir with the comm dir, and next discovery replaces it", func(t *testing.T) {
		// DiscoverMounter is called to discover mounter dir / comm dir.
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEmptyDir)

		// Nothing accepts on the socket, so mountoptions.Send fails, and the driver drops the
		// discovered mounter pod (dm.mounter.Store(nil)).
		staleCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		err := mountWithoutServing(testCtx, staleCtx, "pod-stale", cacheRequest)
		if err == nil {
			t.Fatal("expected the send to fail so the mounter pod is dropped")
		}
		assert.Contains(t, err.Error(), "failed to send mount options")

		// A cache request now reports the missing mounter rather than reading the dropped cache type.
		err = mountWithoutServing(testCtx, testCtx.ctx, "pod-while-stale", cacheRequest)
		assert.ErrorIs(t, err, mounter.ErrCommDirNotReady)

		// Simulate mounter is back + rerun discovery, but has no cache volume;
		// PV accepted above (reached Send before) is now rejected.
		testCtx.setMounterPodCacheVolume(nil)

		err = mountWithoutServing(testCtx, testCtx.ctx, "pod-after-rediscovery", cacheRequest)
		assert.Equals(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("Cache directory survives until the last consumer unmounts", func(t *testing.T) {
		testCtx := setupDMWithCache(t, volumecontext.CacheTypeEmptyDir)
		pvName := "s3-pv-shared-cache"
		mountCacheDir := filepath.Join(testCtx.cacheDir, pvName)

		target1, _ := testCtx.mountAndServe("pod-a-uid", pvName, cacheRequest)

		// Don't use mountAndServe so it doesn't try to connect to socket again for fd passing.
		target2 := testCtx.targetPathFor("pod-b-uid", pvName)
		assert.NoError(t, testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target2, testCtx.driverProvideCtx("pod-b-uid"),
			cacheRequest, mountpoint.ParseArgs(nil), "", nil))

		_, err := os.Stat(mountCacheDir)
		assert.NoError(t, err)

		// First consumer leaves: the mount is still live, so its cache must not be touched.
		assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, target1, credentialprovider.CleanupContext{VolumeID: pvName}))
		_, err = os.Stat(mountCacheDir)
		assert.NoError(t, err)

		// Last consumer leaves: cache subdir now deleted.
		assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, target2, credentialprovider.CleanupContext{VolumeID: pvName}))
		_, err = os.Stat(mountCacheDir)
		assert.Equals(t, true, os.IsNotExist(err))

		// The shared volume root must survive.
		_, err = os.Stat(testCtx.cacheDir)
		assert.NoError(t, err)
	})
}

func (testCtx *dmTestCtx) receiveMountOptions() mountoptions.Options {
	testCtx.t.Helper()
	sockPath := filepath.Join(testCtx.commDir, mounter.MountSockName)
	options, err := mountoptions.Recv(testCtx.ctx, sockPath)
	assert.NoError(testCtx.t, err)
	// Recv takes ownership of a real fd over SCM_RIGHTS, which otherwise stays open for the whole
	// test binary. Closed here so every caller is covered.
	received := os.NewFile(uintptr(options.Fd), "fuse-fd")
	testCtx.t.Cleanup(func() { received.Close() })
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

// --- IsSourceHealthy health-state tests ---
//
// IsSourceHealthy wraps CheckMountpoint -> IsHealthyMountpoint (bounded by ctx),
// so these tests exercise that whole chain and assert the tri-state result:
//   (true, nil)  = healthy
//   (false, nil) = definitely dead
//   (false, err) = UNKNOWN (callers MUST NOT treat as dead)

// mountpointDevice is the device name CheckMountpoint expects (mpmounter's fsName).
const mountpointDevice = "mountpoint-s3"

// fakeHealthMount is a minimal mountutils.Interface stub that lets tests control
// what IsMountPoint and List return. Only these two methods are used by
// CheckMountpoint; any other call would panic (embedded nil interface), which is
// the intended guard.
type fakeHealthMount struct {
	mountutils.Interface

	isMountPoint    bool
	isMountPointErr error
	listResult      []mountutils.MountPoint
	listErr         error
}

func (f *fakeHealthMount) IsMountPoint(_ string) (bool, error) {
	return f.isMountPoint, f.isMountPointErr
}

func (f *fakeHealthMount) List() ([]mountutils.MountPoint, error) {
	return f.listResult, f.listErr
}

// healthPathErr wraps an errno the way filesystem calls do (*os.PathError), which
// is what mount-utils' IsCorruptedMnt expects to unwrap.
func healthPathErr(errno syscall.Errno) error {
	return &os.PathError{Op: "open", Path: "/some/source", Err: errno}
}

// newHealthDM builds a DaemonsetMounter wired only with the given mount interface;
// IsSourceHealthy touches nothing else.
func newHealthDM(mount mountutils.Interface) *mounter.DaemonsetMounter {
	return mounter.NewDaemonsetMounter(nil, "", mpmounter.NewWithMount(mount), nil, nil, nil, nil, "", cluster.DefaultKubernetes)
}

// healthSource returns a real, existing directory when exists is true (so statx
// and os.Open succeed), or a path guaranteed not to exist otherwise.
func healthSource(t *testing.T, exists bool) string {
	t.Helper()
	dir := t.TempDir()
	if exists {
		return dir
	}
	return filepath.Join(dir, "does-not-exist")
}

func TestIsSourceHealthy_States(t *testing.T) {
	tests := []struct {
		name   string
		exists bool // whether the source path exists on disk (drives statx/os.Open)
		fake   *fakeHealthMount

		wantHealthy bool
		wantErr     bool // true => UNKNOWN
	}{
		{
			name:        "missing source is dead",
			exists:      false,
			fake:        &fakeHealthMount{},
			wantHealthy: false,
			wantErr:     false,
		},
		{
			name:        "not a mount is dead",
			exists:      true,
			fake:        &fakeHealthMount{isMountPoint: false},
			wantHealthy: false,
			wantErr:     false,
		},
		{
			name:        "ENOTCONN is dead",
			exists:      true,
			fake:        &fakeHealthMount{isMountPointErr: healthPathErr(syscall.ENOTCONN)},
			wantHealthy: false,
			wantErr:     false,
		},
		{
			name:        "EINTR is unknown",
			exists:      true,
			fake:        &fakeHealthMount{isMountPointErr: healthPathErr(syscall.EINTR)},
			wantHealthy: false,
			wantErr:     true,
		},
		{
			name:   "different device is dead",
			exists: true,
			fake: &fakeHealthMount{
				isMountPoint: true,
				listResult:   []mountutils.MountPoint{{Path: "TARGET", Device: "ext4"}},
			},
			wantHealthy: false,
			wantErr:     false,
		},
		{
			name: "List failure is unknown",
			// CheckMountpoint wraps the List error with %w, so it is no longer a
			// bare *os.PathError; IsCorruptedMnt can't classify it, and it is
			// (correctly) treated as UNKNOWN rather than dead.
			exists: true,
			fake: &fakeHealthMount{
				isMountPoint: true,
				listErr:      healthPathErr(syscall.EIO),
			},
			wantHealthy: false,
			wantErr:     true,
		},
		{
			name:   "live Mountpoint is healthy",
			exists: true,
			fake: &fakeHealthMount{
				isMountPoint: true,
				listResult:   []mountutils.MountPoint{{Path: "TARGET", Device: mountpointDevice}},
			},
			wantHealthy: true,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := healthSource(t, tt.exists)
			// Fill in the real source path for list entries that reference it.
			for i := range tt.fake.listResult {
				if tt.fake.listResult[i].Path == "TARGET" {
					tt.fake.listResult[i].Path = source
				}
			}

			dm := newHealthDM(tt.fake)
			healthy, err := dm.IsSourceHealthy(context.Background(), source)

			assert.Equals(t, tt.wantHealthy, healthy)
			assert.Equals(t, tt.wantErr, err != nil)
		})
	}
}

// blockingMount blocks in IsMountPoint until released, so we can drive the
// ctx-timeout (UNKNOWN) branch of IsSourceHealthy deterministically.
type blockingMount struct {
	mountutils.Interface
	release chan struct{}
}

func (b *blockingMount) IsMountPoint(_ string) (bool, error) {
	<-b.release
	return true, nil
}

// List is reached only after the probe is released on cleanup; returning an
// empty table lets the background goroutine finish without a nil-interface panic.
func (b *blockingMount) List() ([]mountutils.MountPoint, error) {
	return nil, nil
}

func TestIsSourceHealthy_TimeoutIsUnknown(t *testing.T) {
	release := make(chan struct{})
	// Release the blocked probe on cleanup so the background goroutine (which
	// sends to a buffered channel) exits without leaking.
	t.Cleanup(func() { close(release) })

	dm := newHealthDM(&blockingMount{release: release})
	source := healthSource(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	healthy, err := dm.IsSourceHealthy(ctx, source)

	// A timed-out health check is UNKNOWN, not dead — must return an error.
	assert.Equals(t, false, healthy)
	assert.Equals(t, true, err != nil)
}

// --- CheckTargetState health-state tests ---
//
// CheckTargetState uses the same IsHealthyMountpoint probe as IsSourceHealthy, but the
// interpretation differs: ErrMountAbsent maps to (true, nil) rather than (false, nil),
// because a missing or not-yet-mounted target is the normal fresh-mount case.

func TestCheckTargetState_States(t *testing.T) {
	tests := []struct {
		name   string
		exists bool // whether the target path exists on disk (drives statx/os.Open)
		fake   *fakeHealthMount

		wantState mounter.TargetState
		wantErr   bool // true => UNKNOWN (caller retries)
	}{
		{
			// Fresh mount: target directory does not exist yet. ErrMountAbsent → TargetAbsent.
			name:      "missing target is absent (proceed)",
			exists:    false,
			fake:      &fakeHealthMount{},
			wantState: mounter.TargetAbsent,
			wantErr:   false,
		},
		{
			// Fresh mount: target exists but is not a mount. ErrMountAbsent → TargetAbsent.
			name:      "not a mount is absent (proceed)",
			exists:    true,
			fake:      &fakeHealthMount{isMountPoint: false},
			wantState: mounter.TargetAbsent,
			wantErr:   false,
		},
		{
			// Corrupted/dead mount: ENOTCONN from IsMountPoint → TargetDead.
			name:      "ENOTCONN is dead (corrupted)",
			exists:    true,
			fake:      &fakeHealthMount{isMountPointErr: healthPathErr(syscall.ENOTCONN)},
			wantState: mounter.TargetDead,
			wantErr:   false,
		},
		{
			// Transient error: EINTR → UNKNOWN (TargetDead + err).
			name:      "EINTR is unknown",
			exists:    true,
			fake:      &fakeHealthMount{isMountPointErr: healthPathErr(syscall.EINTR)},
			wantState: mounter.TargetDead,
			wantErr:   true,
		},
		{
			// Different device in mount table → not a Mountpoint mount → ErrMountAbsent → TargetAbsent.
			name:   "different device is absent (proceed)",
			exists: true,
			fake: &fakeHealthMount{
				isMountPoint: true,
				listResult:   []mountutils.MountPoint{{Path: "TARGET", Device: "ext4"}},
			},
			wantState: mounter.TargetAbsent,
			wantErr:   false,
		},
		{
			// List failure → UNKNOWN (TargetDead + err).
			name:   "List failure is unknown",
			exists: true,
			fake: &fakeHealthMount{
				isMountPoint: true,
				listErr:      healthPathErr(syscall.EIO),
			},
			wantState: mounter.TargetDead,
			wantErr:   true,
		},
		{
			// Live Mountpoint mount → os.Open succeeds → TargetHealthy.
			name:   "live Mountpoint is healthy",
			exists: true,
			fake: &fakeHealthMount{
				isMountPoint: true,
				listResult:   []mountutils.MountPoint{{Path: "TARGET", Device: mountpointDevice}},
			},
			wantState: mounter.TargetHealthy,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := healthSource(t, tt.exists)
			// Fill in the real target path for list entries that reference it.
			for i := range tt.fake.listResult {
				if tt.fake.listResult[i].Path == "TARGET" {
					tt.fake.listResult[i].Path = target
				}
			}

			dm := newHealthDM(tt.fake)
			state, err := dm.CheckTargetState(context.Background(), target)

			assert.Equals(t, tt.wantState, state)
			assert.Equals(t, tt.wantErr, err != nil)
		})
	}
}

func TestCheckTargetState_TimeoutIsUnknown(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	dm := newHealthDM(&blockingMount{release: release})
	target := healthSource(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	state, err := dm.CheckTargetState(ctx, target)

	// A timed-out health check is UNKNOWN, not dead — must return an error.
	assert.Equals(t, mounter.TargetDead, state)
	assert.Equals(t, true, err != nil)
}

// TestMount_CorruptedTarget_NoMapEntryNoMeta verifies that when Mount is called with a
// corrupted target (ENOTCONN), it returns nil without creating a meta file. This is the
// churn-prevention invariant: a corrupted target causes an early bail, leaving cleanup to
// the periodic job rather than churning mount/creds every republish.
func TestMount_CorruptedTarget_NoMapEntryNoMeta(t *testing.T) {
	kubeletPath := t.TempDir()
	const volumeID = "pv-corrupted-target"

	// A target path that exists on disk (kubelet created it) but IsMountPoint returns
	// a corrupted-mount error. We simulate this by making the fakeHealthMount return
	// ENOTCONN when CheckMountpoint runs IsMountPoint on the target.
	targetDir := filepath.Join(kubeletPath, "pods", "pod-uid-123", "volumes", "kubernetes.io~csi", volumeID, "mount")
	assert.NoError(t, os.MkdirAll(targetDir, 0750))

	// Wire a DaemonsetMounter whose mount.CheckMountpoint (via IsMountPoint) returns ENOTCONN
	// for any path — simulating a corrupted bind mount at the target.
	corruptedMount := &fakeHealthMount{
		isMountPointErr: healthPathErr(syscall.ENOTCONN),
	}
	dm := mounter.NewDaemonsetMounter(
		nil, "test-node",
		mpmounter.NewWithMount(corruptedMount),
		nil, nil, nil, nil,
		"", cluster.DefaultKubernetes,
	)

	ctx := context.Background()
	credCtx := credentialprovider.ProvideContext{
		VolumeID:      volumeID,
		WorkloadPodID: "pod-uid-123",
	}

	err := dm.Mount(ctx, "test-bucket", targetDir, credCtx, nil, mountpoint.Args{}, "", nil)

	// Mount should return nil (nothing to do, corrupted target).
	assert.NoError(t, err)

	// No meta file should exist — Mount bailed before writing anything.
	metaPath := mounter.MetaFileName(kubeletPath, volumeID)
	_, statErr := os.Stat(metaPath)
	assert.Equals(t, true, os.IsNotExist(statErr))
}

// TestMount_AbsentTarget_ProceedsToMount verifies that when Mount is called with a target
// that does not exist (fs.ErrNotExist — the fresh-mount case), IsTargetHealthy returns
// (true, nil) and Mount proceeds past the health check rather than bailing with nil.
// It will eventually error downstream (e.g. commDir not discovered), but the point is it
// does NOT return nil like a corrupted target — it tries to mount. This proves the
// not-exist accommodation works: a missing target is "absent/fresh → proceed", not "dead".
func TestMount_AbsentTarget_ProceedsToMount(t *testing.T) {
	kubeletPath := t.TempDir()
	const volumeID = "pv-absent-target"

	// Target path does NOT exist on disk — simulates a fresh first mount where kubelet
	// hasn't created the dir yet (or it was cleaned up).
	targetDir := filepath.Join(kubeletPath, "pods", "pod-uid-456", "volumes", "kubernetes.io~csi", volumeID, "mount")
	// Intentionally NOT creating targetDir.

	// Use a fakeHealthMount that has never been a mount — but statx will fail with ENOENT
	// since the path doesn't exist. IsTargetHealthy should return (true, nil) and proceed.
	dm := mounter.NewDaemonsetMounter(
		nil, "test-node",
		mpmounter.NewWithMount(&fakeHealthMount{}),
		nil, nil, nil, nil,
		"", cluster.DefaultKubernetes,
	)

	ctx := context.Background()
	credCtx := credentialprovider.ProvideContext{
		VolumeID:      volumeID,
		WorkloadPodID: "pod-uid-456",
	}

	err := dm.Mount(ctx, "test-bucket", targetDir, credCtx, nil, mountpoint.Args{}, "", nil)

	// Mount should NOT return nil — it should proceed past the health check and eventually
	// error out downstream (e.g. "comm dir not yet discovered" since we didn't set up
	// the mounter pod). The key assertion is that err != nil (it tried to mount) rather
	// than err == nil (it bailed like a corrupted target).
	assert.Equals(t, true, err != nil)

	// No meta file should exist — Mount errored before reaching WriteMeta because commDir
	// is not configured in this minimal test setup.
	metaPath := mounter.MetaFileName(kubeletPath, volumeID)
	_, statErr := os.Stat(metaPath)
	assert.Equals(t, true, os.IsNotExist(statErr))
}
