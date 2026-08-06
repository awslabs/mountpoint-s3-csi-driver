package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/s3client"
	custom_testsuites "github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/testsuites"
	"github.com/distribution/reference"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// The upstream Kubernetes e2e framework registers many unrelated conformance
	// specs (e.g. "[sig-storage] Projected configMap", "[sig-node] Variable
	// Expansion") into the suite as an import side-effect of the test framework
	// packages we depend on. We only want to run this driver's specs, which the
	// framework tags with "[Driver: s3.csi.aws.com]".
	suiteConfig, reporterConfig := ginkgo.GinkgoConfiguration()
	if len(suiteConfig.FocusStrings) == 0 {
		suiteConfig.FocusStrings = []string{`\[Driver: s3\.csi\.aws\.com\]`}
	}
	ginkgo.RunSpecs(t, "S3 CSI E2E Suite", suiteConfig, reporterConfig)
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
		// 10 minute timeout -> 2 minutes `cleanupInterval` + 2 minutes `staleAttachmentThreshold` + 5 minutes `CrashLoopBackoff` restarts + 1 minute buffer for actual delete
	}).WithTimeout(10*time.Minute).WithPolling(10*time.Second).Should(gomega.Equal(0),
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
