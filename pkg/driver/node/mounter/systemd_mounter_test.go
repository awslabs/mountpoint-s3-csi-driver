package mounter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"slices"

	"github.com/golang/mock/gomock"
	"k8s.io/mount-utils"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	mock_driver "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/system"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

type mounterTestEnv struct {
	ctx        context.Context
	mockCtl    *gomock.Controller
	mockRunner *mock_driver.MockServiceRunner
	mounter    *mounter.SystemdMounter
}

func initMounterTestEnv(t *testing.T) *mounterTestEnv {
	ctx := context.Background()
	mockCtl := gomock.NewController(t)
	defer mockCtl.Finish()
	mockRunner := mock_driver.NewMockServiceRunner(mockCtl)
	mountpointVersion := "TEST_MP_VERSION-v1.1"

	return &mounterTestEnv{
		ctx:        ctx,
		mockCtl:    mockCtl,
		mockRunner: mockRunner,
		mounter: &mounter.SystemdMounter{
			Runner:      mockRunner,
			Mounter:     mount.NewFakeMounter(nil),
			MpMounter:   mpmounter.NewWithMount(mount.NewFakeMounter(nil)),
			MpVersion:   mountpointVersion,
			MountS3Path: mounter.MountS3Path(),
		},
	}
}

func TestS3MounterMount(t *testing.T) {
	testBucketName := "test-bucket"
	testTargetPath := filepath.Join(t.TempDir(), "mount")
	testProvideCtx := credentialprovider.ProvideContext{
		WorkloadPodID: "test-pod",
		VolumeID:      "test-volume",
		WritePath:     t.TempDir(),
	}

	testCases := []struct {
		name        string
		bucketName  string
		targetPath  string
		provideCtx  credentialprovider.ProvideContext
		options     []string
		expectedErr bool
		before      func(*testing.T, *mounterTestEnv)
	}{
		{
			name:       "success: mounts with empty options",
			bucketName: testBucketName,
			targetPath: testTargetPath,
			provideCtx: testProvideCtx,
			options:    []string{},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).Return("success", nil)
			},
		},
		{
			name:       "success: mounts with nil credentials",
			bucketName: testBucketName,
			targetPath: testTargetPath,
			provideCtx: credentialprovider.ProvideContext{},
			options:    []string{},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).Return("success", nil)
			},
		},
		{
			name:       "success: replaces user agent prefix",
			bucketName: testBucketName,
			targetPath: testTargetPath,
			provideCtx: credentialprovider.ProvideContext{},
			options:    []string{"--user-agent-prefix=mycustomuseragent"},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, config *system.ExecConfig) (string, error) {
					for _, a := range config.Args {
						if strings.Contains(a, "mycustomuseragent") {
							t.Fatal("Bad user agent")
						}
					}
					return "success", nil
				})
			},
		},
		{
			name:       "success: aws max attempts",
			bucketName: testBucketName,
			targetPath: testTargetPath,
			provideCtx: credentialprovider.ProvideContext{},
			options:    []string{"--aws-max-attempts=10"},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, config *system.ExecConfig) (string, error) {
					if slices.Contains(config.Env, "AWS_MAX_ATTEMPTS=10") {
						return "success", nil
					}
					t.Fatal("Bad env")
					return "", nil
				})
			},
		},
		{
			name:        "failure: fails on mount failure",
			bucketName:  testBucketName,
			targetPath:  testTargetPath,
			provideCtx:  credentialprovider.ProvideContext{},
			options:     []string{},
			expectedErr: true,
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).Return("fail", errors.New("test failure"))
			},
		},
		{
			name:        "failure: won't mount empty bucket name",
			targetPath:  testTargetPath,
			provideCtx:  testProvideCtx,
			options:     []string{},
			expectedErr: true,
		},
		{
			name:        "failure: won't mount empty target",
			bucketName:  testBucketName,
			provideCtx:  testProvideCtx,
			options:     []string{},
			expectedErr: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			env := initMounterTestEnv(t)
			if testCase.before != nil {
				testCase.before(t, env)
			}
			err := env.mounter.Mount(env.ctx, testCase.bucketName, testCase.targetPath,
				testCase.provideCtx, mountpoint.ParseArgs(testCase.options), "")
			env.mockCtl.Finish()
			if err != nil && !testCase.expectedErr {
				t.Fatal(err)
			}
		})
	}
}

func TestIsMountPoint(t *testing.T) {
	testDir := t.TempDir()
	mountpointS3MountPath := filepath.Join(testDir, "/var/lib/kubelet/pods/46efe8aa-75d9-4b12-8fdd-0ce0c2cabd99/volumes/kubernetes.io~csi/s3-mp-csi-pv/mount")
	tmpFsMountPath := filepath.Join(testDir, "/var/lib/kubelet/pods/3af4cdb5-6131-4d4b-bed3-4b7a74d357e4/volumes/kubernetes.io~projected/kube-api-access-tmxk4")
	testProcMountsContent := []mount.MountPoint{
		{
			Device: "proc",
			Path:   "/proc",
			Type:   "proc",
			Opts:   []string{"rw", "nosuid", "nodev", "noexec", "relatime"},
			Freq:   0,
			Pass:   0,
		},
		{
			Device: "sysfs",
			Path:   "/sys",
			Type:   "sysfs",
			Opts:   []string{"rw", "seclabel", "nosuid", "nodev", "noexec", "relatime"},
			Freq:   0,
			Pass:   0,
		},
		{
			Device: "tmpfs",
			Path:   tmpFsMountPath,
			Type:   "tmpfs",
			Opts:   []string{"rw", "seclabel", "relatime", "size=3364584k"},
			Freq:   0,
			Pass:   0,
		},
		{
			Device: "mountpoint-s3",
			Path:   mountpointS3MountPath,
			Type:   "fuse",
			Opts:   []string{"rw", "nosuid", "nodev", "noatime", "user_id=0", "group_id=0", "default_permissions"},
			Freq:   0,
			Pass:   0,
		},
	}

	os.MkdirAll(tmpFsMountPath, 0755)
	os.MkdirAll(mountpointS3MountPath, 0755)

	tests := map[string]struct {
		procMountsContent []mount.MountPoint
		target            string
		isMountPoint      bool
		expectErr         bool
	}{
		"mountpoint-s3 mount": {
			procMountsContent: testProcMountsContent,
			target:            mountpointS3MountPath,
			isMountPoint:      true,
			expectErr:         false,
		},
		"tmpfs mount": {
			procMountsContent: testProcMountsContent,
			target:            tmpFsMountPath,
			isMountPoint:      false,
			expectErr:         false,
		},
		"non existing mount on /proc/mounts": {
			procMountsContent: testProcMountsContent[:2],
			target:            mountpointS3MountPath,
			isMountPoint:      false,
			expectErr:         false,
		},
		"non existing mount on filesystem": {
			procMountsContent: testProcMountsContent,
			target:            "/var/lib/kubelet/pods/46efe8aa-75d9-4b12-8fdd-0ce0c2cabd99/volumes/kubernetes.io~csi/s3-mp-csi-pv/mount",
			isMountPoint:      false,
			expectErr:         true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			mounter := &mounter.SystemdMounter{MpMounter: mpmounter.NewWithMount(mount.NewFakeMounter(test.procMountsContent))}
			isMountPoint, err := mounter.IsMountPoint(test.target)
			assert.Equals(t, test.isMountPoint, isMountPoint)
			assert.Equals(t, test.expectErr, err != nil)
		})
	}
}
