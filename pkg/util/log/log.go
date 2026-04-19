// Package log provides shared klog initialization for CSI Driver components.
package log

import (
	"bytes"
	"flag"
	"os"

	"k8s.io/klog/v2"
)

var (
	newline       = []byte("\n")
	newlineEscape = []byte("")
)

// newlineEscapingStderrWriter writes log entries to stderr after escaping newlines.
type newlineEscapingStderrWriter struct{}

func (*newlineEscapingStderrWriter) Write(b []byte) (int, error) {
	n := len(b)
	_, err := os.Stderr.Write(append(bytes.ReplaceAll(b, newline, newlineEscape), newline...))
	return n, err
}

// InitKlog initializes klog to write to stderr with newlines escaped.
func InitKlog() {
	klog.InitFlags(nil)
	flag.Set("logtostderr", "false")
	flag.Set("alsologtostderr", "false")
	klog.SetOutput(&newlineEscapingStderrWriter{})
}
