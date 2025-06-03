// utils.go — shared helpers for the S3‑CSI mount‑option e2e suites.
package customsuites

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

/*──────────────────────────────
  Constants
  ──────────────────────────────*/

const (
	DefaultNonRootUser  = int64(1001)
	DefaultNonRootGroup = int64(2000)
)

/*──────────────────────────────
  Batched creation primitives
  ──────────────────────────────*/

// PathSpec describes a file or directory to materialise inside a pod.
// If Content is empty, the path is treated as a directory.
type PathSpec struct {
	Path     string // absolute path
	Content  string // file content (optional)
	IsBinary bool   // if true, Content is base64-encoded and will be decoded
	Mode     string // octal string (e.g. "0640"), optional
	OwnerUID *int64 // nil → leave unchanged
	OwnerGID *int64 // nil → leave unchanged
}

// MaterialisePaths batches creation of many files/dirs in one exec.
func MaterialisePaths(f *framework.Framework, pod *v1.Pod, specs []PathSpec) error {
	if len(specs) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("set -euo pipefail\n")

	for _, s := range specs {
		sb.WriteString(fmt.Sprintf("mkdir -p %q\n", filepath.Dir(s.Path)))

		if s.Content == "" {
			// Directory
			sb.WriteString(fmt.Sprintf("mkdir -p %q\n", s.Path))
		} else if s.IsBinary {
			// Binary content (base64-encoded)
			sb.WriteString(fmt.Sprintf("echo %s | base64 -d > %q\n", s.Content, s.Path))
		} else {
			// Text content (write directly)
			sb.WriteString(fmt.Sprintf("cat > %q << 'EOF'\n%s\nEOF\n", s.Path, s.Content))
		}

		if s.Mode != "" {
			sb.WriteString(fmt.Sprintf("chmod %s %q\n", s.Mode, s.Path))
		}
		if s.OwnerUID != nil || s.OwnerGID != nil {
			uid := "-"
			gid := "-"
			if s.OwnerUID != nil {
				uid = strconv.FormatInt(*s.OwnerUID, 10)
			}
			if s.OwnerGID != nil {
				gid = strconv.FormatInt(*s.OwnerGID, 10)
			}
			sb.WriteString(fmt.Sprintf("chown %s:%s %q || true\n", uid, gid, s.Path))
		}
	}

	_, stderr, err := e2evolume.PodExec(f, pod, sb.String())
	if err != nil {
		return fmt.Errorf("materialise paths failed: %v — stderr: %s", err, stderr)
	}
	return nil
}

/*──────────────────────────────
  Thin wrapper helpers
  ──────────────────────────────*/

// CreateFileInPod writes a small text file at `path`.
func CreateFileInPod(f *framework.Framework, pod *v1.Pod, path, content string) {
	err := MaterialisePaths(f, pod, []PathSpec{{Path: path, Content: content, IsBinary: false}})
	framework.ExpectNoError(err)
}

// CreateBinaryFileInPod writes a binary file at `path` using base64-encoded `content`.
func CreateBinaryFileInPod(f *framework.Framework, pod *v1.Pod, path, base64Content string) {
	err := MaterialisePaths(f, pod, []PathSpec{{Path: path, Content: base64Content, IsBinary: true}})
	framework.ExpectNoError(err)
}

// CreateDirInPod ensures a directory exists.
func CreateDirInPod(f *framework.Framework, pod *v1.Pod, path string) {
	err := MaterialisePaths(f, pod, []PathSpec{{Path: path}})
	framework.ExpectNoError(err)
}

// CreateMultipleDirsInPod creates many directories in one shot.
func CreateMultipleDirsInPod(f *framework.Framework, pod *v1.Pod, paths ...string) {
	specs := make([]PathSpec, len(paths))
	for i, p := range paths {
		specs[i] = PathSpec{Path: p}
	}
	err := MaterialisePaths(f, pod, specs)
	framework.ExpectNoError(err)
}

// CopyFileInPod copies a file within the same pod.
func CopyFileInPod(f *framework.Framework, pod *v1.Pod, sourcePath, targetPath string) {
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cp %q %q", sourcePath, targetPath))
}

// DeleteFileInPod deletes a file in the pod.
func DeleteFileInPod(f *framework.Framework, pod *v1.Pod, path string) {
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("rm -f %q", path))
}

/*──────────────────────────────
  Data‑integrity helpers
  ──────────────────────────────*/

func genBinDataFromSeed(length int, seed int64) []byte {
	buf := make([]byte, length)
	rnd := rand.New(rand.NewSource(seed))
	_, _ = rnd.Read(buf)
	return buf
}

func checkWriteToPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	data := genBinDataFromSeed(toWrite, seed)
	enc := base64.StdEncoding.EncodeToString(data)

	// Calculate checksum for logging
	framework.Logf("writing data sha256: %x", sha256.Sum256(data))

	// Use our MaterialisePaths API for binary content
	err := MaterialisePaths(f, pod, []PathSpec{{
		Path:     path,
		Content:  enc,
		IsBinary: true,
	}})
	framework.ExpectNoError(err)
}

func checkReadFromPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	sum := sha256.Sum256(genBinDataFromSeed(toWrite, seed))
	e2evolume.VerifyExecInPodSucceed(f, pod,
		fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum | grep -Fq %x", path, toWrite, sum))
}

/*──────────────────────────────
  Security‑context & pod helpers
  ──────────────────────────────*/

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

// createPod creates a pod and waits until it's running.
func createPod(ctx context.Context, client clientset.Interface, ns string, pod *v1.Pod) (*v1.Pod, error) {
	framework.Logf("Creating Pod %s in %s", pod.Name, ns)
	pod, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod Create API error: %w", err)
	}
	if err := e2epod.WaitForPodNameRunningInNamespace(ctx, client, pod.Name, ns); err != nil {
		return pod, fmt.Errorf("pod %q not Running: %w", pod.Name, err)
	}
	return client.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
}

/*──────────────────────────────
  Volume‑creation helpers
  ──────────────────────────────*/

func createVolumeResourceWithMountOptions(
	ctx context.Context,
	config *storageframework.PerTestConfig,
	pattern storageframework.TestPattern,
	mountOptions []string,
) *storageframework.VolumeResource {
	f := config.Framework
	r := storageframework.VolumeResource{Config: config, Pattern: pattern}

	pDriver, _ := config.Driver.(storageframework.PreprovisionedPVTestDriver)
	r.Volume = pDriver.CreateVolume(ctx, config, storageframework.PreprovisionedPV)
	pvSource, nodeAffinity := pDriver.GetPersistentVolumeSource(false, "", r.Volume)

	pvName := fmt.Sprintf("s3-e2e-pv-%s", uuid.NewString())
	pvcName := fmt.Sprintf("s3-e2e-pvc-%s", uuid.NewString())

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: *pvSource,
			StorageClassName:       "",
			NodeAffinity:           nodeAffinity,
			MountOptions:           mountOptions,
			AccessModes:            []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			Capacity:               v1.ResourceList{v1.ResourceStorage: resource.MustParse("1200Gi")},
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
			StorageClassName: ptr.To(""),
			VolumeName:       pvName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1200Gi")},
			},
		},
	}

	framework.Logf("Creating PV %s and PVC %s", pvName, pvcName)
	var err error
	r.Pv, err = f.ClientSet.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	r.Pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	err = e2epv.WaitOnPVandPVC(ctx, f.ClientSet, f.Timeouts, f.Namespace.Name, r.Pv, r.Pvc)
	framework.ExpectNoError(err)
	return &r
}

// BuildVolumeWithOptions assembles mountOptions and calls createVolumeResourceWithMountOptions.
func BuildVolumeWithOptions(
	ctx context.Context,
	config *storageframework.PerTestConfig,
	pattern storageframework.TestPattern,
	uid, gid int64,
	fileModeOption string,
	extraOptions ...string,
) *storageframework.VolumeResource {
	opts := []string{
		fmt.Sprintf("uid=%d", uid),
		fmt.Sprintf("gid=%d", gid),
		"allow-other",
	}
	if fileModeOption != "" {
		opts = append(opts, fmt.Sprintf("file-mode=%s", fileModeOption))
	}
	opts = append(opts, extraOptions...)
	return createVolumeResourceWithMountOptions(ctx, config, pattern, opts)
}

/*──────────────────────────────
  Pod‑construction helpers
  ──────────────────────────────*/

func CreatePodWithVolumeAndSecurity(
	ctx context.Context,
	f *framework.Framework,
	volume *v1.PersistentVolumeClaim,
	containerName string,
	uid, gid int64,
) (*v1.Pod, error) {
	pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{volume}, admissionapi.LevelRestricted, "")
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{}
	}
	pod.Spec.SecurityContext.RunAsUser = ptr.To(uid)
	pod.Spec.SecurityContext.RunAsGroup = ptr.To(gid)
	pod.Spec.SecurityContext.RunAsNonRoot = ptr.To(true)

	if containerName != "" {
		pod.Spec.Containers[0].Name = containerName
	}
	return createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
}

func MakeNonRootPodWithVolume(namespace string, pvcs []*v1.PersistentVolumeClaim, containerName string) *v1.Pod {
	pod := e2epod.MakePod(namespace, nil, pvcs, admissionapi.LevelRestricted, "")
	podModifierNonRoot(pod)
	if containerName != "" {
		pod.Spec.Containers[0].Name = containerName
	}
	return pod
}

// writeAndVerifyFile writes a file to the specified path and verifies it exists
func WriteAndVerifyFile(f *framework.Framework, pod *v1.Pod, filePath, content string) {
	ginkgo.By(fmt.Sprintf("Writing file %s in pod", filePath))
	CreateFileInPod(f, pod, filePath, content)

	// Verify file was written successfully by reading it back
	ginkgo.By("Verifying file was successfully written")
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q '%s'", filePath, content))
}

/*──────────────────────────────
  Pod error-waiting & cleanup helpers
  ──────────────────────────────*/

// WaitForPodError waits until the given pod surfaces an event or a status
// condition whose message contains `expectedPattern`. It returns nil on
// success or an error if the pattern is not found before `timeout` expires.
func WaitForPodError(
	ctx context.Context,
	f *framework.Framework,
	podName string,
	expectedPattern string,
	timeout time.Duration,
) error {
	framework.Logf("Waiting up to %v for pod %s to surface error: %q", timeout, podName, expectedPattern)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// ① Check events for the pod
			events, err := f.ClientSet.CoreV1().Events(f.Namespace.Name).List(ctx, metav1.ListOptions{
				FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
			})
			if err != nil {
				return err
			}
			for _, ev := range events.Items {
				if strings.Contains(ev.Message, expectedPattern) {
					framework.Logf("Found expected error in event: %s", ev.Message)
					return nil
				}
			}

			// ② Check pod status conditions
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			for _, cond := range pod.Status.Conditions {
				if strings.Contains(cond.Message, expectedPattern) {
					framework.Logf("Found expected error in pod condition: %s", cond.Message)
					return nil
				}
			}

			if time.Now().After(deadline) {
				return fmt.Errorf("timed out after %v waiting for error pattern %q on pod %s", timeout, expectedPattern, podName)
			}
		}
	}
}

// CleanupPodInErrorState force-deletes the given pod (grace-period=0, foreground).
// Useful at the end of tests that purposely leave a pod in a Failed/CrashLoop state.
func CleanupPodInErrorState(ctx context.Context, f *framework.Framework, podName string) error {
	deletePolicy := metav1.DeletePropagationForeground
	return f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, podName, metav1.DeleteOptions{
		PropagationPolicy:  &deletePolicy,
		GracePeriodSeconds: ptr.To(int64(0)),
	})
}

/*──────────────────────────────
  Credential testing helpers
  ──────────────────────────────*/

// CreateCredentialSecret creates a Secret with S3 credentials and returns its name.
func CreateCredentialSecret(
	ctx context.Context,
	f *framework.Framework,
	namePrefix, accessKeyID, secretAccessKey string,
) (string, error) {
	secretName := namePrefix + "-" + uuid.NewString()[:8]
	_, err := f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: f.Namespace.Name,
		},
		Type: v1.SecretTypeOpaque,
		StringData: map[string]string{
			"access_key_id":     accessKeyID,
			"secret_access_key": secretAccessKey,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return secretName, nil
}

// BuildSecretVolume creates a volume using a secret reference for authentication.
func BuildSecretVolume(
	ctx context.Context,
	f *framework.Framework,
	driver storageframework.TestDriver,
	pattern storageframework.TestPattern,
	secretName, bucketName string,
) (*storageframework.VolumeResource, error) {
	cfg := driver.PrepareTest(ctx, f)

	// Use exported function from credentials.go
	return CreateVolumeWithSecretReference(
		ctx,
		cfg,
		pattern,
		secretName,
		f.Namespace.Name,
		bucketName,
	), nil
}

// NegativeCredTestSpec and RunNegativeCredentialsTest are moved to credentials.go

/*──────────────────────────────
  Misc small helpers
  ──────────────────────────────*/

// copySmallFileToPod copies a file from the test host into a pod (for tiny files only).
func copySmallFileToPod(_ context.Context, f *framework.Framework, pod *v1.Pod, srcFile, destFile string) {
	content, err := os.ReadFile(srcFile)
	framework.ExpectNoError(err)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf(
		"cat > %s <<'EOF'\n%s\nEOF", destFile, string(content)))
}

// CreateTestFileAndDir makes one file + one dir (handy for quick tests).
func CreateTestFileAndDir(f *framework.Framework, pod *v1.Pod, basePath, prefix string) (string, string) {
	file := fmt.Sprintf("%s/%s.txt", basePath, prefix)
	dir := fmt.Sprintf("%s/%s-dir", basePath, prefix)

	ginkgo.By(fmt.Sprintf("Creating test file %s", file))
	CreateFileInPod(f, pod, file, "test content")

	ginkgo.By(fmt.Sprintf("Creating test directory %s", dir))
	CreateDirInPod(f, pod, dir)

	return file, dir
}

// GetBucketNameFromVolumeResource extracts the bucket name from a VolumeResource
func GetBucketNameFromVolumeResource(resource *storageframework.VolumeResource) string {
	var bucketName string
	if csiSpec := resource.Pv.Spec.CSI; csiSpec != nil {
		if attrs := csiSpec.VolumeAttributes; attrs != nil {
			bucketName = attrs["bucketName"]
		}
	}
	return bucketName
}

// VerifyFilesExistInPod verifies all the given file paths exist in the pod
func VerifyFilesExistInPod(f *framework.Framework, pod *v1.Pod, basePath string, filePaths []string) {
	ginkgo.By(fmt.Sprintf("Verifying %d files exist in pod at path %s", len(filePaths), basePath))

	for i, filePath := range filePaths {
		fullPath := fmt.Sprintf("%s/%s", basePath, filePath)

		// Verify file exists
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("test -f %s", fullPath))

		// Verify file content if it has a standard pattern
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q 'Content for file %d'", fullPath, i+1))
	}
}

// CreateFilesInPod creates multiple files in the pod at the given base path
func CreateFilesInPod(f *framework.Framework, pod *v1.Pod, basePath string, filePaths []string) {
	ginkgo.By(fmt.Sprintf("Creating %d files in pod at path %s", len(filePaths), basePath))

	for i, filePath := range filePaths {
		fullPath := fmt.Sprintf("%s/%s", basePath, filePath)
		dirPath := filepath.Dir(fullPath)

		// Create directory if it doesn't exist
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir -p %s", dirPath))

		// Create the file with content
		fileContent := fmt.Sprintf("Content for file %d created through mount", i+1)
		WriteAndVerifyFile(f, pod, fullPath, fileContent)
	}
}
