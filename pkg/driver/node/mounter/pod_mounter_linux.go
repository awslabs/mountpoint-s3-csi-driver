package mounter

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/mountpoint"
)

// mountSyscallDefault creates a FUSE file descriptor and performs a `mount` syscall with given `target` and mount arguments.
func (pm *PodMounter) mountSyscallDefault(target string, args mountpoint.Args) (int, error) {
	fd, err := syscall.Open("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open /dev/fuse: %w", err)
	}

	// This will set false on a success condition and will stay true
	// in all error conditions to ensure we don't leave the file descriptor open in case we can't do
	// the mount operation.
	closeFd := true
	defer func() {
		if closeFd {
			pm.closeFUSEDevFD(fd)
		}
	}()

	var stat syscall.Stat_t
	err = syscall.Stat(target, &stat)
	if err != nil {
		return 0, fmt.Errorf("failed to stat mount point %s: %w", target, err)
	}

	options := []string{
		fmt.Sprintf("fd=%d", fd),
		fmt.Sprintf("rootmode=%o", stat.Mode&syscall.S_IFMT),
		fmt.Sprintf("user_id=%d", os.Geteuid()),
		fmt.Sprintf("group_id=%d", os.Getegid()),
		"default_permissions",
	}

	var flags uintptr = uintptr(syscall.MS_NODEV | syscall.MS_NOSUID | syscall.MS_NOATIME)

	if args.Has(mountpoint.ArgReadOnly) {
		flags |= syscall.MS_RDONLY
	}

	if args.Has(mountpoint.ArgAllowOther) || args.Has(mountpoint.ArgAllowRoot) {
		options = append(options, "allow_other")
	}

	optionsJoined := strings.Join(options, ",")
	klog.V(4).Infof("Mounting %s with options %s", target, optionsJoined)
	err = syscall.Mount(mountpointDeviceName, target, "fuse", flags, optionsJoined)
	if err != nil {
		return 0, fmt.Errorf("failed to mount %s: %w", target, err)
	}

	// We successfully performed the mount operation, ensure to not close the FUSE file descriptor.
	closeFd = false
	return fd, nil
}

func verifyMountPointStatx(path string) error {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_FORCE_SYNC, 0, &stat); err != nil {
		if err == unix.ENOSYS {
			// statx() syscall is not supported, retry with regular os.Stat
			_, err = os.Stat(path)
		}
		return err
	}

	return nil
}
