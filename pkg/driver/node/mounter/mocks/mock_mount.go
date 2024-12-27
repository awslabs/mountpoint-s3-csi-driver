// Code generated by MockGen. DO NOT EDIT.
// Source: mounter.go

// Package mock_driver is a generated GoMock package.
package mock_driver

import (
	context "context"
	reflect "reflect"

	credentialprovider "github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	envprovider "github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	mountpoint "github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	system "github.com/awslabs/aws-s3-csi-driver/pkg/system"
	gomock "github.com/golang/mock/gomock"
)

// MockServiceRunner is a mock of ServiceRunner interface.
type MockServiceRunner struct {
	ctrl     *gomock.Controller
	recorder *MockServiceRunnerMockRecorder
}

// MockServiceRunnerMockRecorder is the mock recorder for MockServiceRunner.
type MockServiceRunnerMockRecorder struct {
	mock *MockServiceRunner
}

// NewMockServiceRunner creates a new mock instance.
func NewMockServiceRunner(ctrl *gomock.Controller) *MockServiceRunner {
	mock := &MockServiceRunner{ctrl: ctrl}
	mock.recorder = &MockServiceRunnerMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockServiceRunner) EXPECT() *MockServiceRunnerMockRecorder {
	return m.recorder
}

// RunOneshot mocks base method.
func (m *MockServiceRunner) RunOneshot(ctx context.Context, config *system.ExecConfig) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "RunOneshot", ctx, config)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// RunOneshot indicates an expected call of RunOneshot.
func (mr *MockServiceRunnerMockRecorder) RunOneshot(ctx, config interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RunOneshot", reflect.TypeOf((*MockServiceRunner)(nil).RunOneshot), ctx, config)
}

// StartService mocks base method.
func (m *MockServiceRunner) StartService(ctx context.Context, config *system.ExecConfig) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "StartService", ctx, config)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// StartService indicates an expected call of StartService.
func (mr *MockServiceRunnerMockRecorder) StartService(ctx, config interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "StartService", reflect.TypeOf((*MockServiceRunner)(nil).StartService), ctx, config)
}

// MockMounter is a mock of Mounter interface.
type MockMounter struct {
	ctrl     *gomock.Controller
	recorder *MockMounterMockRecorder
}

// MockMounterMockRecorder is the mock recorder for MockMounter.
type MockMounterMockRecorder struct {
	mock *MockMounter
}

// NewMockMounter creates a new mock instance.
func NewMockMounter(ctrl *gomock.Controller) *MockMounter {
	mock := &MockMounter{ctrl: ctrl}
	mock.recorder = &MockMounterMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockMounter) EXPECT() *MockMounterMockRecorder {
	return m.recorder
}

// IsMountPoint mocks base method.
func (m *MockMounter) IsMountPoint(target string) (bool, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "IsMountPoint", target)
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// IsMountPoint indicates an expected call of IsMountPoint.
func (mr *MockMounterMockRecorder) IsMountPoint(target interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "IsMountPoint", reflect.TypeOf((*MockMounter)(nil).IsMountPoint), target)
}

// Mount mocks base method.
func (m *MockMounter) Mount(bucketName, target string, credentials credentialprovider.Credentials, env envprovider.Environment, args mountpoint.Args) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Mount", bucketName, target, credentials, env, args)
	ret0, _ := ret[0].(error)
	return ret0
}

// Mount indicates an expected call of Mount.
func (mr *MockMounterMockRecorder) Mount(bucketName, target, credentials, env, args interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Mount", reflect.TypeOf((*MockMounter)(nil).Mount), bucketName, target, credentials, env, args)
}

// Unmount mocks base method.
func (m *MockMounter) Unmount(target string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Unmount", target)
	ret0, _ := ret[0].(error)
	return ret0
}

// Unmount indicates an expected call of Unmount.
func (mr *MockMounterMockRecorder) Unmount(target interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Unmount", reflect.TypeOf((*MockMounter)(nil).Unmount), target)
}
