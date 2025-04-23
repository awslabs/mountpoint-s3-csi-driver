// This file implements the mount options test suite, which verifies that the S3 CSI
// driver correctly handles volume mount options related to permissions, user/group IDs,
// and access controls when mounting S3 buckets in Kubernetes pods.
package customsuites

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// Constants for user/group IDs used in non-root access tests.
// These values must be different from root (0) and must match what's expected
// by the test infrastructure.
const (
	defaultNonRootUser  = int64(1001)
	defaultNonRootGroup = int64(2000)
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
		// Create volume with mount options that should allow non-root access:
		// - uid/gid set to non-root values
		// - allow-other to permit access by users other than the mounter
		// - debug flags for better logging in case of issues
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{
			fmt.Sprintf("uid=%d", defaultNonRootUser),
			fmt.Sprintf("gid=%d", defaultNonRootGroup),
			"allow-other",
			"debug",
		})
		l.resources = append(l.resources, resource)
		ginkgo.By("Creating pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)
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
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '644 %d %d'", fileInVol, defaultNonRootGroup, defaultNonRootUser))
		ginkgo.By("Checking dir group owner")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '755 %d %d'", volPath, defaultNonRootGroup, defaultNonRootUser))
		ginkgo.By("Checking pod identity")
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("id | grep 'uid=%d gid=%d groups=%d'", defaultNonRootUser, defaultNonRootGroup, defaultNonRootGroup))
	}
	ginkgo.It("should access volume as a non-root user", func(ctx context.Context) {
		validateWriteToVolume(ctx)
	})

	// accessVolAsNonRootUser is a helper function that tests that access is properly denied
	// when mount options for non-root access are NOT provided.
	//
	// This function:
	// 1. Creates a volume with NO special mount options
	// 2. Creates a pod that runs as non-root and tries to access this volume
	// 3. Verifies that access is denied due to lack of permissions
	//
	// This is security test to ensure volumes aren't accessible
	// to non-root users unless explicitly configured to allow such access.
	accessVolAsNonRootUser := func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{})
		l.resources = append(l.resources, resource)
		ginkgo.By("Creating pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()
		volPath := "/mnt/volume1"
		ginkgo.By("Checking file group owner")
		_, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("ls %s", volPath))
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(stderr).To(gomega.ContainSubstring("Permission denied"))
	}
	ginkgo.It("should not be able to access volume as a non-root user", func(ctx context.Context) {
		accessVolAsNonRootUser(ctx)
	})
}
