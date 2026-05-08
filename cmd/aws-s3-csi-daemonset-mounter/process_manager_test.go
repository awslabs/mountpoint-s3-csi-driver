package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// --- Fake ProcessRunner ---

type fakeProcessHandle struct {
	pid      int
	cmd      *exec.Cmd
	extraFds []uintptr // captured at Start time before close
	exitCode int
	stderr   []byte
	done     chan struct{}
	sigCh    chan os.Signal
}

func (h *fakeProcessHandle) Pid() int { return h.pid }

func (h *fakeProcessHandle) Wait() (int, []byte) {
	<-h.done
	return h.exitCode, h.stderr
}

func (h *fakeProcessHandle) Signal(sig os.Signal) error {
	h.sigCh <- sig
	return nil
}

// Exit makes Wait() return with the given code and stderr.
func (h *fakeProcessHandle) Exit(code int, stderr string) {
	h.exitCode = code
	h.stderr = []byte(stderr)
	close(h.done)
}

type fakeProcessRunner struct {
	mu      sync.Mutex
	nextPid int
	handles []*fakeProcessHandle
}

func (r *fakeProcessRunner) Start(cmd *exec.Cmd) (ProcessHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextPid++
	var extraFds []uintptr
	for _, f := range cmd.ExtraFiles {
		extraFds = append(extraFds, f.Fd())
	}
	h := &fakeProcessHandle{
		pid:      r.nextPid,
		cmd:      cmd,
		extraFds: extraFds,
		done:     make(chan struct{}),
		sigCh:    make(chan os.Signal, 1),
	}
	r.handles = append(r.handles, h)
	return h, nil
}

// --- Tests ---

func TestHandleConnection_PropagatesOptionsToRunner(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, fr)

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	assert.NoError(t, err)
	defer listener.Close()

	dev := mountertest.OpenDevNull(t)

	// Send options via UDS in background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mountoptions.Send(ctx, sockPath, mountoptions.Options{
			Fd:         int(dev.Fd()),
			BucketName: "test-bucket",
			Args:       []string{"--region", "us-west-2"},
			Env:        []string{"AWS_REGION=us-west-2"},
			VolumeId:   "pod123-vol456",
		})
	}()

	conn, err := listener.Accept()
	assert.NoError(t, err)

	handleConnection(conn.(*net.UnixConn), "/opt/mount-s3", pm, 5*time.Second)

	// Verify runner received the options
	fr.mu.Lock()
	assert.Equals(t, 1, len(fr.handles))
	fr.mu.Unlock()

	cmd := fr.handles[0].cmd
	assert.Equals(t, "/opt/mount-s3", cmd.Path)
	assert.Equals(t, "test-bucket", cmd.Args[1])
	assert.Equals(t, []string{"AWS_REGION=us-west-2"}, cmd.Env)
	assert.Equals(t, 1, len(fr.handles[0].extraFds)) // FD was passed

	// Cleanup
	fr.handles[0].Exit(0, "")
	pm.Shutdown()
}

func TestProcessManager_Launch_HappyPath(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, fr)
	dev := mountertest.OpenDevNull(t)

	err := pm.Launch("mount-123", "/usr/bin/mount-s3", mountoptions.Options{
		Fd:         int(dev.Fd()),
		BucketName: "my-bucket",
		Env:        []string{"AWS_REGION=us-east-1"},
	})
	assert.NoError(t, err)

	// Verify cmd args
	cmd := fr.handles[0].cmd
	assert.Equals(t, []string{"/usr/bin/mount-s3", "my-bucket", "/dev/fd/3", "--foreground"}, cmd.Args)
	assert.Equals(t, []uintptr{dev.Fd()}, fr.handles[0].extraFds)
	assert.Equals(t, []string{"AWS_REGION=us-east-1"}, cmd.Env)

	// Verify tracked
	pm.mu.Lock()
	assert.Equals(t, 1, len(pm.processes))
	assert.Equals(t, fr.handles[0].Pid(), pm.processes["mount-123"].Pid())
	pm.mu.Unlock()

	// Clean exit
	fr.handles[0].Exit(0, "")
	pm.Shutdown()

	// Verify untracked, no error file
	pm.mu.Lock()
	assert.Equals(t, 0, len(pm.processes))
	pm.mu.Unlock()

	_, err = os.ReadFile(filepath.Join(commDir, "mount-123.error"))
	assert.Equals(t, true, os.IsNotExist(err))
}

func TestProcessManager_Launch_MultipleProcesses(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, fr)

	for i, id := range []string{"mount-a", "mount-b", "mount-c"} {
		dev := mountertest.OpenDevNull(t)
		err := pm.Launch(id, "/usr/bin/mount-s3", mountoptions.Options{
			Fd:         int(dev.Fd()),
			BucketName: fmt.Sprintf("bucket-%d", i),
		})
		assert.NoError(t, err)
	}

	// All 3 tracked
	pm.mu.Lock()
	assert.Equals(t, 3, len(pm.processes))
	pm.mu.Unlock()

	// Verify each got correct bucket and received exactly one FD
	assert.Equals(t, "bucket-0", fr.handles[0].cmd.Args[1])
	assert.Equals(t, "bucket-1", fr.handles[1].cmd.Args[1])
	assert.Equals(t, "bucket-2", fr.handles[2].cmd.Args[1])

	// One exits with error, one clean, one still running
	fr.handles[0].Exit(1, "oom killed")
	fr.handles[1].Exit(0, "")

	// Give goroutines time to process
	time.Sleep(50 * time.Millisecond)

	// Only mount-c still tracked
	pm.mu.Lock()
	assert.Equals(t, 1, len(pm.processes))
	_, hasMountC := pm.processes["mount-c"]
	assert.Equals(t, true, hasMountC)
	pm.mu.Unlock()

	// Error file written for mount-a, not for mount-b
	errBytes, err := os.ReadFile(filepath.Join(commDir, "mount-a.error"))
	assert.NoError(t, err)
	assert.Equals(t, "oom killed", string(errBytes))

	_, err = os.ReadFile(filepath.Join(commDir, "mount-b.error"))
	assert.Equals(t, true, os.IsNotExist(err))

	// Shutdown signals remaining and waits
	fr.handles[2].Exit(0, "")
	pm.Shutdown()

	pm.mu.Lock()
	assert.Equals(t, 0, len(pm.processes))
	pm.mu.Unlock()
}

func TestProcessManager_Launch_DuplicateMountId_Rejected(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, fr)

	dev1 := mountertest.OpenDevNull(t)
	err := pm.Launch("same-mount", "/usr/bin/mount-s3", mountoptions.Options{
		Fd:         int(dev1.Fd()),
		BucketName: "bucket",
	})
	assert.NoError(t, err)

	// Second launch with same mountId should fail
	dev2 := mountertest.OpenDevNull(t)
	err = pm.Launch("same-mount", "/usr/bin/mount-s3", mountoptions.Options{
		Fd:         int(dev2.Fd()),
		BucketName: "bucket",
	})
	if err == nil {
		t.Fatal("Expected error for duplicate mountId, got nil")
	}

	// Only one process tracked
	pm.mu.Lock()
	assert.Equals(t, 1, len(pm.processes))
	pm.mu.Unlock()

	// After first exits, same mountId can be reused
	fr.handles[0].Exit(0, "")
	time.Sleep(10 * time.Millisecond)

	dev3 := mountertest.OpenDevNull(t)
	err = pm.Launch("same-mount", "/usr/bin/mount-s3", mountoptions.Options{
		Fd:         int(dev3.Fd()),
		BucketName: "bucket",
	})
	assert.NoError(t, err)

	fr.handles[1].Exit(0, "")
	pm.Shutdown()
}

func TestProcessManager_Shutdown_SendsSIGTERM(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, fr)

	dev := mountertest.OpenDevNull(t)
	err := pm.Launch("m1", "/usr/bin/mount-s3", mountoptions.Options{
		Fd:         int(dev.Fd()),
		BucketName: "b",
	})
	assert.NoError(t, err)

	go func() {
		select {
		case sig := <-fr.handles[0].sigCh:
			assert.Equals(t, os.Signal(syscall.SIGTERM), sig)
			fr.handles[0].Exit(0, "")
		case <-time.After(5 * time.Second):
			t.Error("Timed out waiting for SIGTERM")
			fr.handles[0].Exit(1, "")
		}
	}()

	pm.Shutdown()
}

// TestHandleConnection_NoFdLeak verifies that handleConnection does not leak file descriptors
// across multiple iterations, including the error path (empty VolumeId).
// NOTE: Do not use t.Parallel() here — fd counting via /proc/self/fd is process-global
// and would be unreliable if other tests open/close fds concurrently.
func TestHandleConnection_NoFdLeak(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, fr)

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	assert.NoError(t, err)
	defer listener.Close()

	fdsBefore := countOpenFds(t)

	const iterations = 5
	for i := range iterations {
		dev := mountertest.OpenDevNull(t)
		sendDone := make(chan struct{})
		volumeId := fmt.Sprintf("vol-%d", i)
		if i == iterations-1 {
			volumeId = "" // no VolumeId — handleConnection should close fd without launching
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			mountoptions.Send(ctx, sockPath, mountoptions.Options{
				Fd:         int(dev.Fd()),
				BucketName: "bucket",
				VolumeId:   volumeId,
			})
			dev.Close()
			close(sendDone)
		}()

		conn, err := listener.Accept()
		assert.NoError(t, err)
		handleConnection(conn.(*net.UnixConn), "/opt/mount-s3", pm, 5*time.Second)
		<-sendDone
		if i < iterations-1 {
			fr.handles[i].Exit(0, "")
		}
	}

	time.Sleep(50 * time.Millisecond)
	pm.Shutdown()

	fdsAfter := countOpenFds(t)
	if fdsAfter > fdsBefore {
		t.Errorf("fd leak: %d before, %d after", fdsBefore, fdsAfter)
	}
}

func countOpenFds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	assert.NoError(t, err)
	return len(entries)
}

func TestProcessManager_Launch_ErrorExit_WritesErrorFile(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, fr)

	dev := mountertest.OpenDevNull(t)
	err := pm.Launch("mount-abc", "/usr/bin/mount-s3", mountoptions.Options{
		Fd:         int(dev.Fd()),
		BucketName: "bucket",
	})
	assert.NoError(t, err)

	fr.handles[0].Exit(1, "credential error")
	pm.Shutdown()

	errBytes, err := os.ReadFile(filepath.Join(commDir, "mount-abc.error"))
	assert.NoError(t, err)
	assert.Equals(t, "credential error", string(errBytes))
}
