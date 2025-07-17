package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const headroomSchedulingGate = "experimental.s3.csi.aws.com/reserve-headroom-for-mppod"
const headroomPriorityClass = "mount-s3-headroom"

type s3CSIHeadroomTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3HeadroomTestSuite() storageframework.TestSuite {
	return &s3CSIHeadroomTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "headroom",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIHeadroomTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIHeadroomTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSIHeadroomTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"cache", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

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

	checkBasicFileOperations := func(pod *v1.Pod, volPath string) {
		seed := time.Now().UTC().UnixNano()
		filename := fmt.Sprintf("test-%d.txt", seed)
		path := filepath.Join(volPath, filename)
		testWriteSize := 1024 // 1KB

		checkWriteToPath(f, pod, path, testWriteSize, seed)
		checkReadFromPath(f, pod, path, testWriteSize, seed)
		checkListingPathWithEntries(f, pod, volPath, []string{filename})
		checkDeletingPath(f, pod, path)
		checkListingPathWithEntries(f, pod, volPath, []string{})
	}

	Describe("Headroom", Ordered, func() {
		BeforeAll(func(ctx context.Context) {
			_, err := f.ClientSet.SchedulingV1().PriorityClasses().Get(ctx, headroomPriorityClass, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					Skip("`experimental.reserveHeadroomForMountpointPods` feature is not enabled, skipping headroom tests")
					return
				}

				framework.Failf("Failed to query priority class for Headroom Pods: %s", err)
			}
		})

		It("should get scheduled automatically after reserving headroom", func(ctx context.Context) {
			vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol.CleanupResource)

			pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
			pod.Spec.SchedulingGates = []v1.PodSchedulingGate{{Name: headroomSchedulingGate}}

			pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

			checkBasicFileOperations(pod, e2epod.VolumeMountPath1)
		})

		It("should get scheduled automatically after reserving headroom for multiple volumes", func(ctx context.Context) {
			vol1 := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol1.CleanupResource)
			vol2 := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol2.CleanupResource)

			pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol1.Pvc, vol2.Pvc}, admissionapi.LevelBaseline, "")
			pod.Spec.SchedulingGates = []v1.PodSchedulingGate{{Name: headroomSchedulingGate}}

			pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

			checkBasicFileOperations(pod, fmt.Sprintf(e2epod.VolumeMountPathTemplate, 1))
			checkBasicFileOperations(pod, fmt.Sprintf(e2epod.VolumeMountPathTemplate, 2))
		})

		It("should get scheduled automatically after reserving headroom with resource specifications", func(ctx context.Context) {
			vol := createVolumeResourceWithMountOptions(contextWithVolumeAttributes(ctx, map[string]string{
				"mountpointContainerResourcesRequestsCpu":    "100m",
				"mountpointContainerResourcesRequestsMemory": "64Mi",
				"mountpointContainerResourcesLimitsCpu":      "200m",
				"mountpointContainerResourcesLimitsMemory":   "128Mi",
			}), l.config, pattern, []string{"allow-delete"})
			deferCleanup(vol.CleanupResource)

			pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
			pod.Spec.SchedulingGates = []v1.PodSchedulingGate{{Name: headroomSchedulingGate}}

			pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
			framework.ExpectNoError(err)
			deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

			checkBasicFileOperations(pod, e2epod.VolumeMountPath1)
		})
	})
}
