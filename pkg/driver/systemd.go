//go:generate mockgen -source=systemd.go -destination=./mocks/mock_systemd.go -package=mock_driver
package driver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// Interface to wrap the external go-systemd dbus connection
// https://pkg.go.dev/github.com/coreos/go-systemd/dbus
type SystemdConnection interface {
	Close()
	ListUnitsContext(ctx context.Context) ([]systemd.UnitStatus, error)
	StartTransientUnitContext(ctx context.Context, name string, mode string,
		properties []systemd.Property, ch chan<- string) (int, error)
	ResetFailedUnitContext(ctx context.Context, name string) error
}

// Factory interface for connections, needed for mocking
type SystemdConnector interface {
	Connect(ctx context.Context) (SystemdConnection, error)
}

type osSystemdConnector struct{}

func (o *osSystemdConnector) Connect(ctx context.Context) (SystemdConnection, error) {
	return systemd.NewSystemConnectionContext(ctx)
}

func NewOsSystemd() SystemdConnector {
	return &osSystemdConnector{}
}

func ConnectOsSystemd(ctx context.Context) (SystemdConnection, error) {
	sd := NewOsSystemd()
	return sd.Connect(ctx)
}

type SystemdRunner struct {
	Connector SystemdConnector
	Pts       Pts
}

// SystemdRunner that talks to the real system dbus
func NewSystemdRunner() SystemdRunner {
	return SystemdRunner{
		Connector: NewOsSystemd(),
		Pts:       &OsPts{},
	}
}

// Run a given command in a transient systemd service. Will wait for the service to become active
func (sr *SystemdRunner) Run(ctx context.Context, cmd string, serviceTag string, env []string, args []string) (string, error) {
	systemdConn, err := sr.Connector.Connect(ctx)
	if err != nil {
		return "", fmt.Errorf("Failed to connect to systemd: %w", err)
	}
	defer systemdConn.Close()

	// Create a new pts
	ptsMaster, ptsN, err := sr.Pts.NewPts()
	if err != nil {
		return "", fmt.Errorf("Failed to connect to systemd: %w", err)
	}
	defer ptsMaster.Close()

	props := []systemd.Property{
		systemd.PropDescription("Mountpoint for S3 CSI driver FUSE daemon"),
		systemd.PropType("forking"),
		// Use a tty to capture stdout/stderr. Older versions of systemd do not support options like named pipes
		{Name: "StandardOutput", Value: dbus.MakeVariant("tty")},
		{Name: "StandardError", Value: dbus.MakeVariant("tty")},
		{Name: "TTYPath", Value: dbus.MakeVariant(fmt.Sprintf("/dev/pts/%d", ptsN))},
		systemd.PropExecStart(append([]string{cmd}, args...), true),
	}
	if len(env) > 0 {
		props = append(props, systemd.Property{Name: "Environment", Value: dbus.MakeVariant(env)})
	}

	// Unit names must be unique in systemd, so include a tag
	serviceName := filepath.Base(cmd) + "-" + serviceTag + ".service"

	respChan := make(chan string, 1)
	defer close(respChan)
	_, err = systemdConn.StartTransientUnitContext(ctx, serviceName, "fail", props, respChan)
	if err != nil {
		return "", fmt.Errorf("Failed to start systemd unit on host: %w", err)
	}

	readOutput := func() string {
		buf := new(bytes.Buffer)
		buf.ReadFrom(ptsMaster) // ignore error
		return buf.String()
	}

	// Wait for systemd dbus response
	select {
	case resp := <-respChan:
		switch resp {
		case "done":
			// Success, continue
		case "cancelled", "timeout", "failed", "dependency", "skipped":
			systemdConn.ResetFailedUnitContext(ctx, serviceName)
			return readOutput(), fmt.Errorf("Failed to create systemd service %s, resp: %s", serviceName, resp)
		default:
			systemdConn.ResetFailedUnitContext(ctx, serviceName)
			return readOutput(), fmt.Errorf("Unknown status starting systemd service %s, resp: %s", serviceName, resp)
		}
	case <-ctx.Done():
		return readOutput(), fmt.Errorf("Context cancelled starting systemd service %s", serviceName)
	}

	// Poll the service status in systemd until error or cancellation
	for {
		pollTimer := time.After(50 * time.Millisecond)
		statuses, err := systemdConn.ListUnitsContext(ctx)
		if err != nil {
			return readOutput(), fmt.Errorf("Failed to read systemd status: %w", err)
		}

		unitFound := false
		for _, status := range statuses {
			if status.Name == serviceName {
				if status.ActiveState == "active" {
					return readOutput(), nil
				}
				unitFound = true
			}
		}
		if !unitFound {
			return readOutput(), fmt.Errorf("Failed to launch systemd service %s", serviceName)
		}
		select {
		case <-pollTimer:
			// keep polling
		case <-ctx.Done():
			return readOutput(), fmt.Errorf("Context cancelled launching service %s",
				serviceName)
		}
	}
}

// Interface for creating new private terminal session. See man pts(4)
type Pts interface {
	NewPts() (io.ReadCloser, int, error)
}

// Real os implementation of the Pts interface
type OsPts struct{}

// Create a new pseduo terminal slave (pts). Returns a ReaderCloser for the master device and a pts number
func (p *OsPts) NewPts() (io.ReadCloser, int, error) {
	ptsMaster, err := os.Open("/hostdev/ptmx")
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
