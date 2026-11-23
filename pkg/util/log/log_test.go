package log

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/klog/v2"
)

const childEnv = "UTILLOG_TEST_CHILD"

// injected is a value an attacker controls, such as a PV's bucketName, carrying a CR/LF and a forged klog record.
const injected = "before\r\nI9999 00:00:00.000000 1 FORGED_RECORD after"

// TestMain lets TestInitKlogKeepsEachEntryOnOneLine run this test binary as a child process that logs with
// InitKlog, since klog can write entries straight to os.Stderr, which only a separate process can capture.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		InitKlog()
		klog.Infof("info: %s", injected)
		klog.Warningf("warning: %s", injected)
		klog.Errorf("error: %s", injected)
		klog.Flush()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestInitKlogKeepsEachEntryOnOneLine(t *testing.T) {
	var stdout, stderrBuf bytes.Buffer
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.Stdout, cmd.Stderr = &stdout, &stderrBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("child failed: %v: %q", err, stderrBuf.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
	stderr := stderrBuf.String()

	if strings.Contains(stderr, "\r") {
		t.Errorf("stderr has a raw CR: %q", stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, one per entry, got %d: %q", len(lines), stderr)
	}
	for i, prefix := range []string{"I", "W", "E"} {
		line := lines[i]
		if !strings.HasPrefix(line, prefix) {
			t.Errorf("line %d should be the %s entry: %q", i, prefix, line)
		}
		if !strings.HasSuffix(line, `before\r\nI9999 00:00:00.000000 1 FORGED_RECORD after`) {
			t.Errorf("line %d should end with the escaped value: %q", i, line)
		}
	}
}

func TestEscapeControlChars(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "",
		"plain bucket-name.1":       "plain bucket-name.1",
		"a\r\nb":                    `a\r\nb`,
		"tab\there":                 `tab\there`,
		"nul\x00esc\x1bdel\x7f":     `nul\x00esc\x1bdel\x7f`,
		"nel\u0085ls\u2028ps\u2029": `nel\u0085ls\u2028ps\u2029`,
		"bad\xffutf8":               `bad\xffutf8`,
		"unicode ok: é 日本":          "unicode ok: é 日本",
		`already \n escaped`:        `already \n escaped`,
	} {
		if got := EscapeControlChars(in); got != want {
			t.Errorf("EscapeControlChars(%q) = %q, want %q", in, got, want)
		}
	}
}
