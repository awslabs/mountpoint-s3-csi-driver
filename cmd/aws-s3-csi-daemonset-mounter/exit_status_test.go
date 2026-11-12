package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/errorfile"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// The real runner tells a signal kill from an exit code; before, a SIGKILL read as "code -1".
func TestDefaultProcessRunner_Wait_ReportsSignal(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		want   exitStatus
	}{
		"SIGKILL":     {"kill -9 $$", exitStatus{code: -1, signal: syscall.SIGKILL}},
		"SIGTERM":     {"kill -15 $$", exitStatus{code: -1, signal: syscall.SIGTERM}},
		"exit code 3": {"exit 3", exitStatus{code: 3}},
		"clean":       {"exit 0", exitStatus{}},
	} {
		t.Run(name, func(t *testing.T) {
			h, err := (&defaultProcessRunner{stderrCapacity: 1024}).Start("pv-a", exec.Command("sh", "-c", tc.script))
			assert.NoError(t, err)
			got, _ := h.Wait()
			assert.Equals(t, tc.want.String(), got.String())
			assert.Equals(t, tc.want.failed(), got.failed())
		})
	}
}

func TestErrorFile_SIGKILL_ReportsSignalAndOOMHint(t *testing.T) {
	for name, tc := range map[string]struct {
		memory memoryLimit
		want   []string
	}{
		"equalSplit": {memoryLimit{strategy: memoryLimitEqualSplit, shareMiB: 1008},
			[]string{"--memory-target to (daemonsetMounters[].resources.requests.memory - 64Mi - in-memory cache) / maxVolumesPerNode; to lower it, raise maxVolumesPerNode or lower the request"}},
		"none": {memoryLimit{strategy: memoryLimitNone},
			[]string{"set or lower --memory-target in the PV mountOptions"}},
	} {
		t.Run(name, func(t *testing.T) {
			msg := launchAndEndWith(t, tc.memory, func(h *fakeProcessHandle) { h.Kill(syscall.SIGKILL, "") })

			assertConcise(t, msg)
			assert.Contains(t, msg, "killed by signal SIGKILL")
			assertNotContains(t, msg, "code -1")
			assert.Contains(t, msg, "out of memory")
			assert.Contains(t, msg, "memoryLimitStrategy="+string(tc.memory.strategy))
			for _, want := range tc.want {
				assert.Contains(t, msg, want)
			}
			// The chart sets no memory limit, so the hint must not tell users to raise one (docs/MEMORY.md).
			assertNotContains(t, msg, "raise daemonsetMounters[].resources.limits.memory")
			assert.Contains(t, msg, "see docs/MEMORY.md")
		})
	}
}

// Only SIGKILL gets the out-of-memory hint.
func TestErrorFile_OtherSignal_ReportsSignalWithoutOOMHint(t *testing.T) {
	msg := launchAndEndWith(t, memoryLimit{strategy: memoryLimitEqualSplit, shareMiB: 1008}, func(h *fakeProcessHandle) {
		h.Kill(syscall.SIGSEGV, "thread 'tokio' panicked at src/x.rs:1:1: bad\n")
	})

	assertConcise(t, msg)
	assert.Contains(t, msg, "killed by signal SIGSEGV")
	assert.Contains(t, msg, "panicked at src/x.rs:1:1: bad")
	assertNotContains(t, msg, "out of memory")
}

// Every hint leaves most of the error file for the stderr summary.
func TestOOMHints_LeaveRoomForTheSummary(t *testing.T) {
	for strategy, hint := range oomHints {
		if room := errorfile.MaxBytes - len("Mountpoint was killed by signal SIGKILL") - len(". "+hint); room < 180 {
			t.Errorf("%s hint leaves %d bytes for the stderr summary, want at least 180", strategy, room)
		}
	}
}

// The CSI node's hint greps the mounter log for `[<mount-id>]`, so every line the mounter logs about a
// mount carries that token too, not only its Mountpoint's output: with a cache (V4), a refused UID, and
// an exit whose cache directory cannot be removed.
func TestMounterLogLinesCarryTheMountIdToken(t *testing.T) {
	var buf bytes.Buffer
	var flags flag.FlagSet
	klog.InitFlags(&flags)
	assert.NoError(t, flags.Set("v", "4"))
	klog.LogToStderr(false)
	klog.SetOutput(&buf)
	t.Cleanup(func() { flags.Set("v", "0"); klog.SetOutput(os.Stderr); klog.LogToStderr(true) })

	fr := &fakeProcessRunner{}
	pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
	launch := func(id string, uid uint32) error {
		dev := mountertest.OpenDevNull(t)
		return pm.Launch(id, "/usr/bin/mount-s3", mountoptions.Options{Uid: uid, Gid: uid, Fd: int(dev.Fd()), BucketName: "b", Args: []string{"--cache"}})
	}
	assert.NoError(t, launch("pv-a", 65536))
	assert.Equals(t, true, launch("pv-b", 65536) != nil) // the UID is in use
	createUnremovableCacheEntry(t, cacheDir, perPvCacheDirName(65536))
	fr.handles[0].Exit(1, "") // its cache directory cannot be removed
	pm.Shutdown()
	klog.Flush()

	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var token string
		for _, id := range []string{"pv-a", "pv-b"} {
			if strings.Contains(line, "] ["+id+"] ") {
				token = id
			}
		}
		// The cache cleaner's lines are about a UID whose mount ID is already freed.
		if token == "" && !strings.Contains(line, "cache directory uid-") {
			t.Errorf("mounter log line without a [<mount-id>] token: %q", line)
		}
		if token != "" {
			seen[token] = true
		}
	}
	for _, want := range []string{"Created cache directory", "Failed to remove cache directory, keeping UID 65536", "exited with code 1"} {
		assert.Contains(t, buf.String(), want)
	}
	assert.Equals(t, map[string]bool{"pv-a": true, "pv-b": true}, seen)
}

// lineRecorder records each Write, so a test can tell lines written whole from lines split across writes.
type lineRecorder struct {
	mu     sync.Mutex
	writes []string
}

func (r *lineRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, string(p))
	return len(p), nil
}

// Concurrent Mountpoints share the container's stdout: every line must reach it as one write, carrying
// its own mount's token.
func TestPrefixWriter_ConcurrentWritersNeverSplitALine(t *testing.T) {
	rec := &lineRecorder{}
	var wg sync.WaitGroup
	for _, id := range []string{"pv-a", "pv-b", "pv-c"} {
		pw := newPrefixWriter(rec, id)
		wg.Go(func() {
			for i := 0; i < 200; i++ {
				fmt.Fprintf(pw, "line %d of %s\nsecond line of %s\n", i, id, id)
			}
		})
	}
	wg.Wait()

	for _, w := range rec.writes {
		for _, line := range strings.Split(strings.TrimSuffix(w, "\n"), "\n") {
			id := strings.TrimSuffix(strings.TrimPrefix(strings.Fields(line)[0], "["), "]")
			if !strings.HasPrefix(line, "["+id+"] ") || !strings.HasSuffix(line, " of "+id) {
				t.Fatalf("line split or mis-tagged: %q (write %q)", line, w)
			}
		}
	}
}

// A long stderr summary is truncated, never the hint after it.
func TestErrorFile_SIGKILL_LongStderrKeepsTheWholeHint(t *testing.T) {
	for _, strategy := range []memoryLimitStrategy{memoryLimitEqualSplit, memoryLimitNone} {
		stderr := "thread 'main' panicked at src/x.rs:1:1: " + strings.Repeat("allocation failed ", 100) + "ROOT CAUSE\n"
		msg := launchAndEndWith(t, memoryLimit{strategy: strategy, shareMiB: 1008}, func(h *fakeProcessHandle) { h.Kill(syscall.SIGKILL, stderr) })

		assertConcise(t, msg)
		assert.Equals(t, true, len(msg) <= errorfile.MaxBytes)
		assert.Equals(t, true, strings.HasPrefix(msg, "Mountpoint was killed by signal SIGKILL: th"))
		assert.Equals(t, true, strings.HasSuffix(msg, ". "+oomHints[strategy]))
		assert.Contains(t, msg, "ROOT CAUSE")
	}
}
