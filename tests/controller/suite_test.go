package controller_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
	"github.com/go-logr/logr"
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
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/awslabs/aws-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/version"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

const s3CSIDriver = "s3.csi.aws.com"
const ebsCSIDriver = "ebs.csi.aws.com"

const defaultNamespace = "default"
const defaultContainerImage = "public.ecr.aws/docker/library/busybox:stable-musl"

// Configuration values passed for `mppod.Config` while creating a controller to use in tests.
const mountpointNamespace = "mount-s3"
const mountpointVersion = "1.10.0"
const mountpointPriorityClassName = "mount-s3-critical"
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
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("Bootstrapping test environment")

	crdv1beta.AddToScheme(scheme.Scheme)
	testEnv = &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{"../crd/mountpoints3podattachments-crd.yaml"},
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())

	IndexMountpointS3PodAttachmentFields(logf.Log.WithName("controller-test"), k8sManager)

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
	createDefaultServiceAccount()
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

// createDefaultServiceAccount creates default service account in the control plane.
func createDefaultServiceAccount() {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: defaultNamespace,
		},
	}

	By(fmt.Sprintf("Creating default service account in %q", mountpointNamespace))
	Expect(k8sClient.Create(ctx, sa)).To(Succeed())
	waitForObject(sa)
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

func IndexMountpointS3PodAttachmentFields(log logr.Logger, mgr manager.Manager) {
	indexField(log, mgr, crdv1beta.FieldNodeName, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.NodeName })
	indexField(log, mgr, crdv1beta.FieldPersistentVolumeName, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.PersistentVolumeName })
	indexField(log, mgr, crdv1beta.FieldVolumeID, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.VolumeID })
	indexField(log, mgr, crdv1beta.FieldMountOptions, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.MountOptions })
	indexField(log, mgr, crdv1beta.FieldAuthenticationSource, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.AuthenticationSource })
	indexField(log, mgr, crdv1beta.FieldWorkloadFSGroup, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadFSGroup })
	indexField(log, mgr, crdv1beta.FieldWorkloadServiceAccountName, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadServiceAccountName })
	indexField(log, mgr, crdv1beta.FieldWorkloadNamespace, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadNamespace })
	indexField(log, mgr, crdv1beta.FieldWorkloadServiceAccountIAMRoleARN, func(cr *crdv1beta.MountpointS3PodAttachment) string { return cr.Spec.WorkloadServiceAccountIAMRoleARN })
}

func indexField(log logr.Logger, mgr manager.Manager, field string, extractor func(*crdv1beta.MountpointS3PodAttachment) string) {
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &crdv1beta.MountpointS3PodAttachment{}, field, func(obj client.Object) []string {
		return []string{extractor(obj.(*crdv1beta.MountpointS3PodAttachment))}
	})
	if err != nil {
		log.Error(err, fmt.Sprintf("Failed to create a %s field indexer", field))
		os.Exit(1)
	}
}
