// Package errorfile formats and writes the error file a mounter writes for a failed Mountpoint: the V3
// `<mount-id>.error` and the V2 Mountpoint Pod's mount error file. The CSI node returns its text as the
// NodePublishVolume error, which the kubelet shows in a FailedMount event, so it is kept to one short
// line of printable ASCII. The full output stays in the mounter's log.
package errorfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxBytes caps the error file. The kubelet puts it in a core/v1 FailedMount event, which has no length
// limit, after its prefix, the target path and the CSI node's log hint; `kubectl describe` prints that
// on one line, so this keeps the whole event under 2 KiB even with worst-case Kubernetes names.
const MaxBytes = 512

// headBytes is how much of the start an over-long message keeps; the rest of MaxBytes goes to its end,
// where an `Error: ... Caused by: 0: ... N: <root cause>` chain puts the root cause.
const headBytes = 260

const elision = " ... "

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// StripANSI removes terminal color escapes.
func StripANSI(s string) string { return ansiEscape.ReplaceAllString(s, "") }

// Format makes msg one line of printable ASCII of at most MaxBytes. An over-long message keeps its
// start and its end.
func Format(msg string) string { return FormatWithin(msg, MaxBytes) }

// FormatWithin is [Format] with a smaller cap, for a caller that appends text of its own. Any cap is
// safe: one too small for the elision just truncates.
func FormatWithin(msg string, maxBytes int) string {
	var b strings.Builder
	for _, r := range StripANSI(msg) {
		if r >= ' ' && r <= '~' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	msg = strings.Join(strings.Fields(b.String()), " ")
	switch {
	case len(msg) <= maxBytes:
	case maxBytes < 4*len(elision):
		msg = msg[:max(maxBytes, 0)]
	default:
		head := headBytes * maxBytes / MaxBytes
		tail := maxBytes - head - len(elision)
		// Trimmed at the cut, so formatting the result again changes nothing.
		msg = strings.TrimRight(msg[:head], " ") + elision + strings.TrimLeft(msg[len(msg)-tail:], " ")
	}
	return msg
}

// Write writes msg, formatted, to path atomically: to a temporary file in the same directory, renamed
// into place, so a reader polling for path never sees a partly written file. It does not fsync: the
// reader needs the rename to be atomic, not durable, and the V3 mounter's exit path calls it while
// holding the ProcessManager lock, where an fsync would stall every launch on the node.
func Write(path, msg string, perm fs.FileMode) error {
	// A fixed short name: one built from path's base could exceed NAME_MAX for a long volume ID.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".error-tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(Format(msg))
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
