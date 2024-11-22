// WIP: Part of https://github.com/awslabs/mountpoint-s3-csi-driver/issues/279.
//
// `aws-s3-csi-controller` is the entrypoint binary for the CSI Driver's controller component.
// It is responsible for acting on cluster events and spawning Mountpoint Pods when necessary.
// It is also responsible for managing Mountpoint Pods, for example it ensures that completed Mountpoint Pods gets deleted.
// It doesn't implement CSI's controller service as of today.
package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/awslabs/aws-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

var mountpointNamespace = flag.String("mountpoint-namespace", "mount-s3", "Namespace to spawn Mountpoint Pods in.")
var mountpointVersion = flag.String("mountpoint-version", os.Getenv("MOUNTPOINT_VERSION"), "Version of Mountpoint within the given Mountpoint image.")
var mountpointImage = flag.String("mountpoint-image", os.Getenv("MOUNTPOINT_IMAGE"), "Image of Mountpoint to use in spawned Mountpoint Pods.")
var mountpointImagePullPolicy = flag.String("mountpoint-image-pull-policy", os.Getenv("MOUNTPOINT_IMAGE_PULL_POLICY"), "Pull policy of Mountpoint images.")
var mountpointContainerCommand = flag.String("mountpoint-container-command", "/bin/aws-s3-csi-mounter", "Entrypoint command of the Mountpoint Pods.")

func main() {
	flag.Parse()

	logf.SetLogger(zap.New())

	log := logf.Log.WithName(csicontroller.Name)

	mgr, err := manager.New(config.GetConfigOrDie(), manager.Options{})
	if err != nil {
		log.Error(err, "Failed to create a new manager")
		os.Exit(1)
	}

	err = csicontroller.NewReconciler(mgr.GetClient(), mppod.Config{
		Namespace: *mountpointNamespace,
		Version:   *mountpointVersion,
		Container: mppod.ContainerConfig{
			Command:         *mountpointContainerCommand,
			Image:           *mountpointImage,
			ImagePullPolicy: corev1.PullPolicy(*mountpointImagePullPolicy),
		},
	}).SetupWithManager(mgr)
	if err != nil {
		log.Error(err, "Failed to create controller")
		os.Exit(1)
	}

	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Error(err, "Failed to start manager")
		os.Exit(1)
	}
}
