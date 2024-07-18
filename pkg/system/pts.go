//go:generate mockgen -source=pts.go -destination=./mocks/mock_pts.go -package=mock_system
package system

import (
	"io"
)

const (
	PtmxPathEnv     = "PTMX_PATH"
	DefaultPtmxPath = "/dev/ptmx"
)

// Interface for creating new private terminal session. See man pts(4)
type Pts interface {
	NewPts() (io.ReadCloser, int, error)
}
