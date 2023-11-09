package driver_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	driver "github.com/awslabs/aws-s3-csi-driver/pkg/driver"
	mock_driver "github.com/awslabs/aws-s3-csi-driver/pkg/driver/mocks"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/golang/mock/gomock"
)

const (
	testExe         = "/usr/bin/testmount"
	testTag         = "1.0.0-abcd"
	testServiceName = "testmount-1.0.0-abcd.service"
)

type systemRunnerTestEnv struct {
	ctx           context.Context
	mockCtl       *gomock.Controller
	mockConnector *mock_driver.MockSystemdConnector
	mockPts       *mock_driver.MockPts
	runner        *driver.SystemdRunner
}

func setupSystemRunnerTest(t *testing.T) *systemRunnerTestEnv {
	mockCtl := gomock.NewController(t)
	return &systemRunnerTestEnv{
		ctx:           context.Background(),
		mockCtl:       mockCtl,
		mockConnector: mock_driver.NewMockSystemdConnector(mockCtl),
		mockPts:       mock_driver.NewMockPts(mockCtl),
	}
}

func TestSystemdRunFailedConnection(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockConnector := mock_driver.NewMockSystemdConnector(mockCtl)
	mockConnector.EXPECT().Connect(gomock.Any()).Return(nil, errors.New(""))
	ctx := context.Background()

	runner := &driver.SystemdRunner{
		Connector: mockConnector,
	}
	out, err := runner.Run(ctx, "", "", nil, nil)
	if err == nil {
		t.Fatalf("Expected error on connection failure")
	}
	if out != "" {
		t.Fatalf("Out should be empty")
	}
}

func TestSystemdRunNewPtsFailure(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockConnector := mock_driver.NewMockSystemdConnector(mockCtl)
	mockConnection := mock_driver.NewMockSystemdConnection(mockCtl)
	mockPts := mock_driver.NewMockPts(mockCtl)
	mockConnection.EXPECT().Close()
	mockConnector.EXPECT().Connect(gomock.Any()).Return(mockConnection, nil)
	mockPts.EXPECT().NewPts().Return(nil, 0, errors.New(""))
	ctx := context.Background()

	runner := driver.SystemdRunner{
		Connector: mockConnector,
		Pts:       mockPts,
	}
	out, err := runner.Run(ctx, "", "", nil, nil)
	if err == nil {
		t.Fatalf("Expected error on connection failure")
	}
	if out != "" {
		t.Fatalf("Output should be empty")
	}
}

func TestSystemdStartUnitFailure(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockConnector := mock_driver.NewMockSystemdConnector(mockCtl)
	mockConnection := mock_driver.NewMockSystemdConnection(mockCtl)
	mockPts := mock_driver.NewMockPts(mockCtl)
	mockConnection.EXPECT().Close()
	mockConnector.EXPECT().Connect(gomock.Any()).Return(mockConnection, nil)
	mockPts.EXPECT().NewPts().Return(io.NopCloser(strings.NewReader("")), 0, nil)
	ctx := context.Background()

	mockConnection.EXPECT().StartTransientUnitContext(
		gomock.Eq(ctx), gomock.Any(), gomock.Eq("fail"), gomock.Any(), gomock.Any()).Return(0, errors.New(""))

	runner := driver.SystemdRunner{
		Connector: mockConnector,
		Pts:       mockPts,
	}
	out, err := runner.Run(ctx, "", "", nil, nil)
	if err == nil {
		t.Fatalf("Expected error on connection failure")
	}
	if out != "" {
		t.Fatalf("Output should be empty")
	}
}

func TestSystemdRunCanceledContext(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockConnector := mock_driver.NewMockSystemdConnector(mockCtl)
	mockConnection := mock_driver.NewMockSystemdConnection(mockCtl)
	mockPts := mock_driver.NewMockPts(mockCtl)
	mockConnection.EXPECT().Close()
	mockConnector.EXPECT().Connect(gomock.Any()).Return(mockConnection, nil)
	testOutput := "testoutputdata"
	mockPts.EXPECT().NewPts().Return(io.NopCloser(strings.NewReader(testOutput)), 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel context

	mockConnection.EXPECT().StartTransientUnitContext(
		gomock.Eq(ctx), gomock.Any(), gomock.Eq("fail"), gomock.Any(), gomock.Any()).Return(0, nil)

	runner := driver.SystemdRunner{
		Connector: mockConnector,
		Pts:       mockPts,
	}
	out, err := runner.Run(ctx, "", "", nil, nil)
	if err == nil {
		t.Fatalf("Expected error on connection failure")
	}
	if out != testOutput {
		t.Fatalf("Unexpected output, expected: %s got: %s", testOutput, out)
	}
}

func TestSystemdRunSuccess(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockConnector := mock_driver.NewMockSystemdConnector(mockCtl)
	mockConnection := mock_driver.NewMockSystemdConnection(mockCtl)
	mockPts := mock_driver.NewMockPts(mockCtl)
	mockConnection.EXPECT().Close()
	mockConnector.EXPECT().Connect(gomock.Any()).Return(mockConnection, nil)
	testOutput := "testoutputdata"
	mockPts.EXPECT().NewPts().Return(io.NopCloser(strings.NewReader(testOutput)), 0, nil)
	ctx := context.Background()

	mockConnection.EXPECT().StartTransientUnitContext(
		gomock.Eq(ctx), gomock.Any(), gomock.Eq("fail"), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, name string, _ string, _ []systemd.Property, ch chan<- string) {
			go func() { ch <- "done" }()
		}).Return(0, nil)

	status := []systemd.UnitStatus{
		{
			Name:        testServiceName,
			ActiveState: "active",
		},
	}
	mockConnection.EXPECT().ListUnitsContext(gomock.Any()).Return(status, nil)

	runner := driver.SystemdRunner{
		Connector: mockConnector,
		Pts:       mockPts,
	}
	out, err := runner.Run(ctx, testExe, testTag, nil, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if out != testOutput {
		t.Fatalf("Unexpected output, expected: %s got: %s", testOutput, out)
	}
}
