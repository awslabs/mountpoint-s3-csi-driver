package customsuites

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
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

// s3CSIMultiVolumeTestSuite implements a test suite for multi-volume scenarios
// with the S3 CSI driver, including sharing volumes between pods and mounting
// multiple volumes in a single pod.
type s3CSIMultiVolumeTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3MultiVolumeTestSuite initializes and returns a test suite that validates
// multi-volume functionality for the S3 CSI driver.
//
// This suite specifically tests:
// - Multiple pods accessing the same S3 volume simultaneously
// - A single pod accessing multiple S3 volumes concurrently
// - Data persistence across pod recreations with the same volume
//
// The test suite verifies the core functionality needed for both stateless and
// stateful workloads in Kubernetes when using S3 CSI volumes.
func InitS3MultiVolumeTestSuite() storageframework.TestSuite {
	return &s3CSIMultiVolumeTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "multivolume",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

// GetTestSuiteInfo returns the test suite information including name and test patterns.
func (t *s3CSIMultiVolumeTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

// SkipUnsupportedTests allows test suites to skip certain tests based on driver capabilities.
// For S3 multi-volume scenarios, all tests should be supported, so this is a no-op.
func (t *s3CSIMultiVolumeTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

// DefineTests implements the test suite by defining all the test cases for multi-volume
// scenarios. It creates the necessary volume resources, pods, and validation logic.
func (t *s3CSIMultiVolumeTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
		driver    storageframework.TestDriver
	}
	var (
		l local
	)

	f := framework.NewFrameworkWithCustomTimeouts("multivolume", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelRestricted

	// Clean up resources after each test
	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}

	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{
			driver: driver,
		}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	// Common mount options needed for write access by non-root user
	mountOptions := []string{
		fmt.Sprintf("uid=%d", DefaultNonRootUser),
		fmt.Sprintf("gid=%d", DefaultNonRootGroup),
		"allow-other",
		"debug",
		"debug-crt",
	}

	// Test 1: Multiple pods accessing the same volume
	// This test validates the following configuration:
	//
	//   [Pod-1]     [Pod-2]
	//      \           /
	//       \         /
	//        \       /
	//     [Single S3 Volume]
	//           |
	//       [S3 Bucket]
	//
	// Validates that multiple pods can simultaneously mount the same volume
	// and read/write data that's visible to all pods.
	//
	// This test verifies:
	// - RWX access mode functionality with S3 volumes
	// - Cross-pod visibility of data
	// - Concurrent read/write operations between pods
	// - Execute permission across pods
	ginkgo.It("should allow multiple pods to access the same volume", func(ctx context.Context) {
		// Create volume resource with mount options
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, mountOptions)
		l.resources = append(l.resources, resource)

		// Create first pod with the volume
		ginkgo.By("Creating first pod with the volume")
		pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		pod1.Name = fmt.Sprintf("pod1-%s", uuid.New().String()[:8])
		podModifierNonRoot(pod1)
		pod1, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
		framework.ExpectNoError(err)

		// Create second pod with the same volume
		ginkgo.By("Creating second pod with the same volume")
		pod2 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		pod2.Name = fmt.Sprintf("pod2-%s", uuid.New().String()[:8])
		podModifierNonRoot(pod2)
		pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
		framework.ExpectNoError(err)

		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1))
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2))
		}()

		// Write data from pod1 and verify it can be read from pod2
		ginkgo.By("Writing data from pod1")
		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/shared-file.txt", volPath)
		seed := time.Now().UTC().UnixNano()
		toWrite := 1024 // 1KB

		checkWriteToPath(f, pod1, fileInVol, toWrite, seed)

		ginkgo.By("Reading data from pod2")
		checkReadFromPath(f, pod2, fileInVol, toWrite, seed)

		ginkgo.By("Verifying file exists on pod2")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("ls -la %s", fileInVol))

		// Write from pod2 and verify on pod1 to check bidirectional access
		ginkgo.By("Writing data from pod2")
		fileInVol2 := fmt.Sprintf("%s/pod2-file.txt", volPath)
		seed2 := time.Now().UTC().UnixNano()

		checkWriteToPath(f, pod2, fileInVol2, toWrite, seed2)

		ginkgo.By("Reading data from pod1")
		checkReadFromPath(f, pod1, fileInVol2, toWrite, seed2)

		// Test execute functionality (the "X" in RWX) without relying on chmod
		ginkgo.By("Creating a shell script on pod1")
		scriptPath := fmt.Sprintf("%s/test-script.sh", volPath)
		e2evolume.VerifyExecInPodSucceed(f, pod1, fmt.Sprintf("echo '#!/bin/sh\necho \"Hello from shared script\"' > %s", scriptPath))

		// chmod gives operation not permitted error which is expecrted for mountpoint-s3, so we execute with 'sh' directly
		ginkgo.By("Executing script content from pod2 using sh")
		stdout, stderr, err := e2evolume.PodExec(f, pod2, fmt.Sprintf("sh %s", scriptPath))
		framework.ExpectNoError(err, "failed to execute script: %s, stderr: %s", stdout, stderr)
		gomega.Expect(stdout).To(gomega.ContainSubstring("Hello from shared script"))

		// Also verify execution from pod1 (the creator)
		ginkgo.By("Executing script content from pod1 (creator) using sh")
		stdout, stderr, err = e2evolume.PodExec(f, pod1, fmt.Sprintf("sh %s", scriptPath))
		framework.ExpectNoError(err, "failed to execute script on creator pod: %s, stderr: %s", stdout, stderr)
		gomega.Expect(stdout).To(gomega.ContainSubstring("Hello from shared script"))
	})

	// Test 2: Single pod accessing multiple volumes
	// This test validates the following configuration:
	//
	//          [Pod]
	//         /     \
	//        /       \
	//       /         \
	//  [Volume-1]  [Volume-2]
	//      |           |
	//  [Bucket-1]  [Bucket-2]
	//
	// Validates that a single pod can mount multiple S3 volumes
	// and access them independently without cross-contamination.
	//
	// This test verifies:
	// - Multiple S3 volume mounts within a single pod
	// - Volume isolation (data in one volume is not visible in another)
	// - Proper mount path handling for multiple volumes
	ginkgo.It("should allow a pod to access multiple volumes", func(ctx context.Context) {
		// Create first volume resource with mount options
		resource1 := createVolumeResourceWithMountOptions(ctx, l.config, pattern, mountOptions)
		l.resources = append(l.resources, resource1)

		// Create second volume resource with mount options
		resource2 := createVolumeResourceWithMountOptions(ctx, l.config, pattern, mountOptions)
		l.resources = append(l.resources, resource2)

		// Create pod with both volumes
		ginkgo.By("Creating pod with multiple volumes")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource1.Pvc, resource2.Pvc}, admissionapi.LevelRestricted, "")
		pod.Name = fmt.Sprintf("multipod-%s", uuid.New().String()[:8])
		podModifierNonRoot(pod)

		// Modify the pod to have two distinct mount paths for the volumes
		pod.Spec.Containers[0].VolumeMounts[0].MountPath = "/mnt/volume1"
		pod.Spec.Containers[0].VolumeMounts[1].MountPath = "/mnt/volume2"

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)

		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		// Write and read data from both volumes
		ginkgo.By("Writing data to first volume")
		vol1Path := "/mnt/volume1"
		fileInVol1 := fmt.Sprintf("%s/file1.txt", vol1Path)
		seed1 := time.Now().UTC().UnixNano()
		toWrite := 1024 // 1KB

		checkWriteToPath(f, pod, fileInVol1, toWrite, seed1)
		checkReadFromPath(f, pod, fileInVol1, toWrite, seed1)

		ginkgo.By("Writing data to second volume")
		vol2Path := "/mnt/volume2"
		fileInVol2 := fmt.Sprintf("%s/file2.txt", vol2Path)
		seed2 := time.Now().UTC().UnixNano() + 1 // Different seed

		checkWriteToPath(f, pod, fileInVol2, toWrite, seed2)
		checkReadFromPath(f, pod, fileInVol2, toWrite, seed2)

		// Verify the volumes are indeed separate by checking file existence
		ginkgo.By("Verifying volume separation")
		_, stderr, err := e2evolume.PodExec(f, pod, fmt.Sprintf("ls %s", fmt.Sprintf("%s/file2.txt", vol1Path)))
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(stderr).To(gomega.ContainSubstring("No such file or directory"))

		_, stderr, err = e2evolume.PodExec(f, pod, fmt.Sprintf("ls %s", fmt.Sprintf("%s/file1.txt", vol2Path)))
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(stderr).To(gomega.ContainSubstring("No such file or directory"))
	})

	// Test 3: Data persistence across pod recreations
	// This test validates the following configuration:
	//
	//  [Pod-1 (writes)]  →  ✓ Delete Pod-1 →  [Pod-2 (reads)]
	//         |                               |
	//         |                               |
	//         ↓                               ↓
	//   [S3 Volume] -------------------- [Same S3 Volume]
	//        |                                |
	//    [S3 Bucket] ------------------- [Same S3 Bucket]
	//
	// Validates that data written to a volume persists after
	// pod deletion when a new pod mounts the same volume.
	//
	// This test verifies:
	// - Data persistence for S3 volumes between pod lifecycles
	// - Correct remounting of existing volumes with preserved data
	// - Fundamental stateful application support with S3 CSI driver
	ginkgo.It("should preserve data when pod is deleted and recreated", func(ctx context.Context) {
		// Create volume resource with mount options
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, mountOptions)
		l.resources = append(l.resources, resource)

		// Create first pod with the volume
		ginkgo.By("Creating first pod with the volume")
		pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		pod1.Name = fmt.Sprintf("persist-pod1-%s", uuid.New().String()[:8])
		podModifierNonRoot(pod1)
		pod1, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
		framework.ExpectNoError(err)

		// Write data from pod1
		ginkgo.By("Writing data from first pod")
		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/persist-file.txt", volPath)
		seed := time.Now().UTC().UnixNano()
		toWrite := 1024 // 1KB

		checkWriteToPath(f, pod1, fileInVol, toWrite, seed)

		// Delete pod1
		ginkgo.By("Deleting the first pod")
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1))

		// Create second pod with the same volume
		ginkgo.By("Creating second pod with the same volume")
		pod2 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		pod2.Name = fmt.Sprintf("persist-pod2-%s", uuid.New().String()[:8])
		podModifierNonRoot(pod2)
		pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
		framework.ExpectNoError(err)

		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2))
		}()

		// Verify data persists by reading from pod2
		ginkgo.By("Reading data from second pod")
		checkReadFromPath(f, pod2, fileInVol, toWrite, seed)

		// Verify file exists on pod2
		ginkgo.By("Verifying file exists on second pod")
		e2evolume.VerifyExecInPodSucceed(f, pod2, fmt.Sprintf("ls -la %s", fileInVol))
	})
}
