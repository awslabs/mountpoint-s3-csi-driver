package driver_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
)

// validateEndpointURL is a function that mimics the validation in driver.NewDriver
// but can be tested without all the dependencies of the full driver
func validateEndpointURL() error {
	if os.Getenv(envprovider.EnvEndpointURL) == "" {
		return fmt.Errorf("AWS_ENDPOINT_URL environment variable must be set for the CSI driver to function")
	}
	return nil
}

func TestValidatesEndpointURL(t *testing.T) {
	// Save original environment variables to restore them after the test
	originalEndpointURL := os.Getenv(envprovider.EnvEndpointURL)
	defer os.Setenv(envprovider.EnvEndpointURL, originalEndpointURL)

	// Test case 1: No endpoint URL set
	t.Run("fails without endpoint URL", func(t *testing.T) {
		// Clear environment variable
		os.Unsetenv(envprovider.EnvEndpointURL)

		// Attempt to validate without endpoint URL
		err := validateEndpointURL()

		// Verify it fails with the expected error
		if err == nil {
			t.Fatal("Expected validation to fail without AWS_ENDPOINT_URL")
		}
		if !strings.Contains(err.Error(), "AWS_ENDPOINT_URL environment variable must be set") {
			t.Fatalf("Unexpected error message: %v", err)
		}
	})

	// Test case 2: Endpoint URL is set
	t.Run("succeeds with endpoint URL", func(t *testing.T) {
		// Set the environment variable
		os.Setenv(envprovider.EnvEndpointURL, "https://test-endpoint.example.com")

		// Attempt to validate with endpoint URL
		err := validateEndpointURL()

		// Verify it succeeds
		if err != nil {
			t.Fatalf("Unexpected error when AWS_ENDPOINT_URL is set: %v", err)
		}
	})
}
