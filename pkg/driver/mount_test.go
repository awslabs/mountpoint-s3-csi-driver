package driver_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	driver "github.com/awslabs/aws-s3-csi-driver/pkg/driver"
	mock_driver "github.com/awslabs/aws-s3-csi-driver/pkg/driver/mocks"
	"github.com/awslabs/aws-s3-csi-driver/pkg/system"
	"github.com/golang/mock/gomock"
	"k8s.io/mount-utils"
)

type TestMountLister struct {
	Mounts []mount.MountPoint
	Err    error
}

func (l *TestMountLister) ListMounts() ([]mount.MountPoint, error) {
	return l.Mounts, l.Err
}

type mounterTestEnv struct {
	ctx             context.Context
	mockCtl         *gomock.Controller
	mockRunner      *mock_driver.MockServiceRunner
	mockMountLister *mock_driver.MockMountLister
	mounter         *driver.S3Mounter
}

func initMounterTestEnv(t *testing.T) *mounterTestEnv {
	ctx := context.Background()
	mockCtl := gomock.NewController(t)
	defer mockCtl.Finish()
	mockRunner := mock_driver.NewMockServiceRunner(mockCtl)
	mockMountLister := mock_driver.NewMockMountLister(mockCtl)
	mountpointVersion := "TEST_MP_VERSION-v1.1"

	return &mounterTestEnv{
		ctx:             ctx,
		mockCtl:         mockCtl,
		mockRunner:      mockRunner,
		mockMountLister: mockMountLister,
		mounter: &driver.S3Mounter{
			Ctx:         ctx,
			Runner:      mockRunner,
			MountLister: mockMountLister,
			MpVersion:   mountpointVersion,
			MountS3Path: driver.MountS3Path(),
		},
	}
}

func TestS3MounterMount(t *testing.T) {
	testBucketName := "test-bucket"
	testTargetPath := filepath.Join(t.TempDir(), "mount")
	testCredentials := &driver.MountCredentials{
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		Region:          "test-region",
		DefaultRegion:   "test-region",
		WebTokenPath:    "test-web-token-path",
		StsEndpoints:    "test-sts-endpoint",
		AwsRoleArn:      "test-aws-role",
	}

	testCases := []struct {
		name        string
		bucketName  string
		targetPath  string
		credentials *driver.MountCredentials
		options     []string
		expectedErr bool
		before      func(*testing.T, *mounterTestEnv)
	}{
		{
			name:        "success: mounts with empty options",
			bucketName:  testBucketName,
			targetPath:  testTargetPath,
			credentials: testCredentials,
			options:     []string{},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockMountLister.EXPECT().ListMounts().Return(nil, nil)
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).Return("success", nil)
			},
		},
		{
			name:        "success: mounts with nil credentials",
			bucketName:  testBucketName,
			targetPath:  testTargetPath,
			credentials: nil,
			options:     []string{},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockMountLister.EXPECT().ListMounts().Return(nil, nil)
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).Return("success", nil)
			},
		},
		{
			name:        "success: replaces user agent prefix",
			bucketName:  testBucketName,
			targetPath:  testTargetPath,
			credentials: nil,
			options:     []string{"--user-agent-prefix=mycustomuseragent"},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockMountLister.EXPECT().ListMounts().Return(nil, nil)
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
			name:        "success: aws max attempts",
			bucketName:  testBucketName,
			targetPath:  testTargetPath,
			credentials: nil,
			options:     []string{"--aws-max-attempts=10"},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockMountLister.EXPECT().ListMounts().Return(nil, nil)
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, config *system.ExecConfig) (string, error) {
					for _, e := range config.Env {
						if e == "AWS_MAX_ATTEMPTS=10" {
							return "success", nil
						}
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
			credentials: nil,
			options:     []string{},
			expectedErr: true,
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockMountLister.EXPECT().ListMounts().Return(nil, nil)
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).Return("fail", errors.New("test failure"))
			},
		},
		{
			name:        "failure: won't mount empty bucket name",
			targetPath:  testTargetPath,
			credentials: testCredentials,
			options:     []string{},
			expectedErr: true,
		},
		{
			name:        "failure: won't mount empty target",
			bucketName:  testBucketName,
			credentials: testCredentials,
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
			err := env.mounter.Mount(testCase.bucketName, testCase.targetPath,
				testCase.credentials, testCase.options)
			env.mockCtl.Finish()
			if err != nil && !testCase.expectedErr {
				t.Fatal(err)
			}
		})
	}
}
