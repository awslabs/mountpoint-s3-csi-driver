package custom_testsuites

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eevents "k8s.io/kubernetes/test/e2e/framework/events"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/s3client"
)

// s3CSIClusterTagTestSuite checks that `docs/iam/cluster-tag-policy.json` limits the CSI Driver to buckets
// tagged for its own cluster. It runs only if the CSI Driver gets its credentials through EKS Pod Identity
// (see `scripts/pod-identity.sh`), where EKS adds the `eks-cluster-name` session tag the policy reads.
type s3CSIClusterTagTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3ClusterTagTestSuite() storageframework.TestSuite {
	return &s3CSIClusterTagTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "clustertag",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIClusterTagTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIClusterTagTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSIClusterTagTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"clustertag", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		if ClusterName == "" {
			e2eskipper.Skipf("Cluster name is not set, skipping cluster tag tests")
		}
		if eksPodIdentityAgentDaemonSetForCluster(ctx, f) == nil ||
			podIdentityAssociationRoleARN(ctx, csiDriverServiceAccount(ctx, f)) == "" {
			e2eskipper.Skipf("CSI Driver does not use EKS Pod Identity, skipping cluster tag tests")
		}

		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	createVolume := func(ctx context.Context, bucket BucketOptions) *storageframework.VolumeResource {
		resource := createVolumeResourceWithAccessMode(contextWithBucketOptions(ctx, bucket), l.config, pattern, v1.ReadWriteMany)
		l.resources = append(l.resources, resource)
		return resource
	}

	expectFailToMount := func(ctx context.Context, bucket BucketOptions) {
		resource := createVolume(ctx, bucket)

		client := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
		pod, err := client.Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func(ctx context.Context) error {
			// The mount keeps failing, so force an immediate deletion.
			return e2epod.DeletePodWithGracePeriod(ctx, f.ClientSet, pod, 0)
		})

		eventSelector := fields.Set{
			"involvedObject.kind":      "Pod",
			"involvedObject.name":      pod.Name,
			"involvedObject.namespace": f.Namespace.Name,
			"reason":                   events.FailedMountVolume,
		}.AsSelector().String()
		framework.Logf("Waiting for FailedMount event: %s", eventSelector)

		// The policy denies the `ListObjectsV2` call Mountpoint makes on startup.
		const expectedErrMsg = "s3:ListBucket"
		err = e2eevents.WaitTimeoutForEvent(ctx, f.ClientSet, f.Namespace.Name, eventSelector, expectedErrMsg, 5*time.Minute)
		framework.ExpectNoError(err, "Expected FailedMount event containing %q", expectedErrMsg)

		pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodPending))
	}

	otherClusterTags := func() []s3types.Tag {
		return []s3types.Tag{{Key: aws.String(s3client.ClusterTagKey), Value: aws.String(ClusterName + "-other")}}
	}

	ginkgo.It("should mount a bucket tagged for this cluster with ABAC enabled", func(ctx context.Context) {
		resource := createVolume(ctx, BucketOptions{Tags: s3client.ClusterTags(), ABAC: true})

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		const testFilePath = e2epod.VolumeMountPath1 + "/file.txt"
		seed := time.Now().UTC().UnixNano()
		checkWriteToPathSucceed(ctx, f, pod, testFilePath, 1024, seed)
		checkReadFromPathSucceed(ctx, f, pod, testFilePath, 1024, seed)
	})

	ginkgo.It("should fail to mount a bucket tagged for another cluster", func(ctx context.Context) {
		expectFailToMount(ctx, BucketOptions{Tags: otherClusterTags(), ABAC: true})
	})

	ginkgo.It("should fail to mount a bucket without the cluster tag", func(ctx context.Context) {
		expectFailToMount(ctx, BucketOptions{ABAC: true})
	})

	ginkgo.It("should fail to mount a bucket tagged for this cluster with ABAC disabled", func(ctx context.Context) {
		expectFailToMount(ctx, BucketOptions{Tags: s3client.ClusterTags(), ABAC: false})
	})
}

//-- Bucket options utils

// BucketOptions sets how the test driver creates the bucket of a volume, instead of tagging it for
// the cluster under test and enabling ABAC.
type BucketOptions struct {
	Tags []s3types.Tag
	ABAC bool
}

const bucketOptionsKey contextKey = "bucketOptions"

// contextWithBucketOptions enhances given context with given bucket options.
// This value is used by `s3Driver.CreateVolume`, in the same way as `contextWithVolumeAttributes`.
func contextWithBucketOptions(ctx context.Context, options BucketOptions) context.Context {
	return context.WithValue(ctx, bucketOptionsKey, options)
}

// BucketOptionsFromContext returns bucket options set for given context, and whether there are any.
func BucketOptionsFromContext(ctx context.Context) (BucketOptions, bool) {
	options, ok := ctx.Value(bucketOptionsKey).(BucketOptions)
	return options, ok
}
