package cluster

import (
	"context"
	"os"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Variant represents different Kubernetes distributions
type Variant int

const (
	DefaultKubernetes Variant = iota // Vanilla K8s
	OpenShift                        // OpenShift K8s
)

// Distribution represents the K8s distribution for user agent reporting.
type Distribution string

const (
	// DistributionEKSAddon is an EKS cluster where the CSI driver is installed as an EKS add-on.
	DistributionEKSAddon Distribution = "eks-addon"
	// DistributionOpenShift is an OpenShift cluster (includes ROSA).
	DistributionOpenShift Distribution = "openshift"
	// DistributionOther is a cluster that does not match known distributions (includes EKS self-managed).
	DistributionOther Distribution = "other"
)

const (
	kubeSystemNamespace         = "kube-system"
	csiDriverServiceAccountName = "s3-csi-driver-sa"
	eksAddonLabel               = "app.kubernetes.io/managed-by"
	eksAddonAnnotation          = "eks.amazonaws.com/addon"
)

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

// DetectDistribution determines the K8s distribution for user agent reporting.
func DetectDistribution(clientset kubernetes.Interface, config *rest.Config, log logr.Logger) Distribution {
	ctx := context.Background()

	variant := DetectVariant(config, log)
	if variant == OpenShift {
		log.Info("Detected OpenShift distribution")
		return DistributionOpenShift
	}

	// Check ENV variable first (more reliable for EKS addon detection)
	if isEKSAddonFromEnv(log) {
		return DistributionEKSAddon
	}

	// Fall back to service account check for backward compatibility
	if isEKSAddon(ctx, clientset, log) {
		return DistributionEKSAddon
	}

	log.V(2).Info("Could not detect known distribution, defaulting to other")
	return DistributionOther
}

func isEKSAddonFromEnv(log logr.Logger) bool {
	installationType := os.Getenv("INSTALLATION_TYPE")
	if installationType == "eks-addon" {
		log.Info("Detected EKS Addon distribution from INSTALLATION_TYPE env var")
		return true
	}
	return false
}

func isEKSAddon(ctx context.Context, clientset kubernetes.Interface, log logr.Logger) bool {
	sa, err := clientset.CoreV1().ServiceAccounts(kubeSystemNamespace).Get(ctx, csiDriverServiceAccountName, metav1.GetOptions{})
	if err != nil {
		log.V(2).Info("Could not find CSI driver service account", "error", err)
		return false
	}

	if sa.Labels[eksAddonLabel] == "eks" {
		log.Info("Detected EKS Addon distribution")
		return true
	}

	if _, ok := sa.Annotations[eksAddonAnnotation]; ok {
		log.Info("Detected EKS Addon distribution")
		return true
	}

	return false
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
