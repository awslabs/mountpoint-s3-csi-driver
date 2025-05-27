package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
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

		// First copy to a temporary location then rename to handle replacing running binaries
		err = util.ReplaceFile(destFile, filepath.Join(binDir, name), 0755)
		if err != nil {
			return fmt.Errorf("Failed to copy file %s: %w", name, err)
		}

	}
	return nil
}
