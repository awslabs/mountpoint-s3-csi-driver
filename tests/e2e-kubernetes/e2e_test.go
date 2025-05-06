package e2e

import (
	"flag"
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/tests/e2e-kubernetes/s3client"
	custom_testsuites "github.com/awslabs/aws-s3-csi-driver/tests/e2e-kubernetes/testsuites"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
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
	flag.StringVar(&BucketPrefix, "bucket-prefix", "local", "prefix for temporary buckets")
	flag.BoolVar(&Performance, "performance", false, "run performance tests")
	flag.BoolVar(&IMDSAvailable, "imds-available", false, "indicates whether instance metadata service is available")
	flag.BoolVar(&IsPodMounter, "pod-mounter", false, "indicates whether CSI Driver is installed with Pod Mounter or not")
	flag.Parse()

	s3client.DefaultRegion = BucketRegion
	custom_testsuites.DefaultRegion = BucketRegion
	custom_testsuites.ClusterName = ClusterName
	custom_testsuites.IMDSAvailable = IMDSAvailable
	custom_testsuites.IsPodMounter = IsPodMounter
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
}

// This executes testSuites for csi volumes.
var _ = utils.SIGDescribe("CSI Volumes", func() {
	if Performance {
		CSITestSuites = []func() framework.TestSuite{custom_testsuites.InitS3CSIPerformanceTestSuite}
	}
	curDriver := initS3Driver()

	args := framework.GetDriverNameWithFeatureTags(curDriver)
	args = append(args, func() {
		framework.DefineTestSuites(curDriver, CSITestSuites)
	})
	f.Context(args...)
})
