package mounter

import (
	"errors"
	"io"
	"io/fs"
	"os"

	"strings"

	"k8s.io/klog/v2"
)

// consumeErrorFile reads and removes the error file the mounter wrote for a mount. It returns the reason
// and whether a file was read. The mounter bounds the file with its --stderr-capacity. A file with no content still
// counts; one that cannot be opened does not, so a poller tries again. A removal error is
// returned with the reason.
func consumeErrorFile(errFilePath string) (string, bool, error) {
	f, err := os.Open(errFilePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			klog.Errorf("DaemonsetMounter: failed to open error file %s: %v", errFilePath, err)
		}
		return "", false, nil
	}
	content, readErr := io.ReadAll(f)
	f.Close()
	if readErr != nil {
		klog.Errorf("DaemonsetMounter: failed to read error file %s: %v", errFilePath, readErr)
	}
	reason := strings.TrimSpace(string(content))
	if err := os.Remove(errFilePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return reason, true, err
	}
	return reason, true, nil
}
