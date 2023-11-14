package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/opencontainers/selinux/go-selinux"
)

const (
	binDirKey  = "MOUNTPOINT_BIN_DIR"
	installDirKey = "MOUNTPOINT_INSTALL_DIR"
	// SELinux labels to set on the installed binaries. The installer will set the writable
	// label on the old binaries, move the new versions on top of them, then set the executable
	// label on the new binaries.
	seLinuxWritableKey   = "SE_LINUX_WRITABLE_LABEL"
	seLinuxExecutableKey = "SE_LINUX_EXECUTABLE_LABEL"
)

// Copies files from a directory to a new directory and sets SELinux labels on them, basically:
// $ cp $SOURCE_DIR/* $DESTDIR/ && chcon -R $SE_LINUX_LABEL $MOUNTPOINT_INSTALL_DIR/*
// Written as a go program to avoid bash, cp, and selinux dependencies in the container.
// Does not handle nested directories or anything beyond the simple install.
func main() {
	binDir := os.Getenv(binDirKey)
	installDir := os.Getenv(installDirKey)
	if binDir == "" || installDir == "" {
		log.Fatalf("Missing environment variable, %s and %s required", binDirKey, installDirKey)
	}

	seLinuxWritableLabel := os.Getenv(seLinuxWritableKey)
	seLinuxExecutableLabel := os.Getenv(seLinuxExecutableKey)

	err := installFiles(binDir, installDir, seLinuxWritableLabel, seLinuxExecutableLabel)
	if err != nil {
		log.Fatalf("Failed install binDir %s installDir %s: %v", binDir, installDir, err)
	}
}

func installFiles(
	binDir string, installDir string, seLinuxWritableLabel string,
	seLinuxExecutableLabel string) error {

	sd, err := os.Open(binDir)
	if err != nil {
		return fmt.Errorf("Failed to open source directory: %w", err)
	}
	defer sd.Close()

	entries, err := sd.Readdirnames(0)
	if err != nil {
		return fmt.Errorf("Failed to read source directory: %w", err)
	}

	seLinuxEnabled := seLinuxWritableLabel != "" && seLinuxExecutableLabel != ""
	for _, name := range entries {
		log.Printf("Copying file %s\n", name)
		destFile := filepath.Join(installDir, name)
		destFileTmp := destFile + ".tmp"

		// First copy to a temporary location then rename to handle replacing running binaries
		err = copyFile(destFileTmp, filepath.Join(binDir, name))
		if err != nil {
			return fmt.Errorf("Failed to copy file %s: %w", name, err)
		}

		if seLinuxEnabled {
			err = selinux.SetFileLabel(destFile, seLinuxWritableLabel)
			if err != nil {
				log.Printf("Ignoring error resetting SELinux label on %s: %v", name, err)
			}
			n := name // Copy so we don't capture the loop var
			defer func() {
				// Set these in a defer to ensure the binaries remain executable
				log.Printf("Setting label %s -- %s", seLinuxExecutableLabel, n)
				err = selinux.SetFileLabel(destFile, seLinuxExecutableLabel)
				if err != nil {
					log.Printf("Failed to set SELinux label on %s: %v", n, err)
				}
			}()
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
