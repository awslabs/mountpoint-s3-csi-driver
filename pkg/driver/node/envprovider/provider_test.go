package envprovider_test

import (
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestDefault(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		want envprovider.Environment
	}{
		{
			name: "no env vars set",
			env:  map[string]string{},
			want: envprovider.Environment{},
		},
		{
			name: "only region env set",
			env: map[string]string{
				"AWS_REGION": "us-west-1",
			},
			want: envprovider.Environment{
				"AWS_REGION": "us-west-1",
			},
		},
		{
			name: "region and additional non-allowed env vars set",
			env: map[string]string{
				"AWS_REGION":       "us-west-1",
				"AWS_MAX_ATTEMPTS": "10",
			},
			want: envprovider.Environment{
				"AWS_REGION": "us-west-1",
			},
		},
		{
			name: "s3 endpoint url env var set",
			env: map[string]string{
				"AWS_REGION":       "us-west-1",
				"AWS_ENDPOINT_URL": "https://custom-endpoint.example.com",
			},
			want: envprovider.Environment{
				"AWS_REGION":       "us-west-1",
				"AWS_ENDPOINT_URL": "https://custom-endpoint.example.com",
			},
		},
		{
			name: "only endpoint url env var set",
			env: map[string]string{
				"AWS_ENDPOINT_URL": "https://custom-endpoint.example.com",
			},
			want: envprovider.Environment{
				"AWS_ENDPOINT_URL": "https://custom-endpoint.example.com",
			},
		},
		{
			name: "empty region value",
			env: map[string]string{
				"AWS_REGION": "",
			},
			want: envprovider.Environment{},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Clear environment variables before setting test values
			t.Setenv("AWS_REGION", "")
			t.Setenv("AWS_ENDPOINT_URL", "")

			// Set environment variables for this test case
			for k, v := range testCase.env {
				t.Setenv(k, v)
			}

			// Test Default() method directly
			assert.Equals(t, testCase.want, envprovider.Default())
		})
	}
}

func TestEnvironmentList(t *testing.T) {
	testCases := []struct {
		name string
		env  envprovider.Environment
		want []string
	}{
		{
			name: "empty environment",
			env:  envprovider.Environment{},
			want: []string{},
		},
		{
			name: "single environment variable",
			env:  envprovider.Environment{"AWS_REGION": "us-west-1"},
			want: []string{"AWS_REGION=us-west-1"},
		},
		{
			name: "multiple environment variables are sorted",
			env: envprovider.Environment{
				"AWS_REGION":       "us-west-1",
				"AWS_ENDPOINT_URL": "https://example.com",
			},
			want: []string{
				"AWS_ENDPOINT_URL=https://example.com",
				"AWS_REGION=us-west-1",
			},
		},
		{
			name: "environment variables with empty values",
			env: envprovider.Environment{
				"AWS_REGION": "",
			},
			want: []string{
				"AWS_REGION=",
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equals(t, testCase.want, testCase.env.List())
		})
	}
}

func TestEnvironmentDelete(t *testing.T) {
	testCases := []struct {
		name string
		env  envprovider.Environment
		key  string
		want envprovider.Environment
	}{
		{
			name: "delete from empty environment",
			env:  envprovider.Environment{},
			key:  "AWS_REGION",
			want: envprovider.Environment{},
		},
		{
			name: "delete existing key",
			env:  envprovider.Environment{"AWS_REGION": "us-west-1"},
			key:  "AWS_REGION",
			want: envprovider.Environment{},
		},
		{
			name: "delete non-existing key",
			env:  envprovider.Environment{"AWS_REGION": "us-west-1"},
			key:  "AWS_MAX_ATTEMPTS",
			want: envprovider.Environment{"AWS_REGION": "us-west-1"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.env.Delete(testCase.key)
			assert.Equals(t, testCase.want, testCase.env)
		})
	}
}

func TestEnvironmentSet(t *testing.T) {
	testCases := []struct {
		name  string
		env   envprovider.Environment
		key   string
		value string
		want  envprovider.Environment
	}{
		{
			name:  "add to empty environment",
			env:   envprovider.Environment{},
			key:   "AWS_REGION",
			value: "us-west-1",
			want:  envprovider.Environment{"AWS_REGION": "us-west-1"},
		},
		{
			name:  "update existing key",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			key:   "AWS_REGION",
			value: "us-east-1",
			want:  envprovider.Environment{"AWS_REGION": "us-east-1"},
		},
		{
			name:  "add new key to non-empty environment",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			key:   "AWS_PROFILE",
			value: "default",
			want:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_PROFILE": "default"},
		},
		{
			name:  "set empty value",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			key:   "AWS_PROFILE",
			value: "",
			want:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_PROFILE": ""},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.env.Set(testCase.key, testCase.value)
			assert.Equals(t, testCase.want, testCase.env)
		})
	}
}

func TestEnvironmentMerge(t *testing.T) {
	testCases := []struct {
		name  string
		env   envprovider.Environment
		other envprovider.Environment
		want  envprovider.Environment
	}{
		{
			name:  "merge into empty environment",
			env:   envprovider.Environment{},
			other: envprovider.Environment{"AWS_REGION": "us-west-1"},
			want:  envprovider.Environment{"AWS_REGION": "us-west-1"},
		},
		{
			name:  "merge empty environment",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			other: envprovider.Environment{},
			want:  envprovider.Environment{"AWS_REGION": "us-west-1"},
		},
		{
			name:  "merge with different keys",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			other: envprovider.Environment{"AWS_PROFILE": "default"},
			want:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_PROFILE": "default"},
		},
		{
			name:  "merge with overlapping keys",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_PROFILE": "default"},
			other: envprovider.Environment{"AWS_REGION": "us-east-1", "AWS_ENDPOINT_URL": "https://example.com"},
			want:  envprovider.Environment{"AWS_REGION": "us-east-1", "AWS_PROFILE": "default", "AWS_ENDPOINT_URL": "https://example.com"},
		},
		{
			name:  "merge with empty values",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			other: envprovider.Environment{"AWS_REGION": ""},
			want:  envprovider.Environment{"AWS_REGION": ""},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.env.Merge(testCase.other)
			assert.Equals(t, testCase.want, testCase.env)
		})
	}
}
