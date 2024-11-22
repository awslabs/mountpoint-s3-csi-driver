package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/awslabs/aws-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

const s3CSIDriver = "s3.csi.aws.com"
const ebsCSIDriver = "ebs.csi.aws.com"

const defaultNamespace = "default"
const defaultContainerImage = "busybox:latest"

// Configuration values passed for `mppod.Config` while creating a controller to use in tests.
const mountpointNamespace = "mount-s3"
const mountpointVersion = "1.10.0"
const mountpointContainerCommand = "/bin/aws-s3-csi-mounter"
const mountpointImage = "mp-image:latest"
const mountpointImagePullPolicy = corev1.PullNever

// Since most things are eventually consistent in the control plane,
// we need to use `Eventually` Ginkgo construct to wait for updates to applied,
// these timeouts should be good default for most use-cases.
const defaultWaitTimeout = 5 * time.Second
const defaultWaitRetryPeriod = 100 * time.Millisecond

// Variables to use during the test, mainly `k8sClient` to interact with the control plane.
var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment

// Context to cancel after the suite to stop the controller and the manager.
var ctx context.Context
var cancel context.CancelFunc

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.Level(zapcore.DebugLevel), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("Bootstrapping test environment")
	testEnv = &envtest.Environment{}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())

	err = csicontroller.NewReconciler(k8sManager.GetClient(), mppod.Config{
		Namespace: mountpointNamespace,
		Version:   mountpointVersion,
		Container: mppod.ContainerConfig{
			Command:         mountpointContainerCommand,
			Image:           mountpointImage,
			ImagePullPolicy: mountpointImagePullPolicy,
		},
	}).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "Failed to run manager")
	}()

	createMountpointNamespace()
})

var _ = AfterSuite(func() {
	By("Tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// createMountpointNamespace creates Mountpoint namespace in the control plane.
func createMountpointNamespace() {
	By(fmt.Sprintf("Creating Mountpoint namespace %q", mountpointNamespace))
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mountpointNamespace}}
	Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
	waitForObject(namespace)
}
