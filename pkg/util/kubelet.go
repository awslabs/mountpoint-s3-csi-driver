package util

import "os"

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
