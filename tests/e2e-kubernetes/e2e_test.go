package e2e

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/s3client"
	custom_testsuites "github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/testsuites"
	"github.com/distribution/reference"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	f "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
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

	// Override the upstream K8s e2e framework's default test image (BusyBox) with
	// our TEST_POD_IMAGE so that tests like InitVolumesTestSuite use an image
	// accessible from all regions instead of registry.k8s.io.
	overrideUpstreamTestImage()
}

func overrideUpstreamTestImage() {
	img := os.Getenv("TEST_POD_IMAGE")
	if img == "" {
		img = "public.ecr.aws/amazonlinux/amazonlinux:2023"
	}

	ref, err := reference.Parse(img)
	if err != nil {
		panic(fmt.Sprintf("overrideUpstreamTestImage: invalid image reference %q: %v", img, err))
	}

	named, ok := ref.(reference.Named)
	if !ok {
		panic(fmt.Sprintf("overrideUpstreamTestImage: image reference %q has no name", img))
	}

	version := "latest"
	if tagged, ok := ref.(reference.Tagged); ok {
		version = tagged.Tag()
	}

	configs := imageutils.GetImageConfigs()
	cfg := configs[imageutils.BusyBox]
	cfg.SetRegistry(reference.Domain(named))
	cfg.SetName(reference.Path(named))
	cfg.SetVersion(version)
	configs[imageutils.BusyBox] = cfg
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
