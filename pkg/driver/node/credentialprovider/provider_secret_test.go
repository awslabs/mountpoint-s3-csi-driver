package credentialprovider_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestProvideFromSecret(t *testing.T) {
	t.Skip("Internal method testing needs to be refactored to expose a testable function")
}

func TestProvideWithSecretAuthentication(t *testing.T) {
	validSecret := map[string]string{
		"key_id":     "AKIA123456789ABC",
		"access_key": "SECRET123456789ABCDEFGHIJKLMNOPQRSTUV",
	}

	invalidSecret := map[string]string{
		"key_id": "invalid-format",
	}

	tests := []struct {
		name        string
		secretData  map[string]string
		expectError bool
	}{
		{
			name:        "valid credentials",
			secretData:  validSecret,
			expectError: false,
		},
		{
			name:        "invalid credentials",
			secretData:  invalidSecret,
			expectError: true,
		},
		{
			name:        "missing credentials",
			secretData:  map[string]string{},
			expectError: true,
		},
	}

	provider := credentialprovider.New(nil, nil)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provideCtx := credentialprovider.ProvideContext{
				AuthenticationSource: credentialprovider.AuthenticationSourceSecret,
				VolumeID:             "test-volume-id",
				SecretData:           tt.secretData,
			}

			env, authSource, err := provider.Provide(context.Background(), provideCtx)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got nil")
				}
				st, ok := status.FromError(err)
				if !ok {
					t.Errorf("Expected status error but got %v", err)
				}
				if st.Code() != codes.InvalidArgument {
					t.Errorf("Expected InvalidArgument code but got %v", st.Code())
				}
			} else {
				assert.NoError(t, err)
				assert.Equals(t, credentialprovider.AuthenticationSourceSecret, authSource)
				if env == nil {
					t.Errorf("Expected environment to be not nil")
				}
			}
		})
	}
}
