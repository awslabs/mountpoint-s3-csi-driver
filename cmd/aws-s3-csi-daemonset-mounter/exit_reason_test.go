package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// mountpointBucketError is what mount-s3 prints to stderr when it cannot reach the bucket: its
// main returns an anyhow error, which Rust prints in Debug form.
const mountpointBucketError = `Error: Failed to create S3 client

Caused by:
    0: initial ListObjectsV2 failed for bucket does-not-exist in region us-east-1
    1: Client error
    2: Unknown response error: MetaRequestResult { response_status: 404, error_code: 14343 }
`

// launchAndExit launches mount `pv-a` on a fake runner, makes its process exit with `code` and
// `stderr`, and returns the error file the mounter wrote ("" if none).
func launchAndExit(t *testing.T, code int, stderr string) string {
	t.Helper()
	return launchAndEnd(t, func(h *fakeProcessHandle) { h.Exit(code, stderr) })
}

// launchAndEnd launches mount `pv-a` on a fake runner, ends its process with `end`, and returns the
// error file the mounter wrote ("" if none).
func launchAndEnd(t *testing.T, end func(*fakeProcessHandle)) string {
	t.Helper()
	return launchAndEndWith(t, memoryLimit{strategy: memoryLimitNone}, end)
}

// launchAndEndWith is launchAndEnd in a container whose memory is split by `memory`.
func launchAndEndWith(t *testing.T, memory memoryLimit, end func(*fakeProcessHandle)) string {
	t.Helper()
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memory, cacheLimit{strategy: cacheLimitNone})

	dev := mountertest.OpenDevNull(t)
	assert.NoError(t, pm.Launch("pv-a", "/usr/bin/mount-s3", mountoptions.Options{
		Uid: 65536, Gid: 65536, Fd: int(dev.Fd()), BucketName: "bucket",
	}))
	end(fr.handles[0])
	pm.Shutdown()

	// Written atomically: no temporary file is left beside it.
	entries, err := os.ReadDir(commDir)
	assert.NoError(t, err)
	for _, e := range entries {
		if e.Name() != "pv-a.error" {
			t.Errorf("unexpected file %q left in the comm dir", e.Name())
		}
	}
	content, err := os.ReadFile(filepath.Join(commDir, "pv-a.error"))
	if os.IsNotExist(err) {
		return ""
	}
	assert.NoError(t, err)
	return string(content)
}

// The exit code comes first, then Mountpoint's stderr as it is.
func TestErrorFile_ExitCodeThenStderr(t *testing.T) {
	msg := launchAndExit(t, 1, mountpointBucketError)
	assert.Equals(t, "Mountpoint exited with code 1: "+strings.TrimSpace(mountpointBucketError), msg)
}

func TestErrorFile_CleanExit_WritesNothing(t *testing.T) {
	assert.Equals(t, "", launchAndExit(t, 0, mountpointBucketError))
}

func assertNotContains(t *testing.T, got, unwanted string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Fatalf("%.300q contains %q", got, unwanted)
	}
}
