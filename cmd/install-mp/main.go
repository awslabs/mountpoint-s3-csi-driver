package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

const (
	binDirKey     = "MOUNTPOINT_BIN_DIR"
	installDirKey = "MOUNTPOINT_INSTALL_DIR"
)

// Copies files from a directory to a new directory
// $ cp $SOURCE_DIR/* $DESTDIR/
// Written as a go program to avoid bash and cp dependencies in the container.
// Does not handle nested directories or anything beyond the simple install.
func main() {
	binDir := os.Getenv(binDirKey)
	installDir := os.Getenv(installDirKey)
	if binDir == "" || installDir == "" {
		log.Fatalf("Missing environment variable, %s and %s required", binDirKey, installDirKey)
	}

	err := installFiles(binDir, installDir)
	if err != nil {
		log.Fatalf("Failed install binDir %s installDir %s: %v", binDir, installDir, err)
	}
}

func installFiles(binDir string, installDir string) error {

	sd, err := os.Open(binDir)
	if err != nil {
		return fmt.Errorf("Failed to open source directory: %w", err)
	}
	defer sd.Close()

	entries, err := sd.Readdirnames(0)
	if err != nil {
		return fmt.Errorf("Failed to read source directory: %w", err)
	}

	for _, name := range entries {
		log.Printf("Copying file %s\n", name)
		destFile := filepath.Join(installDir, name)
		destFileTmp := destFile + ".tmp"

		// First copy to a temporary location then rename to handle replacing running binaries
		err = copyFile(destFileTmp, filepath.Join(binDir, name))
		if err != nil {
			return fmt.Errorf("Failed to copy file %s: %w", name, err)
		}

		err = os.Rename(destFileTmp, destFile)
		if err != nil {
			return fmt.Errorf("Failed to rename file %s: %w", name, err)
		}
	}
	return nil
}

func copyFile(destPath string, sourcePath string) error {
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer destFile.Close()

	buf := make([]byte, 64*1024)

	_, err = io.CopyBuffer(destFile, sourceFile, buf)
	if err != nil {
		return err
	}

	return nil
}
