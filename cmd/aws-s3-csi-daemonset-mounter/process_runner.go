// process_runner.go defines the ProcessRunner abstraction that decouples ProcessManager
// from real process lifecycle (fork/exec/wait). Tests inject a fake implementation to
// control when and how child processes "start" and "exit" without spawning real processes.

package main

import (
	"io"
	"os"
	"os/exec"
	"sync"

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
	stderrBuf := newTailBuf(r.stderrCapacity)
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
	stderrBuf *tailBuf
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

// tailBuf is a fixed-capacity ring buffer that retains the last N bytes written.
type tailBuf struct {
	mu   sync.Mutex
	buf  []byte
	pos  int
	full bool
}

func newTailBuf(capacity uint) *tailBuf {
	return &tailBuf{buf: make([]byte, capacity)}
}

func (tb *tailBuf) Write(p []byte) (int, error) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	n := len(p)
	cap := len(tb.buf)
	if cap == 0 {
		return n, nil
	}
	if len(p) >= cap {
		copy(tb.buf, p[len(p)-cap:])
		tb.pos = 0
		tb.full = true
		return n, nil
	}
	for len(p) > 0 {
		space := cap - tb.pos
		copied := copy(tb.buf[tb.pos:], p[:min(len(p), space)])
		tb.pos = (tb.pos + copied) % cap
		if tb.pos == 0 || copied == space {
			tb.full = true
		}
		p = p[copied:]
	}
	return n, nil
}

func (tb *tailBuf) Bytes() []byte {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if !tb.full {
		return append([]byte(nil), tb.buf[:tb.pos]...)
	}
	out := make([]byte, len(tb.buf))
	n := copy(out, tb.buf[tb.pos:])
	copy(out[n:], tb.buf[:tb.pos])
	return out
}
