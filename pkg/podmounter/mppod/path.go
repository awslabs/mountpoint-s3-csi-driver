package mppod

import (
	"path/filepath"
)

// KnownPathMountSock is the path of Unix socket thats going to be used during exchange of mount options
// between Mountpoint Pod and the CSI Driver Node Pod.
const KnownPathMountSock = "mount.sock"

// KnownPathMountError is the path of error file that's created by `aws-s3-csi-mounter` if Mountpoint fails
// during mount operation. Existence of this file indicates that Mountpoint failed to start and [PodMounter]
// will propagate contents of this error file to the Kubernetes and to the operator to resolve any operator error.
const KnownPathMountError = "mount.err"

// CommunicationDirName is the name of `emptyDir` volume each Mountpoint Pod will create
// for the communication between Mountpoint Pod and the CSI Driver Node Pod.
// Each Pod will have a different view for the files inside this folder,
// `PathOnHost` and `PathInsideMountpointPod` can be used to obtain a correct path for each.
const CommunicationDirName = "comm"

// PathOnHost returns the full path on the host that refers to `path` inside Mountpoint Pod.
// This function should be used in the CSI Driver Node Pod which uses `hostPath` volume to mount kubelet.
func PathOnHost(podPathOnHost string, path ...string) string {
	parts := append([]string{
		podPathOnHost,
		"/volumes/kubernetes.io~empty-dir/",
		CommunicationDirName,
	}, path...)
	return filepath.Join(parts...)
}

// PathInsideMountpointPod returns the full path that refers to `path` inside Mountpoint Pod.
// This function should be used in the Mountpoint Pod.
func PathInsideMountpointPod(path ...string) string {
	parts := append([]string{
		"/",
		CommunicationDirName,
	}, path...)
	return filepath.Join(parts...)
}
