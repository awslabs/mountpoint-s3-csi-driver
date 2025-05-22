package awsprofiletest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// writeFile is a small helper for the tests below.
func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
}

// ------------------------------------------------------------------------
// 1.  os.Open error path
// ------------------------------------------------------------------------

func TestReadConfig_FileNotFound(t *testing.T) {
	_, err := ReadConfig(filepath.Join(t.TempDir(), "no-such-file"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected fs.ErrNotExist, got %v", err)
	}
}

func TestReadIniFile_FileNotFound(t *testing.T) {
	_, err := readIniFile(filepath.Join(t.TempDir(), "no-such-file"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected fs.ErrNotExist, got %v", err)
	}
}

// ------------------------------------------------------------------------
// 2.  skip blank lines and comments
// ------------------------------------------------------------------------

func TestReadConfig_SkipsBlanksAndComments(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	cfg := `
# global comment

[profile foo]
# inside comment
key1 = val1

[profile bar] # trailing comment
key2=val2
`
	writeFile(t, cfgPath, cfg)

	got, err := ReadConfig(cfgPath)
	assert.NoError(t, err)
	want := map[string]map[string]string{
		"profile foo": {
			"key1": "val1",
			"key2": "val2",
		},
	}
	assert.Equals(t, want, got)
}

// ------------------------------------------------------------------------
// readIniFile: skip blank & comment lines
// ------------------------------------------------------------------------

func TestReadIniFile_SkipsBlanksAndComments(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials")

	cred := `
# global comment

[default]
aws_access_key_id = ABC123

# another comment
aws_secret_access_key = DEF456
`
	writeFile(t, credPath, cred)

	got, err := readIniFile(credPath)
	assert.NoError(t, err)
	want := map[string]map[string]string{
		"default": {
			"aws_access_key_id":     "ABC123",
			"aws_secret_access_key": "DEF456",
		},
	}
	assert.Equals(t, want, got)
}

// ------------------------------------------------------------------------
// 3.  lines inside section without "=" are ignored
// ------------------------------------------------------------------------

func TestReadConfig_IgnoresInvalidKeyValueLines(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	cfg := "[profile p]\nnot-a-key-value\nkey = val\n"
	writeFile(t, cfgPath, cfg)

	got, err := ReadConfig(cfgPath)
	assert.NoError(t, err)
	want := map[string]map[string]string{
		"profile p": {"key": "val"},
	}
	assert.Equals(t, want, got)
}

// ------------------------------------------------------------------------
// 4.  scanner.Err() error propagation
// ------------------------------------------------------------------------

// On Unix opening a directory succeeds but reads fail with EISDIR, which
// bufio.Scanner surfaces via scanner.Err().
func TestReadConfig_ScannerError(t *testing.T) {
	dir := t.TempDir()
	scanErrDir := filepath.Join(dir, "dir")
	assert.NoError(t, os.Mkdir(scanErrDir, 0o755))

	_, err := ReadConfig(scanErrDir)
	if err == nil {
		t.Fatalf("expected scanner error, got nil")
	}
}

func TestReadIniFile_ScannerError(t *testing.T) {
	dir := t.TempDir()
	scanErrDir := filepath.Join(dir, "dir")
	assert.NoError(t, os.Mkdir(scanErrDir, 0o755))

	_, err := readIniFile(scanErrDir)
	if err == nil {
		t.Fatalf("expected scanner error, got nil")
	}
}
