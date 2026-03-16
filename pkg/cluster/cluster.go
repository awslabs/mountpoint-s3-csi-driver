package cluster

import (
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// Variant represents different Kubernetes distributions
type Variant int

const (
	DefaultKubernetes Variant = iota // Vanilla K8s
	OpenShift                        // OpenShift K8s
)

func (v Variant) String() string {
	switch v {
	case OpenShift:
		return "openshift"
	default:
		return "kubernetes"
	}
}

var defaultMountpointUID = new(int64(1000))

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

// InstallationMethod returns the installation method for the CSI driver.
// It reads the INSTALLATION_TYPE environment variable and returns its value,
// falling back to "unknown" if the variable is not set.
// Known values: eks-addon, helm, kustomize.
func InstallationMethod() string {
	method := strings.ToLower(strings.TrimSpace(os.Getenv("INSTALLATION_TYPE")))
	switch method {
	case "eks-addon", "helm", "kustomize":
		return method
	}

	return "unknown"
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
