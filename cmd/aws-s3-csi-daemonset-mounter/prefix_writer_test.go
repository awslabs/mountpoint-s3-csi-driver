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

// recordingWriter keeps each Write call, as os.Stderr gets them.
type recordingWriter struct{ writes []string }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

// Every line goes out in one Write, tag and newline included, so lines from concurrent mounts sharing stderr cannot
// interleave within a line.
func TestPrefixWriterWritesEachLineOnce(t *testing.T) {
	w := &recordingWriter{}
	pw := newPrefixWriter(w, "pv-a")
	if _, err := pw.Write([]byte("first\nsecond\n")); err != nil {
		t.Fatal(err)
	}
	want := []string{"[pv-a] first\n", "[pv-a] second\n"}
	if len(w.writes) != len(want) {
		t.Fatalf("got %d writes %q, want %q", len(w.writes), w.writes, want)
	}
	for i := range want {
		if w.writes[i] != want[i] {
			t.Errorf("write %d = %q, want %q", i, w.writes[i], want[i])
		}
	}
}
