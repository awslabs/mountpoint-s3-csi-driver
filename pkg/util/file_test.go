package util_test

import (
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
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
		assert.NoError(t, os.WriteFile(path, []byte("stale"), 0600))
		assert.NoError(t, util.WriteFileOwned(path, []byte("fresh"), 0400, 0))

		got, err := os.ReadFile(path)
		assert.NoError(t, err)
		assert.Equals(t, "fresh", string(got))

		// No temporary file is left alongside it.
		entries, err := os.ReadDir(filepath.Dir(path))
		assert.NoError(t, err)
		assert.Equals(t, 1, len(entries))
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
