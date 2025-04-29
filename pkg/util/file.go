package util

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
