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

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/utils/ptr"
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
	pod.Spec.SecurityContext.RunAsUser = ptr.To(defaultNonRootUser)
	pod.Spec.SecurityContext.RunAsGroup = ptr.To(defaultNonRootGroup)
	pod.Spec.SecurityContext.RunAsNonRoot = ptr.To(true)

	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].SecurityContext == nil {
			pod.Spec.Containers[i].SecurityContext = &v1.SecurityContext{}
		}
		pod.Spec.Containers[i].SecurityContext.RunAsUser = ptr.To(defaultNonRootUser)
		pod.Spec.Containers[i].SecurityContext.RunAsGroup = ptr.To(defaultNonRootGroup)
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
				v1.ResourceStorage: resource.MustParse("1Gi"),
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
					v1.ResourceStorage: resource.MustParse("1Gi"),
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
