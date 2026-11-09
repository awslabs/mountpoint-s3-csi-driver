package envprovider_test

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
)

// A PV's `mountpointEnv.*` volume attributes become Mountpoint's environment. Only proxy settings are
// allowed there, so a PV cannot point Mountpoint at another CA bundle or endpoint, or turn off signing.
func TestUserEnvCannotOverrideTLSOrEndpoint(t *testing.T) {
	for _, tc := range []struct{ key, why string }{
		{"AWS_CA_BUNDLE", "it would trust another CA bundle"},
		{"AWS_ENDPOINT_URL", "it would send every request to another endpoint"},
		{"AWS_ENDPOINT_URL_S3", "it would send S3 requests to another endpoint"},
		{"AWS_ENDPOINT_URL_STS", "it would send STS requests to another endpoint"},
		{"SSL_CERT_FILE", "it would trust another certificate file"},
		{"SSL_CERT_DIR", "it would trust another certificate directory"},
		{"AWS_ACCESS_KEY_ID", "it would replace the mount's credentials"},
		{"AWS_SECRET_ACCESS_KEY", "it would replace the mount's credentials"},
		{"AWS_PROFILE", "it would switch to another credentials profile"},
	} {
		key := tc.key
		t.Run("rejects mountpointEnv."+key+", as "+tc.why, func(t *testing.T) {
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
