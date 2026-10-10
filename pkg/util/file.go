package util

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/renameio"
)

// ReplaceFile safely replaces a file with a new file by copying to a temporary location first
// then renaming.
func ReplaceFile(destPath string, sourcePath string, perm fs.FileMode) error {
	destDir, destBase := filepath.Dir(destPath), filepath.Base(destPath)

	destFile, err := os.CreateTemp(destDir, destBase+".tmp-*")
	if err != nil {
		return fmt.Errorf("replace-file: failed to create a temporary file at the destination directory %q: %w", destPath, err)
	}
	defer destFile.Close()

	// `os.CreateTemp` always creates files with 0600 permission, we need to change it before we write to it.
	err = destFile.Chmod(perm)
	if err != nil {
		return fmt.Errorf("replace-file: failed to change temporary file %q's permissions: %w", destFile.Name(), err)
	}

	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	buf := make([]byte, 64*1024)
	_, err = io.CopyBuffer(destFile, sourceFile, buf)
	if err != nil {
		return err
	}

	err = os.Rename(destFile.Name(), destPath)
	if err != nil {
		return fmt.Errorf("replace-file: failed to rename file %s: %w", destPath, err)
	}

	return nil
}

// WriteFileOwned atomically writes `data` to `path` with mode `perm`, owned by `uid` unless that is
// zero. Mode and owner are applied before any content is written, and `path` only resolves to the
// file once it is complete, so the content is never exposed under the wrong mode or owner.
func WriteFileOwned(path string, data []byte, perm fs.FileMode, uid uint32) error {
	// In the destination directory rather than a shared temp dir, since the content is sensitive.
	pending, err := renameio.TempFile(filepath.Dir(path), path)
	if err != nil {
		return fmt.Errorf("write-file-owned: failed to create a temporary file for %q: %w", path, err)
	}
	defer pending.Cleanup()

	if err := pending.Chmod(perm); err != nil {
		return fmt.Errorf("write-file-owned: failed to set the mode of %q: %w", path, err)
	}
	if uid != 0 {
		if err := pending.Chown(int(uid), int(uid)); err != nil {
			return fmt.Errorf("write-file-owned: failed to set the owner of %q to %d: %w", path, uid, err)
		}
	}
	if _, err := pending.Write(data); err != nil {
		return fmt.Errorf("write-file-owned: failed to write %q: %w", path, err)
	}
	return pending.CloseAtomicallyReplace()
}
