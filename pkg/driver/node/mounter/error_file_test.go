package mounter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestMountFailedError_EmptyReason(t *testing.T) {
	dm := &DaemonsetMounter{nodeID: "node"}
	msg := dm.mountFailedError("/comm", "pv", "").Error()
	assert.Contains(t, msg, "the mounter reported no reason. You can check")
}

// A file that cannot be opened (e.g. EMFILE, EACCES) is not taken as a reasonless failure: the poller retries.
func TestConsumeErrorFile_UnopenableFileIsNotFound(t *testing.T) {
	assert.Equals(t, false, os.Geteuid() == 0)
	path := filepath.Join(t.TempDir(), "pv.error")
	assert.NoError(t, os.WriteFile(path, []byte("Error: x"), 0000))

	_, found, err := consumeErrorFile(path)
	assert.Equals(t, false, found)
	assert.NoError(t, err)

	assert.NoError(t, os.Chmod(path, 0600))
	reason, found, err := consumeErrorFile(path)
	assert.Equals(t, true, found)
	assert.NoError(t, err)
	assert.Equals(t, "Error: x", reason)
}
