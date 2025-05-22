// directorypermissions.go — tests directory‑permission semantics (dir‑mode mount option)
// This complements filepermissions.go.
package customsuites

import (
	"context"
	"fmt"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
)

// s3CSIDirectoryPermissionsTestSuite validates dir‑mode behavior on the S3 CSI driver.
//
// This test suite focuses on how directory permissions are set and maintained when using the
// S3 CSI driver with various dir-mode mount options. It verifies that directory permissions
// are correctly applied across different scenarios including creation, mounting, and remounting
// with different permission settings.
type s3CSIDirectoryPermissionsTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3DirectoryPermissionsTestSuite registers the suite.
//
// This suite tests:
// - Default directory permissions (0755)
// - Custom directory permissions via dir-mode mount option
// - Directory permission inheritance in subdirectories
// - Directory permission behavior during remount with changed options
// - Multi-pod access with different directory permissions
// - Directory permission preservation during file operations
func InitS3DirectoryPermissionsTestSuite() storageframework.TestSuite {
	return &s3CSIDirectoryPermissionsTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "directorypermissions",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIDirectoryPermissionsTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIDirectoryPermissionsTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

// DefineTests implements all cases.
func (t *s3CSIDirectoryPermissionsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type TestResourceRegistry struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var testRegistry TestResourceRegistry

	testFramework := framework.NewFrameworkWithCustomTimeouts("directorypermissions", storageframework.GetDriverTimeouts(driver))
	testFramework.NamespacePodSecurityLevel = admissionapi.LevelRestricted

	// ------------- helper wrappers -------------
	cleanup := func() {
		for _, r := range testRegistry.resources {
			func() {
				defer ginkgo.GinkgoRecover()
				_ = r.CleanupResource(context.Background())
			}()
		}
	}

	createVolume := func(ctx context.Context, cfg *storageframework.PerTestConfig, pat storageframework.TestPattern,
		uid, gid int64, dirMode string, extra ...string,
	) *storageframework.VolumeResource {
		res := BuildVolumeWithOptions(ctx, cfg, pat, uid, gid, "", append([]string{fmt.Sprintf("dir-mode=%s", dirMode)}, extra...)...)
		testRegistry.resources = append(testRegistry.resources, res)
		return res
	}

	// assertDirPerms verifies that a directory at dirPath has the expected
	// permission bits *and*, when uid/gid are provided, the expected owner.
	assertDirPerms := func(framework *framework.Framework, pod *v1.Pod, dirPath, expectedMode string, uid, gid *int64) {
		ginkgo.By(fmt.Sprintf("asserting %s has mode=%s", dirPath, expectedMode))
		e2evolume.VerifyExecInPodSucceed(framework, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^%s$'", dirPath, expectedMode))
		if uid != nil && gid != nil {
			e2evolume.VerifyExecInPodSucceed(framework, pod,
				fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'", dirPath, *uid, *gid))
		}
	}

	// assertFilePerms verifies that a regular file has 0644 permissions – the
	// default enforced by Mountpoint-S3 irrespective of dir-mode settings.
	assertFilePerms := func(framework *framework.Framework, pod *v1.Pod, filePath string) {
		e2evolume.VerifyExecInPodSucceed(framework, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", filePath))
	}

	ginkgo.BeforeEach(func(ctx context.Context) {
		testRegistry = TestResourceRegistry{}
		testRegistry.config = driver.PrepareTest(ctx, testFramework)
		ginkgo.DeferCleanup(cleanup)
	})

	// --------------------------------------------------------------------
	// 1. Default 0755
	//
	// This test verifies the default directory permissions when
	// no specific dir-mode mount option is specified:
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
	// - Directories: 0755 (`drwxr-xr-x`) permissions
	// - Files: 0644 (`-rw-r--r--`) permissions (unaffected by dir-mode)
	// - Ownership: matches specified uid/gid
	ginkgo.It("should default directories to 0755 when dir‑mode not set", func(ctx context.Context) {
		res := BuildVolumeWithOptions(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "")
		testRegistry.resources = append(testRegistry.resources, res)

		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		dir := "/mnt/volume1/testdir"
		CreateDirInPod(testFramework, pod, dir)
		file := fmt.Sprintf("%s/file.txt", dir)
		CreateFileInPod(testFramework, pod, file, "content")

		uid, gid := ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup)
		assertDirPerms(testFramework, pod, dir, "755", uid, gid)
		assertFilePerms(testFramework, pod, file)
	})

	// --------------------------------------------------------------------
	// 2. Custom dir-mode=0700
	//
	// This test verifies that custom directory permissions are applied when
	// the dir-mode mount option is specified:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with dir-mode=0700]
	//        |
	//        ↓
	//    [S3 Bucket]
	//
	// Expected results:
	// - Directories: 0700 (`drwx------`) permissions (from dir-mode option)
	// - Files: 0644 (`-rw-r--r--`) permissions (unaffected by dir-mode)
	// - Ownership: matches specified uid/gid
	ginkgo.It("should apply custom dir‑mode=0700", func(ctx context.Context) {
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0700")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		dir := "/mnt/volume1/private"
		CreateDirInPod(testFramework, pod, dir)
		file := fmt.Sprintf("%s/f.txt", dir)
		CreateFileInPod(testFramework, pod, file, "x")

		assertDirPerms(testFramework, pod, dir, "700", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		assertFilePerms(testFramework, pod, file) // files remain 0644
	})

	// --------------------------------------------------------------------
	// 3. Dual volumes 0700 vs 0777
	//
	// This test verifies that different volumes in the same pod
	// can have different directory permission settings:
	//
	//      [Pod]
	//        |
	//       / \
	//      /   \
	//  [Vol 1]  [Vol 2]
	// dir-mode  dir-mode
	//  =0700     =0777
	//     |         |
	//     ↓         ↓
	// [S3 Bucket] [S3 Bucket]
	//
	// Expected results:
	// - Volume 1 Directories: 0700 (`drwx------`) permissions
	// - Volume 2 Directories: 0777 (`drwxrwxrwx`) permissions
	// - Files: 0644 (`-rw-r--r--`) permissions on both volumes
	// - Ownership: matches specified uid/gid on both volumes
	ginkgo.It("should keep distinct dir‑mode per volume", func(ctx context.Context) {
		v1Res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0700")
		v2Res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0777")

		pod := MakeNonRootPodWithVolume(testFramework.Namespace.Name, []*v1.PersistentVolumeClaim{v1Res.Pvc, v2Res.Pvc}, "")
		pod, err := createPod(ctx, testFramework.ClientSet, testFramework.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		dir1 := "/mnt/volume1/dir"
		dir2 := "/mnt/volume2/dir"
		CreateDirInPod(testFramework, pod, dir1)
		CreateDirInPod(testFramework, pod, dir2)

		assertDirPerms(testFramework, pod, dir1, "700", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		assertDirPerms(testFramework, pod, dir2, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// --------------------------------------------------------------------
	// 4. Remount with new dir-mode
	//
	// This test verifies that changing directory permission mount options
	// and remounting a volume applies the new settings:
	//
	//      [Pod 1]                 [Pod 2]
	//        |                       |
	//        ↓                       ↓
	//   [S3 Volume]  →  1. Delete Pod 1  →  [S3 Volume]
	//   dir-mode=0755   2. Update PV       dir-mode=0555
	//        |             mount options       |
	//        ↓                                 ↓
	//    [S3 Bucket] ──── Same Bucket ──→ [S3 Bucket]
	//
	// Expected results:
	// - Initial directories: 0755 (`drwxr-xr-x`) permissions
	// - After remount: 0555 (`dr-xr-xr-x`) permissions (read-only dirs)
	// - Files: Always 0644 (`-rw-r--r--`) (not affected by dir-mode changes)
	// - Ownership: matches specified uid/gid in both cases
	ginkgo.It("should update dir permissions after PV mountOptions change", func(ctx context.Context) {
		// initial PV default
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0755")
		// first pod
		pod1 := MakeNonRootPodWithVolume(testFramework.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "writer")
		pod1, err := createPod(ctx, testFramework.ClientSet, testFramework.Namespace.Name, pod1)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod1)

		dir := "/mnt/volume1/target"
		CreateDirInPod(testFramework, pod1, dir)

		// persist the directory for future mounts
		dummy := fmt.Sprintf("%s/.keep", dir)
		CreateFileInPod(testFramework, pod1, dummy, "marker")

		assertDirPerms(testFramework, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// update PV -> 0555
		pv, _ := testFramework.ClientSet.CoreV1().PersistentVolumes().Get(ctx, res.Pv.Name, metav1.GetOptions{})
		pv.Spec.MountOptions = []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other",
			"dir-mode=0555",
		}
		_, err = testFramework.ClientSet.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		// second pod
		pod2 := MakeNonRootPodWithVolume(testFramework.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "reader")
		pod2, err = createPod(ctx, testFramework.ClientSet, testFramework.Namespace.Name, pod2)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod2)

		assertDirPerms(testFramework, pod2, dir, "555", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		assertDirPerms(testFramework, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// --------------------------------------------------------------------
	// 5. Inheritance in nested subdirs
	//
	// This test verifies that newly created subdirectories inherit
	// the dir-mode permission settings at all levels:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with dir-mode=0711]
	//        |
	//        ↓
	//       [/a] → 0711
	//        |
	//        ↓
	//      [/a/b] → 0711
	//        |
	//        ↓
	//    [/a/b/c] → 0711
	//
	// Expected results:
	// - All directories at all nesting levels have 0711 (`drwx--x--x`) permissions
	// - Directory permissions are consistent regardless of nesting level
	// - Ownership: matches specified uid/gid for all directories
	ginkgo.It("should apply dir‑mode recursively to new subdirectories", func(ctx context.Context) {
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0711")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		paths := []string{
			"/mnt/volume1/a",
			"/mnt/volume1/a/b",
			"/mnt/volume1/a/b/c",
		}
		CreateMultipleDirsInPod(testFramework, pod, paths...)

		for _, pathItem := range paths {
			assertDirPerms(testFramework, pod, pathItem, "711", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		}
	})

	// --------------------------------------------------------------------
	// 6. Copy/delete does not change dir perms
	//
	// This test verifies that directory permissions are preserved
	// during copy and delete operations:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with dir-mode=0777]
	//           |
	//       /      \
	//      /        \
	//  [src dir] [dst dir]
	//      |        ↑
	//      |        |
	//     file ---copy--→ copy.txt
	//
	// Expected results:
	// - Source directory: 0777 (`drwxrwxrwx`) permissions
	// - Destination directory: 0777 (`drwxrwxrwx`) permissions
	// - Permissions remain consistent throughout file operations
	// - Ownership: matches specified uid/gid throughout
	ginkgo.It("should preserve dir permissions during copy operations", func(ctx context.Context) {
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0777",
			"allow-delete", "allow-overwrite")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		srcDir := "/mnt/volume1/src"
		dstDir := "/mnt/volume1/dst"
		CreateMultipleDirsInPod(testFramework, pod, srcDir, dstDir)

		file := fmt.Sprintf("%s/f.txt", srcDir)
		CreateFileInPod(testFramework, pod, file, "data")
		CopyFileInPod(testFramework, pod, file, fmt.Sprintf("%s/copy.txt", dstDir))

		assertDirPerms(testFramework, pod, srcDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		assertDirPerms(testFramework, pod, dstDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// --------------------------------------------------------------------
	// 7. Recursive copy preserves directory permissions
	//
	// This test verifies that recursively copying a directory preserves
	// directory permissions at all levels:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with dir-mode=0777]
	//            |
	//       /          \
	//      /            \
	// [src dir] --> [dst dir]
	//     |
	//     ↓
	// [nested subdir]
	//
	// Expected results:
	// - Source and destination directories: 0777 (`drwxrwxrwx`) permissions
	// - Nested subdirectory: 0777 (`drwxrwxrwx`) permissions after recursive copy
	// - Files remain 0644 (`-rw-r--r--`) (driver default for files)
	// - Ownership: matches specified uid/gid throughout
	ginkgo.It("should preserve directory permissions during recursive copy operations", func(ctx context.Context) {
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0777",
			"allow-delete", "allow-overwrite")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		// Setup source structure
		srcDir := "/mnt/volume1/src"
		nestedDir := fmt.Sprintf("%s/nested", srcDir)
		dstDir := "/mnt/volume1/dst"

		ginkgo.By("Creating source and nested directories")
		CreateMultipleDirsInPod(testFramework, pod, srcDir, nestedDir, dstDir)

		// Add a file in the nested directory
		nestedFile := fmt.Sprintf("%s/file.txt", nestedDir)
		CreateFileInPod(testFramework, pod, nestedFile, "nested content")

		// Perform recursive copy: cp -r src/* dst/
		ginkgo.By("Recursively copying src to dst")
		_, stderr, err := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("cp -r %s/* %s/", srcDir, dstDir))
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed cp -r: %s", stderr)

		// Paths to verify
		copiedNestedDir := fmt.Sprintf("%s/nested", dstDir)
		copiedNestedFile := fmt.Sprintf("%s/file.txt", copiedNestedDir)

		// Verify permissions for source and destination dirs
		assertDirPerms(testFramework, pod, srcDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		assertDirPerms(testFramework, pod, nestedDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		assertDirPerms(testFramework, pod, dstDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		assertDirPerms(testFramework, pod, copiedNestedDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// Verify the copied file permissions (should be 0644)
		assertFilePerms(testFramework, pod, copiedNestedFile)
	})

	// --------------------------------------------------------------------
	// 8. Concurrent pods see different dir-mode based on mount timing
	//
	// This test verifies that pods mounting the same volume at different times
	// see directory permissions as defined by the mount options *at the time
	// they mounted*:
	//
	//      [Pod 1] ──────────────────────────────── [Pod 1]
	//        |       (keeps running)                  |
	//        ↓                                         |
	//   [S3 Volume] → 1. Update PV mount options →  [S3 Volume]
	//   dir-mode=0755     (without deleting pod 1)   dir-mode=0555
	//        |                                         ↑
	//        ↓                                         |
	//    [S3 Bucket] ── Same bucket ─────────────── [Pod 2]
	//
	// Expected results:
	// - Pod 1 (already running) continues to see 0755 (`drwxr-xr-x`) directory permissions
	// - Pod 2 (started after PV update) sees 0555 (`dr-xr-xr-x`) directory permissions
	// - Confirms that permission views are "frozen" at mount time, even though
	//   the underlying volume is the same and the PV was updated
	ginkgo.It("should show different dir permissions in concurrent pods based on mount timing", func(ctx context.Context) {
		// Step 1: Create volume with dir-mode=0755
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0755")

		// Step 2: Start Pod 1
		pod1 := MakeNonRootPodWithVolume(testFramework.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "early")
		pod1, err := createPod(ctx, testFramework.ClientSet, testFramework.Namespace.Name, pod1)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod1)

		// Step 3: Pod 1 creates a directory and a marker file
		dir := "/mnt/volume1/shared"
		CreateDirInPod(testFramework, pod1, dir)
		marker := fmt.Sprintf("%s/.marker", dir)
		CreateFileInPod(testFramework, pod1, marker, "exists")

		// Verify initial permissions in Pod 1
		ginkgo.By("Pod 1 sees dir as 0755")
		assertDirPerms(testFramework, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// Step 4: Update the PV to dir-mode=0555
		pv, _ := testFramework.ClientSet.CoreV1().PersistentVolumes().Get(ctx, res.Pv.Name, metav1.GetOptions{})
		pv.Spec.MountOptions = []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other",
			"dir-mode=0555",
		}
		_, err = testFramework.ClientSet.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		// Step 5: Start Pod 2 (after PV update)
		pod2 := MakeNonRootPodWithVolume(testFramework.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "late")
		pod2, err = createPod(ctx, testFramework.ClientSet, testFramework.Namespace.Name, pod2)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod2)

		// Verify permissions in Pod 2
		ginkgo.By("Pod 2 sees dir as 0555")
		assertDirPerms(testFramework, pod2, dir, "555", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// Re-check Pod 1 to confirm its view did NOT change
		ginkgo.By("Pod 1 still sees dir as 0755")
		assertDirPerms(testFramework, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// --------------------------------------------------------------------
	// 9. Pod Security Context interaction with dir-mode
	//
	// This test verifies that directory permissions and ownership are governed
	// solely by the mount options (dir-mode + uid/gid), and *not* overridden by
	// the pod's security context:
	//
	//      [Pod with SecurityContext]
	//        |    runAsUser: 3000
	//        |    fsGroup: 4000
	//        |
	//        ↓
	//   [S3 Volume with dir-mode=0700, uid=3000, gid=4000]
	//        |
	//        ↓
	//    [Directory]
	//
	// Expected results:
	// - The created directory has permissions 0700 (`drwx------`) as specified by dir-mode
	// - The ownership matches the uid/gid from the mount options (3000:4000)
	// - Confirms that even with a pod security context (runAsUser, fsGroup),
	//   the ownership and permissions are *fixed* by Mountpoint's settings
	ginkgo.It("should apply dir-mode consistently with pod security context settings", func(ctx context.Context) {
		customUID := int64(3000)
		customGID := int64(4000)
		runAsNonRoot := true

		// Step 1 Create volume with dir-mode=0700 and matching UID/GID
		res := createVolume(ctx, testRegistry.config, pattern, customUID, customGID, "0700")

		// Step 2 Create pod with SecurityContext (runAsUser + fsGroup)
		pod := e2epod.MakePod(testFramework.Namespace.Name, nil, []*v1.PersistentVolumeClaim{res.Pvc}, admissionapi.LevelRestricted, "")
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			RunAsUser:    &customUID,
			FSGroup:      &customGID,
			RunAsNonRoot: &runAsNonRoot,
			SeccompProfile: &v1.SeccompProfile{
				Type: v1.SeccompProfileTypeRuntimeDefault,
			},
		}

		pod, err := createPod(ctx, testFramework.ClientSet, testFramework.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		// Step 3 Create a directory
		dirPath := "/mnt/volume1/test-secdir"
		CreateDirInPod(testFramework, pod, dirPath)

		// Step 4 Verify directory has 0700 and matches UID/GID
		assertDirPerms(testFramework, pod, dirPath, "700", ptr.To(customUID), ptr.To(customGID))
	})

	// --------------------------------------------------------------------
	// 10. Chown operation disallowed
	//
	// This test verifies that directory ownership cannot be changed after mount,
	// consistent with Mountpoint-S3's design where UID/GID are fixed at mount time
	// and not mutable by tools like `chown`.
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with dir-mode=0700]
	//        |
	//        ↓
	//    [Directory]
	//
	// Expected results:
	// - The directory is created with the specified dir-mode and UID/GID
	// - An attempt to change ownership using `chown` fails
	// - The error message from `chown` may vary based on implementation:
	//     - It might report "Operation not permitted" (EPERM)
	//     - Or "Operation not supported" (ENOTSUP)
	// - The directory ownership remains unchanged after the failed attempt
	//
	// Note: This aligns with Mountpoint-S3's documented behavior that metadata like
	// ownership cannot be altered post-mount via filesystem commands
	ginkgo.It("should not allow chown to change directory ownership", func(ctx context.Context) {
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0755")

		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		// Create a test directory
		dirPath := "/mnt/volume1/chown-test-dir"
		CreateDirInPod(testFramework, pod, dirPath)

		// Verify initial ownership
		assertDirPerms(testFramework, pod, dirPath, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		ginkgo.By("Attempting chown to 9999:9999 (should fail)")
		_, stderr, err := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("chown 9999:9999 %s", dirPath))

		// We expect chown to fail (Mountpoint does not support changing ownership)
		gomega.Expect(err).To(gomega.HaveOccurred())

		// The error should mention permission issues or lack of support
		gomega.Expect(stderr).To(gomega.Or(
			gomega.ContainSubstring("Operation not permitted"),
			gomega.ContainSubstring("Operation not supported"),
		), fmt.Sprintf("unexpected chown error message: %s", stderr))
	})

	// 11. Chmod operation (directory)
	//
	// This test verifies that chmod on a directory inside the S3 volume
	// appears to succeed but is a no-op: the mode bits remain unchanged.
	//
	// This mirrors the behavior seen with files, except:
	// - For files: chmod fails (EPERM / ENOTSUP)
	// - For directories: chmod succeeds but has no effect
	//
	// Test scenario:
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with dir-mode=0700]
	//        |
	//        ↓
	//    [Directory]
	//
	// Expected results:
	// - The directory is created with initial mode 0700
	// - chmod 0777 runs without error (no EPERM / ENOTSUP)
	// - stat shows the mode is still 0700 (no change)
	//
	// This confirms Mountpoint-S3 enforces directory permission immutability
	// in the same way as file immutability but differs in syscall response:
	// it accepts chmod for dirs but freezes the actual metadata.
	ginkgo.It("should silently ignore chmod on directories (no-op)", func(ctx context.Context) {
		res := createVolume(ctx, testRegistry.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0700")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		dirPath := "/mnt/volume1/chmod-test-dir"
		CreateDirInPod(testFramework, pod, dirPath)

		// Check initial permissions
		initialPerms, _, err := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("stat -c '%%a' %s", dirPath))
		framework.ExpectNoError(err)
		gomega.Expect(strings.TrimSpace(initialPerms)).To(gomega.Equal("700"))

		// Attempt chmod to change perms (should succeed but not take effect)
		_, stderr, err := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("chmod 0777 %s", dirPath))
		framework.ExpectNoError(err, "chmod failed unexpectedly: %s", stderr)

		// Confirm permissions remain unchanged
		afterPerms, _, err := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("stat -c '%%a' %s", dirPath))
		framework.ExpectNoError(err)
		gomega.Expect(strings.TrimSpace(afterPerms)).To(gomega.Equal("700"),
			"chmod succeeded but did not actually change directory mode")
	})

	// --------------------------------------------------------------------
	// 12. Pod umask MUST NOT override dir‑mode
	//
	// Mountpoint‑S3 applies the dir‑mode given at mount time and ignores
	// the process umask when creating new directories.
	//
	// Test scenario:
	//
	//     [Pod] umask 077
	//         │
	//         ▼
	//    mkdir /mnt/volume1/umask‑dir
	//         │
	//         ▼
	//  [S3 Volume] (dir‑mode = 0755)
	//
	// Expectations:
	// - mkdir succeeds inside the volume even with a restrictive umask
	// - stat shows permissions 0755 (driver‑enforced), not 0700
	// - UID/GID match the mount options (1001/2000 by default)
	//
	// This mirrors the file‑side umask test and proves the driver's
	// directory‑permission logic is immune to container umask settings.
	ginkgo.It("should override pod umask and honour dir‑mode", func(ctx context.Context) {
		// Build volume with dir‑mode=0755
		res := createVolume(ctx, testRegistry.config, pattern,
			DefaultNonRootUser,  // uid
			DefaultNonRootGroup, // gid
			"0755")              // dir‑mode

		// Pod runs as the same non‑root UID/GID
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "",
			DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		dir := "/mnt/volume1/umask-dir"

		// Inside the container, set a *restrictive* umask then mkdir
		_, _, err = e2evolume.PodExec(testFramework, pod,
			fmt.Sprintf(`sh -c 'umask 077; mkdir %s'`, dir))
		framework.ExpectNoError(err, "mkdir with umask 077 failed")

		// Verify the directory got the mount‑option mode (0755), not 0700
		assertDirPerms(testFramework, pod, dir, "755",
			ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// --------------------------------------------------------------------
	// 13. mkdir -m mode flag is ignored, dir-mode prevails
	//
	// Applications may use `mkdir -m MODE` to try setting specific bits,
	// but Mountpoint-S3 enforces that only the mount option's dir-mode
	// controls permissions, ignoring explicit chmod requests.
	//
	// Test scenario:
	//
	//     dir-mode = 0700 (mount option)
	//          │
	//     mkdir -m 0777 /mnt/volume1/app-dir
	//          │
	//          ▼
	//     stat shows 0700, NOT 0777
	//
	// Expectations:
	// - mkdir succeeds (no error)
	// - The actual permissions are 0700 (from dir-mode mount option)
	// - The explicitly requested 0777 is silently ignored
	//
	// Why this matters:
	// Applications expecting to control permissions via mkdir -m
	// must adapt to S3's storage model where permissions are
	// set at mount time and immutable afterward.
	ginkgo.It("should ignore mkdir -m requested mode and apply dir‑mode", func(ctx context.Context) {
		// 1. Provision volume with a *restrictive* dir‑mode
		res := createVolume(ctx, testRegistry.config, pattern,
			DefaultNonRootUser, DefaultNonRootGroup, "0700")

		// 2. Run as the same non‑root user
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "",
			DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		appDir := "/mnt/volume1/app-dir"

		// 3. Application tries to create directory with explicit 0777 bits
		_, _, err = e2evolume.PodExec(testFramework, pod,
			fmt.Sprintf(`mkdir -m 0777 %s`, appDir))
		framework.ExpectNoError(err, "mkdir -m 0777 failed")

		// 4. Verify driver stamped the configured 0700, *not* 0777
		assertDirPerms(testFramework, pod, appDir, "700",
			ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// --------------------------------------------------------------------
	// 14. mv/rename MUST fail (directories)
	//
	// S3 has no native support for directory renames. Mountpoint‑S3 must
	// reject mv/rename operations on directories, returning a clear error
	// (e.g., EXDEV / EPERM / ENOTSUP).
	//
	// Test scenario:
	//
	//     mkdir /mnt/volume1/srcdir
	//          │
	//     mv /mnt/volume1/srcdir /mnt/volume1/dstdir
	//          │
	//          ▼
	//     FAILS with:
	//       - "Operation not permitted"
	//       - "Operation not supported"
	//       - "Function not implemented"
	//
	// Expectations:
	// - mv/rename fails (exit code != 0)
	// - Error message matches expected patterns (see above)
	// - srcdir still exists (directory untouched)
	// - dstdir does NOT exist (rename did not partially succeed)
	// - Metadata (permissions, ownership) of srcdir unchanged
	//
	// Why this matters:
	// This ensures the driver does not silently emulate rename via copy+delete
	// or perform partial / broken renames, preserving S3's semantics and
	// avoiding dangerous race conditions.
	ginkgo.It("should fail mv/rename of directory with ENOTSUP or equivalent", func(ctx context.Context) {
		// Set up volume + pod as before...
		res := createVolume(ctx, testRegistry.config, pattern,
			DefaultNonRootUser, DefaultNonRootGroup, "0700")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "",
			DefaultNonRootUser, DefaultNonRootGroup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		src := "/mnt/volume1/srcdir"
		dst := "/mnt/volume1/dstdir"

		// 1: create the source directory
		CreateDirInPod(testFramework, pod, src)

		// 2: try the mv and expect failure
		_, stderr, mvErr := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("mv %s %s", src, dst))

		// The mv MUST fail
		gomega.Expect(mvErr).To(gomega.HaveOccurred(), "mv/rename unexpectedly succeeded")

		// It must fail for the *correct reason*
		expectErr := gomega.Or(
			gomega.ContainSubstring("Operation not permitted"),
			gomega.ContainSubstring("Operation not supported"),
			gomega.ContainSubstring("Function not implemented"),
		)
		gomega.Expect(stderr).To(expectErr)

		// Validate that srcdir is still there and unchanged
		_, _, err = e2evolume.PodExec(testFramework, pod, fmt.Sprintf("test -d %s", src))
		framework.ExpectNoError(err, "srcdir vanished after failed mv")

		// Confirm dst dir does NOT exist
		_, _, err = e2evolume.PodExec(testFramework, pod, fmt.Sprintf("test -d %s", dst))
		gomega.Expect(err).To(gomega.HaveOccurred(), "dstdir unexpectedly created after failed mv")
	})

	// --------------------------------------------------------------------
	// 15. access() syscall: Consistency with stat() permissions
	//
	// The access() syscall checks file accessibility based on real UID/GID
	// and is subtly different from stat(). This test ensures Mountpoint‑S3
	// reports consistent permission results between both syscalls.
	//
	// Test scenario:
	//
	//     dir‑mode = 0700
	//          │
	//     mkdir /mnt/volume1/permsdir
	//          │
	//          ▼
	//     1. stat shows 0700 permissions
	//     2. test -r: succeeds (read allowed)
	//     3. test -w: succeeds (write allowed)
	//     4. test -x: succeeds (exec allowed)
	//
	// Expectations:
	// - stat correctly reports 0700 permissions for the directory
	// - access() permissions via test -r/-w/-x match what stat reports
	// - No inconsistencies between syscalls' permission handling
	//
	// Why this matters:
	// Applications rely on both syscalls for permission checks;
	// they must be consistent or applications could malfunction.
	ginkgo.It("should report consistent permissions via access() and stat() syscalls", func(ctx context.Context) {
		// Set up volume + pod as before...
		res := createVolume(ctx, testRegistry.config, pattern,
			DefaultNonRootUser, DefaultNonRootGroup, "0700")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "",
			DefaultNonRootUser, DefaultNonRootGroup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		dir := "/mnt/volume1/permsdir"

		// Create the directory
		CreateDirInPod(testFramework, pod, dir)

		// Get mode using stat()
		statMode, _, _ := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("stat -c '%%a' %s", dir))

		// Now test access() permissions: check readability (R_OK), writability (W_OK), executability (X_OK)
		_, _, errR := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("sh -c 'test -r %s'", dir))
		_, _, errW := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("sh -c 'test -w %s'", dir))
		_, _, errX := e2evolume.PodExec(testFramework, pod, fmt.Sprintf("sh -c 'test -x %s'", dir))

		// Convert the numeric mode into the expected permission bits
		// For 0700, expect R/W/X succeed for owner; fail for others
		expectedMode := strings.TrimSpace(statMode)
		gomega.Expect(expectedMode).To(gomega.Equal("700"))

		// Confirm access() matches: we expect R/W/X to succeed
		gomega.Expect(errR).NotTo(gomega.HaveOccurred(), "access(R_OK) failed unexpectedly")
		gomega.Expect(errW).NotTo(gomega.HaveOccurred(), "access(W_OK) failed unexpectedly")
		gomega.Expect(errX).NotTo(gomega.HaveOccurred(), "access(X_OK) failed unexpectedly")
	})

	// --------------------------------------------------------------------
	// 16. Traversal path normalization preserves permissions
	//
	// This regression‑guard test ensures that when a directory is accessed
	// through a path containing one or more “..” components, Mountpoint‑S3
	// collapses the traversal segments before evaluating permissions.  The
	// resulting inode must therefore show the same mode bits and ownership
	// as when it is addressed via its canonical path.
	//
	// Test scenario:
	//
	//     dir‑mode = 0755
	//          │
	//     mkdir /mnt/volume1/root
	//          │
	//     stat  /mnt/volume1/a/b/c/../../../root   → 0755
	//     stat  /mnt/volume1/root                  → 0755
	//
	// Expectations:
	// - Both `stat` invocations report 0755 (`drwxr-xr-x`) permissions.
	// - Ownership matches the uid/gid set via mount options.
	ginkgo.It("should preserve dir permissions when accessed via ../../../ traversal", func(ctx context.Context) {
		// Provision a volume with the default 0755 dir‑mode.
		res := createVolume(ctx, testRegistry.config, pattern,
			DefaultNonRootUser, DefaultNonRootGroup, "0755")

		// Launch a pod running as the same non‑root UID/GID.
		pod, err := CreatePodWithVolumeAndSecurity(ctx, testFramework, res.Pvc, "",
			DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, testFramework.ClientSet, pod)

		// Create the canonical directory plus some nested path components.
		rootDir := "/mnt/volume1/root"
		nestedDirs := []string{
			"/mnt/volume1/a",
			"/mnt/volume1/a/b",
			"/mnt/volume1/a/b/c",
		}
		CreateMultipleDirsInPod(testFramework, pod, append(nestedDirs, rootDir)...)

		// Sanity‑check the canonical path.
		assertDirPerms(testFramework, pod, rootDir, "755",
			ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// Now stat the same inode via a traversal path.
		traversalPath := "/mnt/volume1/a/b/c/../../../root"
		assertDirPerms(testFramework, pod, traversalPath, "755",
			ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// TODO(S3CSI-72): Add attr test after changing image to alpine
}
