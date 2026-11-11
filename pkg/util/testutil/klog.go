package testutil

import (
	"bytes"
	"io"
	"testing"

	"k8s.io/klog/v2"
)

// CaptureKlog redirects klog's output into the returned buffer for the rest of t. SetOutput alone
// captures nothing: logtostderr defaults true and klog short-circuits to os.Stderr before the sink.
// The sink is process-global, so no test that uses this may call t.Parallel.
func CaptureKlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	klog.LogToStderr(false)
	// Capture only the INFO sink: klog also writes every warning and error there, so this keeps each line once.
	// Discard the others rather than leave them nil, which would make klog create a log file.
	klog.SetOutput(io.Discard)
	klog.SetOutputBySeverity("INFO", &buf)
	t.Cleanup(func() {
		klog.SetOutput(nil)
		klog.LogToStderr(true)
	})
	return &buf
}
