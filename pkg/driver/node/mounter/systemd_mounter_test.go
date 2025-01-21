package mounter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/awsprofile"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter"
	mock_driver "github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter/mocks"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/system"
	"github.com/golang/mock/gomock"
	"k8s.io/mount-utils"
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
			Ctx:         ctx,
			Runner:      mockRunner,
			Mounter:     mount.NewFakeMounter(nil),
			MpVersion:   mountpointVersion,
			MountS3Path: mounter.MountS3Path(),
		},
	}
}

func TestS3MounterMount(t *testing.T) {
	testBucketName := "test-bucket"
	testTargetPath := filepath.Join(t.TempDir(), "mount")
	testCredentials := &mounter.MountCredentials{
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
		credentials *mounter.MountCredentials
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
				testCase.credentials, mountpoint.ParseArgs(testCase.options))
			env.mockCtl.Finish()
			if err != nil && !testCase.expectedErr {
				t.Fatal(err)
			}
		})
	}
}

func TestProvidingEnvVariablesForMountpointProcess(t *testing.T) {
	tests := map[string]struct {
		profile     awsprofile.AWSProfile
		credentials *mounter.MountCredentials
		expected    []string
	}{
		"Profile Provider for long-term credentials": {
			profile: awsprofile.AWSProfile{
				Name:            "profile",
				ConfigPath:      "~/.aws/s3-csi-config",
				CredentialsPath: "~/.aws/s3-csi-credentials",
			},
			credentials: &mounter.MountCredentials{},
			expected: []string{
				"AWS_PROFILE=profile",
				"AWS_CONFIG_FILE=~/.aws/s3-csi-config",
				"AWS_SHARED_CREDENTIALS_FILE=~/.aws/s3-csi-credentials",
			},
		},
		"Profile Provider": {
			credentials: &mounter.MountCredentials{
				ConfigFilePath:            "~/.aws/config",
				SharedCredentialsFilePath: "~/.aws/credentials",
			},
			expected: []string{
				"AWS_CONFIG_FILE=~/.aws/config",
				"AWS_SHARED_CREDENTIALS_FILE=~/.aws/credentials",
			},
		},
		"Disabling IMDS Provider": {
			credentials: &mounter.MountCredentials{
				DisableIMDSProvider: true,
			},
			expected: []string{
				"AWS_EC2_METADATA_DISABLED=true",
			},
		},
		"STS Web Identity Provider": {
			credentials: &mounter.MountCredentials{
				WebTokenPath: "/path/to/web/token",
				AwsRoleArn:   "arn:aws:iam::123456789012:role/Role",
			},
			expected: []string{
				"AWS_WEB_IDENTITY_TOKEN_FILE=/path/to/web/token",
				"AWS_ROLE_ARN=arn:aws:iam::123456789012:role/Role",
			},
		},
		"Region and Default Region": {
			credentials: &mounter.MountCredentials{
				Region:        "us-west-2",
				DefaultRegion: "us-east-1",
			},
			expected: []string{
				"AWS_REGION=us-west-2",
				"AWS_DEFAULT_REGION=us-east-1",
			},
		},
		"STS Endpoints": {
			credentials: &mounter.MountCredentials{
				StsEndpoints: "regional",
			},
			expected: []string{
				"AWS_STS_REGIONAL_ENDPOINTS=regional",
			},
		},
		"Mountpoint Cache Key": {
			credentials: &mounter.MountCredentials{
				MountpointCacheKey: "test_cache_key",
			},
			expected: []string{
				"UNSTABLE_MOUNTPOINT_CACHE_KEY=test_cache_key",
			},
		},
		"All Combined": {
			credentials: &mounter.MountCredentials{
				WebTokenPath:              "/path/to/web/token",
				AwsRoleArn:                "arn:aws:iam::123456789012:role/Role",
				Region:                    "us-west-2",
				DefaultRegion:             "us-east-1",
				StsEndpoints:              "legacy",
				ConfigFilePath:            "~/.aws/config",
				SharedCredentialsFilePath: "~/.aws/credentials",
				DisableIMDSProvider:       true,
				MountpointCacheKey:        "test/cache/key",
			},
			expected: []string{
				"AWS_WEB_IDENTITY_TOKEN_FILE=/path/to/web/token",
				"AWS_ROLE_ARN=arn:aws:iam::123456789012:role/Role",
				"AWS_REGION=us-west-2",
				"AWS_DEFAULT_REGION=us-east-1",
				"AWS_STS_REGIONAL_ENDPOINTS=legacy",
				"AWS_EC2_METADATA_DISABLED=true",
				"AWS_CONFIG_FILE=~/.aws/config",
				"AWS_SHARED_CREDENTIALS_FILE=~/.aws/credentials",
				"UNSTABLE_MOUNTPOINT_CACHE_KEY=test/cache/key",
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			actual := test.credentials.Env(test.profile)

			slices.Sort(test.expected)
			slices.Sort(actual)

			if !reflect.DeepEqual(actual, test.expected) {
				t.Errorf("Expected %v, but got %v", test.expected, actual)
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
			mounter := &mounter.SystemdMounter{Mounter: mount.NewFakeMounter(test.procMountsContent)}
			isMountPoint, err := mounter.IsMountPoint(test.target)
			assertEquals(t, test.isMountPoint, isMountPoint)
			assertEquals(t, test.expectErr, err != nil)
		})
	}
}
