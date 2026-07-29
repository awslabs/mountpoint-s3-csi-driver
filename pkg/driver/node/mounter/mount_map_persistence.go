// Package mounter provides mount implementations for the CSI driver.
package mounter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"
)

// MetaFileName returns the path to the .meta.json file for a given volume.
// Meta files live in a dedicated directory separate from FUSE source mounts to avoid
// collisions with PV names that could end in ".meta.json".
func MetaFileName(kubeletPath, volumeID string) string {
	return filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "meta", volumeID+".meta.json")
}

// MountMeta is the JSON-serializable structure persisted alongside each source mount.
// It records the parameters used to create the mount — enabling validation on recovery
// and for subsequent share requests after driver restart.
type MountMeta struct {
	VolumeID                 string   `json:"volumeID"`
	CommDir                  string   `json:"commDir"`
	MountOptions             []string `json:"mountOptions"`
	AuthenticationSource     string   `json:"authenticationSource"`
	ServiceAccountName       string   `json:"serviceAccountName"`
	ServiceAccountEKSRoleARN string   `json:"serviceAccountEKSRoleARN"`
	PodNamespace             string   `json:"podNamespace"`
	FSGroup                  string   `json:"fsGroup"`
}

// WriteMeta atomically writes the .meta.json file for the given volume.
// Uses temp file + os.Rename for atomicity (rename is atomic on Linux).
func WriteMeta(kubeletPath string, entry *MountEntry) error {
	meta := MountMeta{
		VolumeID:                 entry.VolumeID,
		CommDir:                  entry.CommDir,
		MountOptions:             entry.Params.MountOptions,
		AuthenticationSource:     entry.Params.AuthenticationSource,
		ServiceAccountName:       entry.Params.ServiceAccountName,
		ServiceAccountEKSRoleARN: entry.Params.ServiceAccountEKSRoleARN,
		PodNamespace:             entry.Params.PodNamespace,
		FSGroup:                  entry.Params.FSGroup,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to marshal mount meta for volume %s: %w", entry.VolumeID, err)
	}

	metaPath := MetaFileName(kubeletPath, entry.VolumeID)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0750); err != nil {
		return fmt.Errorf("failed to create meta directory: %w", err)
	}

	tmpPath := metaPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp meta file: %w", err)
	}

	if err := os.Rename(tmpPath, metaPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename meta file: %w", err)
	}

	klog.V(4).Infof("MountMap: wrote meta for volume %s at %s", entry.VolumeID, metaPath)
	return nil
}

// RemoveMeta removes the .meta.json file for the given volume.
// Called when the last consumer disconnects and the source mount is torn down.
func RemoveMeta(kubeletPath, volumeID string) {
	metaPath := MetaFileName(kubeletPath, volumeID)
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		klog.Warningf("MountMap: failed to remove meta file %s: %v", metaPath, err)
	}
}

// readMeta reads and parses a .meta.json file.
func readMeta(path string) (*MountMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta MountMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s: %w", path, err)
	}
	return &meta, nil
}

// parseMountInfoFromProc reads /proc/self/mountinfo and returns all entries
// using the well-tested k8s.io/mount-utils library.
func parseMountInfoFromProc() ([]mountutils.MountInfo, error) {
	return mountutils.ParseMountInfo("/proc/self/mountinfo")
}

// deviceID returns the "major:minor" string for a MountInfo entry.
func deviceID(mi *mountutils.MountInfo) string {
	return fmt.Sprintf("%d:%d", mi.Major, mi.Minor)
}

// findMountByPath finds the mountinfo entry for a given mount path.
func findMountByPath(entries []mountutils.MountInfo, path string) *mountutils.MountInfo {
	for i := range entries {
		if entries[i].MountPoint == path {
			return &entries[i]
		}
	}
	return nil
}

// findBindMountTargets finds all mount points sharing the same device ID as the source,
// excluding the source path itself. These are the bind mount targets.
func findBindMountTargets(entries []mountutils.MountInfo, majorMinor, sourcePath string) []string {
	var targets []string
	for i := range entries {
		if deviceID(&entries[i]) == majorMinor && entries[i].MountPoint != sourcePath {
			targets = append(targets, entries[i].MountPoint)
		}
	}
	return targets
}
