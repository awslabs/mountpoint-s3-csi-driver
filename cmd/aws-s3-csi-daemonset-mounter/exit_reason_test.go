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

// assertConcise checks what reaches NodePublishVolume (and the FailedMount event) is short and ASCII.
func assertConcise(t *testing.T, msg string) {
	t.Helper()
	// The FailedMount event also carries kubelet's prefix and csi-node's hint; events.k8s.io caps a note at 1024.
	if len(msg) > 600 {
		t.Fatalf("error file is %d bytes, want at most 600; it starts %q", len(msg), msg[:200])
	}
	for i := 0; i < len(msg); i++ {
		if c := msg[i]; c < ' ' || c > '~' {
			t.Fatalf("error file has non-printable-ASCII byte %#x at %d: %q", c, i, msg)
		}
	}
}

func TestErrorFile_LongStderr_KeepsOnlyTheErrorChain(t *testing.T) {
	// A full 1 MiB stderr ring (the default --stderr-capacity) of noise ahead of the error.
	noise := strings.Repeat("2026-11-03T10:00:00Z  WARN mountpoint_s3_fs::fuse: lookup failed: inode error\n", 1024*1024/80)
	msg := launchAndExit(t, 1, noise+mountpointBucketError)

	assertConcise(t, msg)
	assert.Contains(t, msg, "exited with code 1")
	assert.Contains(t, msg, "Error: Failed to create S3 client")
	assert.Contains(t, msg, "initial ListObjectsV2 failed for bucket does-not-exist in region us-east-1")
	assert.Contains(t, msg, "Unknown response error")
	assertNotContains(t, msg, "WARN")
}

func TestErrorFile_NoErrorLine_KeepsTheLastLines(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "line "+strings.Repeat("x", i))
	}
	lines = append(lines, "thread 'main' panicked at src/fs.rs:10:5: boom", "note: run with `RUST_BACKTRACE=1`")
	msg := launchAndExit(t, 101, strings.Join(lines, "\n")+"\n")

	assertConcise(t, msg)
	assert.Contains(t, msg, "exited with code 101")
	assert.Contains(t, msg, "panicked at src/fs.rs:10:5: boom")
	assertNotContains(t, msg, "line x\n")
}

func TestErrorFile_StripsNonASCIIAndControlBytes(t *testing.T) {
	msg := launchAndExit(t, 1, "Error: \x1b[31mbad\x1b[0m bucket éé name\r\n")
	assertConcise(t, msg)
	assert.Contains(t, msg, "Error: bad bucket")
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
