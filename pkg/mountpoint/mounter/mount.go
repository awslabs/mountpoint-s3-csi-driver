// Package mounter provides functionalities for mounting and unmount Mountpoint instances.
package mounter

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

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
	if target == "" {
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

// CheckMountPoint checks whether `target` is a healthy Mountpoint mount.
//
// If the `target` is a:
//   - Healthy Mountpoint mount, it returns "true, nil"
//   - Healthy any other mount, it returns "false, nil"
//   - Unhealthy mount, it returns a non-nil error.
//
// Some notable errors that requires callers to perform some operations are:
//   - If `errors.Is(err, fs.ErrNotExist)` - it means the `target` does not exists, and the caller should create the target folder
//   - If `mounter.IsMountPointCorrupted(err)` - it means the `target` is corrupted, and the caller should `Unmount` and `Mount` the file system
//
// We implement additional check on top of `mountutils.IsMountPoint()` because we need
// to verify not only that the target is a mount point but also that it is specifically a Mountpoint mount point.
// This is achieved by calling the `mountutils.List()` method to enumerate all mount points.
func (m *Mounter) CheckMountPoint(target Target) (bool, error) {
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

// IsMountPointCorrupted returns whether an error returned from `CheckMountPoint`
// indicates the queried mount point is corrupted or not.
//
// If its corrupted, the mount point should be re-mounted.
func (m *Mounter) IsMountPointCorrupted(err error) bool {
	return mountutils.IsCorruptedMnt(err)
}

// findSourceMountPoint locates the source S3 mount point for a given target path by comparing
// device IDs and inodes with all S3 mount points at driver source directory `sourceMountDir`.
//
// Parameters:
//   - target: The target path whose source mount point needs to be found
//   - sourceMountDir: directory where to find source mount points
//
// Returns:
//   - string: The path of the source mount point if found
//   - error: An error if the operation fails
//
// The function works by:
// 1. Getting the device ID and inode of the target path
// 2. Listing all mount points in the system that has "mountpoint-s3" as device name and prefix `sourceMountDir`
// 3. Finding a mount point that matches both the device ID and inode of the target
func (m *Mounter) FindSourceMountPoint(target, sourceMountDir string) (string, error) {
	targetFileInfo, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("failed to stat %q: %w", target, err)
	}

	targetSysInfo, ok := targetFileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("failed to get system info for target %q", target)
	}

	targetDevID := targetSysInfo.Dev
	targetInodeID := targetSysInfo.Ino

	mountPoints, err := m.mount.List()
	if err != nil {
		return "", fmt.Errorf("failed to list mount points: %w", err)
	}

	for _, mountPoint := range mountPoints {
		if mountPoint.Device != fsName || !strings.HasPrefix(mountPoint.Path, sourceMountDir) {
			continue
		}

		mountPathInfo, err := os.Stat(mountPoint.Path)
		if err != nil {
			klog.V(4).Infof("Skipping mount point %q: unable to stat %v", mountPoint.Path, err)
			continue
		}

		mountSysInfo, ok := mountPathInfo.Sys().(*syscall.Stat_t)
		if !ok {
			klog.V(4).Infof("Skipping mount point %q: unable to get system info", mountPoint.Path)
			continue
		}

		if targetDevID == mountSysInfo.Dev && targetInodeID == mountSysInfo.Ino {
			return mountPoint.Path, nil
		}
	}

	return "", fmt.Errorf("no source mount point found for path %q (device: %d, inode: %d)",
		target, targetDevID, targetInodeID)
}
