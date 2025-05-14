// Package mountoptions provides utilities for passing mount options between
// containers (e.g., Mountpoint and CSI Driver Node Pods) running in the same node.
package mountoptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

// An Options struct represents mount options to use while invoking Mountpoint.
type Options struct {
	// Fd will be passed over Unix socket using `SCM_RIGHTS`, not as part of the serialized JSON.
	Fd         int      `json:"-"`
	BucketName string   `json:"bucketName"`
	Args       []string `json:"args"`
	Env        []string `json:"env"`
}

// Send sends given mount `options` to given `sockPath` to be received by `Recv` function on the other end.
func Send(ctx context.Context, sockPath string, options Options) error {
	sockPath = tryToMakeSockPathRelative(sockPath)

	message, err := json.Marshal(&options)
	if err != nil {
		return fmt.Errorf("failed to marshal message to send %s: %w", sockPath, err)
	}

	unixConn, err := dialWithRetry(ctx, sockPath)
	if err != nil {
		return fmt.Errorf("failed to dial to unix socket %s: %w", sockPath, err)
	}
	defer unixConn.Close()

	// `unixConn.WriteMsgUnix` does not respect `ctx`'s deadline, we need to call `unixConn.SetDeadline` to ensure `unixConn.WriteMsgUnix` has a deadline.
	if deadline, ok := ctx.Deadline(); ok {
		err := unixConn.SetDeadline(deadline)
		if err != nil {
			return fmt.Errorf("failed to set deadline on unix socket %s: %w", sockPath, err)
		}
	}

	unixRights := syscall.UnixRights(options.Fd)
	messageN, unixRightsN, err := unixConn.WriteMsgUnix(message, unixRights, nil)
	if err != nil {
		return fmt.Errorf("failed to write to unix socket %s: %w", sockPath, err)
	}
	if len(message) != messageN || len(unixRights) != unixRightsN {
		return fmt.Errorf("partial write to unix socket %s: message: size %d - written %d, unix rights: size %d - written %d",
			sockPath, len(message), messageN, len(unixRights), unixRightsN)
	}

	return nil
}

// unixSocketDialRetryInterval is the interval between retries on retryable errors in [dialWithRetry].
const unixSocketDialRetryInterval = 5 * time.Millisecond

// dialWithRetry tries to connect to Unix socket `sockPath` with retries until `ctx` is cancelled or hits the deadline.
// It retries on two errors:
//   - [syscall.ENOENT] returned when the Unix socket does not exists,
//     which might be the case until Mountpoint Pod calls [Recv].
//   - [syscall.ECONNREFUSED] returned when the Unix socket exists, but no one accepts the connection yet,
//     which might be the case between the calls [net.Listen] and [net.Listener.Accept] in [Recv].
func dialWithRetry(ctx context.Context, sockPath string) (*net.UnixConn, error) {
	var d net.Dialer
	var unixConn *net.UnixConn

	err := wait.PollUntilContextCancel(ctx, unixSocketDialRetryInterval, true, func(ctx context.Context) (bool, error) {
		conn, err := d.DialContext(ctx, "unix", sockPath)
		if err == nil {
			// no error, stop retrying and return the Unix connection.
			unixConn = conn.(*net.UnixConn)
			return true, nil
		}

		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			// retryable error
			return false, nil
		}

		// non-retryable error, just propagate it and stop retrying.
		return false, err
	})

	return unixConn, err
}

var (
	messageRecvSize = 1024
	// We only pass one file descriptor and it's 32 bits
	unixRightsRecvSize = syscall.CmsgSpace(4)
)

// Recv receives passed mount options via `Send` function through given `sockPath`.
func Recv(ctx context.Context, sockPath string) (Options, error) {
	sockPath = tryToMakeSockPathRelative(sockPath)

	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "unix", sockPath)
	if err != nil {
		return Options{}, fmt.Errorf("failed to listen unix socket %s: %w", sockPath, err)
	}
	defer l.Close()

	// `l.Accept` does not respect `ctx`'s deadline, we need to call `ul.SetDeadline` to ensure `l.Accept` has a deadline.
	if deadline, ok := ctx.Deadline(); ok {
		ul := l.(*net.UnixListener)
		err := ul.SetDeadline(deadline)
		if err != nil {
			return Options{}, fmt.Errorf("failed to set deadline on unix socket %s: %w", sockPath, err)
		}
	}

	conn, err := l.Accept()
	if err != nil {
		return Options{}, fmt.Errorf("failed to accept connection from unix socket %s: %w", sockPath, err)
	}

	unixConn := conn.(*net.UnixConn)

	messageBuf := make([]byte, 0)
	unixRightsBuf := make([]byte, 0)

	// Read in a loop to consume the whole message
	for {
		message := make([]byte, messageRecvSize)
		unixRights := make([]byte, unixRightsRecvSize)

		messageN, unixRightsN, _, _, err := unixConn.ReadMsgUnix(message, unixRights)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return Options{}, fmt.Errorf("failed to read message from unix socket %s: %w", sockPath, err)
		}

		messageBuf = append(messageBuf, message[:messageN]...)
		unixRightsBuf = append(unixRightsBuf, unixRights[:unixRightsN]...)
	}

	var options Options
	err = json.Unmarshal(messageBuf, &options)
	if err != nil {
		return Options{}, fmt.Errorf("failed to decode mount options from unix socket %s: %w", sockPath, err)
	}

	fds, err := parseUnixRights(unixRightsBuf)
	if err != nil {
		return Options{}, fmt.Errorf("failed to decode unix rights from unix socket %s: %w", sockPath, err)
	}

	if len(fds) != 1 {
		return Options{}, fmt.Errorf("expected to got one file descriptor from unix socket %s, but got %d", sockPath, len(fds))
	}

	options.Fd = fds[0]
	return options, nil
}

// parseUnixRights parses given socket control message to extract passed file descriptors.
func parseUnixRights(buf []byte) ([]int, error) {
	socketControlMessages, err := syscall.ParseSocketControlMessage(buf)
	if err != nil {
		return nil, fmt.Errorf("failed to parse socket control message: %w", err)
	}

	var fds []int
	for _, msg := range socketControlMessages {
		fd, err := syscall.ParseUnixRights(&msg)
		if err != nil {
			return nil, fmt.Errorf("failed to parse unix rights: %w", err)
		}
		fds = append(fds, fd...)
	}

	return fds, nil
}

// tryToMakeSockPathRelative tries to make `path` relative to the current working directory
// if its longer than 108 characters. This is because Go returns `invalid argument` errors
// if you try to do `net.Listen()` or `net.Dial()`, see https://github.com/golang/go/issues/6895 for more details.
//
// If the `path` is still longer than 108 characters after making it relative, this function emits a warning log.
// If the `path` is not relative, this function just returns unmodified `path` and emits a warning log if needed.
func tryToMakeSockPathRelative(path string) string {
	if len(path) < 108 {
		// Nothing to do, its already shorter than 108 characters
		return path
	}

	shortPath, err := relative(path)
	if err != nil {
		// Failed to make it relative to the current working directory,
		// just emit a warning if needed and return unmodified path.
		klog.Warningf("Length of Unix domain socket %q is larger than 108 characters, failed to make it relative to the current working directory to shorten it: %v\n", path, err)
		warnAboutLongUnixSocketPath(path)
		return path
	}

	// Successfully turned `path` into a relative path,
	// just return relative path and emit a warning if its still longer than 108 characters.
	warnAboutLongUnixSocketPath(shortPath)
	return shortPath
}

// warnAboutLongUnixSocketPath emits a warning if `path` is longer than 108 characters.
func warnAboutLongUnixSocketPath(path string) {
	if len(path) > 108 {
		klog.Warningf("Length of Unix domain socket is larger than 108 characters and it might not work in some platforms, see https://github.com/golang/go/issues/6895. Fullpath: %q", path)
	}
}

// relative tries to make `p` relative to the current working directory.
// Copied from https://github.com/moby/vpnkit/blob/604ab7f0d2c999693ab4aa920bdda05f350f497e/go/pkg/vpnkit/transport/unix_unix.go#L66-L86
func relative(p string) (string, error) {
	// Assume the parent directory exists already but the child (the socket)
	// hasn't been created.
	path2, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", err
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir2, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(dir2, path2)
	if err != nil {
		return "", err
	}
	return filepath.Join(rel, filepath.Base(p)), nil
}
