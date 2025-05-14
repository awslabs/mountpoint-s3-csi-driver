package mounter

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

func mount(target Target, opts MountOptions) (int, error) {
	fd, err := openFD()
	if err != nil {
		return 0, err
	}

	// This will set false on a success condition and will stay true
	// in all error conditions to ensure we don't leave the file descriptor open in case we can't do
	// the mount operation.
	closeFd := true
	defer func() {
		if closeFd {
			err := CloseFD(fd)
			if err != nil {
				klog.ErrorS(err, "failed to close file descriptor", "fd", fd)
			}
		}
	}()

	var stat syscall.Stat_t
	err = syscall.Stat(target, &stat)
	if err != nil {
		return 0, fmt.Errorf("failed to stat mount point %s: %w", target, err)
	}

	options := []string{
		fmt.Sprintf("fd=%d", fd),
		// We only keep file type bits from the file mode of the target,
		// this is also how libfuse decides on `rootmode`.
		// Mountpoint has `--file-mode` and `--dir-mode` for permission bits on the files and directories,
		// and that will be effective once Mountpoint is mounted.
		fmt.Sprintf("rootmode=%o", stat.Mode&syscall.S_IFMT),
		// Set `uid`/`gid` of the mount owner as this process
		fmt.Sprintf("user_id=%d", os.Geteuid()),
		fmt.Sprintf("group_id=%d", os.Getegid()),
		// Instruct kernel to do its own permissions checks
		"default_permissions",
	}

	// These flags matches with Mountpoint's and `fuser`s defaults
	var flags uintptr = uintptr(
		syscall.MS_NODEV | // Do not allow access to devices
			syscall.MS_NOSUID | // Do not honor set-user-ID and set-group-ID bits or file capabilities when executing programs
			syscall.MS_NOATIME) // Do not update access times

	if opts.ReadOnly {
		flags |= syscall.MS_RDONLY
	}

	if opts.AllowOther {
		options = append(options, "allow_other")
	}

	optionsJoined := strings.Join(options, ",")
	klog.Infof("Mounting %s with options %s", target, optionsJoined)
	err = syscall.Mount(fsName, target, "fuse", flags, optionsJoined)
	if err != nil {
		return 0, fmt.Errorf("failed to mount %s: %w", target, err)
	}

	// We successfully performed the mount operation, ensure to not close the FUSE file descriptor.
	closeFd = false
	return fd, nil
}

// bindMount performs a bind mount syscall from `source` to `target`.
func bindMount(source, target string) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("failed to bind mount from %s to %s: %v", source, target, err)
	}
	return nil
}

func statx(path string) error {
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
