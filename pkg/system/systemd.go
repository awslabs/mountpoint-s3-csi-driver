//go:generate mockgen -source=systemd.go -destination=./mocks/mock_systemd.go -package=mock_system
package system

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sync"

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
	isClosed bool
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
		Conn:     conn,
		Object:   systemd,
		isClosed: false,
	}, nil
}

func (sc *SystemdOsConnection) Close() error {
	// TODO: set isClosed flag?
	return sc.Conn.Close()
}

func (sc *SystemdOsConnection) IsClosed() bool {
	return sc.isClosed
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
			if call.Err == dbus.ErrClosed {
				klog.V(4).ErrorS(call.Err, "Detected that connection was closed")
				sc.isClosed = true
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

type SystemdSupervisor struct {
	conn               SystemdConnection
	conn_mu            sync.Mutex // protects connection, since it may be re-created
	pts                Pts
	serviceWatchers    map[string][]chan<- *UnitProperties
	watchersMutex      sync.Mutex
	dbusServiceNameMap map[string]string
}

func StartOsSystemdSupervisor() (*SystemdSupervisor, error) {
	conn, err := NewSystemdOsConnection()
	if err != nil {
		return nil, err
	}

	supervisor := NewSystemdSupervisor(conn, NewOsPts())
	supervisor.Start()
	return supervisor, nil
}

func NewSystemdSupervisor(conn SystemdConnection, pts Pts) *SystemdSupervisor {

	s := &SystemdSupervisor{
		conn:               conn,
		pts:                pts,
		serviceWatchers:    map[string][]chan<- *UnitProperties{},
		dbusServiceNameMap: map[string]string{},
	}

	return s
}

func (s *SystemdSupervisor) Start() {
	go s.pollSignals()
}

// Recreate the connection to systemd and start a new pollSignals goroutine
func (s *SystemdSupervisor) Restart() error {
	conn, err := NewSystemdOsConnection()
	if err != nil {
		return err
	}
	s.conn_mu.Lock() // acquire the lock to replace the connection
	s.conn = conn
	s.conn_mu.Unlock()
	go s.pollSignals() // TODO: this may start later than we try creating a unit
	return nil
}

func (s *SystemdSupervisor) Stop() error {
	return s.conn.Close()
}

func (s *SystemdSupervisor) AddServiceWatcher(serviceName string, ch chan<- *UnitProperties) {
	s.watchersMutex.Lock()
	defer s.watchersMutex.Unlock()
	s.serviceWatchers[serviceName] = append(s.serviceWatchers[serviceName], ch)
}

func (s *SystemdSupervisor) RemoveServiceWatcher(serviceName string, ch chan<- *UnitProperties) {
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

func (s *SystemdSupervisor) pollSignals() {
	signals := make(chan *dbus.Signal, signalBufferSize)
	s.conn_mu.Lock()
	s.conn.Signal(signals)
	s.conn_mu.Unlock()

	for sig := range signals {
		s.dispatchSignal(sig)
	}

	klog.V(4).ErrorS(nil, "pollSignals finished") // goroutine terminates when underlying connection gets closed
	// todo: detect that connection is closed here?
}

func (s *SystemdSupervisor) dispatchSignal(signal *dbus.Signal) {
	klog.V(5).Infof("SystemdSupervisor dispatch signal: %v", signal)

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
		klog.V(5).Infof("SystemdSupervisor %s unit: %s dbusAddress: %v", signal.Name, unitName, dbusAddress)
		s.watchersMutex.Lock()
		defer s.watchersMutex.Unlock()
		if signal.Name == UnitNewMethod {
			if _, ok := s.serviceWatchers[unitName]; ok {
				s.dbusServiceNameMap[string(dbusAddress)] = unitName
			}
		} else {
			watchers, ok := s.serviceWatchers[unitName]
			if ok {
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

func (sd *SystemdSupervisor) StartService(ctx context.Context, config *ExecConfig) (string, error) {
	return sd.runUnit(ctx, config, "forking", func(props *UnitProperties) (bool, error) {
		return props.ActiveState == "active", nil
	})
}

func (sd *SystemdSupervisor) RunOneshot(ctx context.Context, config *ExecConfig) (string, error) {
	defer func() {
		sd.conn_mu.Lock()
		sd.conn.StopUnit(ctx, config.Name)
		sd.conn_mu.Unlock()
	}()

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

func (sd *SystemdSupervisor) runUnit(ctx context.Context, config *ExecConfig, serviceType string,
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

	// TODO: refactor this locking
	sd.conn_mu.Lock()
	is_closed := sd.conn.IsClosed()
	sd.conn_mu.Unlock()
	if is_closed {
		klog.V(4).ErrorS(nil, "Systemd connection was closed, re-create it")
		err := sd.Restart()
		if err != nil {
			return "", fmt.Errorf("Failed to re-create connection to systemd: %w", err)
		}
	}
	sd.conn_mu.Lock()
	job, err := sd.conn.StartTransientUnit(ctx, config.Name, "fail", props)
	sd.conn_mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("Failed to start transient systemd service: %w", err)
	}
	klog.V(5).Infof("Started service %s, job: %s\n", config.Name, string(job))

	started := false
	for !started {
		select {
		case u := <-unitUpdates:
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
