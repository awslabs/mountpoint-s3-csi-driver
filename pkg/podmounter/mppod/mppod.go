// Package mppod provides utilities for creating and accessing Mountpoint Pods.
package mppod

import (
	"crypto/sha256"
	"fmt"
)

// MountpointPodNameFor returns a consistent and unique Pod name for
// Mountpoint Pod for given `podUID` and `volumeName`.
//
// Changing output of this function might cause duplicate Mountpoint Pods to be spawned,
// ideally multiple implementation of this function shouldn't co-exists in the same cluster
// unless there is a clean install of the CSI Driver.
func MountpointPodNameFor(podUID string, volumeName string) string {
	return fmt.Sprintf("mp-%x", sha256.Sum224([]byte(fmt.Sprintf("%s%s", podUID, volumeName))))
}
