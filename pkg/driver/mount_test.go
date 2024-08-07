package driver_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
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
	mockFs          *mock_driver.MockFs
	mockMountLister *mock_driver.MockMountLister
	mounter         *driver.S3Mounter
}

func initMounterTestEnv(t *testing.T) *mounterTestEnv {
	ctx := context.Background()
	mockCtl := gomock.NewController(t)
	defer mockCtl.Finish()
	mockRunner := mock_driver.NewMockServiceRunner(mockCtl)
	mockFs := mock_driver.NewMockFs(mockCtl)
	mockMountLister := mock_driver.NewMockMountLister(mockCtl)
	mountpointVersion := "TEST_MP_VERSION-v1.1"

	return &mounterTestEnv{
		ctx:             ctx,
		mockCtl:         mockCtl,
		mockRunner:      mockRunner,
		mockFs:          mockFs,
		mockMountLister: mockMountLister,
		mounter: &driver.S3Mounter{
			Ctx:         ctx,
			Runner:      mockRunner,
			Fs:          mockFs,
			MountLister: mockMountLister,
			MpVersion:   mountpointVersion,
			MountS3Path: driver.MountS3Path(),
		},
	}
}

func TestS3MounterMount(t *testing.T) {
	testBucketName := "test-bucket"
	testTargetPath := "/mnt/my-mountpoint/bucket/"
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
			name:        "success: mounts without empty options",
			bucketName:  testBucketName,
			targetPath:  testTargetPath,
			credentials: testCredentials,
			options:     []string{},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockFs.EXPECT().Stat(gomock.Any()).Return(nil, nil)
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
				env.mockFs.EXPECT().Stat(gomock.Any()).Return(nil, nil)
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
				env.mockFs.EXPECT().Stat(gomock.Any()).Return(nil, nil)
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
				env.mockFs.EXPECT().Stat(gomock.Any()).Return(nil, nil)
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
				env.mockFs.EXPECT().Stat(gomock.Any()).Return(nil, nil)
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
		{
			name:        "failure: mounts without empty options",
			bucketName:  testBucketName,
			targetPath:  testTargetPath,
			credentials: testCredentials,
			options:     []string{},
			before: func(t *testing.T, env *mounterTestEnv) {
				env.mockFs.EXPECT().Stat(gomock.Any()).Return(nil, nil)
				env.mockMountLister.EXPECT().ListMounts().Return(nil, nil)
				env.mockRunner.EXPECT().StartService(gomock.Any(), gomock.Any()).Return("success", nil)
			},
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

func TestProvidingEnvVariablesForMountpointProcess(t *testing.T) {
	tests := map[string]struct {
		credentials *driver.MountCredentials
		expected    []string
	}{
		"Access Key and Secret Key": {
			credentials: &driver.MountCredentials{
				AccessKeyID:     "access_key",
				SecretAccessKey: "secret_key",
			},
			expected: []string{
				"AWS_ACCESS_KEY_ID=access_key",
				"AWS_SECRET_ACCESS_KEY=secret_key",
			},
		},
		"Access Key and Secret Key with Session Token": {
			credentials: &driver.MountCredentials{
				AccessKeyID:     "access_key",
				SecretAccessKey: "secret_key",
				SessionToken:    "session_token",
			},
			expected: []string{
				"AWS_ACCESS_KEY_ID=access_key",
				"AWS_SECRET_ACCESS_KEY=secret_key",
				"AWS_SESSION_TOKEN=session_token",
			},
		},
		"Profile Provider": {
			credentials: &driver.MountCredentials{
				ConfigFilePath:            "~/.aws/config",
				SharedCredentialsFilePath: "~/.aws/credentials",
			},
			expected: []string{
				"AWS_CONFIG_FILE=~/.aws/config",
				"AWS_SHARED_CREDENTIALS_FILE=~/.aws/credentials",
			},
		},
		"Disabling IMDS Provider": {
			credentials: &driver.MountCredentials{
				DisableIMDSProvider: true,
			},
			expected: []string{
				"AWS_EC2_METADATA_DISABLED=true",
			},
		},
		"STS Web Identity Provider": {
			credentials: &driver.MountCredentials{
				WebTokenPath: "/path/to/web/token",
				AwsRoleArn:   "arn:aws:iam::123456789012:role/Role",
			},
			expected: []string{
				"AWS_WEB_IDENTITY_TOKEN_FILE=/path/to/web/token",
				"AWS_ROLE_ARN=arn:aws:iam::123456789012:role/Role",
			},
		},
		"Region and Default Region": {
			credentials: &driver.MountCredentials{
				Region:        "us-west-2",
				DefaultRegion: "us-east-1",
			},
			expected: []string{
				"AWS_REGION=us-west-2",
				"AWS_DEFAULT_REGION=us-east-1",
			},
		},
		"STS Endpoints": {
			credentials: &driver.MountCredentials{
				StsEndpoints: "regional",
			},
			expected: []string{
				"AWS_STS_REGIONAL_ENDPOINTS=regional",
			},
		},
		"All Combined": {
			credentials: &driver.MountCredentials{
				AccessKeyID:               "access_key",
				SecretAccessKey:           "secret_key",
				SessionToken:              "session_token",
				WebTokenPath:              "/path/to/web/token",
				AwsRoleArn:                "arn:aws:iam::123456789012:role/Role",
				Region:                    "us-west-2",
				DefaultRegion:             "us-east-1",
				StsEndpoints:              "legacy",
				ConfigFilePath:            "~/.aws/config",
				SharedCredentialsFilePath: "~/.aws/credentials",
				DisableIMDSProvider:       true,
			},
			expected: []string{
				"AWS_ACCESS_KEY_ID=access_key",
				"AWS_SECRET_ACCESS_KEY=secret_key",
				"AWS_SESSION_TOKEN=session_token",
				"AWS_WEB_IDENTITY_TOKEN_FILE=/path/to/web/token",
				"AWS_ROLE_ARN=arn:aws:iam::123456789012:role/Role",
				"AWS_REGION=us-west-2",
				"AWS_DEFAULT_REGION=us-east-1",
				"AWS_STS_REGIONAL_ENDPOINTS=legacy",
				"AWS_EC2_METADATA_DISABLED=true",
				"AWS_CONFIG_FILE=~/.aws/config",
				"AWS_SHARED_CREDENTIALS_FILE=~/.aws/credentials",
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			actual := test.credentials.Env()

			slices.Sort(test.expected)
			slices.Sort(actual)

			if !reflect.DeepEqual(actual, test.expected) {
				t.Errorf("Expected %v, but got %v", test.expected, actual)
			}
		})
	}
}
