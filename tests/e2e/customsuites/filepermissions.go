// This file implements the file permissions test suite for the S3 CSI driver,
// verifying correct application of file permission settings via mount options.
package customsuites

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
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

	// Helper functions for permission verification to reduce code duplication

	// verifyFilePermissions checks if a file has the expected permissions
	// and optionally verifies ownership if uid and gid are specified
	verifyFilePermissions := func(f *framework.Framework, pod *v1.Pod, filePath string, expectedMode string, uid, gid *int64) {
		ginkgo.By(fmt.Sprintf("Verifying file has %s permissions", expectedMode))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^%s$'", filePath, expectedMode))

		if uid != nil && gid != nil {
			ginkgo.By("Verifying file ownership")
			e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
				filePath, *uid, *gid))
		}
	}

	// verifyDirPermissions checks if a directory has the expected permissions
	// and optionally verifies ownership if uid and gid are specified
	verifyDirPermissions := func(f *framework.Framework, pod *v1.Pod, dirPath string, expectedMode string, uid, gid *int64) {
		ginkgo.By(fmt.Sprintf("Verifying directory has %s permissions", expectedMode))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^%s$'", dirPath, expectedMode))

		if uid != nil && gid != nil {
			ginkgo.By("Verifying directory ownership")
			e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
				dirPath, *uid, *gid))
		}
	}

	// verifyPermissions checks permissions and ownership for both a file and directory
	// This combines file and directory permission checking into a single function call
	verifyPermissions := func(f *framework.Framework, pod *v1.Pod, filePath, dirPath, expectedFileMode, expectedDirMode string, uid, gid *int64) {
		verifyFilePermissions(f, pod, filePath, expectedFileMode, uid, gid)
		verifyDirPermissions(f, pod, dirPath, expectedDirMode, uid, gid)
	}

	// verifyPathsPermissions verifies permissions for multiple files and directories
	// filePaths is a slice of file paths to check
	// dirPaths is a slice of directory paths to check
	// expectedFileMode is the expected permission mode for all files
	// expectedDirMode is the expected permission mode for all directories
	verifyPathsPermissions := func(f *framework.Framework, pod *v1.Pod, filePaths, dirPaths []string,
		expectedFileMode, expectedDirMode string, uid, gid *int64) {

		// Check file permissions
		for _, filePath := range filePaths {
			verifyFilePermissions(f, pod, filePath, expectedFileMode, uid, gid)
		}

		// Check directory permissions
		for _, dirPath := range dirPaths {
			verifyDirPermissions(f, pod, dirPath, expectedDirMode, uid, gid)
		}
	}

	// createTestFileAndDir creates a test file and directory at the given base path
	// and returns their paths
	createTestFileAndDir := func(f *framework.Framework, pod *v1.Pod, basePath string, fileNamePrefix string) (string, string) {
		testFile := fmt.Sprintf("%s/%s.txt", basePath, fileNamePrefix)
		testDir := fmt.Sprintf("%s/%s-dir", basePath, fileNamePrefix)

		ginkgo.By(fmt.Sprintf("Creating test file %s", testFile))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'test content' > %s", testFile))

		ginkgo.By(fmt.Sprintf("Creating test directory %s", testDir))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", testDir))

		return testFile, testDir
	}

	// createVolumeResourceWithMountOptions creates a volume resource with specified mount options.
	// This function extends the standard Kubernetes storage framework volume creation by:
	// 1. Creating an S3 bucket via the driver's CreateVolume method
	// 2. Setting up a PV with the specified mount options (crucial for S3 CSI testing)
	// 3. Creating a matching PVC that binds to this PV
	// 4. Waiting for the binding to complete
	//
	// The mount options parameter is key for testing various S3-specific mount behaviors,
	// allowing tests to validate permissions, caching, and other mount parameters.
	//
	// This function is a critical extension point as the standard storage framework
	// does not support mount options out of the box.
	createVolumeWithOptions := func(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern,
		uid, gid int64, fileModeOption string, extraOptions ...string) *storageframework.VolumeResource {

		// Start with required options
		options := []string{
			fmt.Sprintf("uid=%d", uid),
			fmt.Sprintf("gid=%d", gid),
			"allow-other", // Required for non-root access
		}

		// Add file mode if specified
		if fileModeOption != "" {
			options = append(options, fmt.Sprintf("file-mode=%s", fileModeOption))
		}

		// Add any extra options
		options = append(options, extraOptions...)

		resource := createVolumeResourceWithMountOptions(ctx, config, pattern, options)
		l.resources = append(l.resources, resource)
		return resource
	}

	// createPodWithVolume creates a pod with the specified volume and applies non-root security context
	createPodWithVolume := func(ctx context.Context, f *framework.Framework, volume *v1.PersistentVolumeClaim,
		containerName string, uid, gid int64) (*v1.Pod, error) {

		// Create pod spec
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{volume}, admissionapi.LevelRestricted, "")

		// Apply security context
		if pod.Spec.SecurityContext == nil {
			pod.Spec.SecurityContext = &v1.PodSecurityContext{}
		}
		pod.Spec.SecurityContext.RunAsUser = ptr.To(uid)
		pod.Spec.SecurityContext.RunAsGroup = ptr.To(gid)
		pod.Spec.SecurityContext.RunAsNonRoot = ptr.To(true)

		// Set container name if specified
		if containerName != "" {
			pod.Spec.Containers[0].Name = containerName
		}

		// Create the pod
		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		if err != nil {
			return nil, err
		}

		return pod, nil
	}

	// setupTestPaths creates a nested directory structure for testing
	// Returns a map containing paths for the created directories and files
	setupTestPaths := func(f *framework.Framework, pod *v1.Pod, volumePath string) map[string]string {
		paths := make(map[string]string)

		// Define paths
		paths["volPath"] = volumePath
		paths["subdir1"] = fmt.Sprintf("%s/subdir1", volumePath)
		paths["subdir2"] = fmt.Sprintf("%s/subdir2", volumePath)
		paths["subdir3"] = fmt.Sprintf("%s/subdir1/subdir3", volumePath)
		paths["rootFile"] = fmt.Sprintf("%s/root.txt", volumePath)
		paths["file1"] = fmt.Sprintf("%s/file1.txt", paths["subdir1"])
		paths["file2"] = fmt.Sprintf("%s/file2.txt", paths["subdir2"])
		paths["file3"] = fmt.Sprintf("%s/file3.txt", paths["subdir3"])

		// Create directories
		ginkgo.By("Creating nested directory structure")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s %s %s",
			paths["subdir1"], paths["subdir2"], paths["subdir3"]))

		// Create files
		ginkgo.By("Creating files at different directory levels")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'root' > %s", paths["rootFile"]))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'level1' > %s", paths["file1"]))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'level2' > %s", paths["file2"]))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'level3' > %s", paths["file3"]))

		return paths
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
		resource := createVolumeWithOptions(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "")

		// Create a pod with the volume
		ginkgo.By("Creating pod with a volume")
		pod, err := createPodWithVolume(ctx, f, resource.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)

		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Create a test file and directory
		volPath := "/mnt/volume1"
		testFile, testDir := createTestFileAndDir(f, pod, volPath, "testfile")

		// Convert the UID/GID constants to pointers for the verification functions.
		// This is necessary because verifyFilePermissions and verifyDirPermissions
		// accept pointer parameters to support optional ownership verification.
		uidPtr := ptr.To(DefaultNonRootUser)
		gidPtr := ptr.To(DefaultNonRootGroup)

		verifyPermissions(f, pod, testFile, testDir, "644", "755", uidPtr, gidPtr)
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
		resource := createVolumeWithOptions(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0600")

		// Create a pod with the volume
		ginkgo.By("Creating pod with a volume that has file-mode=0600")
		pod, err := createPodWithVolume(ctx, f, resource.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)

		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Create a test file and directory
		volPath := "/mnt/volume1"
		testFile, testDir := createTestFileAndDir(f, pod, volPath, "testfile")

		// Convert the UID/GID constants to pointers for the verification functions.
		// This is necessary because verifyFilePermissions and verifyDirPermissions
		// accept pointer parameters to support optional ownership verification.
		uidPtr := ptr.To(DefaultNonRootUser)
		gidPtr := ptr.To(DefaultNonRootGroup)

		verifyPermissions(f, pod, testFile, testDir, "600", "755", uidPtr, gidPtr)
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
		resource1 := createVolumeWithOptions(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0600")

		// Create second volume with file-mode=0666
		ginkgo.By("Creating second volume with file-mode=0666")
		resource2 := createVolumeWithOptions(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0666")

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
	})

	// Test 4: Remounting Permissions Test
	//
	// This test verifies that changing file permission mount options
	// and remounting a volume applies the new settings:
	//
	//      [Pod 1]                 [Pod 2]
	//        |                       |
	//        ↓                       ↓
	//   [S3 Volume]  →  1. Delete Pod 1  →  [S3 Volume]
	//   Default perms    2. Update PV        file-mode=0444
	//        |              mount options        |
	//        ↓                                   ↓
	//    [S3 Bucket] ──────── Same Bucket ──→ [S3 Bucket]
	//
	// Expected results:
	// - Initial files: 0644 (-rw-r--r--) permissions (default)
	// - After remount: 0444 (-r--r--r--) permissions (read-only)
	// - Directories: Always 0755 (drwxr-xr-x) permissions
	// - Ownership: matches specified uid/gid in both cases
	ginkgo.It("should update file permissions when a volume is remounted with new options", func(ctx context.Context) {
		// Step 1: Create initial volume with default permissions
		ginkgo.By("Creating volume with default permissions")
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access

		})
		l.resources = append(l.resources, resource)

		// Step 2: Create first pod with the volume
		ginkgo.By("Creating first pod with volume using default permissions")
		pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod1)
		// Set container name explicitly
		pod1.Spec.Containers[0].Name = "write-pod"

		var err error
		pod1, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1))
		}()

		// Create a test file and directory
		volPath := "/mnt/volume1"
		testFile := fmt.Sprintf("%s/testfile.txt", volPath)
		testDir := fmt.Sprintf("%s/testdir", volPath)

		ginkgo.By("Creating a test file with default permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("echo 'test content' > %s", testFile))

		ginkgo.By("Creating a test directory")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("mkdir -p %s", testDir))

		// Verify initial permissions
		ginkgo.By("Verifying file has default permissions (0644)")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", testFile))

		ginkgo.By("Verifying directory has default permissions (0755)")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", testDir))

		// Step 3: Delete the pod
		ginkgo.By("Deleting the first pod")
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1))

		// Step 4: Update the PV to use file-mode=0444
		ginkgo.By("Updating volume to use file-mode=0444")

		// Get the PV object
		pv, err := f.ClientSet.CoreV1().PersistentVolumes().Get(ctx, resource.Pv.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get PV")

		// Update the mount options to include file-mode=0444
		newMountOptions := []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access

			"file-mode=0444", // Add read-only file permissions
		}
		pv.Spec.MountOptions = newMountOptions

		// Update the PV
		_, err = f.ClientSet.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "failed to update PV with new mount options")

		// Step 5: Create a new pod with the updated volume
		ginkgo.By("Creating second pod with updated volume permissions")
		pod2 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod2)
		// Set container name explicitly
		pod2.Spec.Containers[0].Name = "read-pod"

		pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2))
		}()

		// Step 6: Verify new permissions
		ginkgo.By("Verifying file now has read-only permissions (0444)")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("stat -c '%%a' %s | grep -q '^444$'", testFile))

		// Creating a new test directory in the second pod since it doesn't persist between pods
		ginkgo.By("Creating a new test directory in the second pod")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("mkdir -p %s", testDir))

		ginkgo.By("Verifying directory still has default permissions (0755)")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", testDir))

		ginkgo.By("Verifying file ownership is maintained")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			testFile, DefaultNonRootUser, DefaultNonRootGroup))

		// Try to write to the file (should fail with read-only permissions)
		ginkgo.By("Verifying file is now read-only")
		_, _, err = e2evolume.PodExec(f, pod2, fmt.Sprintf("echo 'new content' > %s", testFile))
		if err == nil {
			framework.Failf("Was able to write to a read-only file!")
		}
		framework.Logf("As expected, writing to read-only file failed")
	})

	// Test 5: Concurrent Mount Permissions Test
	//
	// This test verifies that pods already mounting a volume see the original
	// permissions, while new pods mounting after an update see new permissions:
	//
	//      [Pod 1] ────────────────────────────────── [Pod 1]
	//        |          Continue running                 |
	//        ↓                                           |
	//   [S3 Volume]  →  1. Update PV mount options  →  [S3 Volume]
	//   Default perms    without deleting Pod 1       file-mode=0444
	//        |                                           ↑
	//        ↓                                           |
	//    [S3 Bucket] ── Same bucket with updated PV ─ [Pod 2]
	//
	// Expected results:
	// - Pod 1 continues to see files with original 0644 (-rw-r--r--) permissions
	// - Pod 2 sees files with updated 0444 (-r--r--r--) permissions
	// - New files created by Pod 1 have 0644 permissions (seen as 0444 by Pod 2)
	// - New files created by Pod 2 have 0444 permissions (seen as 0644 by Pod 1)
	ginkgo.It("should maintain different file permissions in concurrent pods with updated mount options", func(ctx context.Context) {
		// Step 1: Create initial volume with default permissions
		ginkgo.By("Creating volume with default permissions")
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access

		})
		l.resources = append(l.resources, resource)

		// Step 2: Create first pod with the volume
		ginkgo.By("Creating first pod with volume using default permissions")
		pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod1)
		// Set container name explicitly
		pod1.Spec.Containers[0].Name = "write-pod"

		var err error
		pod1, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1))
		}()

		// Create a test file and directory
		volPath := "/mnt/volume1"
		testFile := fmt.Sprintf("%s/testfile.txt", volPath)
		testDir := fmt.Sprintf("%s/testdir", volPath)

		ginkgo.By("Creating a test file with default permissions from pod1")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("echo 'test content from pod1' > %s", testFile))

		ginkgo.By("Creating a test directory from pod1")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("mkdir -p %s", testDir))

		// Verify initial permissions
		ginkgo.By("Verifying file has default permissions (0644) in pod1")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", testFile))

		ginkgo.By("Verifying directory has default permissions (0755) in pod1")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", testDir))

		// Step 3: Update the PV to use file-mode=0444 without deleting the first pod
		ginkgo.By("Updating volume to use file-mode=0444 without deleting the first pod")

		// Get the PV object
		pv, err := f.ClientSet.CoreV1().PersistentVolumes().Get(ctx, resource.Pv.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get PV")

		// Update the mount options to include file-mode=0444
		newMountOptions := []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other", // Required for non-root access

			"file-mode=0444", // Add read-only file permissions
		}
		pv.Spec.MountOptions = newMountOptions

		// Update the PV
		_, err = f.ClientSet.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "failed to update PV with new mount options")

		// Step 4: Create a second pod that mounts the same volume with updated mount options
		ginkgo.By("Creating second pod with the same volume using updated permissions")
		pod2 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod2)
		// Set container name explicitly
		pod2.Spec.Containers[0].Name = "read-pod"

		pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2))
		}()

		// Step 5: Verify that pod1 still sees the original permissions
		ginkgo.By("Verifying pod1 still sees file with original permissions (0644)")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", testFile))

		// Step 6: Verify that pod2 sees the new permissions
		ginkgo.By("Verifying pod2 sees file with updated permissions (0444)")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("stat -c '%%a' %s | grep -q '^444$'", testFile))

		// Step 7: Create new files from both pods
		pod1File := fmt.Sprintf("%s/pod1file.txt", volPath)
		pod2File := fmt.Sprintf("%s/pod2file.txt", volPath)

		ginkgo.By("Creating a new file from pod1")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("echo 'content from pod1' > %s", pod1File))

		ginkgo.By("Creating a new file from pod2")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("echo 'content from pod2' > %s", pod2File))

		// Step 8: Verify permissions for the new files as seen from each pod
		ginkgo.By("Verifying pod1 sees its file with original permissions (0644)")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", pod1File))

		ginkgo.By("Verifying pod1 sees pod2's file with original permissions (0644)")
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", pod2File))

		ginkgo.By("Verifying pod2 sees its file with updated permissions (0444)")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("stat -c '%%a' %s | grep -q '^444$'", pod2File))

		ginkgo.By("Verifying pod2 sees pod1's file with updated permissions (0444)")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("stat -c '%%a' %s | grep -q '^444$'", pod1File))
	})

	// Test 6: Subdirectory Inheritance Test
	//
	// This test verifies that files in subdirectories inherit the
	// specified file mode mount option:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with file-mode=0640]
	//        |
	//        ↓
	//   [Root Directory]
	//      /    \
	//     /      \
	//  [subdir1] [subdir2]
	//     |          \
	//     ↓           ↓
	//  [subdir1/    [subdir2/
	//   subdir3]     file2.txt]
	//     |
	//     ↓
	//  [subdir1/
	//   subdir3/
	//   file3.txt]
	//
	// Expected results:
	// - All files at all levels have 0640 (-rw-r-----) permissions
	// - All directories maintain 0755 (drwxr-xr-x) permissions
	ginkgo.It("should apply the same file permissions to files in subdirectories", func(ctx context.Context) {
		// Step 1: Create volume with custom file-mode=0640 mount option
		ginkgo.By("Creating volume with file-mode=0640 and additional operations permissions")
		resource := createVolumeWithOptions(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0640",
			"allow-delete", "allow-overwrite")

		// Step 2: Create a pod with the volume
		ginkgo.By("Creating pod with the volume")
		pod, err := createPodWithVolume(ctx, f, resource.Pvc, "write-pod", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Step 3: Create nested directory structure and test files
		volPath := "/mnt/volume1"
		paths := setupTestPaths(f, pod, volPath)

		// Step 4: Verify file permissions across all levels using the helper function
		ginkgo.By("Verifying all files have 0640 permissions")
		filePaths := []string{
			paths["rootFile"],
			paths["file1"],
			paths["file2"],
			paths["file3"],
		}

		dirPaths := []string{
			paths["volPath"],
			paths["subdir1"],
			paths["subdir2"],
			paths["subdir3"],
		}

		uidPtr := ptr.To(DefaultNonRootUser)
		gidPtr := ptr.To(DefaultNonRootGroup)

		verifyPathsPermissions(f, pod, filePaths, dirPaths, "640", "755", uidPtr, gidPtr)
	})

	// Test 8: File Copy/Delete Permissions Test
	//
	// This test verifies that file permissions are preserved
	// when files are copied between directories in S3 volumes:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with file-mode=0640]
	//        |
	//       / \
	//      /   \
	// [Dir1]   [Dir2]
	//   |         ↑
	//   |         |
	//  [File] -> Copy -> [File]
	//
	// Expected results:
	// - Initial file has 0640 (-rw-r-----) permissions
	// - File maintains 0640 permissions after being copied
	// - File ownership remains consistent throughout operations
	ginkgo.It("should preserve file permissions during copy operations", func(ctx context.Context) {
		// Step 1: Create volume with custom file-mode=0640 mount option
		ginkgo.By("Creating volume with file-mode=0640 and additional operations permissions")
		resource := createVolumeWithOptions(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0640",
			"allow-delete", "allow-overwrite")

		// Step 2: Create a pod with the volume
		ginkgo.By("Creating pod with the volume")
		pod, err := createPodWithVolume(ctx, f, resource.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Step 3: Create directories for testing file operations
		volPath := "/mnt/volume1"
		sourceDir := fmt.Sprintf("%s/source-dir", volPath)
		targetDir := fmt.Sprintf("%s/target-dir", volPath)

		ginkgo.By("Creating source and target directories")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s %s", sourceDir, targetDir))

		// Step 4: Create a test file in the source directory
		sourceFile := fmt.Sprintf("%s/test-file.txt", sourceDir)
		ginkgo.By("Creating a test file in the source directory")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'test content' > %s", sourceFile))

		// Step 5: Verify initial file permissions
		ginkgo.By("Verifying initial file has 0640 permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^640$'", sourceFile))

		// Step 6: Copy the file to the target directory
		targetFile := fmt.Sprintf("%s/copied-file.txt", targetDir)
		ginkgo.By("Copying file to target directory")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cp %s %s", sourceFile, targetFile))

		// Step 7: Verify permissions after copy
		ginkgo.By("Verifying copied file maintains 0640 permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^640$'", targetFile))

		// Step 8: Create another file with a different name in source directory
		// Move (mv) is not supported by mountpoint-S3, so we are using copy+delete to simulate it.
		newSourceFile := fmt.Sprintf("%s/another-test-file.txt", sourceDir)
		ginkgo.By("Creating another test file for rename simulation")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'content for rename test' > %s", newSourceFile))

		// Step 9: Copy the file to target directory with a different name (simulating rename)
		renamedFile := fmt.Sprintf("%s/renamed-file.txt", targetDir)
		ginkgo.By("Copying file to target directory with new name (simulating rename)")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cp %s %s", newSourceFile, renamedFile))

		// Step 10: Delete the source file (completing the rename simulation)
		ginkgo.By("Deleting source file to complete rename simulation")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("rm %s", newSourceFile))

		// Step 11: Verify permissions after simulated rename
		ginkgo.By("Verifying renamed file maintains 0640 permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^640$'", renamedFile))

		// Step 12: Verify ownership remains consistent
		ginkgo.By("Verifying file ownership is maintained throughout operations")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			renamedFile, DefaultNonRootUser, DefaultNonRootGroup))

		// Step 13: Compare permissions between original and copied files
		ginkgo.By("Comparing permissions between source and copied files")
		sourcePerms, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("stat -c '%%a' %s", sourceFile))
		framework.ExpectNoError(err, "failed to get source permissions: %s", stderr)

		copyPerms, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("stat -c '%%a' %s", targetFile))
		framework.ExpectNoError(err, "failed to get copied permissions: %s", stderr)

		if sourcePerms != copyPerms {
			framework.Failf("Permission mismatch after copy: source=%s, copy=%s", sourcePerms, copyPerms)
		}
	})

	// Test 9: Pod Security Context Test
	// This test verifies how pod security contexts interact
	// with the S3 CSI driver file permissions:
	//
	//	   [Pod with SecurityContext]
	//	     |    runAsUser: 3000
	//	     |    fsGroup: 4000
	//	     |
	//	     ↓
	//	[S3 Volume with file-mode=0640]
	//	     |
	//	     ↓
	//	[Files & Directories]
	//
	// Expected results:
	// - Files have the specified file mode (0640) regardless of security context
	// - File ownership is affected by the pod security context settings
	// - Pod's runAsUser determines the user ownership of created files
	// - Pod's fsGroup determines the group ownership of created files
	ginkgo.It("should properly apply permissions with pod security context settings", func(ctx context.Context) {
		// Define specific security context settings for the pod
		customUID := int64(3000)
		customGID := int64(4000)
		runAsNonRoot := true

		// Step 1: Create volume with custom file-mode=0640 mount option
		// Use the same UID/GID in mount options as in the security context
		ginkgo.By("Creating volume with file-mode=0640")
		resource := createVolumeWithOptions(ctx, l.config, pattern, customUID, customGID, "0640")

		// Step 2: Create a pod with specific security context settings
		ginkgo.By("Creating pod with specific runAsUser and fsGroup security context")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")

		// Set the pod's security context to use specific user and group IDs
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			RunAsUser:    &customUID,
			FSGroup:      &customGID,
			RunAsNonRoot: &runAsNonRoot,
			SeccompProfile: &v1.SeccompProfile{
				Type: v1.SeccompProfileTypeRuntimeDefault,
			},
		}

		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Step 3: Create test files in the volume
		volPath := "/mnt/volume1"
		testFile := fmt.Sprintf("%s/test-file.txt", volPath)
		testDir := fmt.Sprintf("%s/test-dir", volPath)

		ginkgo.By("Creating test file and directory from the pod")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'test content' > %s", testFile))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", testDir))

		// Step 4: Verify file permissions match the specified file-mode
		ginkgo.By("Verifying file has the specified file-mode=0640 permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^640$'", testFile))

		// Step 5: Verify file ownership corresponds to the pod's security context
		ginkgo.By("Verifying file ownership matches pod's security context")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			testFile, customUID, customGID))

		// Step 6: Verify directory permissions remain at default 0755
		ginkgo.By("Verifying directory has default permissions (0755)")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^755$'", testDir))

		// Step 7: Verify directory ownership corresponds to the pod's security context
		ginkgo.By("Verifying directory ownership matches pod's security context")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'",
			testDir, customUID, customGID))

		// Step 8: Create a file with specific permissions using chmod (to verify interaction)
		explicitFile := fmt.Sprintf("%s/explicit-perm-file.txt", volPath)
		ginkgo.By("Creating a file with explicitly set permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'explicit perm test' > %s", explicitFile))

		// Try to change permissions (this is expected to fail with S3 CSI driver)
		ginkgo.By("Verifying chmod operation is not permitted (expected behavior)")
		_, _, err = e2evolume.PodExec(f, pod, fmt.Sprintf("chmod 600 %s", explicitFile))
		if err == nil {
			framework.Failf("Expected chmod to fail, but it succeeded")
		}

		// Step 9: Verify that chmod doesn't actually change permissions (driver-enforced file-mode)
		ginkgo.By("Verifying chmod doesn't override driver-enforced file-mode")
		// The file should still have 0640 (the mount option) regardless of chmod
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^640$'", explicitFile))
	})
}
