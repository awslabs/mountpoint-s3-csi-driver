package mounter_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider/credentialprovidertest"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

const mountpointPodNamespace = "mount-s3"

type testCtx struct {
	t   *testing.T
	ctx context.Context

	podMounter *mounter.PodMounter

	client       *fake.Clientset
	mount        *mount.FakeMounter
	mountSyscall func(target string, args mountpoint.Args) (fd int, err error)

	bucketName  string
	kubeletPath string
	targetPath  string
	podUID      string
	volumeID    string
}

func setup(t *testing.T) *testCtx {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	kubeletPath := t.TempDir()
	t.Setenv("KUBELET_PATH", kubeletPath)

	bucketName := "test-bucket"
	podUID := uuid.New().String()
	volumeID := "s3-csi-driver-volume"
	targetPath := filepath.Join(
		kubeletPath,
		fmt.Sprintf("pods/%s/volumes/kubernetes.io~csi/%s/mount", podUID, volumeID),
	)

	// Same behaviour as Kubernetes, see https://github.com/kubernetes/kubernetes/blob/8f8c94a04d00e59d286fe4387197bc62c6a4f374/pkg/volume/csi/csi_mounter.go#L211-L215
	err := os.MkdirAll(filepath.Dir(targetPath), 0750)
	assert.NoError(t, err)

	client := fake.NewClientset()
	mount := mount.NewFakeMounter(nil)

	testCtx := &testCtx{
		t:           t,
		ctx:         ctx,
		client:      client,
		mount:       mount,
		bucketName:  bucketName,
		kubeletPath: kubeletPath,
		targetPath:  targetPath,
		podUID:      podUID,
		volumeID:    volumeID,
	}

	mountSyscall := func(target string, args mountpoint.Args) (fd int, err error) {
		if testCtx.mountSyscall != nil {
			return testCtx.mountSyscall(target, args)
		}

		mount.Mount("mountpoint-s3", target, "fuse", nil)
		return int(mountertest.OpenDevNull(t).Fd()), nil
	}

	podMounter, err := mounter.NewPodMounter(client.CoreV1(), mountpointPodNamespace, mount, mountSyscall)
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

			args := mountpoint.ParseArgs([]string{mountpoint.ArgReadOnly})
			env := envprovider.Environment{envprovider.Format(envprovider.EnvRegion, "us-east-1")}

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, &credentialprovidertest.DummyCredentials{}, env, args)
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
			got.Fd = 0

			assert.Equals(t, mountoptions.Options{
				BucketName: testCtx.bucketName,
				Args:       []string{"--read-only"},
				Env:        []string{"AWS_REGION=us-east-1"},
			}, got)
		})

		t.Run("Waits for Mountpoint Pod", func(t *testing.T) {
			testCtx := setup(t)

			go func() {
				// Add delays to each Mountpoint Pod step
				time.Sleep(100 * time.Millisecond)
				mpPod := createMountpointPod(testCtx)
				time.Sleep(100 * time.Millisecond)
				mpPod.run()
				time.Sleep(100 * time.Millisecond)
				mpPod.receiveMountOptions(testCtx.ctx)
			}()

			err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, &credentialprovidertest.DummyCredentials{}, nil, mountpoint.ParseArgs(nil))
			assert.NoError(t, err)
		})

		t.Run("Dumps credentials to Mountpoint Pod's volume and passes environment variables", func(t *testing.T) {
			testCtx := setup(t)

			env := envprovider.Environment{envprovider.Format(envprovider.EnvRegion, "us-east-1")}
			args := mountpoint.ParseArgs([]string{mountpoint.ArgAllowRoot})
			dummyCredentialsFilepath := "dummy-credentials"

			credentials := &credentialprovidertest.DummyCredentials{
				DumpFn: func(writePath string, envPath string) (envprovider.Environment, error) {
					_, err := os.Create(filepath.Join(writePath, dummyCredentialsFilepath))
					assert.NoError(t, err)

					return envprovider.Environment{envprovider.Format("DUMMY_CREDENTIALS", filepath.Join(envPath, dummyCredentialsFilepath))}, nil
				},
			}

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, credentials, env, args)
				if err != nil {
					log.Println("Mount failed", err)
				}
				mountRes <- err
			}()

			mpPod := createMountpointPod(testCtx)
			mpPod.run()
			options := mpPod.receiveMountOptions(testCtx.ctx)

			err := <-mountRes
			assert.NoError(t, err)

			assert.Equals(t, []string{"--allow-root"}, options.Args)
			assert.Equals(t, envprovider.Environment{
				envprovider.Format(envprovider.EnvRegion, "us-east-1"),
				envprovider.Format("DUMMY_CREDENTIALS", mppod.PathInsideMountpointPod(mppod.KnownPathCredentials, dummyCredentialsFilepath)),
			}, options.Env)

			_, err = os.Stat(mppod.PathOnHost(mpPod.podPath, mppod.KnownPathCredentials, dummyCredentialsFilepath))
			assert.NoError(t, err)
		})

		t.Run("Does not duplicate mounts if target is already mounted", func(t *testing.T) {
			testCtx := setup(t)

			var mountCount atomic.Int32

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				mountCount.Add(1)
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				return int(mountertest.OpenDevNull(t).Fd()), nil
			}

			go func() {
				mpPod := createMountpointPod(testCtx)
				mpPod.run()
				mpPod.receiveMountOptions(testCtx.ctx)
			}()

			for i := 0; i < 5; i++ {
				err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, &credentialprovidertest.DummyCredentials{}, nil, mountpoint.ParseArgs(nil))
				assert.NoError(t, err)
			}

			assert.Equals(t, int32(1), mountCount.Load())
		})

		t.Run("Unmounts target if Mountpoint Pod does not receive mount options", func(t *testing.T) {
			testCtx := setup(t)

			go func() {
				mpPod := createMountpointPod(testCtx)
				mpPod.run()

				// Create the `mount.sock` but does not receive anything from it
				mountSock := mppod.PathOnHost(mpPod.podPath, mppod.KnownPathMountSock)
				_, err := os.Create(mountSock)
				assert.NoError(t, err)
			}()

			err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, &credentialprovidertest.DummyCredentials{}, nil, mountpoint.ParseArgs(nil))
			if err == nil {
				t.Errorf("mount shouldn't succeeded if Mountpoint does not receive the mount options")
			}

			ok, err := testCtx.mount.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should unmount the target path if Mountpoint does not receive the mount options")
			}
		})

		t.Run("Unmounts target if Mountpoint Pod fails to start", func(t *testing.T) {
			testCtx := setup(t)

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

			err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, &credentialprovidertest.DummyCredentials{}, nil, mountpoint.ParseArgs(nil))
			if err == nil {
				t.Errorf("mount shouldn't succeeded if Mountpoint fails to start")
			}

			ok, err := testCtx.mount.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should unmount the target path if Mountpoint fails to start")
			}
		})
	})

	t.Run("Checking if its mount point", func(t *testing.T) {
		testCtx := setup(t)

		ok, _ := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.Equals(t, false, ok)

		go func() {
			mpPod := createMountpointPod(testCtx)
			mpPod.run()
			mpPod.receiveMountOptions(testCtx.ctx)
		}()

		err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, &credentialprovidertest.DummyCredentials{}, nil, mountpoint.ParseArgs(nil))
		assert.NoError(t, err)

		ok, err = testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.NoError(t, err)
		assert.Equals(t, true, ok)
	})

	t.Run("Unmounting", func(t *testing.T) {
		testCtx := setup(t)

		go func() {
			mpPod := createMountpointPod(testCtx)
			mpPod.run()
			mpPod.receiveMountOptions(testCtx.ctx)
		}()

		err := testCtx.podMounter.Mount(testCtx.bucketName, testCtx.targetPath, &credentialprovidertest.DummyCredentials{}, nil, mountpoint.ParseArgs(nil))
		assert.NoError(t, err)

		ok, err := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.NoError(t, err)
		assert.Equals(t, true, ok)

		err = testCtx.podMounter.Unmount(testCtx.targetPath)
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
			UID:  types.UID(uuid.New().String()),
			Name: mppod.MountpointPodNameFor(testCtx.podUID, testCtx.volumeID),
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

func (mp *mountpointPod) receiveMountOptions(ctx context.Context) mountoptions.Options {
	mp.testCtx.t.Helper()
	mountSock := mppod.PathOnHost(mp.podPath, mppod.KnownPathMountSock)
	options, err := mountoptions.Recv(ctx, mountSock)
	assert.NoError(mp.testCtx.t, err)
	return options
}
