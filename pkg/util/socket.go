package util

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// ErrUnixSocketNotExists is returned if the socket won't appear within the timeout duration.
var ErrUnixSocketNotExists = errors.New("util/socket: unix socket does not exists")

// WaitForUnixSocket waits for the duration of `timeout` until `path` exists by checking it every `interval`.
// It returns `ErrUnixSocketNotExists` if the socket won't appear within the timeout, otherwise it returns nil.
func WaitForUnixSocket(timeout time.Duration, interval time.Duration, path string) error {
	err := wait.PollUntilContextTimeout(context.Background(), interval, timeout, true, func(_ context.Context) (bool, error) {
		_, err := os.Stat(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		return err == nil, nil
	})

	if wait.Interrupted(err) {
		return ErrUnixSocketNotExists
	}

	return err
}
