// This file implements the file permissions test suite for the S3 CSI driver,
// verifying correct application of file permission settings via mount options.
package customsuites

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// s3CSIFilePermissionsTestSuite tests file permission functionality
// for the S3 CSI driver when mounting S3 buckets.
type s3CSIFilePermissionsTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3FilePermissionsTestSuite returns a test suite for file permissions.
//
// This suite tests:
// - Default file/directory permissions (0644/0755)
// - Custom file permissions via file-mode mount option
// - Permission inheritance in subdirectories
// - Permission behavior during remount with changed options
// - Multi-pod access with different permissions
// - Permission preservation during file operations
func InitS3FilePermissionsTestSuite() storageframework.TestSuite {
	return &s3CSIFilePermissionsTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "filepermissions",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

// GetTestSuiteInfo returns test suite information.
func (t *s3CSIFilePermissionsTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

// SkipUnsupportedTests is a no-op as all tests should be supported.
func (t *s3CSIFilePermissionsTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

// DefineTests implements the test suite functionality.
func (t *s3CSIFilePermissionsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var l local

	f := framework.NewFrameworkWithCustomTimeouts("filepermissions", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelRestricted

	cleanup := func() {
		for i := range l.resources {
			resource := l.resources[i]
			func() {
				defer ginkgo.GinkgoRecover()
				ctx := context.Background()
				ginkgo.By("Deleting pv and pvc")
				err := resource.CleanupResource(ctx)
				if err != nil {
					framework.Logf("Warning: Resource cleanup had an error: %v", err)
				}
			}()
		}
	}

	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	// Test 1: Default Permissions Test
	//
	// This test verifies the default file/directory permissions when
	// no specific permission mount options are specified:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume]
	//        |
	//        ↓
	//    [S3 Bucket]
	//
	// Expected results:
	// - Files: 0644 (-rw-r--r--) permissions
	// - Directories: 0755 (drwxr-xr-x) permissions
	// - Ownership: matches specified uid/gid
	ginkgo.It("should have default permissions of 0644 for files when no mount options specified", func(ctx context.Context) {
		// Create volume with mount options required for non-root access
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access
			"debug",
		})
		l.resources = append(l.resources, resource)

		// Create a pod with the volume
		ginkgo.By("Creating pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)

		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Create a test file and directory
		volPath := "/mnt/volume1"
		testFile := fmt.Sprintf("%s/testfile.txt", volPath)
		testDir := fmt.Sprintf("%s/testdir", volPath)

		ginkgo.By("Creating a test file")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'test content' > %s", testFile))

		ginkgo.By("Creating a test directory")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", testDir))

		// Verify permissions
		ginkgo.By("Verifying file has default permissions (0644)")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", testFile))

		ginkgo.By("Verifying directory has default permissions (0755)")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", testDir))

		ginkgo.By("Verifying file ownership")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			testFile, DefaultNonRootUser, DefaultNonRootGroup))
	})

	// Test 2: Custom File Permissions Test
	//
	// This test verifies that custom file permissions are applied when
	// the file-mode mount option is specified:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with file-mode=0600]
	//        |
	//        ↓
	//    [S3 Bucket]
	//
	// Expected results:
	// - Files: 0600 (-rw-------) permissions (from file-mode option)
	// - Directories: 0755 (drwxr-xr-x) permissions (default, unaffected by file-mode)
	// - Ownership: matches specified uid/gid
	ginkgo.It("should apply custom permissions of 0600 for files when file-mode mount option specified", func(ctx context.Context) {
		// Create volume with custom file-mode mount option
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access
			"debug",
			"file-mode=0600", // Custom file permissions
		})
		l.resources = append(l.resources, resource)

		// Create a pod with the volume
		ginkgo.By("Creating pod with a volume that has file-mode=0600")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)

		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Create a test file and directory
		volPath := "/mnt/volume1"
		testFile := fmt.Sprintf("%s/testfile.txt", volPath)
		testDir := fmt.Sprintf("%s/testdir", volPath)

		ginkgo.By("Creating a test file")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'test content' > %s", testFile))

		ginkgo.By("Creating a test directory")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", testDir))

		// Verify permissions
		ginkgo.By("Verifying file has custom permissions (0600)")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^600$'", testFile))

		ginkgo.By("Verifying directory maintains default permissions (0755)")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", testDir))

		ginkgo.By("Verifying file ownership")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			testFile, DefaultNonRootUser, DefaultNonRootGroup))

		// Debug logging to display file and directory permissions
		ginkgo.By("Debug: Displaying file permissions")
		stdout, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("ls -la %s", testFile))
		framework.ExpectNoError(err, "failed to ls file: %s, stderr: %s", stdout, stderr)
		framework.Logf("File permissions: %s", stdout)

		ginkgo.By("Debug: Displaying directory permissions")
		stdout, stderr, err = e2evolume.PodExec(f, pod, fmt.Sprintf("ls -la %s", testDir))
		framework.ExpectNoError(err, "failed to ls directory: %s, stderr: %s", stdout, stderr)
		framework.Logf("Directory permissions: %s", stdout)
	})

	// Test 3: Dual Volume Permissions Test
	//
	// This test verifies that different volumes in the same pod
	// can have different file permission settings:
	//
	//      [Pod]
	//        |
	//       / \
	//      /   \
	//  [Vol 1]  [Vol 2]
	// file-mode  file-mode
	//  =0600     =0666
	//     |         |
	//     ↓         ↓
	// [S3 Bucket] [S3 Bucket]
	//
	// Expected results:
	// - Volume 1 Files: 0600 (-rw-------) permissions
	// - Volume 2 Files: 0666 (-rw-rw-rw-) permissions
	// - Directories: Always 0755 (drwxr-xr-x) permissions
	// - Ownership: matches specified uid/gid on both volumes
	ginkgo.It("should maintain distinct file permissions for multiple volumes in the same pod", func(ctx context.Context) {
		// Create first volume with file-mode=0600
		ginkgo.By("Creating first volume with file-mode=0600")
		resource1 := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access
			"debug",
			"file-mode=0600", // First volume uses 0600 permissions
		})
		l.resources = append(l.resources, resource1)

		// Create second volume with file-mode=0666
		ginkgo.By("Creating second volume with file-mode=0666")
		resource2 := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access
			"debug",
			"file-mode=0666", // Second volume uses 0666 permissions
		})
		l.resources = append(l.resources, resource2)

		// Create a pod with both volumes
		ginkgo.By("Creating pod with both volumes mounted")
		claims := []*v1.PersistentVolumeClaim{resource1.Pvc, resource2.Pvc}
		pod := e2epod.MakePod(f.Namespace.Name, nil, claims, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)

		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Define paths for both volumes
		vol1Path := "/mnt/volume1"
		vol2Path := "/mnt/volume2"
		vol1TestFile := fmt.Sprintf("%s/testfile-vol1.txt", vol1Path)
		vol2TestFile := fmt.Sprintf("%s/testfile-vol2.txt", vol2Path)
		vol1TestDir := fmt.Sprintf("%s/testdir-vol1", vol1Path)
		vol2TestDir := fmt.Sprintf("%s/testdir-vol2", vol2Path)

		// Create test files and directories in both volumes
		ginkgo.By("Creating test files and directories in both volumes")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'volume 1 content' > %s", vol1TestFile))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'volume 2 content' > %s", vol2TestFile))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", vol1TestDir))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", vol2TestDir))

		// Verify first volume file permissions (0600)
		ginkgo.By("Verifying first volume file has permissions 0600")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^600$'", vol1TestFile))

		// Verify second volume file permissions (0666)
		ginkgo.By("Verifying second volume file has permissions 0666")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^666$'", vol2TestFile))

		// Verify directory permissions remain at 0755 for both volumes
		ginkgo.By("Verifying directories in both volumes maintain default permissions (0755)")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", vol1TestDir))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", vol2TestDir))

		// Verify ownership for both volumes
		ginkgo.By("Verifying file ownership in both volumes")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			vol1TestFile, DefaultNonRootUser, DefaultNonRootGroup))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			vol2TestFile, DefaultNonRootUser, DefaultNonRootGroup))

		// Debug logging to display file and directory permissions for both volumes
		ginkgo.By("Debug: Displaying first volume file permissions")
		stdout, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("ls -la %s", vol1TestFile))
		framework.ExpectNoError(err, "failed to ls file: %s, stderr: %s", stdout, stderr)
		framework.Logf("First volume file permissions: %s", stdout)

		ginkgo.By("Debug: Displaying second volume file permissions")
		stdout, stderr, err = e2evolume.PodExec(f, pod, fmt.Sprintf("ls -la %s", vol2TestFile))
		framework.ExpectNoError(err, "failed to ls file: %s, stderr: %s", stdout, stderr)
		framework.Logf("Second volume file permissions: %s", stdout)

		ginkgo.By("Debug: Displaying first volume directory permissions")
		stdout, stderr, err = e2evolume.PodExec(f, pod, fmt.Sprintf("ls -la %s", vol1TestDir))
		framework.ExpectNoError(err, "failed to ls directory: %s, stderr: %s", stdout, stderr)
		framework.Logf("First volume directory permissions: %s", stdout)

		ginkgo.By("Debug: Displaying second volume directory permissions")
		stdout, stderr, err = e2evolume.PodExec(f, pod, fmt.Sprintf("ls -la %s", vol2TestDir))
		framework.ExpectNoError(err, "failed to ls directory: %s, stderr: %s", stdout, stderr)
		framework.Logf("Second volume directory permissions: %s", stdout)
	})

	// TODO: Implement remaining test cases:
	//
	// 4. Remounting permissions test:
	//    Change file-mode on existing volume, remount and verify
	//    - Before remount: 0644 (-rw-r--r--) file permissions
	//    - After remount: 0444 (-r--r--r--) file permissions
	//
	// 5. Subdirectory inheritance test: Let's come at
	//    Verify files in subdirectories inherit the mount option permissions
	//    - All files (at any depth): 0640 (-rw-r-----) permissions
	//    - All directories: 0755 (drwxr-xr-x) permissions
	//
	// 6. Multi-pod permissions test:
	//    Mount same bucket with different permissions in different pods
	//    - Pod 1 sees files with: 0600 (-rw-------) permissions
	//    - Pod 2 sees files with: 0644 (-rw-r--r--) permissions
	//
	// 7. File move/rename test:
	//    Verify permissions are preserved during file operations
	//    - Files maintain 0640 (-rw-r-----) permissions when moved/renamed
	//
	// 8. Security context test:
	//    Test interaction between file-mode and pod security contexts
	//    - Files: 0640 (-rw-r-----) permissions
	//    - Check ownership varies based on pod security context
}
