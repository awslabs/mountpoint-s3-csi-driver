package util

import (
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

	gid, err := FileGroupID(tmpFile.Name())

	assert.NoError(t, err)
	assert.Equals(t, uint32(os.Getgid()), gid)
}
