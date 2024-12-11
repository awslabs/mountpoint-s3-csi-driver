package mppod

import (
	"path/filepath"
)

// KnownPathMountSock is the path of Unix socket thats going to be used during exchange of mount options
// between Mountpoint Pod and the CSI Driver Node Pod.
const KnownPathMountSock = "mount.sock"

// CommunicationDirName is the name of `emptyDir` volume each Mountpoint Pod will create
// for the communication between Mountpoint Pod and the CSI Driver Node Pod.
// Each Pod will have a different view for the files inside this folder,
// `PathOnHost` and `PathInsideMountpointPod` can be used to obtain a correct path for each.
const CommunicationDirName = "comm"

// PathOnHost returns the full path on the host that refers to `path` inside Mountpoint Pod.
// This function should be used in the CSI Driver Node Pod which uses `hostPath` volume to mount kubelet.
func PathOnHost(podPathOnHost string, path string) string {
	return filepath.Join(podPathOnHost, "/volumes/kubernetes.io~empty-dir/", CommunicationDirName, path)
}

// PathInsideMountpointPod returns the full path that refers to `path` inside Mountpoint Pod.
// This function should be used in the Mountpoint Pod.
func PathInsideMountpointPod(path string) string {
	return filepath.Join("/", CommunicationDirName, path)
}
