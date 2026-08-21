// Package mounter provides functionalities for mounting and unmount Mountpoint instances.
package mounter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

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

	isMountPoint, err := m.mount.IsMountPoint(target)
	if err != nil {
		return false, err
	}
	if !isMountPoint {
		return false, nil
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

// FindReferencesToMountpoint returns list of references to Mountpoint at `target`.
func (m *Mounter) FindReferencesToMountpoint(target Target) ([]string, error) {
	return m.mount.GetMountRefs(target)
}

// IsMountPoint checks whether `target` is any mount point (not necessarily Mountpoint).
// Uses the kernel mount table via mount-utils.
func (m *Mounter) IsMountPoint(target Target) (bool, error) {
	return m.mount.IsMountPoint(target)
}

// IsHealthyMountpoint reports whether `target` is a live Mountpoint mount.
//
// It returns a three-way result so callers can distinguish "definitely dead"
// from "couldn't determine", and avoid tearing down a possibly-live source on a
// transient error:
//   - (true, nil):  healthy.
//   - (false, nil): definitely dead — not a Mountpoint mount, corrupted mount,
//     or the FUSE daemon returned ENOTCONN.
//   - (false, err): UNKNOWN — a transient error (e.g. failure reading the mount
//     table, EINTR, fd exhaustion) prevented a determination. Callers MUST NOT
//     treat this as dead; they should retry later.
//
// A dead FUSE mount (after mounter pod crash) stays in the mount table but returns ENOTCONN on any I/O.
func (m *Mounter) IsHealthyMountpoint(target Target) (bool, error) {
	isMounted, err := m.CheckMountpoint(target)
	if err != nil {
		// A missing target directory means there is no mount there at all —
		// definitely not a live Mountpoint, so treat it as dead.
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		// A corrupted mount is a definite "dead" signal.
		if m.IsMountpointCorrupted(err) {
			return false, nil
		}
		// Anything else (e.g. couldn't stat or list the mount table) is UNKNOWN.
		return false, fmt.Errorf("cannot determine mount health for %q: %w", target, err)
	}
	if !isMounted {
		// Not our mount (or not a mount at all) — definitely not a live Mountpoint.
		return false, nil
	}

	// Mount entry exists in the table — verify the FUSE daemon is alive.
	// Opening the root directory issues a FUSE OPENDIR to the daemon. Mountpoint does NOT
	// set FUSE_NO_OPENDIR_SUPPORT, so the kernel always forwards opendir to userspace.
	// A dead mount returns ENOTCONN; a live daemon succeeds without requiring S3 connectivity.
	// We intentionally avoid Readdirnames which would trigger a ListObjectsV2 call to S3,
	// causing false negatives during transient network issues.
	f, err := os.Open(target)
	if err != nil {
		// Same classification as the CheckMountpoint probe above: a vanished target
		// (ENOENT) or a corrupted/dead mount (ENOTCONN, ESTALE, EIO, ...) is a
		// definite "dead" signal. IsMountpointCorrupted unwraps the *os.PathError.
		if errors.Is(err, fs.ErrNotExist) || m.IsMountpointCorrupted(err) {
			return false, nil
		}
		// Transient errors (EINTR, EMFILE/ENFILE, ENOMEM, ...) are UNKNOWN, not dead.
		return false, fmt.Errorf("cannot open %q to probe FUSE liveness: %w", target, err)
	}
	f.Close()
	return true, nil
}

// IsMountpointCorrupted returns whether an error returned from [Mounter.CheckMountpoint]
// indicates the queried mount point is corrupted or not.
//
// If its corrupted, the mount point should be re-mounted.
func (m *Mounter) IsMountpointCorrupted(err error) bool {
	return mountutils.IsCorruptedMnt(err)
}
