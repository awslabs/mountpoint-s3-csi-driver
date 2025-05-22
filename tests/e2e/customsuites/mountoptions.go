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

	"github.com/scality/mountpoint-s3-csi-driver/tests/e2e/pkg/s3client"
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
	var l local

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

	// This test verifies that when a volume is mounted with a specific region option,
	// the CSI driver correctly passes it to mountpoint-s3 and allows write operations.
	//
	// Test scenario:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with region=us-east-1]
	//        |
	//        ↓
	//    [Write operations should succeed]
	//
	// Expected results:
	// - The pod can mount the volume successfully with a specified region
	// - Write operations to the volume succeed
	// - Files created have the expected ownership and permissions
	//
	// This validates that the S3 CSI driver correctly passes the region mount option
	// to mountpoint-s3 and that the driver can correctly connect to S3 in that region.
	ginkgo.It("should successfully write to volume with region specified", func(ctx context.Context) {
		// Create volume with standard non-root options plus region option
		// Note: using a valid region for the test bucket
		resource := BuildVolumeWithOptions(
			ctx,
			l.config,
			pattern,
			DefaultNonRootUser,
			DefaultNonRootGroup,
			"",                 // No specific file mode
			"region=sa-east-1", // Specify a region
		)
		l.resources = append(l.resources, resource)

		// Create pod with the volume
		ginkgo.By("Creating pod with a volume that specifies region")
		pod := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, "")
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/region-test.txt", volPath)
		testContent := "Testing region option"

		// Verify we can write to the volume
		ginkgo.By("Verifying write access to the volume")
		WriteAndVerifyFile(f, pod, fileInVol, testContent)

		// Verify file permissions and ownership
		ginkgo.By("Verifying file ownership and permissions")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '644 %d %d'",
			fileInVol, DefaultNonRootGroup, DefaultNonRootUser))

		// Verify we can read what we wrote
		ginkgo.By("Verifying read from the volume")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q '%s'", fileInVol, testContent))
	})

	// This test verifies that when a volume is mounted with the --prefix option,
	// but no files are created, the prefix doesn't exist in the bucket.
	//
	// Test scenario:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with --prefix=test-prefix/]
	//        |
	//        ↓
	//    [No prefix created]
	//
	// The test specifically checks:
	// 1. The prefix doesn't exist in the bucket before mounting
	// 2. The volume with prefix option can be successfully mounted and accessed
	// 3. The prefix still doesn't exist in the bucket after mounting
	//
	// This validates that the S3 CSI driver doesn't implicitly create the prefix
	// in the S3 bucket just by mounting with the prefix option.
	ginkgo.It("should not create prefix in bucket when no files are created", func(ctx context.Context) {
		// We need to access the S3 client directly to verify objects
		s3Client := s3client.New("", "", "") // Default credentials/region
		var err error

		// Create volume with standard non-root options plus prefix option
		prefix := "empty-prefix/"
		resource := BuildVolumeWithOptions(
			ctx,
			l.config,
			pattern,
			DefaultNonRootUser,
			DefaultNonRootGroup,
			"",                               // No specific file mode
			fmt.Sprintf("prefix=%s", prefix), // Add the prefix option
		)
		l.resources = append(l.resources, resource)

		// Extract the bucket name from the volume for verification
		bucketName := GetBucketNameFromVolumeResource(resource)
		if bucketName == "" {
			framework.Failf("Failed to extract bucket name from volume resource")
		}

		// List all objects in the bucket to verify the prefix doesn't exist BEFORE mounting
		ginkgo.By("Verifying prefix doesn't exist in bucket before mounting")
		rootListOutputBefore, err := s3Client.ListObjects(ctx, bucketName)
		framework.ExpectNoError(err, "Failed to list objects in bucket before mounting")

		// Check if any objects with the prefix exist before mounting
		prefixExistsBefore := false
		for _, obj := range rootListOutputBefore.Contents {
			if strings.HasPrefix(*obj.Key, prefix) {
				prefixExistsBefore = true
				break
			}
		}

		if prefixExistsBefore {
			framework.Failf("Prefix %s already exists in bucket %s before mounting", prefix, bucketName)
		} else {
			framework.Logf("Verified prefix %s does not exist in bucket %s before mounting", prefix, bucketName)
		}

		// Create pod with the prefixed volume
		ginkgo.By("Creating pod with a volume using prefix option")
		pod := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, "")
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Verify the mount point exists and is accessible
		volPath := "/mnt/volume1"
		ginkgo.By("Verifying volume is mounted and accessible")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("ls -la %s", volPath))

		// List all objects in the bucket to verify the prefix doesn't exist AFTER mounting
		ginkgo.By("Verifying prefix doesn't exist in bucket after mounting")
		rootListOutputAfter, err := s3Client.ListObjects(ctx, bucketName)
		framework.ExpectNoError(err, "Failed to list objects in bucket after mounting")

		// Check if any objects with the prefix exist after mounting
		prefixExistsAfter := false
		for _, obj := range rootListOutputAfter.Contents {
			if strings.HasPrefix(*obj.Key, prefix) {
				prefixExistsAfter = true
				break
			}
		}

		if prefixExistsAfter {
			framework.Failf("Prefix %s was created in bucket %s just by mounting", prefix, bucketName)
		} else {
			framework.Logf("Verified prefix %s was not created in bucket %s just by mounting", prefix, bucketName)
		}
	})

	// This test verifies that the --prefix mount option correctly isolates
	// objects within a specific prefix in the S3 bucket.
	//
	// Test scenario:
	//
	//      [Pod]
	//        |
	//        ↓
	//   [S3 Volume with --prefix=test-prefix/]
	//        |
	//        ↓
	//    [Files stored under test-prefix/ in S3]
	//
	// Expected results:
	// - Files created in the mounted volume are stored under the specified prefix in S3
	// - The files can be accessed through the mounted path without the prefix in the path
	// - No objects are created at the root of the bucket (outside the prefix)
	//
	// This validates that the S3 CSI driver correctly handles the --prefix mount option
	// to properly isolate multiple users or applications within a single bucket.
	ginkgo.It("should store files under specified prefix when using --prefix option", func(ctx context.Context) {
		// We need to access the S3 client directly to verify objects
		s3Client := s3client.New("", "", "") // Default credentials/region

		// Create volume with standard non-root options plus prefix option
		prefix := "test-prefix/"
		resource := BuildVolumeWithOptions(
			ctx,
			l.config,
			pattern,
			DefaultNonRootUser,
			DefaultNonRootGroup,
			"",                               // No specific file mode
			fmt.Sprintf("prefix=%s", prefix), // Add the prefix option
		)
		l.resources = append(l.resources, resource)

		// Extract the bucket name from the volume for verification
		bucketName := GetBucketNameFromVolumeResource(resource)
		if bucketName == "" {
			framework.Failf("Failed to extract bucket name from volume resource")
		}

		// Create pod with the prefixed volume
		ginkgo.By("Creating pod with a volume using prefix option")
		pod := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, "")
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		volPath := "/mnt/volume1"
		testFileName := "prefix-test.txt"
		fileInVol := fmt.Sprintf("%s/%s", volPath, testFileName)
		testContent := "Testing prefix mount option"

		// Write a file to the volume
		ginkgo.By("Writing a file to the volume")
		WriteAndVerifyFile(f, pod, fileInVol, testContent)

		// Verify file can be read from the pod
		ginkgo.By("Verifying file can be read from pod")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q '%s'", fileInVol, testContent))

		// List objects in the bucket to verify the object was created under the prefix
		ginkgo.By(fmt.Sprintf("Verifying object exists under prefix %s in bucket", prefix))

		// Use s3client's VerifyObjectsExistInS3 method instead of manual listing and checking
		err = s3Client.VerifyObjectsExistInS3(ctx, bucketName, prefix, []string{testFileName})
		framework.ExpectNoError(err, "Failed to verify object exists under prefix %s", prefix)
		framework.Logf("Successfully found object %s under prefix %s in bucket %s", testFileName, prefix, bucketName)

		// List objects in the bucket without the prefix to verify no objects exist at the root
		ginkgo.By("Verifying no objects exist at the root of the bucket")
		rootListOutput, err := s3Client.ListObjects(ctx, bucketName)
		framework.ExpectNoError(err, "Failed to list objects in bucket")

		// Verify no objects exist at the root (that don't have the prefix)
		for _, obj := range rootListOutput.Contents {
			if !strings.HasPrefix(*obj.Key, prefix) {
				framework.Failf("Found unexpected object %s at root of bucket", *obj.Key)
			}
		}
		framework.Logf("No unexpected objects found at root of bucket - all objects have the prefix %s", prefix)
	})

	// This test verifies that when objects are created directly in S3 under a prefix,
	// they are visible when mounting with that prefix, and new objects created through
	// the mount are also visible when listing the prefix directly from S3.
	//
	// Test scenario:
	//
	//      [Direct S3 API]  →  [Objects under prefix]  ←  [Mounted Volume]
	//
	// The test specifically:
	// 1. Creates objects directly in S3 under a specific prefix
	// 2. Mounts a volume with that same prefix
	// 3. Verifies the pre-created objects are visible through the mount
	// 4. Creates new objects through the mount
	// 5. Verifies the new objects are visible when listing the prefix via S3 API
	//
	// This validates that the prefix mount option works bidirectionally with objects
	// created both through the S3 API and through the mounted volume.
	ginkgo.It("should see objects created directly in S3 under prefix and make new objects visible to S3", func(ctx context.Context) {
		// We need to access the S3 client directly to create and list objects
		s3Client := s3client.New(s3client.DefaultRegion, "", "") // Using DefaultRegion from s3client
		var err error

		// Create volume with standard non-root options plus prefix option
		prefix := "test-both-directions/"
		resource := BuildVolumeWithOptions(
			ctx,
			l.config,
			pattern,
			DefaultNonRootUser,
			DefaultNonRootGroup,
			"",                               // No specific file mode
			fmt.Sprintf("prefix=%s", prefix), // Add the prefix option
		)
		l.resources = append(l.resources, resource)

		// Extract the bucket name from the volume for direct S3 operations
		bucketName := GetBucketNameFromVolumeResource(resource)
		if bucketName == "" {
			framework.Failf("Failed to extract bucket name from volume resource")
		}

		directFileKeys := []string{
			"direct-file1.txt",
			"direct-file2.txt",
			"subdir/direct-file3.txt",
		}

		// Create objects directly in S3 under the prefix
		ginkgo.By(fmt.Sprintf("Creating objects directly in S3 under prefix %s", prefix))
		err = s3Client.CreateObjectsInS3(ctx, bucketName, prefix, directFileKeys)
		framework.ExpectNoError(err, "Failed to create objects directly in S3")

		// Verify objects exist in S3
		ginkgo.By(fmt.Sprintf("Verifying objects exist in S3 under prefix %s", prefix))
		err = s3Client.VerifyObjectsExistInS3(ctx, bucketName, prefix, directFileKeys)
		framework.ExpectNoError(err, "Failed to verify objects exist in S3")

		// Create pod with the prefixed volume
		ginkgo.By("Creating pod with a volume that uses the same prefix")
		pod := MakeNonRootPodWithVolume(f.Namespace.Name, []*v1.PersistentVolumeClaim{resource.Pvc}, "")
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Verify the directly created files are visible through the mount
		volPath := "/mnt/volume1"
		ginkgo.By("Verifying directly created files are visible through the mount")

		// Verify files exist in pod using our helper method
		VerifyFilesExistInPod(f, pod, volPath, directFileKeys)

		// Additional verification for subdirectory
		subdirPath := fmt.Sprintf("%s/subdir", volPath)
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("test -d %s", subdirPath))

		// Define mount-created file paths (these will be created through the mount)
		mountCreatedFiles := []string{
			"mount-file1.txt",
			"mount-file2.txt",
			"subdir/mount-file3.txt",
		}

		// Create files in pod using our helper method
		CreateFilesInPod(f, pod, volPath, mountCreatedFiles)

		// Verify new files are visible via S3 API with the prefix
		ginkgo.By("Verifying new files are visible via S3 API with the prefix")

		// Verify objects exist in S3
		err = s3Client.VerifyObjectsExistInS3(ctx, bucketName, prefix, mountCreatedFiles)
		framework.ExpectNoError(err, "Failed to verify mount-created objects exist in S3")

		// Additional verification that all objects are present (both direct and mount-created)
		allFiles := append([]string{}, directFileKeys...)
		allFiles = append(allFiles, mountCreatedFiles...)

		prefixListAfter, err := s3Client.ListObjectsWithPrefix(ctx, bucketName, prefix)
		framework.ExpectNoError(err, "Failed to list objects with prefix after creating files through mount")

		// We should have all files (direct + mount-created)
		if len(prefixListAfter.Contents) < len(allFiles) {
			framework.Failf("Expected at least %d objects after creating files through mount, but found %d",
				len(allFiles), len(prefixListAfter.Contents))
		}

		framework.Logf("Successfully verified bidirectional visibility between S3 API and mounted volume with prefix")
	})
}
