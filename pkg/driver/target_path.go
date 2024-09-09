package driver

import (
	"errors"
	"path/filepath"
)

var (
	ErrInvalidTargetPath = errors.New("ParseTargetPath: Invalid target path")
)

// A TargetPath represents a parsed target path from Kubernetes.
type TargetPath struct {
	PodID    string
	VolumeID string
}

// ParseTargetPath parses given target path from Kubernetes.
// Target paths are generated by Kubernetes for CSI drivers to mount their filesystem.
// They are in the form of "/var/lib/kubelet/pods/<pod-uuid>/volumes/kubernetes.io~csi/<volume-id>/mount".
func ParseTargetPath(path string) (TargetPath, error) {
	path, mount := filepath.Split(path)
	if path == "" || mount != "mount" {
		// Target path must end with `/mount`.
		return TargetPath{}, ErrInvalidTargetPath
	}

	path, volumeID := filepath.Split(filepath.Dir(path))
	if path == "" || volumeID == "" {
		// Next part is `volume-id` and it shouldn't be empty.
		return TargetPath{}, ErrInvalidTargetPath
	}

	path, csiPluginName := filepath.Split(filepath.Dir(path))
	if path == "" || csiPluginName != "kubernetes.io~csi" {
		// Next part is CSI plugin name, `kubernetes.io~csi`.
		return TargetPath{}, ErrInvalidTargetPath
	}

	path, volumes := filepath.Split(filepath.Dir(path))
	if path == "" || volumes != "volumes" {
		// Next part is `volumes`.
		return TargetPath{}, ErrInvalidTargetPath
	}

	path, podID := filepath.Split(filepath.Dir(path))
	if path == "" || podID == "" {
		// Next part is `<pod-id>` and it shouldn't be empty.
		return TargetPath{}, ErrInvalidTargetPath
	}

	// We got all parts we need, the rest is `<kubelet-path>/pods`.
	return TargetPath{VolumeID: volumeID, PodID: podID}, nil
}
