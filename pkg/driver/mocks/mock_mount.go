// Code generated by MockGen. DO NOT EDIT.
// Source: mount.go

// Package mock_driver is a generated GoMock package.
package mock_driver

import (
	context "context"
	os "os"
	reflect "reflect"

	driver "github.com/awslabs/aws-s3-csi-driver/pkg/driver"
	system "github.com/awslabs/aws-s3-csi-driver/pkg/system"
	gomock "github.com/golang/mock/gomock"
	mount "k8s.io/mount-utils"
)

// MockFs is a mock of Fs interface.
type MockFs struct {
	ctrl     *gomock.Controller
	recorder *MockFsMockRecorder
}

// MockFsMockRecorder is the mock recorder for MockFs.
type MockFsMockRecorder struct {
	mock *MockFs
}

// NewMockFs creates a new mock instance.
func NewMockFs(ctrl *gomock.Controller) *MockFs {
	mock := &MockFs{ctrl: ctrl}
	mock.recorder = &MockFsMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockFs) EXPECT() *MockFsMockRecorder {
	return m.recorder
}

// MkdirAll mocks base method.
func (m *MockFs) MkdirAll(path string, perm os.FileMode) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "MkdirAll", path, perm)
	ret0, _ := ret[0].(error)
	return ret0
}

// MkdirAll indicates an expected call of MkdirAll.
func (mr *MockFsMockRecorder) MkdirAll(path, perm interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "MkdirAll", reflect.TypeOf((*MockFs)(nil).MkdirAll), path, perm)
}

// Remove mocks base method.
func (m *MockFs) Remove(name string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Remove", name)
	ret0, _ := ret[0].(error)
	return ret0
}

// Remove indicates an expected call of Remove.
func (mr *MockFsMockRecorder) Remove(name interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Remove", reflect.TypeOf((*MockFs)(nil).Remove), name)
}

// Stat mocks base method.
func (m *MockFs) Stat(name string) (os.FileInfo, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Stat", name)
	ret0, _ := ret[0].(os.FileInfo)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// Stat indicates an expected call of Stat.
func (mr *MockFsMockRecorder) Stat(name interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Stat", reflect.TypeOf((*MockFs)(nil).Stat), name)
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
func (m *MockMounter) Mount(bucketName, target string, credentials *driver.MountCredentials, options []string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Mount", bucketName, target, credentials, options)
	ret0, _ := ret[0].(error)
	return ret0
}

// Mount indicates an expected call of Mount.
func (mr *MockMounterMockRecorder) Mount(bucketName, target, credentials, options interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Mount", reflect.TypeOf((*MockMounter)(nil).Mount), bucketName, target, credentials, options)
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

// MockMountLister is a mock of MountLister interface.
type MockMountLister struct {
	ctrl     *gomock.Controller
	recorder *MockMountListerMockRecorder
}

// MockMountListerMockRecorder is the mock recorder for MockMountLister.
type MockMountListerMockRecorder struct {
	mock *MockMountLister
}

// NewMockMountLister creates a new mock instance.
func NewMockMountLister(ctrl *gomock.Controller) *MockMountLister {
	mock := &MockMountLister{ctrl: ctrl}
	mock.recorder = &MockMountListerMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockMountLister) EXPECT() *MockMountListerMockRecorder {
	return m.recorder
}

// ListMounts mocks base method.
func (m *MockMountLister) ListMounts() ([]mount.MountPoint, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ListMounts")
	ret0, _ := ret[0].([]mount.MountPoint)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ListMounts indicates an expected call of ListMounts.
func (mr *MockMountListerMockRecorder) ListMounts() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ListMounts", reflect.TypeOf((*MockMountLister)(nil).ListMounts))
}
