//go:generate mockgen -source=systemd.go -destination=./mocks/mock_systemd.go -package=mock_system
package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
	"k8s.io/klog/v2"
)

const (
	systemdSocket    = "unix:path=/run/systemd/private"
	signalBufferSize = 4096 // Messages are dropped if the buffer fills, so make it large

	UnitNewMethod           = "org.freedesktop.systemd1.Manager.UnitNew"
	UnitRemovedMethod       = "org.freedesktop.systemd1.Manager.UnitRemoved"
	PropertiesChangedMethod = "org.freedesktop.DBus.Properties.PropertiesChanged"
)

// DbusConn is a wrapper for the dbus.Conn external type
type DbusConn interface {
	Object(dest string, path dbus.ObjectPath) dbus.BusObject
	Signal(ch chan<- *dbus.Signal)
	Close() error
}

// DbusObject is a wrapper for dbus.BusObject external type
type DbusObject interface {
	Go(method string, flags dbus.Flags, ch chan *dbus.Call, args ...any) *dbus.Call
}

// DbusProperty is used for dbus arguments that are arrays of key value pairs
type DbusProperty struct {
	Name  string
	Value any
}

type DbusPropertySet struct {
	Name  string
	Value []DbusProperty
}

// DbusExecStart property for systemd services
type DbusExecStart struct {
	Path             string
	Args             []string
	UncleanIsFailure bool
}

// SystemdOsConnection is a low level api thinly wrapping the systemd dbus calls
// See https://www.freedesktop.org/wiki/Software/systemd/dbus/ for the DBus API
type SystemdOsConnection struct {
	Conn     DbusConn
	Object   DbusObject
	isClosed atomic.Bool
}

// Connect to the systemd dbus socket and return a SystemdOsConnection
func NewSystemdOsConnection() (*SystemdOsConnection, error) {
	conn, err := dbus.Dial(systemdSocket)
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to systemd: %w", err)
	}

	// Use uid 0 (root) to auth
	err = conn.Auth([]dbus.Auth{dbus.AuthExternal("0")})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("Failed to set systemd connection auth: %w", err)
	}

	systemd := conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))

	return &SystemdOsConnection{
		Conn:   conn,
		Object: systemd,
	}, nil
}

func (sc *SystemdOsConnection) Close() error {
	sc.isClosed.Store(true)
	return sc.Conn.Close()
}

func (sc *SystemdOsConnection) IsClosed() bool {
	return sc.isClosed.Load()
}

func (sc *SystemdOsConnection) Signal(ch chan<- *dbus.Signal) {
	sc.Conn.Signal(ch)
}

type Unit struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Followed    string
	Path        dbus.ObjectPath
	JobId       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}

func (sc *SystemdOsConnection) ListUnits(ctx context.Context) ([]*Unit, error) {
	var ret []*Unit
	err := sc.callDbus(ctx, "org.freedesktop.systemd1.Manager.ListUnits", &ret)
	return ret, err
}

func (sc *SystemdOsConnection) StopUnit(ctx context.Context, unitName string) error {
	var job dbus.ObjectPath
	err := sc.callDbus(ctx, "org.freedesktop.systemd1.Manager.StopUnit", &job, unitName, "fail")
	return err
}

func (sc *SystemdOsConnection) StartTransientUnit(ctx context.Context, name string, mode string,
	props []DbusProperty) (dbus.ObjectPath, error) {

	var job dbus.ObjectPath
	err := sc.callDbus(ctx, "org.freedesktop.systemd1.Manager.StartTransientUnit",
		&job, name, mode, props, []DbusPropertySet{})
	return job, err
}

func (sc *SystemdOsConnection) callDbus(ctx context.Context, method string, ret any,
	args ...any) error {

	ch := make(chan *dbus.Call, 1)
	sc.Object.Go(method, 0, ch, args...)

	select {
	case call := <-ch:
		if call.Err != nil {
			if errors.Is(call.Err, dbus.ErrClosed) {
				klog.V(5).Info("Detected that connection was closed")
				sc.isClosed.Store(true)
			}
			return fmt.Errorf("Failed StartTransientUnit with Call.Err: %w", call.Err)
		}
		err := call.Store(ret)
		if err != nil {
			return fmt.Errorf("Failed StartTransientUnit: %w", err)
		}
	case <-ctx.Done():
		return fmt.Errorf("Failed StartTransientUnit, context cancelled")
	}

	return nil
}

type SystemdConnection interface {
	ListUnits(ctx context.Context) ([]*Unit, error)
	StopUnit(ctx context.Context, unitName string) error
	StartTransientUnit(ctx context.Context, name string, mode string,
		props []DbusProperty) (dbus.ObjectPath, error)
	Signal(ch chan<- *dbus.Signal)
	Close() error
	IsClosed() bool
}

type ExecConfig struct {
	Name        string
	Description string
	ExecPath    string
	Args        []string
	Env         []string
}

func (ec *ExecConfig) ToDbus(ptsN int, serviceType string) []DbusProperty {
	execStart := []DbusExecStart{
		{
			Path:             ec.ExecPath,
			Args:             append([]string{ec.ExecPath}, ec.Args...),
			UncleanIsFailure: true,
		},
	}
	properties := []DbusProperty{
		{Name: "Description", Value: ec.Description},
		{Name: "Type", Value: serviceType},
		{Name: "StandardOutput", Value: "tty"},
		{Name: "StandardError", Value: "tty"},
		{Name: "TTYPath", Value: fmt.Sprintf("/dev/pts/%d", ptsN)},
		{Name: "ExecStart", Value: execStart},
	}
	if serviceType == "oneshot" {
		properties = append(properties, DbusProperty{Name: "RemainAfterExit", Value: true})
	}
	if len(ec.Env) != 0 {
		properties = append(properties, DbusProperty{Name: "Environment", Value: ec.Env})
	}
	return properties
}

type UnitProperties struct {
	ActiveState    string
	ExecMainCode   int
	ExecMainStatus int
}

type SystemdSupervisorFactory interface {
	StartSupervisor() (SystemdSupervisor, error)
}

type OsSystemdSupervisorFactory struct{}

func (s OsSystemdSupervisorFactory) StartSupervisor() (SystemdSupervisor, error) {
	conn, err := NewSystemdOsConnection()
	if err != nil {
		return nil, err
	}

	supervisor := NewOsSystemdSupervisor(conn)
	supervisor.Start()
	return supervisor, nil
}

type SystemdSupervisor interface {
	StartService(ctx context.Context, config *ExecConfig) (string, error)
	RunOneshot(ctx context.Context, config *ExecConfig) (string, error)
	IsConnectionClosed() bool
	Stop() error
}

type OsSystemdSupervisor struct {
	conn               SystemdConnection
	serviceWatchers    map[string][]chan<- *UnitProperties
	watchersMutex      sync.Mutex
	dbusServiceNameMap map[string]string
}

func NewOsSystemdSupervisor(conn SystemdConnection) *OsSystemdSupervisor {
	s := &OsSystemdSupervisor{
		conn:               conn,
		serviceWatchers:    map[string][]chan<- *UnitProperties{},
		dbusServiceNameMap: map[string]string{},
	}

	return s
}

func (s *OsSystemdSupervisor) IsConnectionClosed() bool {
	return s.conn.IsClosed()
}

func (s *OsSystemdSupervisor) Start() {
	// ensure signals channel is registered before we return from this call
	signals := s.startListeningSignals()
	// processing goroutine may start after we return, which is fine
	go s.processSignals(signals)
}

func (s *OsSystemdSupervisor) Stop() error {
	// close all watchers to signal [runUnit] that it should return
	s.watchersMutex.Lock()
	defer s.watchersMutex.Unlock()
	for serviceName, chans := range s.serviceWatchers {
		for _, ch := range chans {
			close(ch)
			klog.V(5).Infof("OsSystemdSupervisor closed watcher: serviceName=%s\n", serviceName)
		}
	}
	// close the dbus connection, this also triggers termination of [processSignals] goroutine
	return s.conn.Close()
}

func (s *OsSystemdSupervisor) AddServiceWatcher(serviceName string, ch chan<- *UnitProperties) {
	s.watchersMutex.Lock()
	defer s.watchersMutex.Unlock()
	s.serviceWatchers[serviceName] = append(s.serviceWatchers[serviceName], ch)
}

func (s *OsSystemdSupervisor) RemoveServiceWatcher(serviceName string, ch chan<- *UnitProperties) {
	s.watchersMutex.Lock()
	defer s.watchersMutex.Unlock()
	watchers, ok := s.serviceWatchers[serviceName]
	if !ok {
		return
	}
	for i, w := range watchers {
		if w == ch {
			watchers = append(watchers[:i], watchers[i+1:]...)
			break
		}
	}
}

func (s *OsSystemdSupervisor) startListeningSignals() <-chan *dbus.Signal {
	signals := make(chan *dbus.Signal, signalBufferSize)
	s.conn.Signal(signals)
	return signals
}

func (s *OsSystemdSupervisor) processSignals(signals <-chan *dbus.Signal) {
	// dbus package closes the [signals] channel when the [s.conn] gets closed
	for sig := range signals {
		s.dispatchSignal(sig)
	}
	klog.V(5).Info("Systemd D-Bus signal channel closed — OsSystemdSupervisor stopped processing systemd signals")
	s.conn.Close()
}

func (s *OsSystemdSupervisor) dispatchSignal(signal *dbus.Signal) {
	switch signal.Name {
	case UnitNewMethod, UnitRemovedMethod:
		if len(signal.Body) != 2 {
			return
		}
		unitName, ok := signal.Body[0].(string)
		if !ok {
			return
		}
		dbusAddress, ok := signal.Body[1].(dbus.ObjectPath)
		if !ok {
			return
		}
		s.watchersMutex.Lock()
		defer s.watchersMutex.Unlock()
		if signal.Name == UnitNewMethod {
			if _, ok := s.serviceWatchers[unitName]; ok {
				klog.V(5).Infof("OsSystemdSupervisor %s unit: %s dbusAddress: %v\n", signal.Name, unitName, dbusAddress)
				s.dbusServiceNameMap[string(dbusAddress)] = unitName
			}
		} else {
			watchers, ok := s.serviceWatchers[unitName]
			if ok {
				klog.V(5).Infof("OsSystemdSupervisor %s unit: %s dbusAddress: %v\n", signal.Name, unitName, dbusAddress)
				for _, w := range watchers {
					close(w)
				}

				delete(s.dbusServiceNameMap, string(dbusAddress))
				delete(s.serviceWatchers, unitName)
			}
		}

	case PropertiesChangedMethod:
		serviceName, ok := s.dbusServiceNameMap[string(signal.Path)]
		if !ok {
			return
		}
		updates, ok := signal.Body[1].(map[string]dbus.Variant)
		if !ok {
			return
		}
		s.watchersMutex.Lock()
		defer s.watchersMutex.Unlock()
		if watchers, ok := s.serviceWatchers[serviceName]; ok {
			klog.V(5).Infof("Systemd properties change: %v", updates)
			for _, w := range watchers {

				props := &UnitProperties{}
				if activeState, ok := updates["ActiveState"]; ok {
					if activeStateStr, ok := activeState.Value().(string); ok {
						props.ActiveState = activeStateStr
					}
				}
				if execCode, ok := updates["ExecMainCode"]; ok {
					if execCodeInt, ok := execCode.Value().(int32); ok {
						props.ExecMainCode = int(execCodeInt)
					}
				}
				if execStatus, ok := updates["ExecMainStatus"]; ok {
					if execStatusInt, ok := execStatus.Value().(int32); ok {
						props.ExecMainStatus = int(execStatusInt)
					}
				}
				w <- props
			}
		}
	}
}

func (sd *OsSystemdSupervisor) StartService(ctx context.Context, config *ExecConfig) (string, error) {
	return sd.runUnit(ctx, config, "forking", func(props *UnitProperties) (bool, error) {
		return props.ActiveState == "active", nil
	})
}

func (sd *OsSystemdSupervisor) RunOneshot(ctx context.Context, config *ExecConfig) (string, error) {
	defer sd.conn.StopUnit(ctx, config.Name)

	return sd.runUnit(ctx, config, "oneshot", func(props *UnitProperties) (bool, error) {
		if props.ExecMainCode == 0 {
			return false, nil
		}
		if props.ExecMainStatus != 0 {
			return true, fmt.Errorf("Non zero status code: %d", props.ExecMainStatus)
		}
		return true, nil
	})
}

// runUnit instructs systemd to start a transient unit with the given configuration.
// It then waits for one of the following to occur:
// - The provided doneFunc returns true, based on systemd unit property updates
// - The systemd D-Bus connection is closed (indicated by a nil update)
// - The context is cancelled
//
// While the unit is running, its output is captured from the pseudo-terminal (pts)
// and returned as a flattened string in case of error or completion.
func (sd *OsSystemdSupervisor) runUnit(ctx context.Context, config *ExecConfig, serviceType string,
	doneFunc func(*UnitProperties) (bool, error)) (string, error) {

	// Create pts to get standard output
	pts := NewOsPts()
	ptm, ptsN, err := pts.NewPts()
	if err != nil {
		return "", fmt.Errorf("Failed to create pts: %w", err)
	}
	defer ptm.Close()

	readOutput := func() string {
		buffer := &bytes.Buffer{}
		buffer.ReadFrom(ptm)
		output := buffer.String()
		re := regexp.MustCompile(`\r?\n`)
		return re.ReplaceAllString(output, " ")
	}

	props := config.ToDbus(ptsN, serviceType)

	unitUpdates := make(chan *UnitProperties, 32)
	sd.AddServiceWatcher(config.Name, unitUpdates)
	defer sd.RemoveServiceWatcher(config.Name, unitUpdates)

	job, err := sd.conn.StartTransientUnit(ctx, config.Name, "fail", props)
	if err != nil {
		return "", fmt.Errorf("Failed to start transient systemd service: %w", err)
	}
	klog.V(5).Infof("Started service %s, job: %s\n", config.Name, string(job))

	defer klog.V(5).Infof("Done running a unit: service=%s job=%s\n", config.Name, string(job))
	started := false
	for !started {
		select {
		case u := <-unitUpdates:
			klog.V(5).Infof("Signal received or the channel was closed: service=%s job=%s update=%v\n", config.Name, string(job), u)
			if u == nil {
				return readOutput(), fmt.Errorf("Failed to start service")
			}
			started, err = doneFunc(u)
			if err != nil {
				return readOutput(), err
			}

		case <-ctx.Done():
			return readOutput(), fmt.Errorf("Failed to start systemd unit, context cancelled")
		}
	}
	return readOutput(), nil
}
