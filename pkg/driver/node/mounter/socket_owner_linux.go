package mounter

import (
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// listenerIsSelf reports whether this process is the one listening on the Unix socket at path. It asks the kernel for
// the listener's PID, since a socket that replaced ours can reuse its inode number.
func listenerIsSelf(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return false
	}
	return int(cred.Pid) == os.Getpid()
}
