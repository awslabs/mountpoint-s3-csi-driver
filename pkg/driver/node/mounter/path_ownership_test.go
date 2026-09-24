package mounter

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
)

// ownershipRecorder records chown and chmod instead of performing them, so these tests exercise the
// real ownership logic without running as root.
type ownershipRecorder struct {
	mu     sync.Mutex
	owners map[string][2]int
	modes  map[string]fs.FileMode
}

func recordOwnership(dm *DaemonsetMounter) *ownershipRecorder {
	r := &ownershipRecorder{owners: map[string][2]int{}, modes: map[string]fs.FileMode{}}
	dm.SetPathOwnership(
		func(path string, uid, gid int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.owners[path] = [2]int{uid, gid}
			return nil
		},
		func(path string, mode fs.FileMode) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.modes[path] = mode
			return nil
		},
	)
	return r
}

func TestOwnCredentialsDirContentsRefusesAPlantedSymlink(t *testing.T) {
	dm := &DaemonsetMounter{}
	recorder := recordOwnership(dm)

	credDir := t.TempDir()
	// Stands in for anything csi-node can name: another mount's token, or the shared comm directory.
	outside := filepath.Join(t.TempDir(), "another-mounts-token")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("failed to write the target file: %v", err)
	}
	// The Mountpoint owns its credential directory, so it can create entries in it.
	if err := os.Symlink(outside, filepath.Join(credDir, "planted")); err != nil {
		t.Fatalf("failed to plant the symlink: %v", err)
	}

	if err := dm.ownCredentialsDirContents(credDir, UIDRangeStart); err == nil {
		t.Fatal("expected a symlink in the credential directory to be refused")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if owner, applied := recorder.owners[outside]; applied {
		t.Errorf("ownership of %q outside the directory was changed to %v", outside, owner)
	}
	if mode, applied := recorder.modes[outside]; applied {
		t.Errorf("mode of %q outside the directory was changed to %04o", outside, mode)
	}
}

func TestOwnCredentialsDirContentsOwnsCredentialFiles(t *testing.T) {
	dm := &DaemonsetMounter{}
	recorder := recordOwnership(dm)

	credDir := t.TempDir()
	token := filepath.Join(credDir, "token")
	if err := os.WriteFile(token, []byte("a-token"), 0o640); err != nil {
		t.Fatalf("failed to write the token: %v", err)
	}

	if err := dm.ownCredentialsDirContents(credDir, UIDRangeStart); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	uid := int(UIDRangeStart)
	if got := recorder.owners[token]; got != [2]int{uid, uid} {
		t.Errorf("token owner is %v, want %v", got, [2]int{uid, uid})
	}
	if got := recorder.modes[token]; got != credentialprovider.IsolatedCredentialFilePerm {
		t.Errorf("token mode is %04o, want %04o", got, credentialprovider.IsolatedCredentialFilePerm)
	}
	if got := recorder.modes[credDir]; got != credentialprovider.IsolatedCredentialDirPerm {
		t.Errorf("directory mode is %04o, want %04o", got, credentialprovider.IsolatedCredentialDirPerm)
	}
}
