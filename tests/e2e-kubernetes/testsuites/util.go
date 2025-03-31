package custom_testsuites

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/google/uuid"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
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

const defaultNonRootUser = int64(1001)
const defaultNonRootGroup = int64(2000)

type jsonMap = map[string]interface{}

const NamespacePrefix = "aws-s3-csi-e2e-"

const (
	csiDriverDaemonSetName      = "s3-csi-node"
	csiDriverDaemonSetNamespace = "kube-system"
)

// genBinDataFromSeed generate binData with random seed
func genBinDataFromSeed(len int, seed int64) []byte {
	binData := make([]byte, len)
	randLocal := rand.New(rand.NewSource(seed))

	_, err := randLocal.Read(binData)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}

	return binData
}

func checkWriteToPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	data := genBinDataFromSeed(toWrite, seed)
	encoded := base64.StdEncoding.EncodeToString(data)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | sha256sum", encoded))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | dd of=%s bs=%d count=1", encoded, path, toWrite))
	framework.Logf("written data with sha: %x", sha256.Sum256(data))
}

func checkWriteToPathFails(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	data := genBinDataFromSeed(toWrite, seed)
	encoded := base64.StdEncoding.EncodeToString(data)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d | sha256sum", encoded))
	e2evolume.VerifyExecInPodFail(f, pod, fmt.Sprintf("echo %s | base64 -d | dd of=%s bs=%d count=1", encoded, path, toWrite), 1)
}

func checkReadFromPath(f *framework.Framework, pod *v1.Pod, path string, toWrite int, seed int64) {
	sum := sha256.Sum256(genBinDataFromSeed(toWrite, seed))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum", path, toWrite))
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("dd if=%s bs=%d count=1 | sha256sum | grep -Fq %x", path, toWrite, sum))
}

func checkDeletingPath(f *framework.Framework, pod *v1.Pod, path string) {
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("rm %s", path))
}

func checkListingPath(f *framework.Framework, pod *v1.Pod, path string) {
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("ls %s", path))
}

func checkListingPathWithEntries(f *framework.Framework, pod *v1.Pod, path string, entries []string) {
	cmd := fmt.Sprintf("ls %s", path)
	stdout, stderr, err := e2evolume.PodExec(f, pod, cmd)
	framework.ExpectNoError(err,
		"%q should succeed, but failed with error message %q\nstdout: %s\nstderr: %s",
		cmd, err, stdout, stderr)

	gomega.Expect(strings.Fields(stdout)).To(gomega.Equal(entries))
}

func createVolumeResource(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern, accessMode v1.PersistentVolumeAccessMode, mountOptions []string) *storageframework.VolumeResource {
	f := config.Framework
	r := storageframework.VolumeResource{
		Config:  config,
		Pattern: pattern,
	}
	pDriver, _ := config.Driver.(storageframework.PreprovisionedPVTestDriver)
	r.Volume = pDriver.CreateVolume(ctx, config, storageframework.PreprovisionedPV)
	pvSource, volumeNodeAffinity := pDriver.GetPersistentVolumeSource(false, "", r.Volume)

	pvName := "s3-e2e-pv-" + uuid.New().String()
	pvcName := "s3-e2e-pvc-" + uuid.New().String()

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: *pvSource,
			StorageClassName:       "", // for static provisioning
			NodeAffinity:           volumeNodeAffinity,
			MountOptions:           mountOptions, // this is not set by storageframework.CreateVolumeResource, which is why we need to implement our own function
			AccessModes:            []v1.PersistentVolumeAccessMode{accessMode},
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
			AccessModes:      []v1.PersistentVolumeAccessMode{accessMode},
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
	framework.ExpectNoError(err, "PVC, PV creation failed")

	r.Pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
	framework.ExpectNoError(err, "PVC, PVC creation failed")

	err = e2epv.WaitOnPVandPVC(ctx, f.ClientSet, f.Timeouts, f.Namespace.Name, r.Pv, r.Pvc)
	framework.ExpectNoError(err, "PVC, PV failed to bind")
	return &r
}

func createVolumeResourceWithAccessMode(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern, accessMode v1.PersistentVolumeAccessMode) *storageframework.VolumeResource {
	return createVolumeResource(ctx, config, pattern, accessMode, []string{})
}

func createVolumeResourceWithMountOptions(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern, mountOptions []string) *storageframework.VolumeResource {
	return createVolumeResource(ctx, config, pattern, v1.ReadWriteMany, mountOptions)
}

func bucketNameFromVolumeResource(vol *storageframework.VolumeResource) string {
	pvc := vol.Pv.Spec.PersistentVolumeSource
	return pvc.CSI.VolumeHandle
}

func createPod(ctx context.Context, client clientset.Interface, namespace string, pod *v1.Pod) (*v1.Pod, error) {
	serviceAccount := pod.Spec.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = "default"
	}
	framework.Logf("Creating Pod %s in %s (SA: %s)", pod.Name, namespace, serviceAccount)
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

func createPodWithServiceAccount(ctx context.Context, client clientset.Interface, namespace string, pvclaims []*v1.PersistentVolumeClaim, serviceAccountName string) (*v1.Pod, error) {
	pod := e2epod.MakePod(namespace, nil, pvclaims, admissionapi.LevelBaseline, "")
	pod.Spec.ServiceAccountName = serviceAccountName
	return createPod(ctx, client, namespace, pod)
}

func podModifierNonRoot(pod *v1.Pod) {
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{}
	}
	pod.Spec.SecurityContext.RunAsUser = ptr.To(defaultNonRootUser)
	pod.Spec.SecurityContext.RunAsGroup = ptr.To(defaultNonRootGroup)
	pod.Spec.SecurityContext.RunAsNonRoot = ptr.To(true)

	for _, container := range pod.Spec.Containers {
		if container.SecurityContext == nil {
			container.SecurityContext = &v1.SecurityContext{}
		}
		container.SecurityContext.RunAsUser = ptr.To(defaultNonRootUser)
		container.SecurityContext.RunAsGroup = ptr.To(defaultNonRootGroup)
		container.SecurityContext.RunAsNonRoot = ptr.To(true)
	}
}

func copySmallFileToPod(_ context.Context, f *framework.Framework, pod *v1.Pod, hostPath, podPath string) {
	data, err := os.ReadFile(hostPath)
	framework.ExpectNoError(err)
	encoded := base64.StdEncoding.EncodeToString(data)
	e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("echo %s | base64 -d > %s", encoded, podPath))
}

// In some cases like changing Secret object, it's useful to trigger recreation of our pods.
func killCSIDriverPods(ctx context.Context, f *framework.Framework) {
	framework.Logf("Killing CSI Driver Pods")
	ds := csiDriverDaemonSet(ctx, f)
	client := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace)

	pods, err := client.List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(ds.Spec.Selector),
	})
	framework.ExpectNoError(err)

	for _, pod := range pods.Items {
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, &pod))
	}
}

func csiDriverDaemonSet(ctx context.Context, f *framework.Framework) *appsv1.DaemonSet {
	client := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace)
	ds, err := client.Get(ctx, csiDriverDaemonSetName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	gomega.Expect(ds).ToNot(gomega.BeNil())
	return ds
}

func csiDriverServiceAccount(ctx context.Context, f *framework.Framework) *v1.ServiceAccount {
	ds := csiDriverDaemonSet(ctx, f)

	client := f.ClientSet.CoreV1().ServiceAccounts(csiDriverDaemonSetNamespace)
	sa, err := client.Get(ctx, ds.Spec.Template.Spec.ServiceAccountName, metav1.GetOptions{})

	framework.ExpectNoError(err)
	gomega.Expect(sa).NotTo(gomega.BeNil())

	return sa
}

func createServiceAccount(ctx context.Context, f *framework.Framework) (*v1.ServiceAccount, func(context.Context) error) {
	framework.Logf("Creating ServiceAccount")

	client := f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name)
	sa, err := client.Create(ctx, &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{GenerateName: f.BaseName + "-sa-"}}, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	framework.ExpectNoError(waitForKubernetesObject(ctx, framework.GetObject(client.Get, sa.Name, metav1.GetOptions{})))

	return sa, func(ctx context.Context) error {
		framework.Logf("Removing ServiceAccount %s", sa.Name)
		return client.Delete(ctx, sa.Name, metav1.DeleteOptions{})
	}
}

func awsConfig(ctx context.Context) aws.Config {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(DefaultRegion))
	framework.ExpectNoError(err)
	return cfg
}

func waitForKubernetesObject[T any](ctx context.Context, get framework.GetFunc[T]) error {
	return framework.Gomega().Eventually(ctx, framework.RetryNotFound(get)).
		WithTimeout(1 * time.Minute).
		ShouldNot(gomega.BeNil())
}
