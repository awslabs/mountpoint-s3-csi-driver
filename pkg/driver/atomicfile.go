package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Copied from Tailscale, https://github.com/tailscale/tailscale/blob/main/atomicfile/atomicfile.go

// WriteFileAtomic writes data to filename+some suffix, then renames it into filename.
// The perm argument is ignored on Windows. If the target filename already
// exists but is not a regular file, WriteFile returns an error.
func WriteFileAtomic(filename string, data []byte, perm os.FileMode) (err error) {
	fi, err := os.Stat(filename)
	if err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s already exists and is not a regular file", filename)
	}
	f, err := os.CreateTemp(filepath.Dir(filename), filepath.Base(filename)+".tmp")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := f.Chmod(perm); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filename)
}
