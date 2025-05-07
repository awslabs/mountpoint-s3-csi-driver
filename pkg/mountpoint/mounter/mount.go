// Package mounter provides functionalities for mounting and unmount Mountpoint instances.
package mounter

import (
	"errors"
	"fmt"

	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"
)

// ErrMissingTarget is returned when `Mount` is called with empty target.
var ErrMissingTarget = errors.New("mounter: missing mount target")

// fsName is the default Mountpoint device name.
// https://github.com/awslabs/mountpoint-s3/blob/9ed8b6243f4511e2013b2f4303a9197c3ddd4071/mountpoint-s3/src/cli.rs#L421
const fsName = "mountpoint-s3"

// A Target represents a mount target to mount Mountpoint at.
type Target = string

// A Mounter provides utilities for mounting, unmounting, and querying whether a Mountpoint instance is mounted.
type Mounter struct {
	mount mountutils.Interface
}

// A MountOptions represents mount options to be passed to `mount` syscall.
type MountOptions struct {
	ReadOnly   bool
	AllowOther bool
}

// New returns a new `Mounter` with default mount util.
func New() *Mounter {
	return NewWithMount(mountutils.New(""))
}

// NewWithMount returns a new `Mounter` with the given mount util.
func NewWithMount(mount mountutils.Interface) *Mounter {
	return &Mounter{mount}
}

// Mount performs `mount` syscall for Mountpoint at `target`.
// It obtains a FUSE file descriptor, calls `mount` syscall at `target` with the obtained fd using provided `opts`,
// and returns the fd for Mountpoint to communicate with the kernel.
//
// It's caller responsibility to call `Unmount` to unmount the registered file system at `target`.
//
// This requires `CAP_SYS_ADMIN` capability in the target namespace.
func (m *Mounter) Mount(target Target, opts MountOptions) (int, error) {
	if target == "" {
		return 0, ErrMissingTarget
	}
	return mount(target, opts)
}

// BindMount performs a bind mount syscall from `source` to `target`.
func (m *Mounter) BindMount(source, target Target) error {
	if target == "" || source == "" {
		return ErrMissingTarget
	}
	return bindMount(source, target)
}

// Unmount unmounts Mountpoint at `target`.
//
// This requires `CAP_SYS_ADMIN` capability in the target namespace.
func (m *Mounter) Unmount(target Target) error {
	return m.mount.Unmount(target)
}

// CheckMountpoint checks whether `target` is a healthy Mountpoint mount.
//
// If the `target` is a:
//   - Healthy Mountpoint mount, it returns "true, nil"
//   - Healthy any other mount, it returns "false, nil"
//   - Unhealthy mount, it returns a non-nil error.
//
// Some notable errors that requires callers to perform some operations are:
//   - If `errors.Is(err, fs.ErrNotExist)` - it means the `target` does not exists, and the caller should create the target folder
//   - If `mounter.IsMountpointCorrupted(err)` - it means the `target` is corrupted, and the caller should `Unmount` and `Mount` the file system
//
// We implement additional check on top of `mountutils.IsMountPoint()` because we need
// to verify not only that the target is a mount point but also that it is specifically a Mountpoint mount point.
// This is achieved by calling the `mountutils.List()` method to enumerate all mount points.
func (m *Mounter) CheckMountpoint(target Target) (bool, error) {
	if err := statx(target); err != nil {
		return false, err
	}

	mountPoints, err := m.mount.List()
	if err != nil {
		return false, fmt.Errorf("failed to list mounts for %q: %w", target, err)
	}

	for _, mp := range mountPoints {
		if mp.Path == target {
			if mp.Device != fsName {
				klog.Infof("mounter: %q is a %q mount, but %q is expected, ignoring", target, mp.Device, fsName)
				continue
			}
			return true, nil
		}
	}

	return false, nil
}

// IsMountpointCorrupted returns whether an error returned from [Mounter.CheckMountpoint]
// indicates the queried mount point is corrupted or not.
//
// If its corrupted, the mount point should be re-mounted.
func (m *Mounter) IsMountpointCorrupted(err error) bool {
	return mountutils.IsCorruptedMnt(err)
}
