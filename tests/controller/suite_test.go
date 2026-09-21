package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/awslabs/mountpoint-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
)

const s3CSIDriver = "s3.csi.aws.com"
const ebsCSIDriver = "ebs.csi.aws.com"

const defaultNamespace = "default"
const defaultContainerImage = "public.ecr.aws/docker/library/busybox:stable-musl"

// Configuration values passed for `mppod.Config` while creating a controller to use in tests.
const mountpointNamespace = "mount-s3"
const mountpointVersion = "1.10.0"
const mountpointPriorityClassName = "mount-s3-critical"
const preemptingPodPriorityClassName = "mount-s3-preempting-critical"
const headroomPodPriorityClassName = "mount-s3-headroom"
const mountpointContainerCommand = "/bin/aws-s3-csi-mounter"
const mountpointImage = "mp-image:latest"
const headroomImage = "pause:latest"
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
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("Bootstrapping test environment")

	crdv2.AddToScheme(scheme.Scheme)
	testEnv, cfg, k8sClient = createClient()

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())

	if err := crdv2.SetupManagerIndices(k8sManager); err != nil {
		Expect(err).NotTo(HaveOccurred())
	}

	reconciler := csicontroller.NewReconciler(k8sManager.GetClient(), reconcilerConfig(), logf.Log)
	err = reconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "Failed to run manager")
	}()

	bootstrapCluster(ctx, k8sClient)
})

var _ = AfterSuite(func() {
	By("Tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

func createClient() (*envtest.Environment, *rest.Config, client.Client) {
	env := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{"../crd/mountpoints3podattachments-crd.yaml"},
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	restCfg, err := env.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(restCfg).NotTo(BeNil())

	apiClient, err := client.New(restCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(apiClient).NotTo(BeNil())
	return env, restCfg, apiClient
}

func bootstrapCluster(ctx context.Context, client client.Client) {
	GinkgoHelper()

	createMountpointNamespace(ctx, client)
	createDefaultServiceAccount(ctx, client)
	createMountpointPriorityClasses(ctx, client)
}

// createMountpointNamespace creates Mountpoint namespace in the control plane.
func createMountpointNamespace(ctx context.Context, client client.Client) {
	By(fmt.Sprintf("Creating Mountpoint namespace %q", mountpointNamespace))
	namespace := &corev1.Namespace{Name: mountpointNamespace}
	Expect(client.Create(ctx, namespace)).To(Succeed())
	waitForObjectWithClient(ctx, client, namespace)
}

// createDefaultServiceAccount creates default service account in the control plane.
func createDefaultServiceAccount(ctx context.Context, client client.Client) {
	sa := &corev1.ServiceAccount{
		Name:      "default",
		Namespace: defaultNamespace,
	}

	By(fmt.Sprintf("Creating default service account in %q", mountpointNamespace))
	Expect(client.Create(ctx, sa)).To(Succeed())
	waitForObjectWithClient(ctx, client, sa)
}

// createMountpointPriorityClasses creates priority classes for Mountpoint/Headroom Pods.
func createMountpointPriorityClasses(ctx context.Context, client client.Client) {
	for _, name := range []string{
		mountpointPriorityClassName,
		preemptingPodPriorityClassName,
		headroomPodPriorityClassName,
	} {
		By(fmt.Sprintf("Creating priority class  %q for Mountpoint Pods", name))
		priorityClass := &schedulingv1.PriorityClass{Name: name, Value: 1000000}
		Expect(client.Create(ctx, priorityClass)).To(Succeed())
		waitForObjectWithClient(ctx, client, priorityClass)
	}
}

func reconcilerConfig() mppod.Config {
	return mppod.Config{
		Namespace:                   mountpointNamespace,
		MountpointVersion:           mountpointVersion,
		PriorityClassName:           mountpointPriorityClassName,
		PreemptingPriorityClassName: preemptingPodPriorityClassName,
		HeadroomPriorityClassName:   headroomPodPriorityClassName,
		Container: mppod.ContainerConfig{
			Command:         mountpointContainerCommand,
			Image:           mountpointImage,
			HeadroomImage:   headroomImage,
			ImagePullPolicy: mountpointImagePullPolicy,
		},
		CSIDriverVersion:  version.GetVersion().DriverVersion,
		PodLabels:         map[string]string{"test-label": "test-value", "env": "test"},
		HeadroomPodLabels: map[string]string{"headroom-label": "headroom-value", "tier": "headroom"},
	}
}
