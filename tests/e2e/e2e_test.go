package e2e

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/tests/e2e/customsuites"
	"github.com/scality/mountpoint-s3-csi-driver/tests/e2e/pkg/s3client"

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
	// Finalize and validate the test context after all flags are parsed.
	// This sets up global test configuration (e.g., kubeconfig, kubectl path, timeouts)
	// and ensures the E2E framework is ready to run tests.
	f.AfterReadingAllFlags(&f.TestContext)

	flag.StringVar(&AccessKeyId, "access-key-id", "", "S3 access key, e.g. accessKey1")
	flag.StringVar(&SecretAccessKey, "secret-access-key", "", "S3 secret access key, e.g. verySecretKey1")
	flag.StringVar(&S3EndpointUrl, "s3-endpoint-url", "", "S3 endpoint URL, e.g. http://s3.scality.com:8000")
	flag.BoolVar(&Performance, "performance", false, "run performance tests")
	flag.Parse()

	// Check if mandatory flags are provided
	if AccessKeyId == "" || SecretAccessKey == "" || S3EndpointUrl == "" {
		fmt.Println("Error: --access-key-id, --secret-access-key, and --s3-endpoint-url are required flags")
		os.Exit(1)
	}

	s3client.DefaultAccessKey = AccessKeyId
	s3client.DefaultSecretAccessKey = SecretAccessKey
	s3client.DefaultS3EndpointUrl = S3EndpointUrl
}

func TestE2E(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Scality S3 CSI Driver E2E Suite")
}

var CSITestSuites = []func() framework.TestSuite{
	// [sig-storage] CSI Volumes Test: Basic Data Persistence with Pre-provisioned PV.
	//
	// This test verifies that the S3 CSI driver supports storing and retrieving data
	// using a pre-provisioned PersistentVolume (PV). The backing S3 bucket is
	// created by the test driver's CreateVolume method.
	//
	// The test performs the following steps:
	// 1. Creates a Kubernetes namespace for test isolation.
	// 2. Creates an S3 bucket via the test driver.
	// 3. Sets up a pre-provisioned PV referencing this S3 bucket and a corresponding PVC.
	// 4. Launches a pod that mounts the PVC.
	// 5. Writes data to the S3 bucket through the mounted volume.
	// 6. Reads the data back and compares it to the expected content.
	// 7. Deletes all created resources (pod, PVC, PV, S3 bucket).
	//
	// This is part of the standard CSI driver compliance test suite and is
	// used to verify functional support for static provisioning with S3 storage.
	testsuites.InitVolumesTestSuite,

	// Custom test suites specific to Scality S3 CSI driver.
	customsuites.InitS3MountOptionsTestSuite,
	customsuites.InitS3MultiVolumeTestSuite,
	customsuites.InitS3CSICacheTestSuite,
	customsuites.InitS3FilePermissionsTestSuite,
}

// initS3Driver initializes and returns an S3 CSI driver implementation for E2E testing.
// This function creates a test driver that implements required Kubernetes framework interfaces
// (TestDriver, PreprovisionedVolumeTestDriver, PreprovisionedPVTestDriver). The framework
// orchestrates testing by calling the driver's methods to:
// - Create S3 buckets via CreateVolume
// - Configure the buckets as CSI persistent volumes via GetPersistentVolumeSource
// - Clean up by deleting buckets via DeleteVolume
// This implementation supports both ReadWriteMany and ReadOnlyMany access modes and only works
// with pre-provisioned persistent volumes.
var _ = utils.SIGDescribe("CSI Volumes", func() {
	if Performance {
		CSITestSuites = []func() framework.TestSuite{customsuites.InitS3PerformanceTestSuite}
	}
	curDriver := initS3Driver()

	args := framework.GetDriverNameWithFeatureTags(curDriver)
	args = append(args, func() {
		framework.DefineTestSuites(curDriver, CSITestSuites)
	})
	f.Context(args...)
})
