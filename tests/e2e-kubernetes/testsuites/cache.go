package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
)

const volumeName1 = "volume1"
const root = int64(0)
const defaultNonRootGroup = int64(2000)

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

	createPod := func(ctx context.Context, additionalMountOptions []string, podModifiers ...func(*v1.Pod)) *v1.Pod {
		cacheDir := randomCacheDir()

		vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, append(additionalMountOptions, fmt.Sprintf("cache %s", cacheDir)))
		deferCleanup(vol.CleanupResource)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
		ensureCacheDirExistsInNode(pod, cacheDir)
		for _, pm := range podModifiers {
			pm(pod)
		}

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		return pod
	}

	Describe("Cache", func() {
		Describe("Local", func() {
			It("basic file operations as root", func(ctx context.Context) {
				pod := createPod(ctx, []string{"allow-delete"}, func(pod *v1.Pod) {
					pod.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To(root)
					pod.Spec.Containers[0].SecurityContext.RunAsGroup = ptr.To(root)
				})
				checkBasicFileOperations(f, pod, e2epod.VolumeMountPath1)
			})

			It("basic file operations as non-root", func(ctx context.Context) {
				mountOptions := []string{
					"allow-delete",
					"allow-other",
					fmt.Sprintf("uid=%d", *e2epod.GetDefaultNonRootUser()),
					fmt.Sprintf("gid=%d", defaultNonRootGroup),
				}
				pod := createPod(ctx, mountOptions, func(pod *v1.Pod) {
					pod.Spec.Containers[0].SecurityContext.RunAsUser = e2epod.GetDefaultNonRootUser()
					pod.Spec.Containers[0].SecurityContext.RunAsGroup = ptr.To(defaultNonRootGroup)
					pod.Spec.Containers[0].SecurityContext.RunAsNonRoot = ptr.To(true)
				})

				checkBasicFileOperations(f, pod, e2epod.VolumeMountPath1)
			})

			It("two containers in the same pod using the same cache", func(ctx context.Context) {
				testFile := filepath.Join(e2epod.VolumeMountPath1, "helloworld.txt")

				pod := createPod(ctx, []string{"allow-delete"}, func(pod *v1.Pod) {
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

				e2evolume.VerifyExecInPodSucceed(f, pod, fmt.Sprintf("cat %s | grep -q 'hello world!'", testFile))
			})
		})
	})
}

func randomCacheDir() string {
	return filepath.Join("/tmp/mp-cache", uuid.New().String())
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
