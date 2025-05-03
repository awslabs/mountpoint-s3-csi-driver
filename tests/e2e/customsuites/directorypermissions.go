// directorypermissions.go — tests directory‑permission semantics (dir‑mode mount option)
// This complements filepermissions.go.
package customsuites

import (
	"context"
	"fmt"

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
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var l local

	f := framework.NewFrameworkWithCustomTimeouts("directorypermissions", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelRestricted

	// ------------- helper wrappers -------------
	cleanup := func() {
		for _, r := range l.resources {
			func() {
				defer ginkgo.GinkgoRecover()
				_ = r.CleanupResource(context.Background())
			}()
		}
	}

	createVolume := func(ctx context.Context, cfg *storageframework.PerTestConfig, pat storageframework.TestPattern,
		uid, gid int64, dirMode string, extra ...string) *storageframework.VolumeResource {

		res := BuildVolumeWithOptions(ctx, cfg, pat, uid, gid, "", append([]string{fmt.Sprintf("dir-mode=%s", dirMode)}, extra...)...)
		l.resources = append(l.resources, res)
		return res
	}

	verifyDir := func(fr *framework.Framework, pod *v1.Pod, path, mode string, uid, gid *int64) {
		ginkgo.By(fmt.Sprintf("verify dir %s mode=%s", path, mode))
		e2evolume.VerifyExecInPodSucceed(fr, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^%s$'", path, mode))
		if uid != nil && gid != nil {
			e2evolume.VerifyExecInPodSucceed(fr, pod,
				fmt.Sprintf("stat -c '%%u %%g' %s | grep '%d %d'", path, *uid, *gid))
		}
	}

	verifyFile := func(fr *framework.Framework, pod *v1.Pod, path string) {
		e2evolume.VerifyExecInPodSucceed(fr, pod, fmt.Sprintf("stat -c '%%a' %s | grep -q '^644$'", path))
	}

	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
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
		res := BuildVolumeWithOptions(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "")
		l.resources = append(l.resources, res)

		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		dir := "/mnt/volume1/testdir"
		CreateDirInPod(f, pod, dir)
		file := fmt.Sprintf("%s/file.txt", dir)
		CreateFileInPod(f, pod, file, "content")

		uid, gid := ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup)
		verifyDir(f, pod, dir, "755", uid, gid)
		verifyFile(f, pod, file)
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
		res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0700")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		dir := "/mnt/volume1/private"
		CreateDirInPod(f, pod, dir)
		file := fmt.Sprintf("%s/f.txt", dir)
		CreateFileInPod(f, pod, file, "x")

		verifyDir(f, pod, dir, "700", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		verifyFile(f, pod, file) // files remain 0644
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
		v1Res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0700")
		v2Res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0777")

		pod := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{v1Res.Pvc, v2Res.Pvc}, "")
		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		dir1 := "/mnt/volume1/dir"
		dir2 := "/mnt/volume2/dir"
		CreateDirInPod(f, pod, dir1)
		CreateDirInPod(f, pod, dir2)

		verifyDir(f, pod, dir1, "700", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		verifyDir(f, pod, dir2, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
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
		res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0755")
		// first pod
		pod1 := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "writer")
		pod1, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1)

		dir := "/mnt/volume1/target"
		CreateDirInPod(f, pod1, dir)

		// persist the directory for future mounts
		dummy := fmt.Sprintf("%s/.keep", dir)
		CreateFileInPod(f, pod1, dummy, "marker")

		verifyDir(f, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// update PV -> 0555
		pv, _ := f.ClientSet.CoreV1().PersistentVolumes().Get(ctx, res.Pv.Name, metav1.GetOptions{})
		pv.Spec.MountOptions = []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other",
			"dir-mode=0555",
		}
		_, err = f.ClientSet.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		// second pod
		pod2 := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "reader")
		pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2)

		verifyDir(f, pod2, dir, "555", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		verifyDir(f, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
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
		res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0711")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		paths := []string{
			"/mnt/volume1/a",
			"/mnt/volume1/a/b",
			"/mnt/volume1/a/b/c",
		}
		CreateMultipleDirsInPod(f, pod, paths...)

		for _, p := range paths {
			verifyDir(f, pod, p, "711", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
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
		res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0777",
			"allow-delete", "allow-overwrite")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		srcDir := "/mnt/volume1/src"
		dstDir := "/mnt/volume1/dst"
		CreateMultipleDirsInPod(f, pod, srcDir, dstDir)

		file := fmt.Sprintf("%s/f.txt", srcDir)
		CreateFileInPod(f, pod, file, "data")
		CopyFileInPod(f, pod, file, fmt.Sprintf("%s/copy.txt", dstDir))

		verifyDir(f, pod, srcDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		verifyDir(f, pod, dstDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
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
		res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0777",
			"allow-delete", "allow-overwrite")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		// Setup source structure
		srcDir := "/mnt/volume1/src"
		nestedDir := fmt.Sprintf("%s/nested", srcDir)
		dstDir := "/mnt/volume1/dst"

		ginkgo.By("Creating source and nested directories")
		CreateMultipleDirsInPod(f, pod, srcDir, nestedDir, dstDir)

		// Add a file in the nested directory
		nestedFile := fmt.Sprintf("%s/file.txt", nestedDir)
		CreateFileInPod(f, pod, nestedFile, "nested content")

		// Perform recursive copy: cp -r src/* dst/
		ginkgo.By("Recursively copying src to dst")
		_, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("cp -r %s/* %s/", srcDir, dstDir))
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed cp -r: %s", stderr)

		// Paths to verify
		copiedNestedDir := fmt.Sprintf("%s/nested", dstDir)
		copiedNestedFile := fmt.Sprintf("%s/file.txt", copiedNestedDir)

		// Verify permissions for source and destination dirs
		verifyDir(f, pod, srcDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		verifyDir(f, pod, nestedDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		verifyDir(f, pod, dstDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
		verifyDir(f, pod, copiedNestedDir, "777", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// Verify the copied file permissions (should be 0644)
		verifyFile(f, pod, copiedNestedFile)
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
		res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0755")

		// Step 2: Start Pod 1
		pod1 := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "early")
		pod1, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1)

		// Step 3: Pod 1 creates a directory and a marker file
		dir := "/mnt/volume1/shared"
		CreateDirInPod(f, pod1, dir)
		marker := fmt.Sprintf("%s/.marker", dir)
		CreateFileInPod(f, pod1, marker, "exists")

		// Verify initial permissions in Pod 1
		ginkgo.By("Pod 1 sees dir as 0755")
		verifyDir(f, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// Step 4: Update the PV to dir-mode=0555
		pv, _ := f.ClientSet.CoreV1().PersistentVolumes().Get(ctx, res.Pv.Name, metav1.GetOptions{})
		pv.Spec.MountOptions = []string{
			fmt.Sprintf("uid=%d", DefaultNonRootUser),
			fmt.Sprintf("gid=%d", DefaultNonRootGroup),
			"allow-other",
			"dir-mode=0555",
		}
		_, err = f.ClientSet.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		// Step 5: Start Pod 2 (after PV update)
		pod2 := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{res.Pvc}, "late")
		pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2)

		// Verify permissions in Pod 2
		ginkgo.By("Pod 2 sees dir as 0555")
		verifyDir(f, pod2, dir, "555", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		// Re-check Pod 1 to confirm its view did NOT change
		ginkgo.By("Pod 1 still sees dir as 0755")
		verifyDir(f, pod1, dir, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))
	})

	// --------------------------------------------------------------------
	// 9. Pod Security Context interaction with dir-mode
	//
	// This test verifies that directory permissions and ownership are governed
	// solely by the mount options (dir-mode + uid/gid), and *not* overridden by
	// the pod’s security context:
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
	//   the ownership and permissions are *fixed* by Mountpoint’s settings
	ginkgo.It("should apply dir-mode consistently with pod security context settings", func(ctx context.Context) {
		customUID := int64(3000)
		customGID := int64(4000)
		runAsNonRoot := true

		// Step 1 Create volume with dir-mode=0700 and matching UID/GID
		res := createVolume(ctx, l.config, pattern, customUID, customGID, "0700")

		// Step 2 Create pod with SecurityContext (runAsUser + fsGroup)
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{res.Pvc}, admissionapi.LevelRestricted, "")
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			RunAsUser:    &customUID,
			FSGroup:      &customGID,
			RunAsNonRoot: &runAsNonRoot,
			SeccompProfile: &v1.SeccompProfile{
				Type: v1.SeccompProfileTypeRuntimeDefault,
			},
		}

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		// Step 3 Create a directory
		dirPath := "/mnt/volume1/test-secdir"
		CreateDirInPod(f, pod, dirPath)

		// Step 4 Verify directory has 0700 and matches UID/GID
		verifyDir(f, pod, dirPath, "700", ptr.To(customUID), ptr.To(customGID))
	})

	// --------------------------------------------------------------------
	// 10. Chown operation disallowed
	//
	// This test verifies that directory ownership cannot be changed after mount,
	// consistent with Mountpoint-S3’s design where UID/GID are fixed at mount time
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
	// Note: This aligns with Mountpoint-S3’s documented behavior that metadata like
	// ownership cannot be altered post-mount via filesystem commands
	ginkgo.It("should not allow chown to change directory ownership", func(ctx context.Context) {
		res := createVolume(ctx, l.config, pattern, DefaultNonRootUser, DefaultNonRootGroup, "0755")

		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, res.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)

		// Create a test directory
		dirPath := "/mnt/volume1/chown-test-dir"
		CreateDirInPod(f, pod, dirPath)

		// Verify initial ownership
		verifyDir(f, pod, dirPath, "755", ptr.To(DefaultNonRootUser), ptr.To(DefaultNonRootGroup))

		ginkgo.By("Attempting chown to 9999:9999 (should fail)")
		_, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("chown 9999:9999 %s", dirPath))

		// We expect chown to fail (Mountpoint does not support changing ownership)
		gomega.Expect(err).To(gomega.HaveOccurred())

		// The error should mention permission issues or lack of support
		gomega.Expect(stderr).To(gomega.Or(
			gomega.ContainSubstring("Operation not permitted"),
			gomega.ContainSubstring("Operation not supported"),
		), fmt.Sprintf("unexpected chown error message: %s", stderr))
	})
}
