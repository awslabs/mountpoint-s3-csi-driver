// process_runner.go defines the ProcessRunner abstraction that decouples ProcessManager
// from real process lifecycle (fork/exec/wait). Tests inject a fake implementation to
// control when and how child processes "start" and "exit" without spawning real processes.

package main

import (
	"io"
	"os"
	"os/exec"

	"github.com/armon/circbuf"
	"k8s.io/klog/v2"
)

// ProcessHandle represents a started process that can be waited on.
type ProcessHandle interface {
	Pid() int
	Wait() (exitCode int, stderr []byte)
	Signal(sig os.Signal) error
}

// ProcessRunner starts a command and returns a handle to wait on it.
type ProcessRunner interface {
	Start(cmd *exec.Cmd) (ProcessHandle, error)
}

// defaultProcessRunner is the real implementation that starts OS processes.
type defaultProcessRunner struct {
	stderrCapacity uint
}

func (r *defaultProcessRunner) Start(cmd *exec.Cmd) (ProcessHandle, error) {
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
	return &defaultProcessHandle{cmd: cmd, stderrBuf: stderrBuf}, nil
}

type defaultProcessHandle struct {
	cmd       *exec.Cmd
	stderrBuf *circbuf.Buffer
}

func (h *defaultProcessHandle) Pid() int { return h.cmd.Process.Pid }

func (h *defaultProcessHandle) Wait() (int, []byte) {
	err := h.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			klog.Errorf("Unexpected error waiting for process %d: %v", h.cmd.Process.Pid, err)
			exitCode = 1
		}
	} else {
		exitCode = h.cmd.ProcessState.ExitCode()
	}
	return exitCode, h.stderrBuf.Bytes()
}

func (h *defaultProcessHandle) Signal(sig os.Signal) error {
	return h.cmd.Process.Signal(sig)
}
