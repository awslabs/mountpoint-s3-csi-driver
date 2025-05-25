package customsuites

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"

	"github.com/scality/mountpoint-s3-csi-driver/tests/e2e/pkg/s3client"
)

const (
	// Lisa's real test credentials per cloudserver without Vault
	lisaAK  = "accessKey2"
	lisaSK  = "verySecretKey2"
	lisaCID = "79a59df900b949e55d96a1e698fbacedfd6e09d98eacf8f8d5218e7cd47ef2bf"

	// Bart's canonical ID. Bart's credentials are being used in the tests
	bartAK  = "accessKey1"
	bartSK  = "verySecretKey1"
	bartCID = "79a59df900b949e55d96a1e698fbacedfd6e09d98eacf8f8d5218e7cd47ef2be"
)

// NegativeCredentialTestSpec defines parameters for a negative credential test.
type NegativeCredentialTestSpec struct {
	BucketOwnerAK, BucketOwnerSK string // Credentials used to create the bucket
	PodAK, PodSK                 string // Credentials used in pod (should fail)
	ErrorPattern                 string // Error message to expect
	TestDescription              string // Human-readable test description
	CustomPodName                string // Optional custom pod name (defaults to "test-credentials-error-{uuid}")
}

// RunNegativeCredentialsTest runs a standard test to verify credentials error handling.
// It uses bucket owner's credentials to create a bucket, then attempts to access it
// using the pod credentials, which should fail with the expected error pattern.
func RunNegativeCredentialsTest(
	ctx context.Context,
	f *framework.Framework,
	driver storageframework.TestDriver,
	pattern storageframework.TestPattern,
	spec NegativeCredentialTestSpec,
) {
	// 1. Create a bucket with bucket owner's credentials
	ownerS3Client := s3client.New("", spec.BucketOwnerAK, spec.BucketOwnerSK)
	bucketName, deleteBucket := ownerS3Client.CreateBucket(ctx)
	ginkgo.DeferCleanup(deleteBucket)
	framework.Logf("Created bucket %s with bucket owner's credentials", bucketName)

	// 2. Create a secret with pod's credentials
	secretName, err := CreateCredentialSecret(ctx, f, "credentials-test", spec.PodAK, spec.PodSK)
	framework.ExpectNoError(err, "failed to create secret with test credentials")

	// 3. Build PV/PVC that uses the secret
	cfg := driver.PrepareTest(ctx, f)
	volumeResource := CreateVolumeWithSecretReference(
		ctx,
		cfg,
		pattern,
		secretName,
		f.Namespace.Name,
		bucketName,
	)

	// 4. Create a non-root pod with the volume - should fail with expected error
	ginkgo.By(fmt.Sprintf("Creating pod with credentials that should fail: %s", spec.TestDescription))

	// Create pod with unique name
	podName := spec.CustomPodName
	if podName == "" {
		podName = "test-credentials-error-" + uuid.NewString()[:8]
	}
	framework.Logf("Creating pod %s in namespace %s", podName, f.Namespace.Name)

	pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{volumeResource.Pvc}, admissionapi.LevelRestricted, "")
	pod.Name = podName
	podModifierNonRoot(pod)

	// Create the pod
	pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create pod")
	framework.Logf("Pod %s created successfully, now waiting for error event", pod.Name)

	// 5. Wait for expected error pattern
	ginkgo.By(fmt.Sprintf("Waiting for error: %q", spec.ErrorPattern))
	framework.ExpectNoError(WaitForPodError(ctx, f, pod.Name, spec.ErrorPattern, 1*time.Minute))

	framework.Logf("Test complete - found expected error")

	// 6. Clean up pod
	framework.Logf("Cleaning up pod %s", pod.Name)
	framework.ExpectNoError(CleanupPodInErrorState(ctx, f, pod.Name))
}

// Test‑suite boilerplate
type s3CSICredentialsSuite struct {
	info storageframework.TestSuiteInfo
}

func InitS3CredentialsTestSuite() storageframework.TestSuite {
	return &s3CSICredentialsSuite{
		info: storageframework.TestSuiteInfo{
			Name: "credentials",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (s *s3CSICredentialsSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo { return s.info }
func (s *s3CSICredentialsSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

// getMountOptionsForNonRootUser returns standard mount options for non-root access
func getMountOptionsForNonRootUser() []string {
	return []string{
		"allow-other",
		fmt.Sprintf("uid=%d", DefaultNonRootUser),
		fmt.Sprintf("gid=%d", DefaultNonRootGroup),
	}
}

// CreateVolumeWithSecretReference creates a volume with authentication settings and S3 bucket configuration
func CreateVolumeWithSecretReference(
	ctx context.Context,
	config *storageframework.PerTestConfig,
	pattern storageframework.TestPattern,
	secretName string,
	namespace string,
	bucketName string,
) *storageframework.VolumeResource {
	f := config.Framework
	r := storageframework.VolumeResource{Config: config, Pattern: pattern}

	pDriver := config.Driver.(storageframework.PreprovisionedPVTestDriver)
	r.Volume = pDriver.CreateVolume(ctx, config, storageframework.PreprovisionedPV)
	pvSource, nodeAffinity := pDriver.GetPersistentVolumeSource(false, "", r.Volume)

	pvName := fmt.Sprintf("s3-e2e-pv-%s", uuid.NewString())
	pvcName := fmt.Sprintf("s3-e2e-pvc-%s", uuid.NewString())

	// Use standard mount options for non-root access
	mountOptions := getMountOptionsForNonRootUser()

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
				Namespace: namespace,
			},
		},
	}

	// Set authentication attributes
	pv.Spec.CSI.VolumeAttributes = map[string]string{
		"bucketName":           bucketName,
		"authenticationSource": "secret",
	}
	pv.Spec.CSI.NodePublishSecretRef = &v1.SecretReference{
		Name:      secretName,
		Namespace: namespace,
	}

	// Create the PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
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

	// Create PV and PVC
	framework.Logf("Creating PV %s and PVC %s with bucket %s", pvName, pvcName, bucketName)
	var err error
	r.Pv, err = f.ClientSet.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	r.Pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// wait until both PV and PVC are Bound
	err = e2epv.WaitOnPVandPVC(ctx, f.ClientSet, f.Timeouts, namespace, r.Pv, r.Pvc)
	framework.ExpectNoError(err)

	return &r
}

func createVolumeWithSecretReference(
	ctx context.Context,
	config *storageframework.PerTestConfig,
	pattern storageframework.TestPattern,
	secretName string,
	namespace string,
	bucketName string,
) *storageframework.VolumeResource {
	return CreateVolumeWithSecretReference(ctx, config, pattern, secretName, namespace, bucketName)
}

func (s *s3CSICredentialsSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts("credentials", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelRestricted

	ginkgo.It("mounts with default driver credentials and sees Bart‑owned objects", func(ctx context.Context) {
		type TestResourceRegistry struct {
			resources []*storageframework.VolumeResource // tracks resources for cleanup
			config    *storageframework.PerTestConfig    // storage framework configuration
		}
		var testRegistry TestResourceRegistry
		cleanup := func(ctx context.Context) {
			var errs []error
			for _, resource := range testRegistry.resources {
				errs = append(errs, resource.CleanupResource(ctx))
			}
			framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
		}
		testRegistry = TestResourceRegistry{}
		testRegistry.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
		bartS3Client := s3client.New("", bartAK, bartSK)

		// Use createVolumeResourceWithMountOptions from utils
		resource := createVolumeResourceWithMountOptions(ctx, testRegistry.config, pattern, getMountOptionsForNonRootUser())
		testRegistry.resources = append(testRegistry.resources, resource)

		bucketName := resource.Pv.Spec.CSI.VolumeAttributes["bucketName"]

		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, resource.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		filePath := "/mnt/volume1/pod-write-default.txt"
		WriteAndVerifyFile(f, pod, filePath, "hello default")

		// List all objects in the bucket to verify the file exists
		ginkgo.By("Listing all objects in the bucket to verify file exists")
		framework.Logf("Bucket name: %s, Looking for object: pod-write-default.txt", bucketName)
		_, err = bartS3Client.ListObjects(ctx, bucketName)
		framework.ExpectNoError(err)

		// Attempt to verify owner ID, but handle potential errors gracefully
		ginkgo.By("Attempting to verify object has Bart's canonical ID (if owner info available)")
		ownerID, err := bartS3Client.GetObjectOwnerID(ctx, bucketName, "pod-write-default.txt")
		framework.ExpectNoError(err)
		gomega.Expect(ownerID).To(gomega.Equal(bartCID),
			"Object owner ID should match Bart's canonical ID. Default credentials might be incorrect.")
	})

	ginkgo.It("mounts with Secret credentials and sees Lisa‑owned objects", func(ctx context.Context) {
		// Create a bucket with Lisa's credentials
		lisaS3Client := s3client.New("", lisaAK, lisaSK)
		bucketName, deleteBucket := lisaS3Client.CreateBucket(ctx)
		ginkgo.DeferCleanup(deleteBucket)

		// Make a Secret with Lisa's credentials in test namespace
		secretName, err := CreateCredentialSecret(ctx, f, "lisa-cred", lisaAK, lisaSK)
		framework.ExpectNoError(err)

		// Build PV/PVC that use the Secret (authSource=secret)
		cfg := driver.PrepareTest(ctx, f)

		volumeResource := createVolumeWithSecretReference(
			ctx,
			cfg,
			pattern,
			secretName,
			f.Namespace.Name,
			bucketName,
		)

		// Create a non-root pod with the volume
		ginkgo.By("Creating pod with a volume using secret credentials")
		pod, err := CreatePodWithVolumeAndSecurity(ctx, f, volumeResource.Pvc, "", DefaultNonRootUser, DefaultNonRootGroup)
		framework.ExpectNoError(err)

		// Write a test file
		filePath := "/mnt/volume1/pod-write.txt"
		WriteAndVerifyFile(f, pod, filePath, "hello lisa")

		// Verify Lisa's canonical ID as owner via ListObjectsV2 FetchOwner
		ginkgo.By("Verifying object has Lisa's canonical ID")
		ownerID, err := lisaS3Client.GetObjectOwnerID(ctx, bucketName, "pod-write.txt")
		framework.ExpectNoError(err)
		gomega.Expect(ownerID).To(gomega.Equal(lisaCID),
			"object owner ID should match Lisa's canonical ID – Secret creds were not applied")
	})

	ginkgo.It("fails to mount with 'access key Id does not exist' error when using invalid credentials", func(ctx context.Context) {
		RunNegativeCredentialsTest(
			ctx,
			f,
			driver,
			pattern,
			NegativeCredentialTestSpec{
				// Use Lisa to create the bucket
				BucketOwnerAK: lisaAK,
				BucketOwnerSK: lisaSK,
				// Use invalid credentials in the pod
				PodAK:           "invalid" + uuid.NewString()[:8],
				PodSK:           "veryInvalidKey" + uuid.NewString()[:8],
				ErrorPattern:    "Forbidden: The AWS access key Id you provided does not exist in our records",
				TestDescription: "non-existent access key causing authentication failure",
				CustomPodName:   "test-invalid-key-" + uuid.NewString()[:8],
			},
		)
	})

	ginkgo.It("fails to mount with 'Access Denied Error: Failed to create mount process' error when using valid credentials without permissions", func(ctx context.Context) {
		RunNegativeCredentialsTest(
			ctx,
			f,
			driver,
			pattern,
			NegativeCredentialTestSpec{
				// Use Bart to create the bucket
				BucketOwnerAK: bartAK,
				BucketOwnerSK: bartSK,
				// Use Lisa's credentials to try to access Bart's bucket
				PodAK:           lisaAK,
				PodSK:           lisaSK,
				ErrorPattern:    "Access Denied Error: Failed to create mount process",
				TestDescription: "valid credentials without permission to access bucket",
				CustomPodName:   "test-access-denied-" + uuid.NewString()[:8],
			},
		)
	})
}
