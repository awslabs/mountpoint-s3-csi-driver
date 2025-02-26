package util

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

const (
	FileModeUserReadWrite      = fs.FileMode(0600) // User: read/write, Group: none, Others: none
	FileModeUserGroupReadWrite = fs.FileMode(0640) // User: read/write, Group: read-only, Others: none
	FileModeUserFull           = fs.FileMode(0700) // User: full access, Group: none, Others: none
	FileModeUserFullGroupRead  = fs.FileMode(0750) // User: full access, Group: read/execute only, Others: none
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

// FileGroupID returns gid of the file or directory.
func FileGroupID(path string) (gid uint32, err error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, err
	}

	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("Failed to retrieve stat information for path: %s", path)
	}

	return stat.Gid, nil
}
