package util

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

// ReplaceFile safely replaces a file with a new file by copying to a temporary location first
// then renaming.
func ReplaceFile(destPath string, sourcePath string, perm fs.FileMode) error {
	tmpDest := destPath + ".tmp"

	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.OpenFile(tmpDest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer destFile.Close()

	buf := make([]byte, 64*1024)
	_, err = io.CopyBuffer(destFile, sourceFile, buf)
	if err != nil {
		return err
	}

	err = os.Rename(tmpDest, destPath)
	if err != nil {
		return fmt.Errorf("Failed to rename file %s: %w", destPath, err)
	}

	return nil
}
