package util_test

import (
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
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
			wg.Add(1)
			go func() {
				defer wg.Done()

				err := util.ReplaceFile(dest, source, 0644)
				assert.NoError(t, err)
			}()
		}
		wg.Wait()

		expectContentAndPerm(t, dest, content, 0644)
	})
}
