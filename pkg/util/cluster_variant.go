package util

import "k8s.io/utils/ptr"

// ClusterVariant represents different Kubernetes distributions
type ClusterVariant int

const (
	DefaultKubernetes ClusterVariant = iota // Vanilla K8s
	OpenShift                               // OpenShift K8s
)

var defaultMountpointUID = ptr.To(int64(1000))

// MountpointPodUserID returns the appropriate RunAsUser for Mountpoint Pod based on the cluster variant.
func (c ClusterVariant) MountpointPodUserID() *int64 {
	if c == OpenShift {
		// OpenShift clusters automatically assign non-root uid from predefined namespace range
		// https://www.redhat.com/en/blog/a-guide-to-openshift-and-uids
		return nil
	}

	return defaultMountpointUID
}
