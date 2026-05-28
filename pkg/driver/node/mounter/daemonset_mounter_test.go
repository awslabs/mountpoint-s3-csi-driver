package mounter_test

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
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
	// to overcome `bind: invalid argument`.
	t.Chdir(kubeletPath)

	nodeName := "test-node"
	bucketName := "test-bucket"
	volumeID := "s3-csi-driver-volume"
	podUID := uuid.New().String()
	mounterPodUID := uuid.New().String()

	commDir := filepath.Join(kubeletPath, "pods", mounterPodUID, "volumes", "kubernetes.io~empty-dir", mounter.CommVolumeName)
	err = os.MkdirAll(commDir, 0755)
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

	tc := &dmTestCtx{
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
		if tc.mountSyscall != nil {
			return tc.mountSyscall(target, opts)
		}
		fakeMounter.Mount("mountpoint-s3", target, "fuse", nil)
		fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
		assert.NoError(t, err)
		return fd, nil
	}

	t.Setenv("CONTAINER_KUBELET_PATH", kubeletPath)
	dm := mounter.NewDaemonsetMounter(client, nodeName, mpmounter.NewWithMount(fakeMounter), mountSyscall)
	err = dm.DiscoverCommDir(ctx)
	assert.NoError(t, err)

	tc.dm = dm
	return tc
}

func (tc *dmTestCtx) receiveMountOptions(target string) mountoptions.Options {
	tc.t.Helper()
	sockPath := filepath.Join(tc.commDir, mounter.MountSockName)
	options, err := mountoptions.Recv(tc.ctx, sockPath)
	assert.NoError(tc.t, err)
	tc.mount.Mount("mountpoint-s3", target, "fuse", nil)
	return options
}

func TestDaemonsetMounter(t *testing.T) {
	t.Run("Mounting", func(t *testing.T) {
		t.Run("Correctly passes mount options", func(t *testing.T) {
			testCtx := setupDM(t)
			target := filepath.Join(testCtx.kubeletPath, "target")

			devNull := mountertest.OpenDevNull(t)
			testCtx.mountSyscall = func(target string, opts mpmounter.MountOptions) (int, error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err := syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
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

			got := testCtx.receiveMountOptions(target)

			err := <-mountRes
			assert.NoError(t, err)

			gotFile := os.NewFile(uintptr(got.Fd), "fd")
			t.Cleanup(func() { gotFile.Close() })
			mountertest.AssertSameFile(t, devNull, gotFile)

			for _, a := range got.Args {
				if strings.Contains(a, "read-only") {
					t.Errorf("--read-only should be removed from sent args, got: %v", got.Args)
				}
			}

			assert.Equals(t, testCtx.bucketName, got.BucketName)
			assert.Equals(t, testCtx.podUID+"-"+testCtx.volumeID, got.VolumeId)
		})

		t.Run("Does not duplicate mounts if target is already mounted", func(t *testing.T) {
			testCtx := setupDM(t)
			target := filepath.Join(testCtx.kubeletPath, "target")

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
	})

	t.Run("Unmounting", func(t *testing.T) {
		t.Run("Removes mount from target", func(t *testing.T) {
			testCtx := setupDM(t)
			target := filepath.Join(testCtx.kubeletPath, "target")

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

			testCtx.receiveMountOptions(target)
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
}
