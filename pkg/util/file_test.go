package util_test

import (
	"crypto/rand"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestReplaceFile(t *testing.T) {
	expectContentAndPerm := func(t *testing.T, path string, contentWant []byte, permWant fs.FileMode) {
		gotStat, err := os.Stat(path)
		assert.NoError(t, err)

		assert.Equals(t, permWant, gotStat.Mode().Perm())

		contentGot, err := os.ReadFile(path)
		assert.NoError(t, err)

		assert.Equals(t, contentWant, contentGot)
	}

	createFileWithRandomBytes := func(t *testing.T, path string, perm fs.FileMode, size int) []byte {
		content := make([]byte, size)
		_, err := rand.Read(content)
		assert.NoError(t, err)

		source := filepath.Join(path)
		err = os.WriteFile(source, content, perm)
		assert.NoError(t, err)

		return content
	}

	t.Run("Non-existent dest", func(t *testing.T) {
		basedir := t.TempDir()

		source := filepath.Join(basedir, "source")
		content := createFileWithRandomBytes(t, source, 0600, 2*64*1024)

		dest := filepath.Join(basedir, "dest")

		err := util.ReplaceFile(dest, source, 0644)
		assert.NoError(t, err)

		expectContentAndPerm(t, dest, content, 0644)
	})

	t.Run("Existing dest", func(t *testing.T) {
		basedir := t.TempDir()

		source := filepath.Join(basedir, "source")
		content := createFileWithRandomBytes(t, source, 0600, 2*64*1024)

		dest := filepath.Join(basedir, "dest")
		createFileWithRandomBytes(t, dest, 0644, 1024)

		err := util.ReplaceFile(dest, source, 0644)
		assert.NoError(t, err)

		expectContentAndPerm(t, dest, content, 0644)
	})

	t.Run("Existing dest with different permissions", func(t *testing.T) {
		basedir := t.TempDir()

		source := filepath.Join(basedir, "source")
		content := createFileWithRandomBytes(t, source, 0600, 2*64*1024)

		dest := filepath.Join(basedir, "dest")
		createFileWithRandomBytes(t, dest, 0777, 1024)

		err := util.ReplaceFile(dest, source, 0644)
		assert.NoError(t, err)

		expectContentAndPerm(t, dest, content, 0644)
	})

	t.Run("Concurrently", func(t *testing.T) {
		basedir := t.TempDir()

		source := filepath.Join(basedir, "source")
		content := createFileWithRandomBytes(t, source, 0600, 2*64*1024)

		dest := filepath.Join(basedir, "dest")

		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() {

				err := util.ReplaceFile(dest, source, 0644)
				assert.NoError(t, err)
			})
		}
		wg.Wait()

		expectContentAndPerm(t, dest, content, 0644)
	})
}

func TestWriteFileOwned(t *testing.T) {
	t.Run("Writes the content and mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		assert.NoError(t, util.WriteFileOwned(path, []byte("a-token"), 0400, 0))

		got, err := os.ReadFile(path)
		assert.NoError(t, err)
		assert.Equals(t, "a-token", string(got))

		stat, err := os.Stat(path)
		assert.NoError(t, err)
		assert.Equals(t, fs.FileMode(0400), stat.Mode().Perm())
	})

	t.Run("Applies the owner to the file it leaves behind", func(t *testing.T) {
		if os.Getuid() != 0 {
			t.Skip("changing a file's owner and group needs root")
		}
		// Not the writer's own UID, so the assertions below cannot hold without the chown.
		const uid = uint32(65536)
		path := filepath.Join(t.TempDir(), "token")
		assert.NoError(t, util.WriteFileOwned(path, []byte("a-token"), 0400, uid))

		stat, err := os.Stat(path)
		assert.NoError(t, err)
		sys, ok := stat.Sys().(*syscall.Stat_t)
		assert.Equals(t, true, ok)
		assert.Equals(t, uid, sys.Uid)
		assert.Equals(t, uid, sys.Gid)
	})

	t.Run("Replaces an existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		// A wider mode than the one written, so a refresh is shown not to inherit it.
		assert.NoError(t, os.WriteFile(path, []byte("stale"), 0644))
		assert.NoError(t, util.WriteFileOwned(path, []byte("fresh"), 0400, 0))

		got, err := os.ReadFile(path)
		assert.NoError(t, err)
		assert.Equals(t, "fresh", string(got))
		stat, err := os.Stat(path)
		assert.NoError(t, err)
		assert.Equals(t, fs.FileMode(0400), stat.Mode().Perm())

		// No temporary file is left alongside it.
		entries, err := os.ReadDir(filepath.Dir(path))
		assert.NoError(t, err)
		assert.Equals(t, 1, len(entries))
	})

	t.Run("Keeps the owner and owner-only mode when a credential refresh replaces the file", func(t *testing.T) {
		if os.Getuid() != 0 {
			t.Skip("changing a file's owner and group needs root")
		}
		// A credential refresh rewrites the file; the mount's UID must still own the new one.
		const uid = uint32(65536)
		path := filepath.Join(t.TempDir(), "token")
		assert.NoError(t, util.WriteFileOwned(path, []byte("stale"), 0400, uid))
		assert.NoError(t, util.WriteFileOwned(path, []byte("fresh"), 0400, uid))

		stat, err := os.Stat(path)
		assert.NoError(t, err)
		assert.Equals(t, fs.FileMode(0400), stat.Mode().Perm())
		sys, ok := stat.Sys().(*syscall.Stat_t)
		assert.Equals(t, true, ok)
		assert.Equals(t, uid, sys.Uid)
		assert.Equals(t, uid, sys.Gid)
	})

	t.Run("Leaves nothing behind when the path is unwritable", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing-dir", "token")
		if err := util.WriteFileOwned(path, []byte("a-token"), 0400, 0); err == nil {
			t.Fatal("expected a write into a missing directory to fail")
		}

		entries, err := os.ReadDir(dir)
		assert.NoError(t, err)
		assert.Equals(t, 0, len(entries))
	})
}

// allowedCryptoImports are the only cryptographic packages production code may import: standard-library
// primitives, with no custom implementations, no weak algorithms (MD5, SHA-1, DES, RC4, DSA) and no
// third-party crypto. Request signing (SigV4) and TLS to AWS are done by Mountpoint and the AWS SDK, not here.
var allowedCryptoImports = []string{
	"crypto/rand",
	"crypto/sha256",
	"crypto/tls",
	"crypto/x509",
}

// isCryptoImport reports whether an import path is a cryptographic package, standard library or not: any of
// its path elements starts with "crypto", as in crypto/sha256, golang.org/x/crypto or a third-party cryptolib.
func isCryptoImport(path string) bool {
	return slices.ContainsFunc(strings.Split(path, "/"), func(elem string) bool {
		return strings.HasPrefix(elem, "crypto")
	})
}

func TestProductionCodeUsesNoCustomOrThirdPartyCrypto(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, dir := range []string{"cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			// Test code may use anything; only what ships is checked.
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			for _, imp := range file.Imports {
				importPath, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				if isCryptoImport(importPath) && !slices.Contains(allowedCryptoImports, importPath) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %q, which is not an approved cryptographic package %v", rel, importPath, allowedCryptoImports)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Guards against the walk silently finding nothing, e.g. after the package moves.
	if checked < 50 {
		t.Fatalf("checked only %d files under cmd/ and pkg/, expected the whole tree", checked)
	}
}

func TestIsCryptoImport(t *testing.T) {
	for path, want := range map[string]bool{
		"crypto/sha256":                  true,
		"crypto/md5":                     true,
		"golang.org/x/crypto/chacha20":   true,
		"github.com/example/cryptolib":   true,
		"github.com/example/encrypted":   false,
		"github.com/example/crypto/aes":  true,
		"encoding/json":                  false,
		"k8s.io/apimachinery/pkg/util/x": false,
	} {
		if got := isCryptoImport(path); got != want {
			t.Errorf("isCryptoImport(%q) = %v, want %v", path, got, want)
		}
	}
}
