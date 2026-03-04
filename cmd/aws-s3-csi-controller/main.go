// `aws-s3-csi-controller` is the entrypoint binary for the CSI Driver's controller component.
// It is responsible for acting on cluster events and spawning Mountpoint Pods when necessary.
// It is also responsible for managing Mountpoint Pods, for example it ensures that completed Mountpoint Pods gets deleted.
// It doesn't implement CSI's controller service as of today.
//
// See /docs/ARCHITECTURE.md for more details.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/awslabs/mountpoint-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/go-logr/logr"
)

var mountpointNamespace = flag.String("mountpoint-namespace", os.Getenv("MOUNTPOINT_NAMESPACE"), "Namespace to spawn Mountpoint Pods in.")
var mountpointVersion = flag.String("mountpoint-version", os.Getenv("MOUNTPOINT_VERSION"), "Version of Mountpoint within the given Mountpoint image.")
var mountpointPriorityClassName = flag.String("mountpoint-priority-class-name", os.Getenv("MOUNTPOINT_PRIORITY_CLASS_NAME"), "Priority class name of the Mountpoint Pods.")
var mountpointPreemptingPriorityClassName = flag.String("mountpoint-preempting-priority-class-name", os.Getenv("MOUNTPOINT_PREEMPTING_PRIORITY_CLASS_NAME"), "Preempting priority class name of the Mountpoint Pods.")
var mountpointHeadroomPriorityClassName = flag.String("mountpoint-headroom-priority-class-name", os.Getenv("MOUNTPOINT_HEADROOM_PRIORITY_CLASS_NAME"), "Priority class name of the Headroom Pods.")
var mountpointImage = flag.String("mountpoint-image", os.Getenv("MOUNTPOINT_IMAGE"), "Image of Mountpoint to use in spawned Mountpoint Pods.")
var headroomImage = flag.String("headroom-image", os.Getenv("MOUNTPOINT_HEADROOM_IMAGE"), "Image of a pause container to use in spawned Headroom Pods.")
var mountpointImagePullPolicy = flag.String("mountpoint-image-pull-policy", os.Getenv("MOUNTPOINT_IMAGE_PULL_POLICY"), "Pull policy of Mountpoint images.")
var mountpointContainerCommand = flag.String("mountpoint-container-command", "/bin/aws-s3-csi-mounter", "Entrypoint command of the Mountpoint Pods.")
var mountpointPodLabels = flag.String("mountpoint-pod-labels", os.Getenv("MOUNTPOINT_POD_LABELS"), "Pod labels to apply to Mountpoint Pods (JSON format).")
var mountpointHeadroomPodLabels = flag.String("mountpoint-headroom-pod-labels", os.Getenv("MOUNTPOINT_HEADROOM_POD_LABELS"), "Pod labels to apply to Headroom Pods (JSON format).")

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(crdv2.AddToScheme(scheme))
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

	if err := crdv2.SetupManagerIndices(mgr); err != nil {
		log.Error(err, "Failed to setup field indexers")
		os.Exit(1)
	}

	podLabels := parseLabels(*mountpointPodLabels, log)
	headroomPodLabels := parseLabels(*mountpointHeadroomPodLabels, log)

	reconciler := csicontroller.NewReconciler(mgr.GetClient(), mppod.Config{
		Namespace:                   *mountpointNamespace,
		MountpointVersion:           *mountpointVersion,
		PriorityClassName:           *mountpointPriorityClassName,
		PreemptingPriorityClassName: *mountpointPreemptingPriorityClassName,
		HeadroomPriorityClassName:   *mountpointHeadroomPriorityClassName,
		Container: mppod.ContainerConfig{
			Command:         *mountpointContainerCommand,
			Image:           *mountpointImage,
			HeadroomImage:   *headroomImage,
			ImagePullPolicy: corev1.PullPolicy(*mountpointImagePullPolicy),
		},
		CSIDriverVersion:  version.GetVersion().DriverVersion,
		ClusterVariant:    cluster.DetectVariant(conf, log),
		PodLabels:         podLabels,
		HeadroomPodLabels: headroomPodLabels,
	}, log)

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

// parseLabels parses a JSON string into a map of labels and validates them.
// Returns an empty map if the input is empty, invalid JSON, or contains invalid labels.
func parseLabels(labelsJSON string, log logr.Logger) map[string]string {
	const reservedLabelPrefix = "s3.csi.aws.com/"

	if labelsJSON == "" || labelsJSON == "{}" || labelsJSON == "null" {
		return map[string]string{}
	}

	var labels map[string]string
	if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
		log.Error(err, "Failed to parse labels JSON, ignoring", "json", labelsJSON)
		return map[string]string{}
	}

	// Validate and filter out invalid labels
	validLabels := make(map[string]string)
	for key, value := range labels {
		if strings.HasPrefix(key, reservedLabelPrefix) {
			log.Error(fmt.Errorf("reserved prefix"), "Invalid label key, skipping", "key", key, "prefix", reservedLabelPrefix)
			continue
		}

		// Validate key and value
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			log.Error(fmt.Errorf("invalid key"), "Invalid label key, skipping", "key", key, "errors", strings.Join(errs, "; "))
			continue
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			log.Error(fmt.Errorf("invalid value"), "Invalid label value, skipping", "key", key, "value", value, "errors", strings.Join(errs, "; "))
			continue
		}

		validLabels[key] = value
	}

	return validLabels
}
