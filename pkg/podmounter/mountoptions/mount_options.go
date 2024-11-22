// Package mountoptions provides utilities for passing mount options between
// Mountpoint and CSI Driver Node Pods running in the same node.
package mountoptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"

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
	warnAboutLongUnixSocketPath(sockPath)

	message, err := json.Marshal(&options)
	if err != nil {
		return fmt.Errorf("failed to marshal message to send %s: %w", sockPath, err)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("failed to dial to unix socket %s: %w", sockPath, err)
	}
	defer conn.Close()

	unixConn := conn.(*net.UnixConn)

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

var (
	messageRecvSize = 1024
	// We only pass one file descriptor and its 32 bits
	unixRightsRecvSize = syscall.CmsgSpace(4)
)

// Recv receives passed mount options via `Send` function through given `sockPath`.
func Recv(ctx context.Context, sockPath string) (Options, error) {
	warnAboutLongUnixSocketPath(sockPath)

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return Options{}, fmt.Errorf("failed to listen unix socket %s: %w", sockPath, err)
	}
	defer l.Close()

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

// warnAboutLongUnixSocketPath emits a warning if `path` is longer than 108 characters.
// There is a limit on Unix domain socket path on some platforms and
// Go returns `bind: invalid argument` in this case which is hard to debug.
// See https://github.com/golang/go/issues/6895 for more details.
func warnAboutLongUnixSocketPath(path string) {
	if len(path) > 108 {
		klog.Warningf("Length of Unix domain socket is larger than 108 characters and it might not work in some platforms, see https://github.com/golang/go/issues/6895. Fullpath: %q", path)
	}
}
