package mounter

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
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
	dm.SetChownChmodForTesting(
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

func TestOwnCredentialsDirContentsSkipsAPlantedSymlink(t *testing.T) {
	dm := &DaemonsetMounter{}
	recorder := recordOwnership(dm)

	credDir := t.TempDir()
	// Stands in for anything csi-node can name: another mount's token, or the shared comm directory.
	outside := filepath.Join(t.TempDir(), "another-mounts-token")
	assert.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))

	// Sorts before the token, so a walk that gave up here would leave the token unowned.
	link := filepath.Join(credDir, "a-planted-link")
	assert.NoError(t, os.Symlink(outside, link))
	token := filepath.Join(credDir, "token")
	assert.NoError(t, os.WriteFile(token, []byte("a-token"), 0o640))

	const mountUID = uint32(UIDRangeStart)
	if err := dm.ownCredentialsDirContents(credDir, mountUID); err != nil {
		t.Fatalf("a symlink must not stop the walk: %v", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if owner, applied := recorder.owners[link]; applied {
		t.Errorf("the planted link was chowned to %v", owner)
	}
	if mode, applied := recorder.modes[link]; applied {
		t.Errorf("the planted link was chmoded to %04o", mode)
	}

	// Mountpoint still gets the credentials it needs.
	uid := int(mountUID)
	assert.Equals(t, [2]int{uid, uid}, recorder.owners[token])
	assert.Equals(t, credentialprovider.IsolatedCredentialFilePerm, recorder.modes[token])
}

// Runs the real chmod, so this covers the [os.Root] resolution that keeps a planted link from
// reaching the file it names.
func TestChmodWithDefaultRefusesALinkOutOfTheDirectory(t *testing.T) {
	dm := &DaemonsetMounter{}

	credDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "another-mounts-token")
	assert.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	link := filepath.Join(credDir, "planted")
	assert.NoError(t, os.Symlink(outside, link))

	if err := dm.chmodWithDefault(link, credentialprovider.IsolatedCredentialFilePerm); err == nil {
		t.Error("expected a link naming a file outside the directory to be refused")
	}

	st, err := os.Stat(outside)
	assert.NoError(t, err)
	assert.Equals(t, fs.FileMode(0o600), st.Mode().Perm())
}

func TestOwnCredentialsDirContentsOwnsCredentialFiles(t *testing.T) {
	dm := &DaemonsetMounter{}
	recorder := recordOwnership(dm)

	credDir := t.TempDir()
	token := filepath.Join(credDir, "token")
	if err := os.WriteFile(token, []byte("a-token"), 0o640); err != nil {
		t.Fatalf("failed to write the token: %v", err)
	}

	const mountUID = uint32(UIDRangeStart)
	if err := dm.ownCredentialsDirContents(credDir, mountUID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	uid := int(mountUID)
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
