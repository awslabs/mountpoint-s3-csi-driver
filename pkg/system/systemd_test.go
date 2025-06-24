package system_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/system"
	mock_system "github.com/awslabs/mountpoint-s3-csi-driver/pkg/system/mocks"
	"github.com/godbus/dbus/v5"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
)

func isRoot() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	return u.Uid == "0"
}

func TestSystemdConnectionCloseSignal(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockDbusConn := mock_system.NewMockDbusConn(mockCtl)
	mockObject := mock_system.NewMockDbusObject(mockCtl)
	conn := &system.SystemdOsConnection{
		Conn:   mockDbusConn,
		Object: mockObject,
	}
	mockDbusConn.EXPECT().Close()
	defer conn.Close()

	signalChan := make(chan *dbus.Signal, 2)
	mockDbusConn.EXPECT().Signal(gomock.Eq(signalChan))
	conn.Signal(signalChan)
}

func TestSystemdConnection(t *testing.T) {
	ctx := context.Background()
	mockCtl := gomock.NewController(t)
	mockDbusConn := mock_system.NewMockDbusConn(mockCtl)
	mockObject := mock_system.NewMockDbusObject(mockCtl)
	conn := &system.SystemdOsConnection{
		Conn:   mockDbusConn,
		Object: mockObject,
	}
	mockDbusConn.EXPECT().Close()
	defer conn.Close()

	signalChan := make(chan *dbus.Signal, 2)
	mockDbusConn.EXPECT().Signal(gomock.Eq(signalChan))
	conn.Signal(signalChan)

	testCases := []struct {
		name     string
		testFunc func(*testing.T)
	}{
		{
			name: "success lists units",
			testFunc: func(t *testing.T) {
				testUnits := []system.Unit{
					{Name: "test-unit-1.service"},
				}
				mockObject.EXPECT().Go(gomock.Eq("org.freedesktop.systemd1.Manager.ListUnits"),
					gomock.Any(), gomock.Any()).Do(
					func(method string, flags dbus.Flags, ch chan *dbus.Call, args ...any) {
						go func() {
							ch <- &dbus.Call{
								Err:  nil,
								Body: []any{testUnits},
							}
						}()
					})
				units, err := conn.ListUnits(ctx)
				if err != nil {
					t.Fatalf("Failed ListUnits: %v", err)
				}
				for i := range testUnits {
					if units[i].Name != testUnits[i].Name {
						t.Fatalf("Expected unit name %s, got %s", testUnits[i].Name, units[i].Name)
					}
				}

			},
		},
		{
			name: "success stop unit",
			testFunc: func(t *testing.T) {
				unitName := "test-unit-1.service"
				mockObject.EXPECT().Go(gomock.Eq("org.freedesktop.systemd1.Manager.StopUnit"),
					gomock.Any(), gomock.Any(), gomock.Eq(unitName), gomock.Eq("fail")).Do(
					func(method string, flags dbus.Flags, ch chan *dbus.Call, args ...any) {
						go func() {
							ch <- &dbus.Call{
								Err:  nil,
								Body: []any{unitName},
							}
						}()
					})
				err := conn.StopUnit(ctx, unitName)
				if err != nil {
					t.Fatalf("Failed StopUnit: %v", err)
				}
			},
		},
		{
			name: "failure lists units call error",
			testFunc: func(t *testing.T) {
				mockObject.EXPECT().Go(gomock.Eq("org.freedesktop.systemd1.Manager.ListUnits"),
					gomock.Any(), gomock.Any()).Do(
					func(method string, flags dbus.Flags, ch chan *dbus.Call, args ...any) {
						go func() {
							ch <- &dbus.Call{
								Err: errors.New("ListUnits test error"),
							}
						}()
					})
				_, err := conn.ListUnits(ctx)
				if err == nil {
					t.Fatalf("Expected error, got nil")
				}

			},
		},
		{
			name: "failure lists units bad data",
			testFunc: func(t *testing.T) {
				mockObject.EXPECT().Go(gomock.Eq("org.freedesktop.systemd1.Manager.ListUnits"),
					gomock.Any(), gomock.Any()).Do(
					func(method string, flags dbus.Flags, ch chan *dbus.Call, args ...any) {
						go func() {
							ch <- &dbus.Call{
								Body: []any{1, 2, 3},
							}
						}()
					})
				_, err := conn.ListUnits(ctx)
				if err == nil {
					t.Fatalf("Expected error, got nil")
				}

			},
		},
		{
			name: "failure lists units context cancelled",
			testFunc: func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				defer func() { ctx = context.Background() }()
				testUnits := []system.Unit{
					{Name: "test-unit-1.service"},
				}
				mockObject.EXPECT().Go(gomock.Eq("org.freedesktop.systemd1.Manager.ListUnits"),
					gomock.Any(), gomock.Any()).Do(
					func(method string, flags dbus.Flags, ch chan *dbus.Call, args ...any) {
						go func() {
							ch <- &dbus.Call{
								Body: []any{testUnits},
							}
						}()
					})
				if _, err := conn.ListUnits(ctx); err == nil {
					t.Fatalf("Expected error, got nil")
				}
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, testCase.testFunc)
	}
}

func TestSystemdOsConnection(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	systemd, err := system.NewSystemdOsConnection()
	if err != nil {
		t.Fatalf("Failed to connect to systemd: %v", err)
	}
	err = systemd.Close()
	if err != nil {
		t.Fatalf("Failed to close systemd connection: %v", err)
	}
}

func TestSystemdOsListUnits(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	systemd, err := system.NewSystemdOsConnection()
	if err != nil {
		t.Fatal(err)
	}
	defer systemd.Close()

	ctx := context.Background()
	units, err := systemd.ListUnits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) == 0 {
		t.Fatal("Expected at least one unit")
	}
}

func TestSystemdOsListUnitsCancel(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	systemd, err := system.NewSystemdOsConnection()
	if err != nil {
		t.Fatal(err)
	}
	defer systemd.Close()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = systemd.ListUnits(canceledCtx)
	if err == nil {
		t.Fatal("Expected error from ListUnits with cancelled context")
	}
}

func TestSystemdSupervisorWatchers(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockConn := mock_system.NewMockSystemdConnection(mockCtl)
	serviceName := "test.service"
	servicePath := "test_service"

	mockConn.EXPECT().Close()
	mockConn.EXPECT().Signal(gomock.Any()).Do(func(ch chan<- *dbus.Signal) {
		go func() {
			ch <- &dbus.Signal{
				Name: system.UnitNewMethod,
				Body: []any{serviceName, dbus.ObjectPath(servicePath)},
			}
			ch <- &dbus.Signal{
				Name: system.UnitNewMethod,
				Body: []any{}, // Missing body
			}
			ch <- &dbus.Signal{
				Name: system.UnitNewMethod,
				Body: []any{5, dbus.ObjectPath(servicePath)}, // Bad unit name type
			}
			ch <- &dbus.Signal{
				Name: system.UnitNewMethod,
				Body: []any{serviceName, 7}, // Bad path type
			}
			ch <- &dbus.Signal{
				Name: system.PropertiesChangedMethod,
				Path: dbus.ObjectPath("badpath"), // Bad path
				Body: []any{"", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant("active")}},
			}
			ch <- &dbus.Signal{
				Name: system.PropertiesChangedMethod,
				// Missing path
				Body: []any{"", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant("active")}},
			}
			ch <- &dbus.Signal{
				Name: system.PropertiesChangedMethod,
				Body: []any{"", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant("active")}},
			}
			ch <- &dbus.Signal{
				Name: system.PropertiesChangedMethod,
				Path: dbus.ObjectPath(servicePath),
				Body: []any{"", map[string]dbus.Variant{
					"ActiveState":    dbus.MakeVariant("active"),
					"ExecMainCode":   dbus.MakeVariant(int32(1)),
					"ExecMainStatus": dbus.MakeVariant(int32(2)),
				}},
			}
			ch <- &dbus.Signal{
				Name: system.UnitRemovedMethod,
				Body: []any{serviceName, dbus.ObjectPath(servicePath)},
			}
		}()
	})

	unitProps := make(chan *system.UnitProperties)
	supervisor := system.NewOsSystemdSupervisor(mockConn)
	defer supervisor.Stop()
	supervisor.AddServiceWatcher(serviceName, unitProps)
	supervisor.Start()

	prop := <-unitProps
	if prop.ActiveState != "active" {
		t.Fatalf("Expected ActiveState active, got %s", prop.ActiveState)
	}
	prop = <-unitProps
	if prop != nil {
		t.Fatalf("Expected nil from closed channel, got %+v", prop)
	}
}

func TestSystemdStartTransientUnit(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	systemd, err := system.NewSystemdOsConnection()
	if err != nil {
		t.Fatal(err)
	}
	defer systemd.Close()

	ctx := context.Background()
	serviceName := "s3-csi-integ-test.service"

	signalChan := make(chan *dbus.Signal, 256)
	systemd.Signal(signalChan)

	pts := system.NewOsPts()
	ptm, ptsN, err := pts.NewPts()
	if err != nil {
		t.Fatal(err)
	}
	defer ptm.Close()

	testString := "Test output magic"

	props := []system.DbusProperty{
		{Name: "Description", Value: "test"},
		{Name: "Type", Value: "oneshot"},
		{Name: "StandardOutput", Value: "tty"},
		{Name: "StandardError", Value: "tty"},
		{Name: "TTYPath", Value: fmt.Sprintf("/dev/pts/%d", ptsN)},
		{Name: "ExecStart", Value: []system.DbusExecStart{
			{
				Path:             "/usr/bin/echo",
				Args:             []string{"/usr/bin/echo", "-n", testString},
				UncleanIsFailure: true,
			},
		}},
	}
	job, err := systemd.StartTransientUnit(ctx, serviceName, "fail", props)
	if err != nil {
		t.Fatal(err)
	}
	jobDone := false
	unitDone := false
	for s := range signalChan {
		//fmt.Printf("Got signal: %v\n", s)
		if s.Name == "org.freedesktop.systemd1.Manager.JobRemoved" &&
			s.Body[1] == job {
			if s.Body[3] == "done" {
				jobDone = true
			} else {
				t.Fatalf("Expected job to be done, got %s", s.Body[3])
			}
		} else if s.Name == "org.freedesktop.systemd1.Manager.UnitRemoved" &&
			s.Body[0] == serviceName {
			unitDone = true
		}
		if jobDone && unitDone {
			break
		}
	}

	// read from ptm
	buffer := &bytes.Buffer{}
	buffer.ReadFrom(ptm)
	output := string(buffer.Bytes())

	if output != testString {
		t.Fatalf("Strings do not match, expected \"%s\" got \"%s\"", testString, output)
	}
}

func TestSystemdNew(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	wd, err := system.OsSystemdSupervisorFactory{}.StartSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	err = wd.Stop()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSystemdStartServiceFailure(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	sysd, err := system.OsSystemdSupervisorFactory{}.StartSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	defer sysd.Stop()

	mountPath := "/tmp/" + uuid.New().String()
	os.Mkdir(mountPath, 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	config := &system.ExecConfig{
		Name:        "test-integ" + uuid.New().String() + ".service",
		Description: "S3 CSI Integ tests",
		ExecPath:    "/usr/bin/false",
		Args:        []string{},
		Env:         []string{},
	}

	_, err = sysd.StartService(ctx, config)
	if err == nil {
		t.Fatal("Expected non-nil error")
	}
}

func TestSystemdRunOneshotSuccess(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	sysd, err := system.OsSystemdSupervisorFactory{}.StartSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	defer sysd.Stop()

	testString := "test-echo-oneshot"

	ctx := context.Background()
	config := &system.ExecConfig{
		Name:        "test-integ" + uuid.New().String() + ".service",
		Description: "S3 CSI Integ tests",
		ExecPath:    "/usr/bin/echo",
		Args:        []string{"-n", testString},
		Env:         []string{},
	}

	output, err := sysd.RunOneshot(ctx, config)
	if err != nil {
		t.Fatal(err)
	}

	if output != testString {
		t.Fatalf("Oneshot echo expected %s got %s", testString, output)
	}
}

func TestSystemdRunOneshotFailure(t *testing.T) {
	if !isRoot() {
		t.Skip("Skipping test, not root")
	}
	sysd, err := system.OsSystemdSupervisorFactory{}.StartSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	defer sysd.Stop()

	ctx := context.Background()
	config := &system.ExecConfig{
		Name:        "test-integ" + uuid.New().String() + ".service",
		Description: "S3 CSI Integ tests",
		ExecPath:    "/usr/bin/false",
		Args:        []string{},
		Env:         []string{},
	}

	_, err = sysd.RunOneshot(ctx, config)
	if err == nil {
		t.Fatal("Expected non-nil error")
	}
}
