package util

import (
	"fmt"
	"os"
	"strings"
)

const defaultKubeletPath = "/var/lib/kubelet"

// ContainerKubeletPath returns the kubelet path as seen from inside the container.
// It looks for `CONTAINER_KUBELET_PATH` variable, and returns a default path if its not defined.
func ContainerKubeletPath() string {
	kubeletPath := os.Getenv("CONTAINER_KUBELET_PATH")
	if kubeletPath == "" {
		return defaultKubeletPath
	}
	return kubeletPath
}

// HostKubeletPath returns the kubelet path as seen on the host.
// It looks for `HOST_KUBELET_PATH` variable, and returns a default path if its not defined.
func HostKubeletPath() string {
	kubeletPath := os.Getenv("HOST_KUBELET_PATH")
	if kubeletPath == "" {
		return defaultKubeletPath
	}
	return kubeletPath
}

// KubeletHostPathToContainerPath translates a path from host kubelet path to container kubelet path.
// This is useful when the kubelet path on the host is different from the kubelet path in the container.
// For example, in MicroK8s, the host path is /var/snap/microk8s/common/var/lib/kubelet
// but the container sees it as /var/lib/kubelet.
func KubeletHostPathToContainerPath(path string) (string, error) {
	hostPath := HostKubeletPath()
	containerPath := ContainerKubeletPath()

	// If paths are the same, no translation needed
	if hostPath == containerPath {
		if strings.HasPrefix(path, containerPath) {
			return path, nil
		}
		return "", fmt.Errorf("path %q does not start with kubelet path %q", path, containerPath)
	}

	// If the path starts with the host kubelet path, replace it with the container path
	if strings.HasPrefix(path, hostPath) {
		return strings.Replace(path, hostPath, containerPath, 1), nil
	}

	return "", fmt.Errorf("path %q does not start with host kubelet path %q", path, hostPath)
}
