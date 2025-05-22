package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/scality/mountpoint-s3-csi-driver/cmd/scality-csi-controller/csicontroller"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/version"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
)

const (
	s3CSIDriver  = "s3.csi.scality.com"
	ebsCSIDriver = "ebs.csi.aws.com"
)

const (
	defaultNamespace      = "default"
	defaultContainerImage = "public.ecr.aws/docker/library/busybox:stable-musl"
)

// Configuration values passed for `mppod.Config` while creating a controller to use in tests.
const (
	mountpointNamespace         = "mount-s3"
	mountpointVersion           = "1.10.0"
	mountpointPriorityClassName = "mount-s3-critical"
	mountpointContainerCommand  = "/bin/scality-s3-csi-mounter"
	mountpointImage             = "mp-image:latest"
	mountpointImagePullPolicy   = corev1.PullNever
)

// Since most things are eventually consistent in the control plane,
// we need to use `Eventually` Ginkgo construct to wait for updates to applied,
// these timeouts should be good default for most use-cases.
const (
	defaultWaitTimeout     = 5 * time.Second
	defaultWaitRetryPeriod = 100 * time.Millisecond
)

// Variables to use during the test, mainly `k8sClient` to interact with the control plane.
var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
)

// Context to cancel after the suite to stop the controller and the manager.
var (
	ctx    context.Context
	cancel context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

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
		Namespace:         mountpointNamespace,
		MountpointVersion: mountpointVersion,
		PriorityClassName: mountpointPriorityClassName,
		Container: mppod.ContainerConfig{
			Command:         mountpointContainerCommand,
			Image:           mountpointImage,
			ImagePullPolicy: mountpointImagePullPolicy,
		},
		CSIDriverVersion: version.GetVersion().DriverVersion,
	}).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "Failed to run manager")
	}()

	createMountpointNamespace()
	createMountpointPriorityClass()
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

// createMountpointPriorityClass creates priority class for Mountpoint Pods.
func createMountpointPriorityClass() {
	By(fmt.Sprintf("Creating priority class  %q for Mountpoint Pods", mountpointPriorityClassName))
	priorityClass := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: mountpointPriorityClassName},
		Value:      1000000,
	}
	Expect(k8sClient.Create(ctx, priorityClass)).To(Succeed())
	waitForObject(priorityClass)
}
