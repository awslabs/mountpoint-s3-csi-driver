package mounter

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

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
