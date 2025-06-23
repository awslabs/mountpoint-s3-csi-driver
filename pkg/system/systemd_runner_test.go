package system_test

import (
	"context"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/system"
	mock_system "github.com/awslabs/mountpoint-s3-csi-driver/pkg/system/mocks"
	"github.com/golang/mock/gomock"
)

func TestSystemdRunnerStartService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockSupervisor := mock_system.NewMockSystemdSupervisor(ctrl)
	mockFactory := mock_system.NewMockSystemdSupervisorFactory(ctrl)
	mockFactory.EXPECT().StartSupervisor().Return(mockSupervisor, nil)

	runner, err := system.StartSystemdRunner(mockFactory)
	if err != nil {
		t.Fatalf("Failed to create SystemdRunner: %v", err)
	}

	ctx := context.Background()
	config := &system.ExecConfig{
		Name:        "test.service",
		ExecPath:    "/bin/test",
		Args:        []string{"arg1", "arg2"},
		Description: "Test service",
	}

	mockSupervisor.EXPECT().IsConnectionClosed().Return(false)
	mockSupervisor.EXPECT().StartService(ctx, config).Return("test output", nil)

	output, err := runner.StartService(ctx, config)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if output != "test output" {
		t.Errorf("Unexpected output, got '%s'", output)
	}
}

func TestSystemdRunnerRecreateConnection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Create first supervisor that will have a closed connection
	mockSupervisor1 := mock_system.NewMockSystemdSupervisor(ctrl)
	mockFactory := mock_system.NewMockSystemdSupervisorFactory(ctrl)
	mockFactory.EXPECT().StartSupervisor().Return(mockSupervisor1, nil)

	runner, err := system.StartSystemdRunner(mockFactory)
	if err != nil {
		t.Fatalf("Failed to create SystemdRunner: %v", err)
	}

	ctx := context.Background()
	config := &system.ExecConfig{
		Name:        "test.service",
		ExecPath:    "/bin/test",
		Args:        []string{"arg1", "arg2"},
		Description: "Test service",
	}

	// Create second supervisor that will handle the service start
	mockSupervisor2 := mock_system.NewMockSystemdSupervisor(ctrl)

	// Expectation sequence: enforce call order strictly
	gomock.InOrder(
		// First supervisor reports closed connection twice (fast + slow path)
		mockSupervisor1.EXPECT().IsConnectionClosed().Return(true),
		mockSupervisor1.EXPECT().IsConnectionClosed().Return(true),

		// First supervisor is stopped
		mockSupervisor1.EXPECT().Stop(),

		// Second supervisor created
		mockFactory.EXPECT().StartSupervisor().Return(mockSupervisor2, nil),

		// This expectation ensures that only mockSupervisor2 is allowed to handle StartService.
		// If mockSupervisor1.StartService(...) is called instead, the test will fail.
		mockSupervisor2.EXPECT().StartService(ctx, config).Return("test output", nil),
	)

	output, err := runner.StartService(ctx, config)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if output != "test output" {
		t.Errorf("Unexpected output, got '%s'", output)
	}
}
