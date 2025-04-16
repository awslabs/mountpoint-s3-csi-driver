package mounter

import (
	"errors"
	"os"

	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
)

func (pm *PodMounter) mountSyscallDefault(_ string, _ mountpoint.Args) (int, error) {
	return 0, errors.New("Only supported on Linux")
}

func (pm *PodMounter) bindMountSyscallDefault(source, target string) error {
	return errors.New("Only supported on Linux")
}

func verifyMountPointStatx(path string) error {
	// statx is a Linux-specific syscall, let's simulate with os.Stat
	_, err := os.Stat(path)
	return err
}
