package main

import (
	"bytes"
	"testing"
)

// Mountpoint's stderr keeps one line per record, each tagged with the mount, and a CR or other control
// character within a line is escaped so it cannot hide or rewrite the tag.
func TestPrefixWriterEscapesControlCharacters(t *testing.T) {
	var out bytes.Buffer
	pw := newPrefixWriter(&out, "pv-a")
	in := "error: invalid value 'before\r\nafter\x1b[2K' for '<BUCKET_NAME>'\n"
	if n, err := pw.Write([]byte(in)); err != nil || n != len(in) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	want := "[pv-a] error: invalid value 'before\\r\n[pv-a] after\\x1b[2K' for '<BUCKET_NAME>'\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}
