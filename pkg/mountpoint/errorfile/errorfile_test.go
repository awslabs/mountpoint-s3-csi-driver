package errorfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestFormat_OneLineOfPrintableASCII(t *testing.T) {
	assert.Equals(t, "Error: bad bucket name", Format("Error: \x1b[31mbad\x1b[0m\r\n bucket\téé name\n"))
}

func TestFormat_LongMessageKeepsStartAndRootCause(t *testing.T) {
	msg := "Mountpoint exited with code 1: Error: Failed to create S3 client Caused by: " +
		strings.Repeat("0: initial ListObjectsV2 failed, ", 40) + "9: Forbidden: is not authorized to perform: s3:ListBucket"
	got := Format(msg)
	assert.Equals(t, MaxBytes, len(got))
	assert.Equals(t, true, strings.HasPrefix(got, "Mountpoint exited with code 1: Error: Failed to create S3 client"))
	assert.Equals(t, true, strings.HasSuffix(got, "is not authorized to perform: s3:ListBucket"))
	assert.Contains(t, got, " ... ")
}

// Formatting changes layout only, never content.
func TestFormat_KeepsContent(t *testing.T) {
	s := "User: arn:aws:sts::111122223333:assumed-role/MyRole/i-0abc is not authorized; x-amz-id-2: vlR7PnpV2Ce81l0PRw6jlUpck7Jo5ZsQjryTjKlc5aLWGVHPZLj5NeC6qMa0emYBDXOo6QBU0Wo="
	assert.Equals(t, s, Format(s))
}

func TestFormatWithin_StaysWithinAnyCapWithoutPanicking(t *testing.T) {
	long := strings.Repeat("word ", 200)
	for _, maxBytes := range []int{-1, 0, 1, 5, 6, 7, 8, 9, 50, 100, MaxBytes} {
		got := FormatWithin(long, maxBytes)
		if len(got) > max(maxBytes, 0) {
			t.Errorf("FormatWithin(_, %d) is %d bytes", maxBytes, len(got))
		}
	}
}

// Formatting a formatted message again changes nothing, also at the elision.
func TestFormat_IsIdempotent(t *testing.T) {
	for _, msg := range []string{
		strings.Repeat("a", 259) + " " + strings.Repeat("b ", 300),
		strings.Repeat("ab ", 400),
		strings.Repeat("ab ", 400) + "x", // the tail cut starts on a space
		"short",
	} {
		once := Format(msg)
		assert.Equals(t, once, Format(once))
	}
}

func TestWrite_FormattedAtomicWithMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pv.error")
	assert.NoError(t, Write(path, "Error: x\n  Caused by: y\n", 0600))

	got, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.Equals(t, "Error: x Caused by: y", string(got))
	info, err := os.Stat(path)
	assert.NoError(t, err)
	assert.Equals(t, fs.FileMode(0600), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	assert.NoError(t, err)
	assert.Equals(t, 1, len(entries)) // no temporary file left beside it
}

// The temporary name does not grow with the target's: a volume ID of up to 249 bytes keeps "<id>.error"
// within NAME_MAX (255).
func TestWrite_LongFileName(t *testing.T) {
	path := filepath.Join(t.TempDir(), strings.Repeat("v", 249)+".error")
	assert.NoError(t, Write(path, "Error: x", 0600))
	got, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.Equals(t, "Error: x", string(got))
}
