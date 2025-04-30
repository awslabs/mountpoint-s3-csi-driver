// WIP: Part of https://github.com/awslabs/mountpoint-s3-csi-driver/issues/279.
//
// `aws-s3-csi-controller` is the entrypoint binary for the CSI Driver's controller component.
// It is responsible for acting on cluster events and spawning Mountpoint Pods when necessary.
// It is also responsible for managing Mountpoint Pods, for example it ensures that completed Mountpoint Pods gets deleted.
// It doesn't implement CSI's controller service as of today.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/awslabs/aws-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
	"github.com/awslabs/aws-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/version"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/go-logr/logr"
)

var mountpointNamespace = flag.String("mountpoint-namespace", os.Getenv("MOUNTPOINT_NAMESPACE"), "Namespace to spawn Mountpoint Pods in.")
var mountpointVersion = flag.String("mountpoint-version", os.Getenv("MOUNTPOINT_VERSION"), "Version of Mountpoint within the given Mountpoint image.")
var mountpointPriorityClassName = flag.String("mountpoint-priority-class-name", os.Getenv("MOUNTPOINT_PRIORITY_CLASS_NAME"), "Priority class name of the Mountpoint Pods.")
var mountpointImage = flag.String("mountpoint-image", os.Getenv("MOUNTPOINT_IMAGE"), "Image of Mountpoint to use in spawned Mountpoint Pods.")
var mountpointImagePullPolicy = flag.String("mountpoint-image-pull-policy", os.Getenv("MOUNTPOINT_IMAGE_PULL_POLICY"), "Pull policy of Mountpoint images.")
var mountpointContainerCommand = flag.String("mountpoint-container-command", "/bin/aws-s3-csi-mounter", "Entrypoint command of the Mountpoint Pods.")

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(crdv1beta.AddToScheme(scheme))
}

func main() {
	flag.Parse()

	logf.SetLogger(zap.New())

	log := logf.Log.WithName(csicontroller.Name)
	conf := config.GetConfigOrDie()

	mgr, err := manager.New(conf, manager.Options{
		Scheme: scheme,
	})
	if err != nil {
		log.Error(err, "Failed to create a new manager")
		os.Exit(1)
	}

	IndexMountpointS3PodAttachmentFields(log, mgr)

	reconciler := csicontroller.NewReconciler(mgr.GetClient(), mppod.Config{
		Namespace:         *mountpointNamespace,
		MountpointVersion: *mountpointVersion,
		PriorityClassName: *mountpointPriorityClassName,
		Container: mppod.ContainerConfig{
			Command:         *mountpointContainerCommand,
			Image:           *mountpointImage,
			ImagePullPolicy: corev1.PullPolicy(*mountpointImagePullPolicy),
		},
		CSIDriverVersion: version.GetVersion().DriverVersion,
		ClusterVariant:   cluster.DetectVariant(conf, log),
	})

	if err := reconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "Failed to create controller")
		os.Exit(1)
	}

	if err := mgr.Add(csicontroller.NewStaleAttachmentCleaner(reconciler)); err != nil {
		log.Error(err, "Failed to add stale attachment cleaner to manager")
		os.Exit(1)
	}

	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Error(err, "Failed to start manager")
		os.Exit(1)
	}
}

// IndexMountpointS3PodAttachmentFields adds internal index on fields for our custom resource.
// This is needed for `List()` method to work with field filters.
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

// indexField adds index on a field.
func indexField(log logr.Logger, mgr manager.Manager, field string, extractor func(*crdv1beta.MountpointS3PodAttachment) string) {
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &crdv1beta.MountpointS3PodAttachment{}, field, func(obj client.Object) []string {
		return []string{extractor(obj.(*crdv1beta.MountpointS3PodAttachment))}
	})
	if err != nil {
		log.Error(err, fmt.Sprintf("Failed to create a %s field indexer", field))
		os.Exit(1)
	}
}
