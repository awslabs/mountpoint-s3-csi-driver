// Package log provides shared klog initialization for CSI Driver components.
package log

import (
	"flag"
	"os"

	"k8s.io/klog/v2"
)

// noStderrThreshold is a klog -stderrthreshold above FATAL, the highest severity. klog writes an entry at or above
// the threshold to os.Stderr as is, in addition to the output InitKlog sets, so no entry may reach it. (klog.Fatal
// still writes the goroutine stacks to os.Stderr as is; use klog.Exit for an error that needs no stacks.)
const noStderrThreshold = "4"

// escapingStderrWriter writes each klog entry to stderr as one line, with its control characters escaped.
type escapingStderrWriter struct{}

func (*escapingStderrWriter) Write(b []byte) (int, error) {
	n := len(b)
	entry := string(b)
	if len(entry) > 0 && entry[len(entry)-1] == '\n' {
		entry = entry[:len(entry)-1]
	}
	_, err := os.Stderr.WriteString(EscapeControlChars(entry) + "\n")
	return n, err
}

// InitKlog initializes klog to write each entry to stderr once, as one line: control characters in it, such as
// the CR/LF of untrusted text it logs, are escaped (see EscapeControlChars).
func InitKlog() {
	klog.InitFlags(nil)
	flag.Set("logtostderr", "false")
	flag.Set("alsologtostderr", "false")
	if err := flag.Set("stderrthreshold", noStderrThreshold); err != nil {
		flag.Set("stderrthreshold", "FATAL")
	}
	// Write an entry only to the output of its own severity. All severities share the output set below, which
	// would otherwise get an error entry three times: as ERROR, WARNING and INFO.
	flag.Set("one_output", "true")
	klog.SetOutput(&escapingStderrWriter{})
}
