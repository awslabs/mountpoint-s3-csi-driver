package mounter

import (
	"fmt"
	"os"
	"syscall"
)

const dev = "/dev/fuse"

// openFD opens FUSE device (`/dev/fuse`) with read-write mode to obtain
// a file descriptor to use for mounting Mountpoint.
func openFD() (int, error) {
	fd, err := syscall.Open(dev, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open %s: %w", dev, err)
	}
	return fd, nil
}

// CloseFD closes given FUSE file descriptor.
func CloseFD(fd int) error {
	return syscall.Close(fd)
}
