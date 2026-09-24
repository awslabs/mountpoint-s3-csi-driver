package mounter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
)

const (
	// sharedDirPerm is the mode for the comm directory, which every mount shares. Only root may
	// create, delete or rename entries, so no Mountpoint can leave a file behind for a later mount.
	// The execute bit lets a Mountpoint reach its own directory; withholding read stops it listing
	// the parent to discover other mounts.
	sharedDirPerm = fs.FileMode(0711)

	// mountSockPerm is the mode for the mount request socket. Only csi-node uses it, and the
	// per-mount UID travels across it.
	mountSockPerm = fs.FileMode(0600)
)

// chownFunc and chmodFunc are injectable for testing: applying ownership requires root, which
// csi-node has on a node but a unit test does not.
type chownFunc func(path string, uid, gid int) error
type chmodFunc func(path string, mode fs.FileMode) error

// SetPathOwnership replaces the chown and chmod implementations. A nil argument keeps the real
// syscall.
func (dm *DaemonsetMounter) SetPathOwnership(chown chownFunc, chmod chmodFunc) {
	dm.chown = chown
	dm.chmod = chmod
}

func (dm *DaemonsetMounter) chownWithDefault(path string, uid, gid int) error {
	if dm.chown != nil {
		return dm.chown(path, uid, gid)
	}
	// Lchown never follows a symlink, so a link planted where csi-node applies ownership cannot
	// redirect it onto the path the link points at.
	return os.Lchown(path, uid, gid)
}

func (dm *DaemonsetMounter) chmodWithDefault(path string, mode fs.FileMode) error {
	if dm.chmod != nil {
		return dm.chmod(path, mode)
	}
	return os.Chmod(path, mode)
}

// secureSharedPaths makes the comm directory and the mount socket root-owned and unwritable by any
// Mountpoint, so none can plant a file for a later mount to pick up or connect to the socket to
// issue mount requests.
func (dm *DaemonsetMounter) secureSharedPaths(commDir string) error {
	if err := dm.own(commDir, 0, 0, sharedDirPerm); err != nil {
		return fmt.Errorf("failed to secure comm dir: %w", err)
	}

	// Go cannot set a socket's mode at creation, so it needs an explicit chmod.
	if err := dm.own(filepath.Join(commDir, MountSockName), 0, 0, mountSockPerm); err != nil {
		return fmt.Errorf("failed to secure mount socket: %w", err)
	}

	return nil
}

// ensureCredentialsDir creates this mount's credential directory, owned by `uid` and inaccessible to
// any other UID, and returns its path.
//
// It is the gate that isolates the mount: once the directory is owned by `uid` and not traversable
// by others, nothing inside is reachable by another Mountpoint whatever mode the files carry.
// Credentials are therefore written only after this returns.
func (dm *DaemonsetMounter) ensureCredentialsDir(commDir, volumeID string, uid uint32) (string, error) {
	dir := filepath.Join(commDir, volumeID)
	perm := credentialprovider.IsolatedCredentialDirPerm

	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("failed to create directory %q: %w", dir, err)
	}
	return dir, dm.own(dir, int(uid), int(uid), perm)
}

// ownCredentialsDirContents makes `uid` the owner of every entry inside `dir`, so this mount's
// Mountpoint can read the credentials written there.
//
// It runs after the files are written. `dir` is already closed to other mounts by
// [DaemonsetMounter.ensureCredentialsDir], so no file is reachable by another Mountpoint in the
// moment before its owner is set.
func (dm *DaemonsetMounter) ownCredentialsDirContents(dir string, uid uint32) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// csi-node writes only regular files here, so anything else was planted by the Mountpoint
		// that owns this directory. Chmod follows symlinks, so a link would apply the mode to
		// whatever it points at.
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("refusing to own %q in %q: unexpected file type %s", path, dir, d.Type())
		}

		perm := credentialprovider.IsolatedCredentialFilePerm
		if d.IsDir() {
			perm = credentialprovider.IsolatedCredentialDirPerm
		}
		return dm.own(path, int(uid), int(uid), perm)
	})
}

// own sets `path`'s owner and mode.
func (dm *DaemonsetMounter) own(path string, uid, gid int, perm fs.FileMode) error {
	if err := dm.chownWithDefault(path, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %q to %d:%d: %w", path, uid, gid, err)
	}
	if err := dm.chmodWithDefault(path, perm); err != nil {
		return fmt.Errorf("failed to chmod %q to %o: %w", path, perm, err)
	}
	return nil
}
