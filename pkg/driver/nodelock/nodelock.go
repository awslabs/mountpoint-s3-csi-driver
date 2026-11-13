// Package nodelock makes sure at most one CSI node process works on a node at a time.
//
// Two CSI node pods can land on one node, for example when two DaemonSets match it. They share the node's CSI socket,
// mount state and credential directories, but each keeps its own view of them in memory, so a second one acting on
// the node corrupts what the first set up. The first to take the lock works on the node; any other waits until it exits.
//
// The lock is an exclusive flock(2) on a file in the driver's plugin directory, which every CSI node pod on a node
// mounts from the host. The kernel releases it when its holder exits, crashed or not, so the next one takes over
// without anyone cleaning up.
package nodelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// FileName is the lock file's name in the plugin directory.
const FileName = "node.lock"

// Variables, not constants, so tests can shorten them.
var (
	// pollInterval is how often a waiting process retries the lock.
	pollInterval = time.Second
	// waitLogInterval is how often a waiting process logs that it is waiting.
	waitLogInterval = 30 * time.Second
)

// Lock is a held node lock. It stays held until the process exits, so keep it reachable for the process's lifetime: if
// it were garbage collected, the file's finalizer would close the descriptor and release the lock.
type Lock struct {
	path string
	file *os.File
}

// Acquire blocks until this process holds the lock at path, or ctx is done. It logs an error while it waits, since a
// wait means another CSI node runs on this node.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("nodelock: failed to create directory for %s: %w", path, err)
	}
	var lastLog time.Time
	for {
		lock, err := tryAcquire(path)
		if err != nil && !errors.Is(err, errHeldElsewhere) {
			return nil, err
		}
		if lock != nil {
			klog.Infof("nodelock: holding %s; this is the only CSI node working on this node", path)
			return lock, nil
		}
		if err != nil && time.Since(lastLog) >= waitLogInterval {
			klog.Errorf("nodelock: another CSI node on this node holds %s, so this one waits, serving nothing, until it "+
				"exits. Only one s3-csi-node Pod may run per node: check that no node matches two s3-csi-node DaemonSets "+
				"(from additionalDaemonsets, a second Helm release, or the EKS add-on alongside Helm)", path)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("nodelock: gave up waiting for %s: %w", path, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// errHeldElsewhere means another process holds the lock.
var errHeldElsewhere = errors.New("held by another process")

// tryAcquire takes the lock at path without waiting. It returns errHeldElsewhere if another process holds it, and a nil
// Lock with no error if the file at path changed while it was locking, in which case the caller should try again.
func tryAcquire(path string) (*Lock, error) {
	// Go opens files close-on-exec, so the mount and umount commands this process runs don't inherit the lock.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nodelock: failed to open %s: %w", path, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errHeldElsewhere
		}
		return nil, fmt.Errorf("nodelock: failed to lock %s: %w", path, err)
	}
	lock := &Lock{path: path, file: file}
	// A lock belongs to the file, not its name. If the name was deleted or replaced between our open and our flock,
	// we hold a file nobody else will open, and another process could lock the one now at path.
	held, err := lock.isAtPath()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !held {
		file.Close()
		return nil, nil
	}
	return lock, nil
}

// isAtPath reports whether the file this lock holds is still the one at its path.
func (l *Lock) isAtPath() (bool, error) {
	var held, current unix.Stat_t
	if err := unix.Fstat(int(l.file.Fd()), &held); err != nil {
		return false, fmt.Errorf("nodelock: failed to stat the held %s: %w", l.path, err)
	}
	if err := unix.Stat(l.path, &current); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("nodelock: failed to stat %s: %w", l.path, err)
	}
	return held.Dev == current.Dev && held.Ino == current.Ino, nil
}

// Watch checks every interval, until stop closes, that the file at the lock's path is still the one it holds. Deleting
// or replacing the file doesn't release the lock, but lets another process lock the new file. When that happens Watch
// locks the new file itself; if another process got there first, it calls onLost, which should end the process without
// touching the node's shared state, since another CSI node now works on it.
func (l *Lock) Watch(stop <-chan struct{}, interval time.Duration, onLost func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		held, err := l.isAtPath()
		if err != nil {
			// Can't tell, so keep holding what we have and check again next tick.
			klog.Errorf("%v", err)
			continue
		}
		if held {
			continue
		}
		klog.Errorf("nodelock: %s was deleted or replaced while held; locking the file now at that path", l.path)
		replacement, err := tryAcquire(l.path)
		if errors.Is(err, errHeldElsewhere) {
			onLost(fmt.Errorf("nodelock: %s was deleted or replaced while held, and another CSI node locked the new file", l.path))
			return
		}
		if err != nil {
			klog.Errorf("%v", err)
			continue
		}
		if replacement == nil {
			// The file changed again while we were locking it; try again next tick.
			continue
		}
		// Locked the new file before releasing the old one, so this process never holds neither.
		l.file.Close()
		l.file = replacement.file
		klog.Infof("nodelock: holding %s again", l.path)
	}
}
