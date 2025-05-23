package mounter_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/podmounter/mountoptions"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const (
	mountpointPodNamespace = "mount-s3"
	testK8sVersion         = "v1.28.0"
)

// testCwdMu serialises process‑wide working‑directory changes across all tests
// so they remain safe when "go test" runs packages in parallel (-p flag).
var testCwdMu sync.Mutex

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
	pvName      string
}

func setup(t *testing.T) *testCtx {
	// Set test environment variables for static credentials
	t.Setenv("AWS_ACCESS_KEY_ID", "TESTKEY123456789")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "TESTSECRET123456789ABCDEFGHIJKLMNOPQRSTUV")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	kubeletPath := t.TempDir()
	t.Setenv("KUBELET_PATH", kubeletPath)

	testCwdMu.Lock()
	oldwd, err := os.Getwd()
	assert.NoError(t, err)
	assert.NoError(t, os.Chdir(kubeletPath))
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
		testCwdMu.Unlock()
	})

	bucketName := "test-bucket"
	podUID := uuid.New().String()
	volumeID := "s3-csi-driver-volume"
	pvName := "s3-csi-driver-pv"
	targetPath := filepath.Join(
		kubeletPath,
		fmt.Sprintf("pods/%s/volumes/kubernetes.io~csi/%s/mount", podUID, pvName),
	)

	// Same behaviour as Kubernetes, see https://github.com/kubernetes/kubernetes/blob/8f8c94a04d00e59d286fe4387197bc62c6a4f374/pkg/volume/csi/csi_mounter.go#L211-L215
	err = os.MkdirAll(filepath.Dir(targetPath), 0o750)
	assert.NoError(t, err)

	// Eval symlinks on `targetPath` as `mount.NewFakeMounter` also does that and we rely on
	// `mount.List()` to compare mount points and they need to be the same.
	parentDir, err := filepath.EvalSymlinks(filepath.Dir(targetPath))
	assert.NoError(t, err)
	targetPath = filepath.Join(parentDir, filepath.Base(targetPath))

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
		pvName:      pvName,
	}

	mountSyscall := func(target string, args mountpoint.Args) (fd int, err error) {
		if testCtx.mountSyscall != nil {
			return testCtx.mountSyscall(target, args)
		}

		mount.Mount("mountpoint-s3", target, "fuse", nil)
		return int(mountertest.OpenDevNull(t).Fd()), nil
	}

	credProvider := credentialprovider.New(client.CoreV1())

	podWatcher := watcher.New(client, mountpointPodNamespace, 10*time.Second)
	stopCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
	})
	err = podWatcher.Start(stopCh)
	assert.NoError(t, err)

	podMounter, err := mounter.NewPodMounter(podWatcher, credProvider, mount, mountSyscall, testK8sVersion)
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

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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

			assertMountOptionsEqual(t, mountoptions.Options{
				BucketName: testCtx.bucketName,
				Args: []string{
					"--read-only",
					"--user-agent-prefix=" + mounter.UserAgent(credentialprovider.AuthenticationSourceDriver, testK8sVersion),
				},
				Env: envprovider.Default().List(),
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

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID: testCtx.volumeID,
				PodID:    testCtx.podUID,
			}, mountpoint.ParseArgs(nil))
			assert.NoError(t, err)
		})

		t.Run("Creates credential directory with group access", func(t *testing.T) {
			testCtx := setup(t)

			args := mountpoint.ParseArgs([]string{mountpoint.ArgReadOnly})
			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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

		t.Run("success: driver environment s3 endpoint url", func(t *testing.T) {
			testCtx := setup(t)

			// Set AWS_ENDPOINT_URL in the environment
			t.Setenv("AWS_ENDPOINT_URL", "https://s3.example.com:8000")

			devNull := mountertest.OpenDevNull(t)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err = syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			args := mountpoint.ParseArgs([]string{
				mountpoint.ArgReadOnly,
			})

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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

			// Verify AWS_ENDPOINT_URL environment variable is passed to the pod
			endpointPassed := false
			for _, env := range got.Env {
				if env == "AWS_ENDPOINT_URL=https://s3.example.com:8000" {
					endpointPassed = true
					break
				}
			}

			if !endpointPassed {
				t.Fatal("Driver level AWS_ENDPOINT_URL should be passed to mountpoint-s3 pod")
			}
		})

		t.Run("Always removes endpoint URL from mount options for security", func(t *testing.T) {
			testCtx := setup(t)

			devNull := mountertest.OpenDevNull(t)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err = syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			// Explicitly include endpoint-url in the mount options
			args := mountpoint.ParseArgs([]string{
				mountpoint.ArgReadOnly,
				mountpoint.ArgEndpointURL + "=https://malicious-endpoint.example.com",
			})

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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
			got.Fd = 0

			// Verify --endpoint-url is not in the args list
			for _, arg := range got.Args {
				if strings.HasPrefix(arg, "--endpoint-url") {
					t.Fatalf("Expected --endpoint-url to be removed from args, but found: %s", arg)
				}
			}

			assertMountOptionsEqual(t, mountoptions.Options{
				BucketName: testCtx.bucketName,
				Args: []string{
					"--read-only",
					"--user-agent-prefix=" + mounter.UserAgent(credentialprovider.AuthenticationSourceDriver, testK8sVersion),
				},
				Env: envprovider.Default().List(),
			}, got)
		})

		t.Run("Security: both driver and mount options endpoint URLs - driver takes precedence", func(t *testing.T) {
			testCtx := setup(t)

			// Set AWS_ENDPOINT_URL in the environment
			t.Setenv("AWS_ENDPOINT_URL", "https://s3.example.com:8000")

			devNull := mountertest.OpenDevNull(t)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err = syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			// Explicitly include endpoint-url in the mount options
			args := mountpoint.ParseArgs([]string{
				mountpoint.ArgReadOnly,
				mountpoint.ArgEndpointURL + "=https://malicious-endpoint.example.com",
			})

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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
			got.Fd = 0

			// Verify --endpoint-url is not in the args list
			for _, arg := range got.Args {
				if strings.HasPrefix(arg, "--endpoint-url") {
					t.Fatalf("Expected --endpoint-url to be removed from args, but found: %s", arg)
				}
			}

			// Verify the trusted environment variable is passed through
			hasEndpointURL := false
			hasTrustedEndpoint := false
			for _, envVar := range got.Env {
				if strings.HasPrefix(envVar, "AWS_ENDPOINT_URL=") {
					hasEndpointURL = true
					if envVar == "AWS_ENDPOINT_URL=https://s3.example.com:8000" {
						hasTrustedEndpoint = true
					}
				}
			}
			assert.Equals(t, true, hasEndpointURL)
			assert.Equals(t, true, hasTrustedEndpoint)
		})

		t.Run("Security: endpoint URL with space separator is removed", func(t *testing.T) {
			testCtx := setup(t)

			devNull := mountertest.OpenDevNull(t)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err = syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			// Use space separator instead of equals
			args := mountpoint.ParseArgs([]string{
				mountpoint.ArgReadOnly,
				"--endpoint-url https://malicious-endpoint.example.com",
			})

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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

			// Verify endpoint-url is not in the args list regardless of format
			for _, arg := range got.Args {
				if strings.Contains(arg, "--endpoint-url") {
					t.Fatalf("Expected --endpoint-url to be removed from args, but found: %s", arg)
				}
			}
		})

		t.Run("Security: endpoint URL without -- prefix is removed", func(t *testing.T) {
			testCtx := setup(t)

			devNull := mountertest.OpenDevNull(t)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err = syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			// Without -- prefix
			args := mountpoint.ParseArgs([]string{
				mountpoint.ArgReadOnly,
				"endpoint-url=https://malicious-endpoint.example.com",
			})

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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

			// Verify endpoint-url is not in the args list regardless of format
			for _, arg := range got.Args {
				if strings.Contains(arg, "--endpoint-url") || strings.Contains(arg, "endpoint-url") {
					t.Fatalf("Expected endpoint-url to be removed from args, but found: %s", arg)
				}
			}
		})

		t.Run("Mount arg policy: strips disallowed flags", func(t *testing.T) {
			// Define test cases for each policy-disallowed flag
			testCases := []struct {
				name        string
				argToStrip  string
				description string
			}{
				{
					name:        "cache-xz",
					argToStrip:  "--cache-xz",
					description: "Express One Zone shared cache",
				},
				{
					name:        "incremental-upload",
					argToStrip:  "--incremental-upload",
					description: "Express One Zone incremental upload",
				},
				{
					name:        "storage-class",
					argToStrip:  "--storage-class=REDUCED_REDUNDANCY",
					description: "non-STANDARD storage class",
				},
				{
					name:        "profile",
					argToStrip:  "--profile=my-aws-profile",
					description: "AWS profile credentials",
				},
			}

			for _, tc := range testCases {
				t.Run(tc.name, func(t *testing.T) {
					testCtx := setup(t)

					devNull := mountertest.OpenDevNull(t)

					testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
						testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
						fd, err = syscall.Dup(int(devNull.Fd()))
						assert.NoError(t, err)
						return fd, nil
					}

					args := mountpoint.ParseArgs([]string{
						mountpoint.ArgReadOnly,
						tc.argToStrip,
					})

					mountRes := make(chan error)
					go func() {
						err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
							AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
							VolumeID:             testCtx.volumeID,
							PodID:                testCtx.podUID,
						}, args)
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

					// Verify the disallowed flag is not in the args list
					for _, arg := range got.Args {
						if strings.Contains(arg, tc.argToStrip) {
							t.Fatalf("Expected %s to be removed from args, but found: %s", tc.argToStrip, arg)
						}
					}
				})
			}
		})

		t.Run("Mount arg policy: strips multiple disallowed flags", func(t *testing.T) {
			testCtx := setup(t)

			devNull := mountertest.OpenDevNull(t)

			testCtx.mountSyscall = func(target string, args mountpoint.Args) (fd int, err error) {
				testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil)
				fd, err = syscall.Dup(int(devNull.Fd()))
				assert.NoError(t, err)
				return fd, nil
			}

			args := mountpoint.ParseArgs([]string{
				mountpoint.ArgReadOnly,
				"--cache-xz",
				"--incremental-upload",
				"--storage-class=STANDARD",
				"--profile=my-aws-profile",
			})

			mountRes := make(chan error)
			go func() {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					VolumeID:             testCtx.volumeID,
					PodID:                testCtx.podUID,
				}, args)
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

			// Verify none of the policy-disallowed options are in the args list
			for _, arg := range got.Args {
				if strings.Contains(arg, "--cache-xz") ||
					strings.Contains(arg, "--incremental-upload") ||
					strings.Contains(arg, "--storage-class") ||
					strings.Contains(arg, "--profile") {
					t.Fatalf("Expected policy-disallowed options to be removed from args, but found: %s", arg)
				}
			}
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

			for range 5 {
				err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
					VolumeID: testCtx.volumeID,
					PodID:    testCtx.podUID,
				}, mountpoint.ParseArgs(nil))
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

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID: testCtx.volumeID,
				PodID:    testCtx.podUID,
			}, mountpoint.ParseArgs(nil))
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
				err := os.WriteFile(mountErrorPath, []byte("mount failed"), 0o777)
				assert.NoError(t, err)
			}()

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID: testCtx.volumeID,
				PodID:    testCtx.podUID,
			}, mountpoint.ParseArgs(nil))
			if err == nil {
				t.Errorf("mount shouldn't succeeded if Mountpoint fails to start")
			}

			ok, err := testCtx.mount.IsMountPoint(testCtx.targetPath)
			assert.NoError(t, err)
			if ok {
				t.Errorf("it should unmount the target path if Mountpoint fails to start")
			}
		})

		t.Run("Adds a help message to see Mountpoint logs if Mountpoint Pod fails to start", func(t *testing.T) {
			testCtx := setup(t)

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
				err := os.WriteFile(mountErrorPath, []byte("mount failed"), 0o777)
				assert.NoError(t, err)
			}()

			err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
				VolumeID: testCtx.volumeID,
				PodID:    testCtx.podUID,
			}, mountpoint.ParseArgs(nil))
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

		ok, _ := testCtx.podMounter.IsMountPoint(testCtx.targetPath)
		assert.Equals(t, false, ok)

		go func() {
			mpPod := createMountpointPod(testCtx)
			mpPod.run()
			mpPod.receiveMountOptions(testCtx.ctx)
		}()

		err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
			VolumeID: testCtx.volumeID,
			PodID:    testCtx.podUID,
		}, mountpoint.ParseArgs(nil))
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

		err := testCtx.podMounter.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath, credentialprovider.ProvideContext{
			VolumeID: testCtx.volumeID,
			PodID:    testCtx.podUID,
		}, mountpoint.ParseArgs(nil))
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
			Name: mppod.MountpointPodNameFor(testCtx.podUID, testCtx.pvName),
		},
	}
	pod, err := testCtx.client.CoreV1().Pods(mountpointPodNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	assert.NoError(t, err)

	podPath := filepath.Join(testCtx.kubeletPath, "pods", string(pod.UID))
	// same with `emptyDir` volume, https://github.com/kubernetes/kubernetes/blob/8f8c94a04d00e59d286fe4387197bc62c6a4f374/pkg/volume/emptydir/empty_dir.go#L43-L48
	err = os.MkdirAll(mppod.PathOnHost(podPath), 0o777)
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

func assertMountOptionsEqual(t *testing.T, expected, actual mountoptions.Options) {
	t.Helper()

	// Check fields individually, ignoring exact env list
	assert.Equals(t, expected.BucketName, actual.BucketName)
	assert.Equals(t, expected.Fd, actual.Fd)

	// For args, ensure all expected args are in the actual args
	for _, expectedArg := range expected.Args {
		found := false
		for _, actualArg := range actual.Args {
			if actualArg == expectedArg {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected arg %q not found in actual args: %v", expectedArg, actual.Args)
		}
	}

	// For environment variables, check that we have AWS credential related vars
	containsCredentials := false
	for _, env := range actual.Env {
		if strings.Contains(env, "AWS_CONFIG_FILE") ||
			strings.Contains(env, "AWS_PROFILE") ||
			strings.Contains(env, "AWS_SHARED_CREDENTIALS_FILE") {
			containsCredentials = true
			break
		}
	}

	if !containsCredentials {
		t.Error("Expected environment variables to contain AWS credential configuration")
	}
}
