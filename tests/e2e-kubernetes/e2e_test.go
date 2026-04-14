package e2e

import (
	"context"
	"flag"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/s3client"
	custom_testsuites "github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/testsuites"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	f "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
)

func init() {
	testing.Init()
	f.RegisterClusterFlags(flag.CommandLine) // configures --kubeconfig flag
	f.RegisterCommonFlags(flag.CommandLine)  // configures --kubectl flag
	f.AfterReadingAllFlags(&f.TestContext)

	flag.StringVar(&CommitId, "commit-id", "local", "commit id will be used to name buckets")
	flag.StringVar(&BucketRegion, "bucket-region", "us-east-1", "region where temporary buckets will be created")
	flag.StringVar(&ClusterName, "cluster-name", "", "name of the cluster")
	flag.StringVar(&ClusterType, "cluster-type", "eksctl", "type of cluster (eksctl or openshift)")
	flag.StringVar(&BucketPrefix, "bucket-prefix", "local", "prefix for temporary buckets")
	flag.BoolVar(&Performance, "performance", false, "run performance tests")
	flag.BoolVar(&UpgradeTests, "run-upgrade-tests", false, "run upgrade tests")
	flag.BoolVar(&IMDSAvailable, "imds-available", false, "indicates whether instance metadata service is available")
	flag.Parse()

	s3client.DefaultRegion = BucketRegion
	custom_testsuites.DefaultRegion = BucketRegion
	custom_testsuites.ClusterName = ClusterName
	custom_testsuites.ClusterType = ClusterType
	custom_testsuites.IMDSAvailable = IMDSAvailable
}

func TestE2E(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "S3 CSI E2E Suite")
}

var CSITestSuites = []func() framework.TestSuite{
	// testsuites.InitCapacityTestSuite,
	testsuites.InitVolumesTestSuite, // success: writes 53 bytes to index.html file, reads and verifies content from another pod
	// testsuites.InitVolumeIOTestSuite,   // tries to open a file for writing multiple times, which is unsupported by MP
	// testsuites.InitVolumeModeTestSuite, // fail: tries to mount in block mode, success: check unused volume is not mounted
	// testsuites.InitSubPathTestSuite,
	// testsuites.InitProvisioningTestSuite,
	// testsuites.InitMultiVolumeTestSuite,
	// testsuites.InitVolumeExpandTestSuite,
	// testsuites.InitDisruptiveTestSuite,
	// testsuites.InitVolumeLimitsTestSuite,
	// testsuites.InitTopologyTestSuite,
	// testsuites.InitVolumeStressTestSuite,
	// testsuites.InitFsGroupChangePolicyTestSuite,
	// testsuites.InitSnapshottableTestSuite,
	// testsuites.InitSnapshottableStressTestSuite,
	// testsuites.InitVolumePerformanceTestSuite,
	// testsuites.InitReadWriteOncePodTestSuite,
	custom_testsuites.InitS3AccessModeTestSuite,
	custom_testsuites.InitS3CSIMultiVolumeTestSuite,
	custom_testsuites.InitS3MountOptionsTestSuite,
	custom_testsuites.InitS3CSICredentialsTestSuite,
	custom_testsuites.InitS3CSICacheTestSuite,
	custom_testsuites.InitS3CSIPodSharingTestSuite,
	custom_testsuites.InitS3TaintRemovalTestSuite,
	custom_testsuites.InitS3CSIEvictionOrderTestSuite,
	custom_testsuites.InitS3ProxyTestSuite,
}

func getCSITestSuites() []func() framework.TestSuite {
	suites := CSITestSuites
	// Headroom feature is not supported on OpenShift
	if ClusterType != "openshift" {
		suites = append(suites, custom_testsuites.InitS3HeadroomTestSuite)
	}
	return suites
}

// Wait for all Mountpoint pods in mount-s3 namespace to be cleaned up after tests complete.
// This ensures the mount-s3 namespace is not stuck with stale pods when the driver is uninstalled,
// which would cause the namespace to get stuck in Terminating state and block the next CI run's install.
var _ = ginkgo.SynchronizedAfterSuite(func() {}, func() {
	cs, err := f.LoadClientset()
	f.ExpectNoError(err, "creating kubernetes client")

	ctx := context.Background()
	f.Logf("Waiting for Mountpoint pods in mount-s3 namespace to be cleaned up...")
	gomega.Eventually(ctx, func(ctx context.Context) (int, error) {
		pods, err := cs.CoreV1().Pods("mount-s3").List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, err
		}
		if len(pods.Items) > 0 {
			names := make([]string, len(pods.Items))
			for i, pod := range pods.Items {
				names[i] = pod.Name
			}
			f.Logf("Still waiting for %d Mountpoint pod(s) to be cleaned up: %v", len(pods.Items), names)
		}
		return len(pods.Items), nil
	}).WithTimeout(5*time.Minute).WithPolling(10*time.Second).Should(gomega.Equal(0),
		"Mountpoint pods in mount-s3 namespace were not cleaned up in time")
	f.Logf("All Mountpoint pods cleaned up successfully")
})

// This executes testSuites for csi volumes.
var _ = utils.SIGDescribe("CSI Volumes", func() {
	var testSuites []func() framework.TestSuite
	if Performance {
		testSuites = []func() framework.TestSuite{custom_testsuites.InitS3CSIPerformanceTestSuite}
	} else if UpgradeTests {
		testSuites = []func() framework.TestSuite{custom_testsuites.InitS3CSIUpgradeTestSuite}
	} else {
		testSuites = getCSITestSuites()
	}
	curDriver := initS3Driver()

	args := framework.GetDriverNameWithFeatureTags(curDriver)
	args = append(args, func() {
		framework.DefineTestSuites(curDriver, testSuites)
	})
	f.Context(args...)
})
