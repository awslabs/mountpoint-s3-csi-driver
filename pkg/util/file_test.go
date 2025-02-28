package util

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestFileGroupIDReturnsProcessGid(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatalf("Failed to create temp file for unit test: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	gid, err := FileGroupID(tmpFile.Name())

	assert.NoError(t, err)
	assert.Equals(t, uint32(os.Getgid()), gid)
}

func TestFileGroupIDReturnsErrorIfFileNonExistent(t *testing.T) {
	_, err := FileGroupID("/nonexistent/file/path")
	assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))
}
