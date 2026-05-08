// process_runner.go defines the ProcessRunner abstraction that decouples ProcessManager
// from real process lifecycle (fork/exec/wait). Tests inject a fake implementation to
// control when and how child processes "start" and "exit" without spawning real processes.

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
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
type defaultProcessRunner struct{}

func (r *defaultProcessRunner) Start(cmd *exec.Cmd) (ProcessHandle, error) {
	var stderrBuf bytes.Buffer
	if cmd.Stderr != nil {
		cmd.Stderr = io.MultiWriter(cmd.Stderr, &stderrBuf)
	} else {
		cmd.Stderr = &stderrBuf
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &realProcessHandle{cmd: cmd, stderrBuf: &stderrBuf}, nil
}

type realProcessHandle struct {
	cmd       *exec.Cmd
	stderrBuf *bytes.Buffer
}

func (h *realProcessHandle) Pid() int { return h.cmd.Process.Pid }

func (h *realProcessHandle) Wait() (int, []byte) {
	err := h.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	} else {
		exitCode = h.cmd.ProcessState.ExitCode()
	}
	return exitCode, h.stderrBuf.Bytes()
}

func (h *realProcessHandle) Signal(sig os.Signal) error {
	return h.cmd.Process.Signal(sig)
}
