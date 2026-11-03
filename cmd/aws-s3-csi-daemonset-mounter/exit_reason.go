// exit_reason.go turns how a Mountpoint process ended into the message written to its error file,
// which writeErrorFile writes with [errorfile.Write] as one short line. The full stderr stays in this container's log.

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/errorfile"
)

// stderrTailLines is how many of the last stderr lines are kept when stderr holds no `Error:` line.
const stderrTailLines = 3

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
// each memoryLimitStrategy; docs/MEMORY.md has the full guidance. They are kept short so most of
// errorfile.MaxBytes is left for the stderr summary.
var oomHints = map[memoryLimitStrategy]string{
	memoryLimitEqualSplit: "If nobody killed it, it likely ran out of memory. memoryLimitStrategy=equalSplit sets " +
		"--memory-target to (daemonsetMounters[].resources.requests.memory - 64Mi - in-memory cache) / " +
		"maxVolumesPerNode; to lower it, raise maxVolumesPerNode or lower the request, see docs/MEMORY.md",
	memoryLimitNone: "If nobody killed it, it likely ran out of memory. With memoryLimitStrategy=none, set or lower " +
		"--memory-target in the PV mountOptions, see docs/MEMORY.md",
}

// exitReason is the error file content for a Mountpoint that ended with `status` in a container whose
// memory is split by `strategy`; writeErrorFile formats it. A SIGKILL hint goes last and is never cut:
// the status and stderr summary before it are formatted within what is left of errorfile.MaxBytes.
func exitReason(status exitStatus, stderr []byte, strategy memoryLimitStrategy) string {
	msg := "Mountpoint " + status.String()
	if summary := summarizeStderr(stderr); summary != "" {
		msg += ": " + summary
	}
	hint, ok := oomHints[strategy]
	if !ok || status.signal != syscall.SIGKILL {
		return msg
	}
	suffix := ". " + hint
	return errorfile.FormatWithin(msg, errorfile.MaxBytes-len(suffix)) + suffix
}

// summarizeStderr keeps what explains a failure. Mountpoint logs to stdout when in the foreground, so
// stderr holds mostly its final error, which Rust prints as `Error: <context>`, then `Caused by:`
// and an indented chain. Without that, it keeps the last few lines, e.g. a panic.
func summarizeStderr(stderr []byte) string {
	var lines []string
	for _, line := range strings.Split(string(stderr), "\n") {
		if line = strings.TrimSpace(errorfile.StripANSI(line)); line != "" {
			lines = append(lines, line)
		}
	}
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "Error: ") || strings.HasPrefix(line, "error: ") {
			start = i
		}
	}
	if start < 0 {
		start = max(0, len(lines)-stderrTailLines)
	}
	return strings.Join(lines[start:], " ")
}
