package testutil

import (
	"bytes"
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
	klog.SetOutput(&buf)
	t.Cleanup(func() {
		klog.SetOutput(nil)
		klog.LogToStderr(true)
	})
	return &buf
}
