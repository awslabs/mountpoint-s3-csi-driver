package mounter

import (
	"errors"
	"os"
)

func mount(_ Target, _ MountOptions) (int, error) {
	return 0, errors.New("Only supported on Linux")
}

func bindMount(_, _ Target) error {
	return errors.New("Only supported on Linux")
}

func statx(path string) error {
	// statx is a Linux-specific syscall, let's simulate with os.Stat
	_, err := os.Stat(path)
	return err
}
