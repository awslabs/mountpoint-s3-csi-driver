//go:generate mockgen -source=pts.go -destination=./mocks/mock_pts.go -package=mock_system
package system

import (
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	PtmxPathEnv     = "PTMX_PATH"
	DefaultPtmxPath = "/dev/ptmx"
)

// Interface for creating new private terminal session. See man pts(4)
type Pts interface {
	NewPts() (io.ReadCloser, int, error)
}

// Real os implementation of the Pts interface
type OsPts struct{}

func NewOsPts() Pts {
	return &OsPts{}
}

// Create a new pseduo terminal (pts). Returns a ReaderCloser for the master device and a pts number
func (p *OsPts) NewPts() (io.ReadCloser, int, error) {
	ptmxPath := os.Getenv(PtmxPathEnv)
	if ptmxPath == "" {
		ptmxPath = DefaultPtmxPath
	}
	ptsMaster, err := os.Open(ptmxPath)
	if err != nil {
		return nil, 0, fmt.Errorf("Failed to open tty: %w", err)
	}
	success := false
	defer func() {
		if !success {
			ptsMaster.Close()
		}
	}()
	// grantpt ioctl to allow mount-s3 process access to the pts
	var n uintptr // dummy int for ioctl
	if err = unix.IoctlSetInt(int(ptsMaster.Fd()), unix.TIOCGPTN, int(uintptr(unsafe.Pointer(&n)))); err != nil {
		return nil, 0, fmt.Errorf("Failed grantpt: %w", err)
	}
	n = 0
	// unlockpt ioctl
	err = unix.IoctlSetInt(int(ptsMaster.Fd()), unix.TIOCSPTLCK, int(uintptr(unsafe.Pointer(&n))))
	if err != nil {
		return nil, 0, fmt.Errorf("Failed unlockpt: %w", err)
	}
	// ptsname ioctl to get pts path for systemd
	ptsN, err := unix.IoctlGetInt(int(ptsMaster.Fd()), unix.TIOCGPTN)
	if err != nil {
		return nil, 0, fmt.Errorf("Failed to get ptsname: %w", err)
	}

	success = true
	return ptsMaster, ptsN, err
}
