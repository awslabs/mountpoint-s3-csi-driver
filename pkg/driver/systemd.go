//go:generate mockgen -source=systemd.go -destination=./mocks/mock_systemd.go -package=mock_driver
package driver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// Interface to wrap the external go-systemd dbus connection
type SystemdConnection interface {
	Close()
	SubscribeUnitsCustom(interval time.Duration, buffer int,
		isChanged func(*systemd.UnitStatus, *systemd.UnitStatus) bool,
		filterUnit func(string) bool) (<-chan map[string]*systemd.UnitStatus, <-chan error)
	StartTransientUnitContext(ctx context.Context, name string, mode string,
		properties []systemd.Property, ch chan<- string) (int, error)
	ResetFailedUnitContext(ctx context.Context, name string) error
}

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

type SystemdRunner struct {
	Connector SystemdConnector
	Pts       Pts
}

func NewSystemdRunner() SystemdRunner {
	return SystemdRunner{
		Connector: NewOsSystemd(),
		Pts:       &OsPts{},
	}
}

func (sr *SystemdRunner) Run(ctx context.Context, cmd string, args []string) (string, error) {
	systemdConn, err := sr.Connector.Connect(ctx)
	if err != nil {
		// TODO fallback to launching in container if systemd doesn't exist on host
		return "", fmt.Errorf("Failed to connect to systemd: %w", err)
	}
	defer systemdConn.Close()

	// Create a new pts
	ptsMaster, ptsN, err := sr.Pts.NewPts()
	if err != nil {
		return "", fmt.Errorf("Failed to connect to systemd: %w", err)
	}
	defer ptsMaster.Close()

	// Use a tty to capture stdout/stderr. Older versions of systemd do not support options like named pipes
	props := []systemd.Property{
		systemd.PropDescription("Mountpoint for S3 CSI driver FUSE daemon"),
		systemd.PropType("forking"),
		{Name: "StandardOutput", Value: dbus.MakeVariant("tty")},
		{Name: "StandardError", Value: dbus.MakeVariant("tty")},
		{Name: "TTYPath", Value: dbus.MakeVariant(fmt.Sprintf("/dev/pts/%d", ptsN))},
		systemd.PropExecStart(append([]string{cmd}, args...), true),
	}

	// Unit names must be unique in systemd, so include a uuid
	serviceName := filepath.Base(cmd) + "." + uuid.New().String() + ".service"

	// Subscribe to status updates
	isChanged := func(l *systemd.UnitStatus, r *systemd.UnitStatus) bool {
		return l.ActiveState != r.ActiveState
	}
	filter := func(name string) bool { return !strings.Contains(name, serviceName) }
	updates, errChan := systemdConn.SubscribeUnitsCustom(50*time.Millisecond, 10, isChanged, filter)

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
	resp := <-respChan
	switch resp {
	case "done":
		// Success, don't return
	case "cancelled", "timeout", "failed", "dependency", "skipped":
		systemdConn.ResetFailedUnitContext(ctx, serviceName)
		return "", fmt.Errorf("Failed to create systemd service %s, resp: %s", serviceName, resp)
	default:
		systemdConn.ResetFailedUnitContext(ctx, serviceName)
		return "", fmt.Errorf("Unknown status starting systemd service %s, resp: %s", serviceName, resp)
	}

	starting := true
	// Wait 1 second for the status to reach active. The status can briefly go inactive before becoming active
	startTimeout := time.After(time.Second)
	for starting {
		select {
		case update := <-updates:
			for k, v := range update {
				klog.V(5).Infof("Systemd service update [%s]: %v", k, v)
				if k == serviceName {
					if v == nil {
						mountOutput := readOutput()
						return mountOutput, fmt.Errorf("%s failed to launch, mount-s3 output: %s",
							serviceName, mountOutput)
					} else if v.ActiveState == "active" {
						starting = false
					}
				}
			}
		case <-startTimeout:
			return "", fmt.Errorf("%s failed to launch, mount-s3 output: %s",
				serviceName, readOutput())
		case err = <-errChan:
			return "", fmt.Errorf("Failed to start systemd service %s err: %w, mount-s3 output: %s",
				serviceName, err, readOutput())
		}
	}

	return readOutput(), nil
}

// Interface for creating new private terminal session. See pts(4)
type Pts interface {
	NewPts() (io.ReadCloser, int, error)
}

type OsPts struct{}

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
