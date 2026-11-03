package mounter

import (
	"errors"
	"io"
	"io/fs"
	"os"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/errorfile"
)

// maxErrorFileReadBytes bounds how much of an error file is read; the mounter writes at most errorfile.MaxBytes.
const maxErrorFileReadBytes = 4096

// consumeErrorFile reads and removes the error file the mounter wrote for a mount. It returns the reason,
// formatted again in case of an older mounter, and whether a file was read. A file whose content formats
// to nothing still counts; one that cannot be opened does not, so a poller tries again. A removal error is
// returned with the reason.
func consumeErrorFile(errFilePath string) (string, bool, error) {
	f, err := os.Open(errFilePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			klog.Errorf("DaemonsetMounter: failed to open error file %s: %v", errFilePath, err)
		}
		return "", false, nil
	}
	content, readErr := io.ReadAll(io.LimitReader(f, maxErrorFileReadBytes))
	f.Close()
	if readErr != nil {
		klog.Errorf("DaemonsetMounter: failed to read error file %s: %v", errFilePath, readErr)
	}
	reason := errorfile.Format(string(content))
	if err := os.Remove(errFilePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return reason, true, err
	}
	return reason, true, nil
}
