package testutil

import (
	"bytes"
	"flag"
	"regexp"
	"strconv"
	"sync"
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

// syncBuffer is a bytes.Buffer safe for klog's concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// CaptureKlogAtVerbosity redirects klog to a buffer at the given verbosity for the rest of `t`, and
// returns a function that flushes klog and returns everything logged so far. Unlike CaptureKlog it
// raises -v, so V(n) logs are captured too, and it tolerates concurrent writers. klog is global, so a
// test using this must not run in parallel with others that log.
func CaptureKlogAtVerbosity(t *testing.T, verbosity int) func() string {
	t.Helper()
	flags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(flags)
	set := func(name, value string) {
		if err := flags.Set(name, value); err != nil {
			t.Fatalf("setting klog flag %s: %v", name, err)
		}
	}
	set("logtostderr", "false")
	set("alsologtostderr", "false")
	set("v", strconv.Itoa(verbosity))

	buf := &syncBuffer{}
	klog.SetOutput(buf)
	t.Cleanup(func() {
		klog.Flush()
		klog.SetOutput(nil)
		set("v", "0")
		set("logtostderr", "true")
	})
	return func() string {
		klog.Flush()
		return buf.String()
	}
}

// secretShapes are patterns of credential material that must never be logged, whatever the value.
var secretShapes = map[string]*regexp.Regexp{
	"an AWS access key ID":           regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`),
	"an aws_secret_access_key entry": regexp.MustCompile(`(?i)aws_secret_access_key`),
	"a JWT":                          regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
}

// AssertNoCredentialsLogged fails `t` if `logs` contains any of `secrets` verbatim, or anything shaped
// like an AWS access key ID, an `aws_secret_access_key` entry or a JWT.
func AssertNoCredentialsLogged(t *testing.T, logs string, secrets ...string) {
	t.Helper()
	if logs == "" {
		t.Fatal("captured no log output, so there is nothing to check; is the verbosity high enough?")
	}
	for _, secret := range secrets {
		if secret != "" && bytes.Contains([]byte(logs), []byte(secret)) {
			t.Errorf("logs contain the credential value %q", secret)
		}
	}
	for name, shape := range secretShapes {
		if match := shape.FindString(logs); match != "" {
			t.Errorf("logs contain %s: %q", name, match)
		}
	}
}
