// This file implements the mount options test suite, which verifies that the S3 CSI
// driver correctly handles volume mount options related to permissions, user/group IDs,
// and access controls when mounting S3 buckets in Kubernetes pods.
package customsuites

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// s3CSIMountOptionsTestSuite implements the Kubernetes storage framework TestSuite interface.
// It validates that the S3 CSI driver properly handles various mount options, particularly
// those related to file ownership, permissions, and access control.
type s3CSIMountOptionsTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3MountOptionsTestSuite initializes and returns a test suite that validates
// mount options functionality for the S3 CSI driver.
//
// This suite specifically tests:
// - Access to volumes when mounted with non-root user/group IDs
// - Proper enforcement of permissions when mount options are absent
// - File and directory ownership when mounting with specific uid/gid
// - Enforcement of mount options policy (disallowed options)
//
// The test suite is registered with the E2E framework and will be automatically
// executed when the test runner is invoked.
func InitS3MountOptionsTestSuite() storageframework.TestSuite {
	return &s3CSIMountOptionsTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "mountoptions",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

// GetTestSuiteInfo returns metadata about this test suite for the framework.
func (t *s3CSIMountOptionsTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

// SkipUnsupportedTests allows test suites to skip certain tests based on driver capabilities.
// For S3 mount options, all tests should be supported, so this is a no-op.
func (t *s3CSIMountOptionsTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

// DefineTests implements the actual test suite functionality.
// This method is called by the storage framework to execute the tests.
func (t *s3CSIMountOptionsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	// local struct to maintain test state across BeforeEach/AfterEach/It blocks
	type local struct {
		resources []*storageframework.VolumeResource // tracks resources for cleanup
		config    *storageframework.PerTestConfig    // storage framework configuration
	}
	var (
		l local
	)

	// Create a framework with custom timeouts based on the driver's requirements
	f := framework.NewFrameworkWithCustomTimeouts("mountoptions", storageframework.GetDriverTimeouts(driver))
	// Use restricted pod security level to better represent real-world scenarios
	f.NamespacePodSecurityLevel = admissionapi.LevelRestricted

	// cleanup function to be called after each test to ensure resources are properly deleted
	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	// validateWriteToVolume is a helper function that tests write access to a volume
	// when mounted with specific options to allow non-root access.
	//
	// This function:
	// 1. Creates a volume with mount options for non-root access
	// 2. Creates a pod that runs as non-root and mounts this volume
	// 3. Verifies the pod can write to and read from the volume
	// 4. Checks that files and directories have correct ownership and permissions
	//
	// These checks validate that the S3 CSI driver correctly applies mount options
	// like uid, gid, and allow-other to enable non-root access to S3 buckets.
	validateWriteToVolume := func(ctx context.Context) {
		// Use BuildVolumeWithOptions from util.go which provides standard non-root options
		// plus debug for better diagnostics
		resource := BuildVolumeWithOptions(
			ctx,
			l.config,
			pattern,
			DefaultNonRootUser,
			DefaultNonRootGroup,
			"", // No specific file mode
			"debug",
		)
		l.resources = append(l.resources, resource)

		ginkgo.By("Creating pod with a volume")
		pod := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, "")
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/file.txt", volPath)
		seed := time.Now().UTC().UnixNano()
		toWrite := 1024 // 1KB

		ginkgo.By("Checking write to a volume")
		checkWriteToPath(f, pod, fileInVol, toWrite, seed)

		ginkgo.By("Checking read from a volume")
		checkReadFromPath(f, pod, fileInVol, toWrite, seed)

		ginkgo.By("Checking file group owner")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '644 %d %d'", fileInVol, DefaultNonRootGroup, DefaultNonRootUser))

		ginkgo.By("Checking dir group owner")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '755 %d %d'", volPath, DefaultNonRootGroup, DefaultNonRootUser))

		ginkgo.By("Checking pod identity")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("id | grep 'uid=%d gid=%d groups=%d'", DefaultNonRootUser, DefaultNonRootGroup, DefaultNonRootGroup))
	}
	ginkgo.It("should access volume as a non-root user", func(ctx context.Context) {
		validateWriteToVolume(ctx)
	})

	// ---------------------------------------------------------------------------
	// Unsupported Mount-arg tests
	//
	// Context
	// -------
	// If any of these flags reach Mountpoint-S3 it refuses writes or targets the
	// wrong backend:
	//
	//   --endpoint-url            → traffic goes to the wrong place
	//   --cache-xz                → Express One Zone cache (unsupported)
	//   --incremental-upload      → Express One Zone append (unsupported)
	//   --storage-class=<non-STD> → non-STANDARD class (unsupported)
	//
	// Our CSI driver strips them.  The proof: create a PVC that *asks* for the
	// flag, run a pod, and show we can still write.
	//
	// Helper
	// ------
	// validateStrippedOption provisions a PVC with *one* or many bad flag and confirms
	// the pod can read-write, implying the flag was removed.
	// ---------------------------------------------------------------------------

	validateStrippedOption := func(ctx context.Context, badFlag, label string) {
		ginkgo.By(fmt.Sprintf("PVC with disallowed flag: %s", label))

		// Use BuildVolumeWithOptions with a single bad flag as extra option
		res := BuildVolumeWithOptions(
			ctx,
			l.config,
			pattern,
			DefaultNonRootUser,
			DefaultNonRootGroup,
			"", // No specific file mode
			badFlag,
		)
		l.resources = append(l.resources, res)

		ginkgo.By("Starting pod that mounts the PVC")
		pod, err := CreatePodWithVolumeAndSecurity(
			ctx,
			f,
			res.Pvc,
			fmt.Sprintf("policy-test-%s", strings.ReplaceAll(label, "-", "")), // Create a valid container name
			DefaultNonRootUser,
			DefaultNonRootGroup,
		)
		framework.ExpectNoError(err)
		defer func() { _ = e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) }()

		// Create a test file in the volume to verify mount works
		volPath := "/mnt/volume1"
		file := fmt.Sprintf("%s/policy-ok-%s.txt", volPath, label)
		WriteAndVerifyFile(
			f,
			pod,
			file,
			fmt.Sprintf("policy-strip %s @ %s", label, time.Now().Format(time.RFC3339)),
		)

		// verify we can create directories and check ownership
		testDir := fmt.Sprintf("%s/test-dir-%s", volPath, label)
		CreateDirInPod(f, pod, testDir)

		ginkgo.By("Checking directory ownership and permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod,
			fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '%d %d'",
				testDir, DefaultNonRootGroup, DefaultNonRootUser))
	}

	ginkgo.Describe("Mount arg policy enforcement", func() {
		ginkgo.It("strips --endpoint-url flag", func(ctx context.Context) {
			validateStrippedOption(ctx,
				"--endpoint-url=https://wrong.example.com",
				"endpoint-url",
			)
		})

		ginkgo.It("strips --cache-xz volume level mount flag", func(ctx context.Context) {
			validateStrippedOption(ctx, "--cache-xz", "cache-xz")
		})

		ginkgo.It("strips --incremental-upload volume level mount flag", func(ctx context.Context) {
			validateStrippedOption(ctx, "--incremental-upload", "incremental-upload")
		})

		ginkgo.It("strips --storage-class volume level mount flag", func(ctx context.Context) {
			validateStrippedOption(ctx,
				"--storage-class=EXPRESS_ONEZONE",
				"storage-class",
			)
		})

		ginkgo.It("strips --profile volume level mount flag", func(ctx context.Context) {
			validateStrippedOption(ctx,
				"--profile=my-aws-profile",
				"profile",
			)
		})

		ginkgo.It("strips all unsupported volume level mount flags when they arrive together", func(ctx context.Context) {
			ginkgo.By("PVC with every disallowed flag at once")

			// Use BuildVolumeWithOptions for the multi-option test with additional unsupported flags
			unsupportedFlags := []string{
				"--endpoint-url=https://wrong.example.com",
				"--cache-xz",
				"--incremental-upload",
				"--storage-class=EXPRESS_ONEZONE",
				"--profile=my-aws-profile",
			}

			res := BuildVolumeWithOptions(
				ctx,
				l.config,
				pattern,
				DefaultNonRootUser,
				DefaultNonRootGroup,
				"", // No specific file mode
				unsupportedFlags...,
			)
			l.resources = append(l.resources, res)

			// Pod + write test
			// Use CreatePodWithVolumeAndSecurity to create the pod with the same security context
			ginkgo.By("Creating pod with all disallowed flags in volume mount options")
			pod, err := CreatePodWithVolumeAndSecurity(
				ctx,
				f,
				res.Pvc,
				"multi-unsupported-flag-test",
				DefaultNonRootUser,
				DefaultNonRootGroup,
			)
			framework.ExpectNoError(err)
			defer func() { _ = e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) }()

			file := "/mnt/volume1/policy-multi-ok.txt"

			// Create a test file and directory as a more thorough functionality check
			testFile, testDir := CreateTestFileAndDir(f, pod, "/mnt/volume1", "policy-test")

			// Verify the test file and directory were created correctly
			ginkgo.By("Verifying test file and directory exist")
			e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("ls -la %s", testFile))
			e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("ls -la %s", testDir))

			// Also write a file with timestamp to document the test (double checking)
			WriteAndVerifyFile(
				f,
				pod,
				file,
				fmt.Sprintf("multi-unsupported-flag test @ %s", time.Now().Format(time.RFC3339)),
			)
		})
	})

	// This test verifies that when a volume is mounted with the read-only flag,
	// write operations to the volume fail with the expected permission error.
	//
	// Test scenario:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with read-only flag]
	//        |
	//        ↓
	//    [No write access]
	//
	// Expected results:
	// - Reading from the volume is successful
	// - All write operations (file creation, mkdir) fail with permission errors
	// - The error message contains "Read-only file system"
	//
	// This validates that the S3 CSI driver correctly enforces the read-only mount option.
	ginkgo.It("should enforce read-only flag when specified", func(ctx context.Context) {
		// Create volume with standard non-root options plus read-only flag
		resource := BuildVolumeWithOptions(
			ctx,
			l.config,
			pattern,
			DefaultNonRootUser,
			DefaultNonRootGroup,
			"",          // No specific file mode
			"read-only", // Add the read-only flag
		)
		l.resources = append(l.resources, resource)

		// Create pod with the read-only volume
		ginkgo.By("Creating pod with a read-only volume")
		pod := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, "")
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/test-file.txt", volPath)
		dirInVol := fmt.Sprintf("%s/test-dir", volPath)

		// Verify reading from volume works (list directory)
		ginkgo.By("Verifying read access to the volume")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("ls -la %s", volPath))

		// Try to create a file - should fail with read-only error
		ginkgo.By("Verifying write access is denied when creating a file")
		_, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("touch %s", fileInVol))
		if err == nil {
			framework.Failf("Expected write operation to fail on read-only volume, but it succeeded")
		}
		if !strings.Contains(stderr, "Read-only file system") {
			framework.Failf("Expected 'Read-only file system' error, but got: %s", stderr)
		}
		framework.Logf("Got expected error creating file: %s", stderr)

		// Try to create a directory - should also fail with read-only error
		ginkgo.By("Verifying write access is denied when creating a directory")
		_, stderr, err = e2evolume.PodExec(f, pod, fmt.Sprintf("mkdir -p %s", dirInVol))
		if err == nil {
			framework.Failf("Expected directory creation to fail on read-only volume, but it succeeded")
		}
		if !strings.Contains(stderr, "Read-only file system") {
			framework.Failf("Expected 'Read-only file system' error, but got: %s", stderr)
		}
		framework.Logf("Got expected error creating directory: %s", stderr)
	})
}
