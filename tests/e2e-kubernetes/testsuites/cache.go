package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"

	"github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/s3client"
)

const volumeName1 = "volume1"
const root = int64(0)

type s3CSICacheTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSICacheTestSuite() storageframework.TestSuite {
	return &s3CSICacheTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "cache",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSICacheTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSICacheTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSICacheTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"cache", storageframework.GetDriverTimeouts(driver))
	// This is required for now due to hack mentioned in `ensureCacheDirExistsInNode` function, see the comments there for more context.
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	type local struct {
		config *storageframework.PerTestConfig

		// A list of cleanup functions to be called after each test to clean resources created during the test.
		cleanup []func(context.Context) error
	}

	var l local

	deferCleanup := func(f func(context.Context) error) {
		l.cleanup = append(l.cleanup, f)
	}

	cleanup := func(ctx context.Context) {
		var errs []error
		slices.Reverse(l.cleanup) // clean items in reverse order similar to how `defer` works
		for _, f := range l.cleanup {
			errs = append(errs, f(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		DeferCleanup(cleanup)
	})

	// checkBasicFileOperations verifies basic file operations works in the given `basePath` inside the `pod`.
	checkBasicFileOperations := func(ctx context.Context, pod *v1.Pod, bucketName, basePath string) {
		framework.Logf("Checking basic file operations inside pod %s at %s", pod.UID, basePath)

		dir := filepath.Join(basePath, "test-dir")
		first := filepath.Join(basePath, "first")
		second := filepath.Join(dir, "second")

		seed := time.Now().UTC().UnixNano()
		testWriteSize := 1024 // 1KB

		checkWriteToPath(f, pod, first, testWriteSize, seed)
		checkListingPathWithEntries(f, pod, basePath, []string{"first"})
		// Test reading multiple times to ensure cached-read works
		for range 3 {
			checkReadFromPath(f, pod, first, testWriteSize, seed)
		}

		// Now remove the file from S3
		deleteObjectFromS3(ctx, bucketName, "first")

		// Ensure the data still read from the cache - without cache this would fail as its removed from underlying bucket
		checkReadFromPath(f, pod, first, testWriteSize, seed)

		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("mkdir %s && cd %s && echo 'second!' > %s; sync", dir, dir, second))
		e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q 'second!'", second))
		checkListingPathWithEntries(f, pod, dir, []string{"second"})
		checkListingPathWithEntries(f, pod, basePath, []string{"test-dir"})
		checkDeletingPath(f, pod, first)
		checkDeletingPath(f, pod, second)
	}

	createPod := func(ctx context.Context, mountOptions []string, podModifiers ...func(*v1.Pod)) (*v1.Pod, string) {
		vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, mountOptions)
		deferCleanup(vol.CleanupResource)

		bucketName := bucketNameFromVolumeResource(vol)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
		for _, pm := range podModifiers {
			pm(pod)
		}

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		return pod, bucketName
	}

	type localCacheKind string
	const (
		localCacheUnspecified  localCacheKind = ""
		localCacheMountOptions                = "localCacheMountOptions"
		localCacheEmptyDir                    = "localCacheEmptyDir"
		localCacheEBSEphemeral                = "localCacheEBSEphemeral"
	)

	type cacheTestConfig struct {
		localCacheKind  localCacheKind
		useExpressCache bool
	}

	testCache := func(config cacheTestConfig) {
		var baseMountOptions []string
		var basePodModifiers []func(*v1.Pod)
		var enhanceContext func(context.Context) context.Context
		var expressCacheBucketName string

		BeforeEach(func(ctx context.Context) {
			// Reset shared configuration on each run
			baseMountOptions = nil
			basePodModifiers = nil
			enhanceContext = func(ctx context.Context) context.Context { return ctx }
			expressCacheBucketName = ""

			switch config.localCacheKind {
			case localCacheMountOptions:
				cacheDir := randomCacheDir()

				if !IsPodMounter {
					// This hacky workaround is only needed for `SystemdMounter`,
					// `PodMounter` will automatically create cache folder within the Mountpoint Pod.
					basePodModifiers = append(basePodModifiers, func(pod *v1.Pod) {
						ensureCacheDirExistsInNode(pod, cacheDir)
					})
				}

				baseMountOptions = append(baseMountOptions, fmt.Sprintf("cache %s", cacheDir))
			case localCacheEmptyDir:
				enhanceContext = func(ctx context.Context) context.Context {
					return contextWithVolumeAttributes(ctx, map[string]string{
						"cache":                  "emptyDir",
						"cacheEmptyDirSizeLimit": "32Mi",
						"cacheEmptyDirMedium":    "Memory",
					})
				}
			case localCacheEBSEphemeral:
				scName := createEBSCacheSC(ctx, f)
				enhanceContext = func(ctx context.Context) context.Context {
					return contextWithVolumeAttributes(ctx, map[string]string{
						"cache":                                "ephemeral",
						"cacheEphemeralStorageClassName":       scName,
						"cacheEphemeralStorageResourceRequest": "10Mi",
					})
				}
			}

			if config.useExpressCache {
				client := s3client.New()
				cacheBucketName, deleteBucket := client.CreateDirectoryBucket(ctx)
				deferCleanup(deleteBucket)
				baseMountOptions = append(baseMountOptions, fmt.Sprintf("cache-xz %s", cacheBucketName))
				expressCacheBucketName = cacheBucketName
			}
		})

		It("basic file operations as root", func(ctx context.Context) {
			ctx = enhanceContext(ctx)

			mountOptions := append(baseMountOptions, "allow-delete")
			podModifiers := append(basePodModifiers, func(pod *v1.Pod) {
				pod.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To(root)
				pod.Spec.Containers[0].SecurityContext.RunAsGroup = ptr.To(root)
			})

			pod, bucketName := createPod(ctx, mountOptions, podModifiers...)
			checkBasicFileOperations(ctx, pod, bucketName, e2epod.VolumeMountPath1)
		})

		It("basic file operations as non-root", func(ctx context.Context) {
			ctx = enhanceContext(ctx)

			mountOptions := append(baseMountOptions,
				"allow-delete",
				"allow-other",
				fmt.Sprintf("uid=%d", defaultNonRootUser),
				fmt.Sprintf("gid=%d", defaultNonRootGroup))
			podModifiers := append([]func(*v1.Pod){podModifierNonRoot}, basePodModifiers...)

			pod, bucketName := createPod(ctx, mountOptions, podModifiers...)
			checkBasicFileOperations(ctx, pod, bucketName, e2epod.VolumeMountPath1)
		})

		It("two containers in the same pod using the same cache", func(ctx context.Context) {
			ctx = enhanceContext(ctx)

			testFile := filepath.Join(e2epod.VolumeMountPath1, "helloworld.txt")

			mountOptions := append(baseMountOptions, "allow-delete")
			podModifiers := append(basePodModifiers, func(pod *v1.Pod) {
				// Make it init container to ensure it runs before regular containers
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, v1.Container{
					Name:  "populate-cache",
					Image: e2epod.GetDefaultTestImage(),
					Command: e2epod.GenerateScriptCmd(
						fmt.Sprintf("echo 'hello world!' > %s && cat %s | grep -q 'hello world!'", testFile, testFile)),
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName1,
							MountPath: e2epod.VolumeMountPath1,
						},
					},
				})
			})

			pod, _ := createPod(ctx, mountOptions, podModifiers...)
			e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q 'hello world!'", testFile))
		})

		// If we're testing multi-level cache, add two more test cases:
		// 	1) Ensure it still works if local-cache is wiped out
		// 	1) Ensure it still works if Express-cache is wiped out
		if config.localCacheKind != localCacheUnspecified && config.useExpressCache {
			It("should use Express cache if local cache is empty", func(ctx context.Context) {
				ctx = enhanceContext(ctx)

				mountOptions := append(baseMountOptions, "allow-delete")
				podModifiers := append(basePodModifiers, func(pod *v1.Pod) {
					pod.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To(root)
					pod.Spec.Containers[0].SecurityContext.RunAsGroup = ptr.To(root)
				})

				pod, bucketName := createPod(ctx, mountOptions, podModifiers...)

				seed := time.Now().UTC().UnixNano()
				testWriteSize := 1024 // 1KB

				first := filepath.Join(e2epod.VolumeMountPath1, "first")

				checkWriteToPath(f, pod, first, testWriteSize, seed)
				// Initial read should work and populate both local and Express cache
				for range 3 {
					checkReadFromPath(f, pod, first, testWriteSize, seed)
				}

				// Now remove the file from S3 and wipe out local cache
				deleteObjectFromS3(ctx, bucketName, "first")
				e2evolume.VerifyExecInPodSucceed(f, pod, "rm -rf /cache/*")

				// Reading should still work
				for range 3 {
					checkReadFromPath(f, pod, first, testWriteSize, seed)
				}
			})

			It("should use local cache if Express cache is empty", func(ctx context.Context) {
				ctx = enhanceContext(ctx)

				mountOptions := append(baseMountOptions, "allow-delete")
				podModifiers := append(basePodModifiers, func(pod *v1.Pod) {
					pod.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To(root)
					pod.Spec.Containers[0].SecurityContext.RunAsGroup = ptr.To(root)
				})

				pod, bucketName := createPod(ctx, mountOptions, podModifiers...)

				seed := time.Now().UTC().UnixNano()
				testWriteSize := 1024 // 1KB

				first := filepath.Join(e2epod.VolumeMountPath1, "first")

				checkWriteToPath(f, pod, first, testWriteSize, seed)
				// Initial read should work and populate both local and Express cache
				for range 3 {
					checkReadFromPath(f, pod, first, testWriteSize, seed)
				}

				// Now remove the file from S3 and wipe out Express cache
				deleteObjectFromS3(ctx, bucketName, "first")
				s3client.New().WipeoutBucket(ctx, expressCacheBucketName)

				// Reading should still work
				for range 3 {
					checkReadFromPath(f, pod, first, testWriteSize, seed)
				}
			})
		}
	}

	Describe("Cache", func() {
		Describe("MountOptions (Legacy)", func() {
			Describe("Local", func() {
				testCache(cacheTestConfig{
					localCacheKind: localCacheMountOptions,
				})
			})

			Describe("Multi-Level", Serial, func() {
				testCache(cacheTestConfig{
					localCacheKind:  localCacheMountOptions,
					useExpressCache: true,
				})
			})
		})

		Describe("EmptyDir", func() {
			BeforeEach(func() {
				if !IsPodMounter {
					Skip("Skipping `emptyDir` cache tests for `SystemdMounter`")
				}
			})

			Describe("Local", func() {
				testCache(cacheTestConfig{
					localCacheKind: localCacheEmptyDir,
				})
			})

			Describe("Multi-Level", Serial, func() {
				testCache(cacheTestConfig{
					localCacheKind:  localCacheEmptyDir,
					useExpressCache: true,
				})
			})
		})

		Describe("EBS Ephemeral Volume", Ordered, Serial, func() {
			BeforeAll(func(ctx context.Context) {
				if ebsCSIDriverDaemonSet(ctx, f) == nil {
					Skip("EBS CSI Driver is not installed, skipping EBS ephemeral volume cache tests")
				}
			})

			BeforeEach(func() {
				if !IsPodMounter {
					Skip("Skipping `ephemeral` cache tests for `SystemdMounter`")
				}
			})

			Describe("Local", func() {
				testCache(cacheTestConfig{
					localCacheKind: localCacheEBSEphemeral,
				})
			})

			Describe("Multi-Level", func() {
				testCache(cacheTestConfig{
					localCacheKind:  localCacheEBSEphemeral,
					useExpressCache: true,
				})
			})
		})

		Describe("Express", Serial, func() {
			testCache(cacheTestConfig{
				useExpressCache: true,
			})
		})
	})
}

// deleteObjectFromS3 deletes an object from given bucket by using S3 SDK.
// This is useful to create some side-effects by bypassing Mountpoint.
func deleteObjectFromS3(ctx context.Context, bucket string, key string) {
	client := s3.NewFromConfig(awsConfig(ctx))
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	framework.ExpectNoError(err)
}

func randomCacheDir() string {
	return filepath.Join("/tmp/mp-cache", uuid.New().String())
}

// createEBSCacheSC creates a StorageClass to provision an EBS volume as local-cache.
// It automatically cleans up SC after the test-case.
func createEBSCacheSC(ctx context.Context, f *framework.Framework) string {
	scName := f.UniqueName + "-sc"

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: scName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
		ReclaimPolicy:     ptr.To(v1.PersistentVolumeReclaimDelete),
		Parameters: map[string]string{
			"encrypted": "true",
		},
	}

	framework.Logf("Creating StorageClass %s with EBS CSI Driver provisioner", scName)
	_, err := f.ClientSet.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
	framework.ExpectNoError(err, "Failed to create StorageClass for cache")

	DeferCleanup(func(ctx context.Context) {
		framework.Logf("Deleting StorageClass %s", scName)
		err := f.ClientSet.StorageV1().StorageClasses().Delete(ctx, scName, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "Failed to delete StorageClass for cache")
	})

	return scName
}

// ebsCSIDriverDaemonSet returns the DaemonSet of EBS CSI Driver if its installed in the cluster.
func ebsCSIDriverDaemonSet(ctx context.Context, f *framework.Framework) *appsv1.DaemonSet {
	client := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace)
	ds, err := client.Get(ctx, "ebs-csi-node", metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return ds
}

// ensureCacheDirExistsInNode adds a hostPath for given `cacheDir` with `DirectoryOrCreate` type.
// This hack required because Mountpoint process is running on the underlying host and not inside the container,
// so we need to ensure cache directory exists on the host.
// This hack hopefully will go away with https://github.com/awslabs/mountpoint-s3-csi-driver/issues/279.
func ensureCacheDirExistsInNode(pod *v1.Pod, cacheDir string) {
	cacheVolumeMount := v1.VolumeMount{
		Name:      "make-cache-dir",
		MountPath: "/cache",
	}

	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{}
	}
	// We need to set this false at Pod-level as `chmod-cache-dir` needs to run as `root` and this
	// would prevent container creation if its true.
	pod.Spec.SecurityContext.RunAsNonRoot = ptr.To(false)

	// The directory created with `DirectoryOrCreate` will have 0755 permissions and will be owned by kubelet.
	// Unless we change permissions here, non-root containers won't be able to access to the cache dir.
	pod.Spec.InitContainers = append(pod.Spec.DeepCopy().InitContainers, v1.Container{
		Name:    "chmod-cache-dir",
		Image:   e2epod.GetDefaultTestImage(),
		Command: e2epod.GenerateScriptCmd("chmod -R 777 /cache"),
		SecurityContext: &v1.SecurityContext{
			RunAsUser:  ptr.To(root),
			RunAsGroup: ptr.To(root),
		},
		VolumeMounts: []v1.VolumeMount{cacheVolumeMount},
	})
	pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
		Name: "make-cache-dir",
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: cacheDir,
				Type: ptr.To(v1.HostPathDirectoryOrCreate),
			},
		},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, cacheVolumeMount)
}
