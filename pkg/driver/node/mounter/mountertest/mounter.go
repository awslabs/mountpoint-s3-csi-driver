package mountertest

import (
	"os"
	"syscall"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// OpenDevNull opens `/dev/null` and returns the file handle.
func OpenDevNull(t *testing.T) *os.File {
	file, err := os.Open(os.DevNull)
	assert.NoError(t, err)
	t.Cleanup(func() {
		_ = file.Close()
	})
	return file
}

// AssertSameFile checks if given file handles points to the same underlying file description.
func AssertSameFile(t *testing.T, want *os.File, got *os.File) {
	t.Helper()

	wantStat := &syscall.Stat_t{}
	err := syscall.Fstat(int(want.Fd()), wantStat)
	assert.NoError(t, err)

	gotStat := &syscall.Stat_t{}
	err = syscall.Fstat(int(got.Fd()), gotStat)
	assert.NoError(t, err)

	assert.Equals(t, wantStat.Dev, gotStat.Dev)
	assert.Equals(t, wantStat.Ino, gotStat.Ino)
}
