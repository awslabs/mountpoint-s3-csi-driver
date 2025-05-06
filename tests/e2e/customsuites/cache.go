// This file implements a cache test suite for the S3 CSI driver, providing smoke tests
// to validate that the caching functionality of the Mountpoint S3 client works properly
// when deployed in a Kubernetes environment.
//
// Note: These are basic smoke tests to verify cache integration works with the CSI driver.
// Comprehensive caching tests are already part of the upstream Mountpoint S3 project:
// https://github.com/awslabs/mountpoint-s3
package customsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"

	"github.com/scality/mountpoint-s3-csi-driver/tests/e2e/pkg/s3client"
)

const volumeName1 = "volume1"
const root = int64(0)

// s3CSICacheTestSuite defines a test suite for testing the S3 CSI driver's caching functionality.
// This test suite ensures that the cache feature of Mountpoint S3 works correctly within
// the Kubernetes environment when deployed through the CSI driver.
type s3CSICacheTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3CSICacheTestSuite initializes a test suite for S3 CSI driver's local cache functionality.
// This test suite validates that the local cache enhances performance and provides offline access
// to recently accessed data even when the S3 backend is unavailable.
//
// The tests specifically verify:
// - Basic read/write operations with caching enabled
// - Cache persistence after objects are deleted from the underlying S3 bucket
// - Cache behavior with different user contexts (root vs non-root)
// - Cache sharing between containers in the same pod
func InitS3CSICacheTestSuite() storageframework.TestSuite {
	return &s3CSICacheTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "cache",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

// GetTestSuiteInfo returns information about the test suite.
func (t *s3CSICacheTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

// SkipUnsupportedTests allows test suites to skip certain tests based on driver capabilities.
// For S3 cache tests, all tests should be supported, so this is a no-op.
func (t *s3CSICacheTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

// DefineTests defines all test cases for this test suite.
// The tests focus on validating fundamental caching behaviors:
// 1. Files can be written to and read from a cached volume
// 2. Files remain accessible in the cache even after removal from S3
// 3. Cache functionality works with different user contexts
// 4. Cache can be shared between containers in the same pod
func (t *s3CSICacheTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts("cache", storageframework.GetDriverTimeouts(driver))
	// This is required due to the cache directory creation approach that needs privileged access
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	type local struct {
		config *storageframework.PerTestConfig

		// A list of cleanup functions to be called after each test to clean resources created during the test
		cleanup []func(context.Context) error
	}

	var l local

	deferCleanup := func(f func(context.Context) error) {
		l.cleanup = append(l.cleanup, f)
	}

	cleanup := func(ctx context.Context) {
		var errs []error
		slices.Reverse(l.cleanup) // clean items in reverse order similar to how `defer` works
		for _, f := range l.cleanup {
			errs = append(errs, f(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	// checkBasicFileOperations verifies that basic file operations work properly with caching.
	// This function:
	// 1. Writes data to a file and verifies it can be read back
	// 2. Checks that multiple reads work (testing cached reads)
	// 3. Deletes the file from S3 but verifies it's still accessible via cache
	// 4. Tests directory creation and nested file operations
	// 5. Verifies file deletion works properly
	//
	// This is the core validation of the caching functionality - data should remain
	// accessible even after it's removed from the underlying S3 bucket.
	checkBasicFileOperations := func(ctx context.Context, pod *v1.Pod, bucketName, basePath string) {
		framework.Logf("Checking basic file operations inside pod %s at %s", pod.UID, basePath)

		dir := filepath.Join(basePath, "test-dir")
		first := filepath.Join(basePath, "first")
		second := filepath.Join(dir, "second")

		seed := time.Now().UTC().UnixNano()
		testWriteSize := 1024 // 1KB

		checkWriteToPath(f, pod, first, testWriteSize, seed)
		checkListingPathWithEntries(f, pod, basePath, []string{"first"})
		// Test reading multiple times to ensure cached-read works
		for i := 0; i < 3; i++ {
			checkReadFromPath(f, pod, first, testWriteSize, seed)
		}

		// Now remove the file from S3
		deleteObjectFromS3(ctx, bucketName, "first")

		// Ensure the data still read from the cache - without cache this would fail as it's removed from underlying bucket
		checkReadFromPath(f, pod, first, testWriteSize, seed)

		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir %s && cd %s && echo 'second!' > %s", dir, dir, second))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q 'second!'", second))
		checkListingPathWithEntries(f, pod, dir, []string{"second"})
		checkListingPathWithEntries(f, pod, basePath, []string{"test-dir"})
		checkDeletingPath(f, pod, first)
		checkDeletingPath(f, pod, second)
	}

	// createPod creates a pod with the specified mount options and applies any pod modifiers.
	// Returns the created pod and the associated bucket name.
	// This is a helper function used by the test cases to create pods with volumes configured
	// for cache testing.
	createPod := func(ctx context.Context, mountOptions []string, podModifiers ...func(*v1.Pod)) (*v1.Pod, string) {
		vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, mountOptions)
		deferCleanup(vol.CleanupResource)

		bucketName := bucketNameFromVolumeResource(vol)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
		for _, pm := range podModifiers {
			pm(pod)
		}

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		return pod, bucketName
	}

	// Test suite for local cache functionality.
	// These tests validate that the local caching feature of Mountpoint S3 works correctly
	// when used through the CSI driver in a Kubernetes environment.
	ginkgo.Describe("Local Cache", func() {
		// Test case: basic file operations with root user
		// This verifies cache functionality works with default root permissions.
		ginkgo.It("basic file operations as root", func(ctx context.Context) {
			cacheDir := randomCacheDir()
			mountOptions := []string{"allow-delete", fmt.Sprintf("cache %s", cacheDir)}
			podModifiers := []func(*v1.Pod){func(pod *v1.Pod) {
				ensureCacheDirExistsInNode(pod, cacheDir)
				pod.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To(root)
				pod.Spec.Containers[0].SecurityContext.RunAsGroup = ptr.To(root)
			}}

			pod, bucketName := createPod(ctx, mountOptions, podModifiers...)
			checkBasicFileOperations(ctx, pod, bucketName, e2epod.VolumeMountPath1)
		})

		// Test case: basic file operations with non-root user
		// This verifies that non-root users can also benefit from caching when
		// proper permissions are set via the mount options.
		ginkgo.It("basic file operations as non-root", func(ctx context.Context) {
			cacheDir := randomCacheDir()
			mountOptions := []string{
				"allow-delete",
				"allow-other",
				fmt.Sprintf("cache %s", cacheDir),
				fmt.Sprintf("uid=%d", DefaultNonRootUser),
				fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			}
			podModifiers := []func(*v1.Pod){
				podModifierNonRoot,
				func(pod *v1.Pod) {
					ensureCacheDirExistsInNode(pod, cacheDir)
				},
			}

			pod, bucketName := createPod(ctx, mountOptions, podModifiers...)
			checkBasicFileOperations(ctx, pod, bucketName, e2epod.VolumeMountPath1)
		})

		// Test case: cache sharing between containers
		// This verifies that the cache can be effectively shared between
		// containers within the same pod, which is important for sidecar patterns.
		ginkgo.It("two containers in the same pod using the same cache", func(ctx context.Context) {
			testFile := filepath.Join(e2epod.VolumeMountPath1, "helloworld.txt")
			cacheDir := randomCacheDir()
			mountOptions := []string{"allow-delete", fmt.Sprintf("cache %s", cacheDir)}
			podModifiers := []func(*v1.Pod){func(pod *v1.Pod) {
				ensureCacheDirExistsInNode(pod, cacheDir)
				// Make it init container to ensure it runs before regular containers
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, v1.Container{
					Name:  "populate-cache",
					Image: e2epod.GetDefaultTestImage(),
					Command: e2epod.GenerateScriptCmd(
						fmt.Sprintf("echo 'hello world!' > %s && cat %s | grep -q 'hello world!'", testFile, testFile)),
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName1,
							MountPath: e2epod.VolumeMountPath1,
						},
					},
				})
			}}

			pod, _ := createPod(ctx, mountOptions, podModifiers...)
			e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q 'hello world!'", testFile))
		})
	})
}

// checkListingPathWithEntries verifies that listing a directory shows the expected entries.
// This helper function is used to validate directory listings as part of cache testing.
func checkListingPathWithEntries(f *framework.Framework, pod *v1.Pod, path string, expectedEntries []string) {
	cmd := fmt.Sprintf("ls -1 %s", path)
	for _, entry := range expectedEntries {
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("%s | grep -q %s", cmd, entry))
	}
}

// checkDeletingPath verifies that a file can be deleted.
// This helps validate that the cache doesn't interfere with normal file deletion operations.
func checkDeletingPath(f *framework.Framework, pod *v1.Pod, path string) {
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("rm -f %s", path))
	// Check it's no longer there
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("[ ! -e %s ]", path))
}

// bucketNameFromVolumeResource extracts the bucket name from the volume resource.
// This is an implementation detail that depends on how volumes are created in the test environment.
func bucketNameFromVolumeResource(vol *storageframework.VolumeResource) string {
	// Extract the bucket name from the CSI volume attributes
	return vol.Pv.Spec.CSI.VolumeAttributes["bucketName"]
}

// deleteObjectFromS3 deletes an object from given bucket by using S3 SDK.
// This is useful to create side-effects by bypassing Mountpoint to test cache behavior
// when objects are removed from the underlying S3 storage.
func deleteObjectFromS3(ctx context.Context, bucket string, key string) {
	client := s3client.New()
	err := client.DeleteObject(ctx, bucket, key)
	framework.ExpectNoError(err)
}

// randomCacheDir returns a random directory path for cache.
// This ensures test isolation by using a unique cache directory for each test run.
func randomCacheDir() string {
	return filepath.Join("/tmp/mp-cache", uuid.New().String())
}

// ensureCacheDirExistsInNode adds a hostPath for given `cacheDir` with `DirectoryOrCreate` type.
// This hack is required because Mountpoint process is running on the underlying host and not inside the container,
// so we need to ensure cache directory exists on the host.
//
// The function:
// 1. Creates a volume that maps to the host cache directory
// 2. Adds an init container that sets proper permissions on the cache directory
// 3. Mounts the cache directory in the main container
func ensureCacheDirExistsInNode(pod *v1.Pod, cacheDir string) {
	cacheVolumeMount := v1.VolumeMount{
		Name:      "make-cache-dir",
		MountPath: "/cache",
	}

	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{}
	}
	// We need to set this false at Pod-level as `chmod-cache-dir` needs to run as `root` and this
	// would prevent container creation if its true
	pod.Spec.SecurityContext.RunAsNonRoot = ptr.To(false)

	// The directory created with `DirectoryOrCreate` will have 0755 permissions and will be owned by kubelet
	// Unless we change permissions here, non-root containers won't be able to access to the cache dir
	pod.Spec.InitContainers = append(pod.Spec.DeepCopy().InitContainers, v1.Container{
		Name:    "chmod-cache-dir",
		Image:   e2epod.GetDefaultTestImage(),
		Command: e2epod.GenerateScriptCmd("chmod -R 777 /cache"),
		SecurityContext: &v1.SecurityContext{
			RunAsUser:  ptr.To(root),
			RunAsGroup: ptr.To(root),
		},
		VolumeMounts: []v1.VolumeMount{cacheVolumeMount},
	})
	pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
		Name: "make-cache-dir",
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: cacheDir,
				Type: ptr.To(v1.HostPathDirectoryOrCreate),
			},
		},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, cacheVolumeMount)
}
