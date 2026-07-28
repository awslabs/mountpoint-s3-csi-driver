// Package mounter provides mount implementations for the CSI driver.
package mounter

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// MountParams captures the parameters that must be identical across all pods sharing a mount.
// If any of these differ between the existing mount and a new share request, sharing is rejected.
type MountParams struct {
	// MountOptions is the sorted list of Mountpoint arguments used for this mount.
	MountOptions []string

	// AuthenticationSource is "driver" or "pod".
	AuthenticationSource string

	// ServiceAccountName is the K8s service account of the pod.
	ServiceAccountName string

	// ServiceAccountEKSRoleARN is the IAM role ARN from the service account's
	// eks.amazonaws.com/role-arn annotation. Empty when authenticationSource is "driver".
	ServiceAccountEKSRoleARN string

	// PodNamespace is the namespace of the pod.
	PodNamespace string

	// FSGroup is the volume mount group (security context).
	FSGroup string
}

// ValidateCompatibility checks whether the new request's params are compatible with the
// existing mount's params. Returns nil if sharing is allowed, or an error describing
// the first incompatibility found.
func (existing *MountParams) ValidateCompatibility(incoming *MountParams) error {
	if !slices.Equal(existing.MountOptions, incoming.MountOptions) {
		return fmt.Errorf("mount options mismatch: existing=%v, incoming=%v",
			existing.MountOptions, incoming.MountOptions)
	}

	if existing.AuthenticationSource != incoming.AuthenticationSource {
		return fmt.Errorf("authenticationSource mismatch: existing=%q, incoming=%q",
			existing.AuthenticationSource, incoming.AuthenticationSource)
	}

	// SA and role ARN only matter for pod-level authentication. When auth source is "driver",
	// credentials come from the CSI driver's own service account — the workload pod's SA is irrelevant.
	if existing.AuthenticationSource == "pod" {
		if existing.ServiceAccountName != incoming.ServiceAccountName {
			return fmt.Errorf("serviceAccountName mismatch: existing=%q, incoming=%q",
				existing.ServiceAccountName, incoming.ServiceAccountName)
		}

		if existing.ServiceAccountEKSRoleARN != incoming.ServiceAccountEKSRoleARN {
			return fmt.Errorf("serviceAccountEKSRoleARN mismatch: existing=%q, incoming=%q",
				existing.ServiceAccountEKSRoleARN, incoming.ServiceAccountEKSRoleARN)
		}
	}

	if existing.PodNamespace != incoming.PodNamespace {
		return fmt.Errorf("podNamespace mismatch: existing=%q, incoming=%q",
			existing.PodNamespace, incoming.PodNamespace)
	}

	if existing.FSGroup != incoming.FSGroup {
		return fmt.Errorf("fsGroup (security context) mismatch: existing=%q, incoming=%q",
			existing.FSGroup, incoming.FSGroup)
	}

	return nil
}

// String returns a human-readable summary of the params for logging.
func (p *MountParams) String() string {
	return fmt.Sprintf("{auth=%s, sa=%s, roleARN=%s, ns=%s, fsGroup=%s, opts=[%s]}",
		p.AuthenticationSource, p.ServiceAccountName, p.ServiceAccountEKSRoleARN,
		p.PodNamespace, p.FSGroup, strings.Join(p.MountOptions, ", "))
}

// MountEntry tracks a shared source mount and the set of bind-mount targets referencing it.
// The embedded mutex provides per-PV locking — callers hold entry.mu during multi-step
// operations (health check → validate → bind mount → add target).
type MountEntry struct {
	mu sync.Mutex // per-PV lock, held during mount/unmount operations

	// SourcePath is the path where the FUSE mount lives (one Mountpoint process).
	SourcePath string

	// VolumeID is the PV name, used as the sharing key, credential directory name,
	// error file name, and mount identifier when communicating with the secondary daemonset.
	VolumeID string

	// CommDir is the comm directory path at the time this mount was created.
	// Cleanup uses this to ensure resources are cleaned in the same location where they were
	// provisioned, avoiding mismatches if the mounter pod restarts between mount and unmount.
	CommDir string

	// Params records the mount parameters for validation of subsequent share requests.
	Params MountParams

	// RefCount is the number of pods currently using this mount via bind mounts.
	// Invariant: RefCount == len(Targets) when entry.mu is not held.
	RefCount int

	// Targets is the set of active per-pod bind-mount target paths.
	Targets []string

	// initialized is true once the entry has a real source mount backing it.
	// Used to distinguish between a placeholder (LoadOrStore created) and a populated entry.
	initialized bool
}

// MountMap is a lock-free map tracking active source mounts keyed by volume ID.
// Uses sync.Map for zero-contention concurrent access across different volumes.
// Per-PV locking is achieved by locking entry.mu on the MountEntry itself.
type MountMap struct {
	entries sync.Map // map[volumeID]*MountEntry
}

// NewMountMap creates a new empty MountMap.
func NewMountMap() *MountMap {
	return &MountMap{}
}

// GetOrCreate returns the existing entry for volumeID, or creates a new uninitialized one.
// The caller must lock the returned entry before using it.
// The bool indicates whether the entry already existed (true) or was just created (false).
func (m *MountMap) GetOrCreate(volumeID string) (*MountEntry, bool) {
	entry := &MountEntry{VolumeID: volumeID}
	actual, loaded := m.entries.LoadOrStore(volumeID, entry)
	return actual.(*MountEntry), loaded
}

// Get returns the mount entry for the given volume ID, or nil if not found.
func (m *MountMap) Get(volumeID string) *MountEntry {
	val, ok := m.entries.Load(volumeID)
	if !ok {
		return nil
	}
	return val.(*MountEntry)
}

// Delete removes the entry for the given volume ID from the map.
func (m *MountMap) Delete(volumeID string) {
	m.entries.Delete(volumeID)
}

// SourceMountPath returns the source mount directory for a given volume.
// This path is stable across pod lifecycles — it's keyed by volume, not pod.
func SourceMountPath(kubeletPath, volumeID string) string {
	return filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt", volumeID)
}
