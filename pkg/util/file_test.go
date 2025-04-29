package util_test

import (
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/util"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
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

	// Error path tests
	t.Run("Error opening non-existent source file", func(t *testing.T) {
		basedir := t.TempDir()
		nonExistentSource := filepath.Join(basedir, "non-existent-source")
		dest := filepath.Join(basedir, "dest")

		err := util.ReplaceFile(dest, nonExistentSource, 0644)

		// The error should not be nil
		if err == nil {
			t.Fatal("Expected error when opening non-existent source file, got nil")
		}

		// The error should contain "no such file"
		if !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("Expected error to contain 'no such file', got: %v", err)
		}
	})

	t.Run("Error with non-readable source file", func(t *testing.T) {
		// Skip on non-Unix systems as permissions work differently
		if os.PathSeparator != '/' {
			t.Skip("Skipping on non-Unix systems")
		}

		basedir := t.TempDir()
		source := filepath.Join(basedir, "non-readable-source")

		// Create the file first
		err := os.WriteFile(source, []byte("test content"), 0600)
		if err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		// Then remove read permissions
		err = os.Chmod(source, 0)
		if err != nil {
			t.Fatalf("Failed to change permissions: %v", err)
		}

		dest := filepath.Join(basedir, "dest")

		// Try to copy from a non-readable source
		err = util.ReplaceFile(dest, source, 0644)

		// The error should not be nil
		if err == nil {
			t.Fatal("Expected error when reading from non-readable source file, got nil")
		}

		// On some systems, we may get 'permission denied'
		if !(strings.Contains(err.Error(), "permission denied") ||
			strings.Contains(err.Error(), "no such file")) {
			t.Fatalf("Expected permission error, got: %v", err)
		}
	})

	t.Run("Error during rename", func(t *testing.T) {
		// Skip on non-Unix systems as permissions work differently
		if os.PathSeparator != '/' {
			t.Skip("Skipping on non-Unix systems")
		}

		basedir := t.TempDir()
		source := filepath.Join(basedir, "source")
		err := os.WriteFile(source, []byte("test content"), 0600)
		if err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		// Create a read-only directory
		readOnlyDir := filepath.Join(basedir, "readonly")
		err = os.Mkdir(readOnlyDir, 0500) // read + execute, no write
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		dest := filepath.Join(readOnlyDir, "dest")

		// Try to replace file in read-only directory
		err = util.ReplaceFile(dest, source, 0644)

		// The error should not be nil
		if err == nil {
			t.Fatal("Expected error when operating in read-only directory, got nil")
		}

		// The error should contain "failed to create a temporary file"
		if !strings.Contains(err.Error(), "failed to create a temporary file") {
			t.Fatalf("Expected error to contain 'failed to create a temporary file', got: %v", err)
		}
	})
}
