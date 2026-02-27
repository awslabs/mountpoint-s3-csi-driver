package cluster

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
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
	DistributionEKSAddon       Distribution = "eks-addon"
	DistributionEKSSelfManaged Distribution = "eks"
	DistributionROSA           Distribution = "rosa"
	DistributionOther          Distribution = "other"
	DistributionOpenShift      Distribution = "openshift"
)

const (
	userAgentDistributionEKSAddon       Distribution = "eks-addon"
	userAgentDistributionEKSSelfManaged Distribution = "eks-self-managed"
	userAgentDistributionROSA           Distribution = "rosa"
	userAgentDistributionOther          Distribution = "other"
)

const (
	kubeSystemNamespace         = "kube-system"
	csiDriverServiceAccountName = "aws-mountpoint-s3-csi-driver"
	eksAddonLabel               = "app.kubernetes.io/managed-by"
	eksAddonAnnotation          = "eks.amazonaws.com/addon"
	rosaLabel                   = "rosa.openshift.io/managed"
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

// DetectDistribution determines the K8s distribution for user agent reporting.
func DetectDistribution(clientset kubernetes.Interface, config *rest.Config, log logr.Logger) Distribution {
	ctx := context.Background()

	variant := DetectVariant(config, log)
	if variant == OpenShift {
		if isROSA(ctx, clientset, log) {
			return DistributionROSA
		}
		log.Info("Detected OpenShift distribution")
		return DistributionOpenShift
	}

	if isEKSAddon(ctx, clientset, log) {
		return DistributionEKSAddon
	}

	if isEKSCluster(config, log) {
		return DistributionEKSSelfManaged
	}

	log.V(2).Info("Could not detect known distribution, defaulting to other")
	return DistributionOther
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

func isEKSCluster(config *rest.Config, log logr.Logger) bool {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		log.V(2).Info("Failed to create discovery client for EKS detection", "error", err)
		return false
	}

	version, err := discoveryClient.ServerVersion()
	if err != nil {
		log.V(2).Info("Failed to get server version for EKS detection", "error", err)
		return false
	}

	if strings.Contains(version.GitVersion, "-eks-") {
		log.Info("Detected EKS cluster distribution")
		return true
	}

	return false
}

func isROSA(ctx context.Context, clientset kubernetes.Interface, log logr.Logger) bool {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, kubeSystemNamespace, metav1.GetOptions{})
	if err != nil {
		log.V(2).Info("Could not get kube-system namespace", "error", err)
		return false
	}

	if ns.Labels[rosaLabel] == "true" {
		log.Info("Detected ROSA distribution")
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

// UserAgent returns normalized distribution value for user-agent reporting.
// Values are grouped into one of:
// - eks-addon
// - eks-self-managed
// - rosa
// - other
func (d Distribution) UserAgent() Distribution {
	switch d {
	case DistributionEKSAddon:
		return userAgentDistributionEKSAddon
	case DistributionEKSSelfManaged:
		return userAgentDistributionEKSSelfManaged
	case DistributionROSA:
		return userAgentDistributionROSA
	default:
		return userAgentDistributionOther
	}
}
