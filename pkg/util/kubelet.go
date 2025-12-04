package util

import (
	"os"
	"strings"
)

const defaultKubeletPath = "/var/lib/kubelet"

// KubeletPath returns path of the kubelet.
// It looks for `KUBELET_PATH` variable, and returns a default path if its not defined.
func KubeletPath() string {
	kubeletPath := os.Getenv("KUBELET_PATH")
	if kubeletPath == "" {
		return defaultKubeletPath
	}
	return kubeletPath
}

// TranslateKubeletPath translates a path from host kubelet path to container kubelet path.
// This is useful when the kubelet path on the host is different from the kubelet path in the container.
// For example, in MicroK8s, the host path is /var/snap/microk8s/common/var/lib/kubelet
// but the container sees it as /var/lib/kubelet.
func TranslateKubeletPath(path string) string {
	hostPath := os.Getenv("KUBELET_PATH_HOST")
	containerPath := KubeletPath()

	// If no host path is configured, or paths are the same, no translation needed
	if hostPath == "" || hostPath == containerPath {
		return path
	}

	// If the path starts with the host kubelet path, replace it with the container path
	if strings.HasPrefix(path, hostPath) {
		return strings.Replace(path, hostPath, containerPath, 1)
	}

	return path
}
