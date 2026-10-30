package mounter_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// TLS and endpoint settings Mountpoint would honour from its environment. csi-node's own environment
// may carry any of them (set on the DaemonSet, or inherited), and none may reach a Mountpoint, which
// would otherwise trust another CA, or talk to another (possibly plaintext) endpoint.
var tlsAndEndpointEnv = []string{
	"AWS_CA_BUNDLE",
	"AWS_ENDPOINT_URL",
	"AWS_ENDPOINT_URL_S3",
	"AWS_ENDPOINT_URL_STS",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"HTTPS_PROXY",
	"HTTP_PROXY",
}

// Mountpoint arguments that change where, or whether securely and signed, S3 traffic goes.
var tlsAndEndpointArgs = []string{
	"--endpoint-url",
	mountpoint.ArgCABundle,
	"--no-sign-request",
}

// With nothing in the PV asking for it, the options csi-node sends a Mountpoint carry no TLS, endpoint or
// signing override, so Mountpoint keeps its defaults: HTTPS to the regional S3 endpoint, signed with SigV4.
// TLS itself is negotiated by Mountpoint's CRT client, outside this repository.
func TestDaemonsetMounter_SendsNoTLSOrEndpointOverrideByDefault(t *testing.T) {
	testCtx := setupDM(t)
	for _, key := range tlsAndEndpointEnv {
		t.Setenv(key, "https://attacker.example.com")
	}

	got := testCtx.mountAndServe(testCtx.podUID, nil)

	for _, entry := range got.Env {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains(tlsAndEndpointEnv, key) {
			t.Errorf("csi-node passed its own %s to Mountpoint: %q", key, entry)
		}
	}
	args := mountpoint.ParseArgs(got.Args)
	for _, key := range tlsAndEndpointArgs {
		if value, ok := args.Value(key); ok {
			t.Errorf("csi-node added %s=%q to Mountpoint's arguments", key, value)
		}
	}
}

// csi-node hands credentials to the mounter in the options it sends; at the most verbose level it must
// log that it sent them, never what they are. The provider here returns the credentials in the
// environment, the most exposed place it could put them, so nothing short of logging the options hides them.
func TestDaemonsetMounter_MountLogsNoCredentials(t *testing.T) {
	const (
		accessKeyID     = "AKIAIOSFODNN7EXAMPLE"
		secretAccessKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		sessionToken    = "IQoJb3JpZ2luX2VjEXAMPLESESSIONTOKEN"
	)
	testCtx := setupDM(t)
	provider := mock_credentialprovider.NewMockProviderInterface(gomock.NewController(t))
	provider.EXPECT().Provide(gomock.Any(), gomock.Any()).Return(envprovider.Environment{
		envprovider.EnvAccessKeyID:     accessKeyID,
		envprovider.EnvSecretAccessKey: secretAccessKey,
		envprovider.EnvSessionToken:    sessionToken,
	}, credentialprovider.AuthenticationSourceDriver, nil).AnyTimes()
	provider.EXPECT().Cleanup(gomock.Any()).Return(nil).AnyTimes()
	testCtx.useCredProvider(provider)

	logs := testutil.CaptureKlog(t, 10)
	got := testCtx.mountAndServe(testCtx.podUID, nil)
	assert.NoError(t, testCtx.dm.Unmount(testCtx.ctx, testCtx.targetPath(testCtx.podUID), credentialprovider.CleanupContext{
		WritePath: testCtx.commDir,
		PodID:     testCtx.podUID,
		VolumeID:  testCtx.volumeID,
		MountKind: credentialprovider.MountKindDaemonset,
	}))

	// Guards the check below: had the credentials not reached the options, there would be nothing to leak.
	if !slices.Contains(got.Env, envprovider.EnvSecretAccessKey+"="+secretAccessKey) {
		t.Fatalf("expected the provider's credentials in the options sent to the mounter, got %v", got.Env)
	}
	testutil.AssertNoCredentialsLogged(t, logs(), accessKeyID, secretAccessKey, sessionToken)
}
