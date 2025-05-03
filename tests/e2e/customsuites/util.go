// This file contains utility functions that support the mount options test suite.
// It provides helpers for file operations, pod configuration, and PV/PVC creation
// needed for testing S3 CSI driver mount functionality.
package customsuites

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
)

// Constants for non-root user/group IDs used in pod security contexts
const (
	DefaultNonRootUser  = int64(1001)
	DefaultNonRootGroup = int64(2000)
)

// genBinDataFromSeed generates binary data with random seed for testing file operations.
// This is useful for creating consistent test data that can be verified after read operations.
//
// Parameters:
// - len: size of the data to generate in bytes
// - seed: random seed to ensure reproducibility across test operations
//
// Returns a byte slice containing the generated data.
func genBinDataFromSeed(len int, seed int64) []byte {
	binData := make([]byte, len)
	randLocal := rand.New(rand.NewSource(seed))

	_, err := randLocal.Read(binData)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}

	return binData
}

// checkWriteToPath writes data to a file in the pod and verifies it succeeded.
// This function:
// 1. Generates random data using the provided seed
// 2. Encodes it to base64 for safe transmission to the pod
// 3. Executes commands in the pod to write the data to the specified path
// 4. Verifies the operation succeeded
//
// This is a core test utility for validating write access to mounted volumes.
func checkWriteToPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	data := genBinDataFromSeed(toWrite, seed)
	encoded := base64.StdEncoding.EncodeToString(data)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | sha256sum", encoded))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | dd of=%s bs=%d count=1", encoded, path, toWrite))
	framework.Logf("written data with sha: %x", sha256.Sum256(data))
}

// checkReadFromPath reads data from a file in the pod and verifies its content.
// This function:
// 1. Calculates the expected SHA256 hash based on the same seed used for writing
// 2. Reads the file content in the pod
// 3. Computes a hash of the content
// 4. Verifies the hash matches the expected value
//
// This ensures that data integrity is maintained throughout volume operations.
func checkReadFromPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	sum := sha256.Sum256(genBinDataFromSeed(toWrite, seed))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum", path, toWrite))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum | grep -Fq %x", path, toWrite, sum))
}

// podModifierNonRoot modifies a pod to run as a non-root user.
// This utility function:
// 1. Configures the pod's security context to use non-root user/group IDs
// 2. Sets the RunAsNonRoot flag to enforce non-root execution
// 3. Applies the same settings to all containers in the pod
//
// This is essential for testing permission boundaries and security aspects
// of volume mounts, ensuring they respect proper access controls.
func podModifierNonRoot(pod *v1.Pod) {
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{}
	}
	pod.Spec.SecurityContext.RunAsUser = ptr.To(DefaultNonRootUser)
	pod.Spec.SecurityContext.RunAsGroup = ptr.To(DefaultNonRootGroup)
	pod.Spec.SecurityContext.RunAsNonRoot = ptr.To(true)

	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].SecurityContext == nil {
			pod.Spec.Containers[i].SecurityContext = &v1.SecurityContext{}
		}
		pod.Spec.Containers[i].SecurityContext.RunAsUser = ptr.To(DefaultNonRootUser)
		pod.Spec.Containers[i].SecurityContext.RunAsGroup = ptr.To(DefaultNonRootGroup)
		pod.Spec.Containers[i].SecurityContext.RunAsNonRoot = ptr.To(true)
	}
}

// createPod creates a pod with PVC mounts and waits for it to be running.
// This function:
// 1. Submits the pod creation request to the Kubernetes API
// 2. Waits for the pod to reach Running state
// 3. Fetches the latest pod information after it's running
//
// This utility handles common pod creation patterns needed for volume testing,
// ensuring pods are fully ready before tests proceed to volume operations.
func createPod(ctx context.Context, client clientset.Interface, namespace string, pod *v1.Pod) (*v1.Pod, error) {
	framework.Logf("Creating Pod %s in %s", pod.Name, namespace)
	pod, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod Create API error: %w", err)
	}
	// Waiting for pod to be running
	err = e2epod.WaitForPodNameRunningInNamespace(ctx, client, pod.Name, namespace)
	if err != nil {
		return pod, fmt.Errorf("pod %q is not Running: %w", pod.Name, err)
	}
	// get fresh pod info
	pod, err = client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return pod, fmt.Errorf("pod Get API error: %w", err)
	}
	return pod, nil
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
func createVolumeResourceWithMountOptions(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern, mountOptions []string) *storageframework.VolumeResource {
	f := config.Framework
	r := storageframework.VolumeResource{
		Config:  config,
		Pattern: pattern,
	}
	pDriver, _ := config.Driver.(storageframework.PreprovisionedPVTestDriver)
	r.Volume = pDriver.CreateVolume(ctx, config, storageframework.PreprovisionedPV)
	pvSource, volumeNodeAffinity := pDriver.GetPersistentVolumeSource(false, "", r.Volume)

	pvName := fmt.Sprintf("s3-e2e-pv-%s", uuid.New().String())
	pvcName := fmt.Sprintf("s3-e2e-pvc-%s", uuid.New().String())

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: *pvSource,
			StorageClassName:       "", // for static provisioning
			NodeAffinity:           volumeNodeAffinity,
			MountOptions:           mountOptions, // this is not set by kubernetes storageframework.CreateVolumeResource, which is why we need this function
			AccessModes:            []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1200Gi"),
			},
			ClaimRef: &v1.ObjectReference{
				Name:      pvcName,
				Namespace: f.Namespace.Name,
			},
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: f.Namespace.Name,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(""), // for static provisioning
			VolumeName:       pvName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: resource.MustParse("1200Gi"),
				},
			},
		},
	}

	framework.Logf("Creating PVC and PV")
	var err error

	r.Pv, err = f.ClientSet.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	framework.ExpectNoError(err, "PV creation failed")

	r.Pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
	framework.ExpectNoError(err, "PVC creation failed")

	err = e2epv.WaitOnPVandPVC(ctx, f.ClientSet, f.Timeouts, f.Namespace.Name, r.Pv, r.Pvc)
	framework.ExpectNoError(err, "PVC, PV failed to bind")
	return &r
}

// BuildVolumeWithOptions creates a volume with specified UID, GID, file mode, and optional extra options.
// This function builds a slice of mount options and then creates a volume resource with those options.
//
// Parameters:
// - ctx: Context for the API calls
// - config: Per-test configuration
// - pattern: Test pattern to use
// - uid: User ID to set for the mounted volume
// - gid: Group ID to set for the mounted volume
// - fileModeOption: Optional file mode to set (e.g., "0600") - if empty, default permissions are used
// - extraOptions: Additional mount options to include
//
// Returns a volume resource ready for use in tests.
func BuildVolumeWithOptions(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern,
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

	return createVolumeResourceWithMountOptions(ctx, config, pattern, options)
}

// CreatePodWithVolumeAndSecurity creates a pod with the specified volume and security context settings.
// This function handles common pod creation operations for test cases:
// 1. Creates a pod specification with the provided PVC mounted
// 2. Applies the specified UID and GID to the pod's security context
// 3. Sets an optional container name if provided
// 4. Creates the pod and waits for it to reach Running state
//
// Parameters:
// - ctx: Context for the API calls
// - f: Framework for the test case
// - volume: PersistentVolumeClaim to mount in the pod
// - containerName: Optional name for the main container (empty string for default)
// - uid: User ID to set in the pod's security context
// - gid: Group ID to set in the pod's security context
//
// Returns the created pod and any error that occurred.
func CreatePodWithVolumeAndSecurity(ctx context.Context, f *framework.Framework, volume *v1.PersistentVolumeClaim,
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
	return createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
}

// MakeNonRootPodWithVolume creates a pod specification with the given PVCs,
// applying the DefaultNonRootUser/Group security context, and setting an optional container name.
//
// This is a helper function to create consistent pod specs without having to repeatedly call
// e2epod.MakePod followed by podModifierNonRoot and container name setting.
//
// Parameters:
// - namespace: The namespace for the pod
// - pvcs: The PVCs to mount in the pod
// - containerName: Optional name for the main container (empty string for default)
//
// Returns the created pod specification (not yet submitted to the API).
func MakeNonRootPodWithVolume(namespace string, pvcs []*v1.PersistentVolumeClaim, containerName string) *v1.Pod {
	// Create pod spec
	pod := e2epod.MakePod(namespace, nil, pvcs, admissionapi.LevelRestricted, "")

	// Apply nonroot security context
	podModifierNonRoot(pod)

	// Set container name if specified
	if containerName != "" {
		pod.Spec.Containers[0].Name = containerName
	}

	return pod
}

// copySmallFileToPod copies a small file from host to pod
func copySmallFileToPod(_ context.Context, f *framework.Framework, pod *v1.Pod, srcFile, destFile string) {
	content, err := os.ReadFile(srcFile)
	framework.ExpectNoError(err)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat > %s << 'EOF'\n%s\nEOF", destFile, string(content)))
}

// CreateTestFileAndDir creates a test file and directory at the given base path.
// This function is a helper for test cases that need to verify file/directory operations.
//
// Parameters:
// - f: Framework for the test case
// - pod: Pod where the file and directory will be created
// - basePath: Base directory path where test file and directory will be created
// - fileNamePrefix: Prefix for the test file and directory names
//
// Returns the paths to the created file and directory.
func CreateTestFileAndDir(f *framework.Framework, pod *v1.Pod, basePath string, fileNamePrefix string) (string, string) {
	testFile := fmt.Sprintf("%s/%s.txt", basePath, fileNamePrefix)
	testDir := fmt.Sprintf("%s/%s-dir", basePath, fileNamePrefix)

	ginkgo.By(fmt.Sprintf("Creating test file %s", testFile))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo 'test content' > %s", testFile))

	ginkgo.By(fmt.Sprintf("Creating test directory %s", testDir))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", testDir))

	return testFile, testDir
}

// CreateFileInPod creates a file with the given content in a pod.
//
// Parameters:
// - f: Framework for the test case
// - pod: Pod where the file will be created
// - path: Full path to the file to create
// - content: Content to write to the file
func CreateFileInPod(f *framework.Framework, pod *v1.Pod, path, content string) {
	ginkgo.By(fmt.Sprintf("Creating file at %s", path))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo '%s' > %s", content, path))
}

// CreateDirInPod creates a directory (and parent directories as needed) in a pod.
//
// Parameters:
// - f: Framework for the test case
// - pod: Pod where the directory will be created
// - path: Full path to the directory to create
func CreateDirInPod(f *framework.Framework, pod *v1.Pod, path string) {
	ginkgo.By(fmt.Sprintf("Creating directory at %s", path))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", path))
}

// CreateMultipleDirsInPod creates multiple directories in a pod with a single command.
//
// Parameters:
// - f: Framework for the test case
// - pod: Pod where the directories will be created
// - paths: Array of full paths to create
func CreateMultipleDirsInPod(f *framework.Framework, pod *v1.Pod, paths ...string) {
	ginkgo.By(fmt.Sprintf("Creating multiple directories: %v", paths))
	dirsArg := strings.Join(paths, " ")
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", dirsArg))
}

// CopyFileInPod copies a file within a pod.
//
// Parameters:
// - f: Framework for the test case
// - pod: Pod where the file copy operation will occur
// - sourcePath: Path to the source file
// - targetPath: Path to the target file
func CopyFileInPod(f *framework.Framework, pod *v1.Pod, sourcePath, targetPath string) {
	ginkgo.By(fmt.Sprintf("Copying file from %s to %s", sourcePath, targetPath))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cp %s %s", sourcePath, targetPath))
}

// DeleteFileInPod deletes a file in a pod.
//
// Parameters:
// - f: Framework for the test case
// - pod: Pod where the file deletion will occur
// - path: Path to the file to delete
func DeleteFileInPod(f *framework.Framework, pod *v1.Pod, path string) {
	ginkgo.By(fmt.Sprintf("Deleting file at %s", path))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("rm %s", path))
}
