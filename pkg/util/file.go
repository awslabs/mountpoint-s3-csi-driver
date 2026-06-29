package util

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// FileOwnership specifies UID/GID for file ownership applied atomically before rename.
type FileOwnership struct {
	UID int
	GID int
}

// ReplaceFile safely replaces a file with a new file by copying to a temporary location first
// then renaming. If owner is non-nil, the file is chowned before rename.
func ReplaceFile(destPath string, sourcePath string, perm fs.FileMode, owner *FileOwnership) error {
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

	if owner != nil {
		if err := syscall.Fchown(int(destFile.Fd()), owner.UID, owner.GID); err != nil {
			os.Remove(destFile.Name())
			return fmt.Errorf("replace-file: failed to chown temporary file %q: %w", destFile.Name(), err)
		}
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
