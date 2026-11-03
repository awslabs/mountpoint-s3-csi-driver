// process_runner.go defines the ProcessRunner abstraction that decouples ProcessManager
// from real process lifecycle (fork/exec/wait). Tests inject a fake implementation to
// control when and how child processes "start" and "exit" without spawning real processes.

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/armon/circbuf"
	"k8s.io/klog/v2"
)

// ProcessHandle represents a started process that can be waited on.
type ProcessHandle interface {
	Pid() int
	Wait() (status exitStatus, stderr []byte)
	Signal(sig os.Signal) error
}

// ProcessRunner starts a command and returns a handle to wait on it.
type ProcessRunner interface {
	// Start starts cmd, the Mountpoint of mountId, which tags the log lines of the runner itself.
	Start(mountId string, cmd *exec.Cmd) (ProcessHandle, error)
}

// defaultProcessRunner is the real implementation that starts OS processes.
type defaultProcessRunner struct {
	stderrCapacity uint
}

func (r *defaultProcessRunner) Start(mountId string, cmd *exec.Cmd) (ProcessHandle, error) {
	stderrBuf, err := circbuf.NewBuffer(int64(r.stderrCapacity))
	if err != nil {
		return nil, err
	}
	if cmd.Stderr != nil {
		cmd.Stderr = io.MultiWriter(cmd.Stderr, stderrBuf)
	} else {
		cmd.Stderr = stderrBuf
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &defaultProcessHandle{mountId: mountId, cmd: cmd, stderrBuf: stderrBuf}, nil
}

type defaultProcessHandle struct {
	mountId   string
	cmd       *exec.Cmd
	stderrBuf *circbuf.Buffer
}

func (h *defaultProcessHandle) Pid() int { return h.cmd.Process.Pid }

func (h *defaultProcessHandle) Wait() (exitStatus, []byte) {
	err := h.cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		// e.g. copying its output failed; the process itself may still have exited cleanly.
		klog.Errorf("[%s] Unexpected error waiting for Mountpoint process %d: %v", h.mountId, h.cmd.Process.Pid, err)
		if h.cmd.ProcessState == nil {
			return exitStatus{code: 1}, h.stderrBuf.Bytes()
		}
	}
	return exitStatusOf(h.cmd.ProcessState), h.stderrBuf.Bytes()
}

func (h *defaultProcessHandle) Signal(sig os.Signal) error {
	return h.cmd.Process.Signal(sig)
}
