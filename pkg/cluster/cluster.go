package cluster

import (
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
)

// Variant represents different Kubernetes distributions
type Variant int

const (
	DefaultKubernetes Variant = iota // Vanilla K8s
	OpenShift                        // OpenShift K8s
)

var defaultMountpointUID = ptr.To(int64(1000))

// DetectVariant determines Kubernetes variant by checking API groups.
func DetectVariant(client *rest.Config, log logr.Logger) Variant {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(client)
	if err != nil {
		log.Error(err, "Failed to create DiscoveryClient to determine cluster variant. Assuming this is Default Kubernetes variant")
		return DefaultKubernetes
	}

	// Get API groups
	apiGroups, err := discoveryClient.ServerGroups()
	if err != nil {
		log.Error(err, "Failed to get API groups to determine cluster variant. Assuming this is Default Kubernetes variant")
		return DefaultKubernetes
	}

	// Check if the cluster is an OpenShift cluster by detecting the "config.openshift.io" API group
	for _, group := range apiGroups.Groups {
		if group.Name == "config.openshift.io" {
			log.Info("Detected OpenShift cluster variant")
			return OpenShift
		}
	}

	return DefaultKubernetes
}

// MountpointPodUserID returns the appropriate RunAsUser for Mountpoint Pod based on the cluster variant.
func (c Variant) MountpointPodUserID() *int64 {
	if c == OpenShift {
		// OpenShift clusters automatically assign non-root uid from predefined namespace range
		// https://www.redhat.com/en/blog/a-guide-to-openshift-and-uids
		return nil
	}

	return defaultMountpointUID
}

// Helper function to check availability of selectableFields on CustomResourceDefinitions feature in K8s cluster
func IsSelectableFieldsSupported(serverVersion string) (bool, error) {
	currentVersion, err := version.ParseGeneric(serverVersion)
	if err != nil {
		return false, err
	}

	// Selectable fields are supported from 1.32
	// https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#crd-selectable-fields
	selectableFieldsVersion := version.MustParseGeneric("v1.32.0")

	return !currentVersion.LessThan(selectableFieldsVersion), nil
}
