package e2e

import (
	"flag"
	"testing"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	f "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
)

var (
	PullRequest string
)

func init() {
	testing.Init()
	f.RegisterClusterFlags(flag.CommandLine) // configures kubeconfig flag
	f.RegisterCommonFlags(flag.CommandLine)  // configures kubectl flag
	f.AfterReadingAllFlags(&f.TestContext)

	// how kubecongig is found?
	flag.StringVar(&PullRequest, "pull-request", "local", "the associated pull request number if present")
	flag.Parse()
}

func TestE2E(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	// TODO: create bucket for PR
	ginkgo.RunSpecs(t, "S3 CSI E2E Suite")
}

var CSITestSuites = []func() framework.TestSuite{
	testsuites.InitVolumesTestSuite,
	// testsuites.InitVolumeIOTestSuite,
	// testsuites.InitVolumeModeTestSuite,
	// testsuites.InitSubPathTestSuite,
	// testsuites.InitProvisioningTestSuite,
	//testsuites.InitMultiVolumeTestSuite,
}

// This executes testSuites for csi volumes.
var _ = utils.SIGDescribe("CSI Volumes", func() {
	curDriver := initS3Driver()
	ginkgo.Context(framework.GetDriverNameWithFeatureTags(curDriver), func() {
		framework.DefineTestSuites(curDriver, CSITestSuites)
	})
})
