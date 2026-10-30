package envprovider_test

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
)

// A PV's `mountpointEnv.*` volume attributes become Mountpoint's environment. Only proxy settings are
// allowed there, so a PV cannot point Mountpoint at another CA bundle or endpoint, or turn off signing.
func TestUserEnvCannotOverrideTLSOrEndpoint(t *testing.T) {
	for _, key := range []string{
		"AWS_CA_BUNDLE",
		"AWS_ENDPOINT_URL",
		"AWS_ENDPOINT_URL_S3",
		"AWS_ENDPOINT_URL_STS",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_PROFILE",
	} {
		t.Run(key, func(t *testing.T) {
			env, err := envprovider.ParseUserEnvFromVolumeContext(map[string]string{
				"bucketName":           "test-bucket",
				"mountpointEnv." + key: "http://attacker.example.com",
			})
			if err == nil {
				t.Fatalf("mountpointEnv.%s must be rejected, got environment %v", key, env)
			}
		})
	}
}
