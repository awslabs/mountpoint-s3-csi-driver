// exit_reason.go turns how a Mountpoint process ended into the message written to its error file. The
// full stderr stays in this container's log.

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// exitStatus is how a Mountpoint process ended.
type exitStatus struct {
	code   int            // exit code, or -1 when killed by a signal
	signal syscall.Signal // the killing signal, or 0
}

func (s exitStatus) failed() bool { return s.code != 0 || s.signal != 0 }

// String completes "Mountpoint for mount X ...".
func (s exitStatus) String() string {
	if s.signal != 0 {
		return "was killed by signal " + signalName(s.signal)
	}
	return fmt.Sprintf("exited with code %d", s.code)
}

// exitStatusOf reads the exit code or killing signal from a finished process.
func exitStatusOf(state *os.ProcessState) exitStatus {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return exitStatus{code: -1, signal: ws.Signal()}
	}
	return exitStatus{code: state.ExitCode()}
}

func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return name
	}
	return fmt.Sprintf("%d", int(sig))
}

// oomHints explain the usual sender of SIGKILL and the setting that sizes a Mountpoint's memory under
// each memoryLimitStrategy; docs/MEMORY.md has the full guidance.
var oomHints = map[memoryLimitStrategy]string{
	memoryLimitEqualSplit: "If nobody killed it, it likely ran out of memory. memoryLimitStrategy=equalSplit sets " +
		"--memory-target to (daemonsetMounters[].resources.requests.memory - 64Mi - in-memory cache) / " +
		"maxVolumesPerNode; to lower it, raise maxVolumesPerNode or lower the request, see docs/MEMORY.md",
	memoryLimitNone: "If nobody killed it, it likely ran out of memory. With memoryLimitStrategy=none, set or lower " +
		"--memory-target in the PV mountOptions, see docs/MEMORY.md",
}

// exitReason is the error file content for a Mountpoint that ended with `status` in a container whose
// memory is split by `strategy`: how it ended, a hint on SIGKILL, then its stderr tail as it is.
func exitReason(status exitStatus, stderr []byte, strategy memoryLimitStrategy) string {
	msg := "Mountpoint " + status.String()
	if hint, ok := oomHints[strategy]; ok && status.signal == syscall.SIGKILL {
		msg += ". " + hint
	}
	if tail := strings.TrimSpace(string(stderr)); tail != "" {
		msg += ": " + tail
	}
	return msg
}
