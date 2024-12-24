package envprovider_test

import (
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGettingRegion(t *testing.T) {
	testCases := []struct {
		name             string
		envRegion        string
		envDefaultRegion string
		want             string
	}{
		{
			name:             "both region envs are set",
			envRegion:        "us-west-1",
			envDefaultRegion: "us-east-1",
			want:             "us-west-1",
		},
		{
			name:             "only default region env is set",
			envRegion:        "",
			envDefaultRegion: "us-east-1",
			want:             "us-east-1",
		},
		{
			name:             "no region env is set",
			envRegion:        "",
			envDefaultRegion: "",
			want:             "",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("AWS_REGION", testCase.envRegion)
			t.Setenv("AWS_DEFAULT_REGION", testCase.envDefaultRegion)
			assert.Equals(t, testCase.want, envprovider.Region())
		})
	}
}

func TestProvidingEnvironmentVariables(t *testing.T) {
	testCases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "no env vars set",
			env:  map[string]string{},
			want: []string{},
		},
		{
			name: "some allowed env vars set",
			env: map[string]string{
				"AWS_REGION":                 "us-west-1",
				"AWS_DEFAULT_REGION":         "us-east-1",
				"AWS_STS_REGIONAL_ENDPOINTS": "regional",
				"AWS_MAX_ATTEMPTS":           "10",
			},
			want: []string{
				"AWS_REGION=us-west-1",
				"AWS_DEFAULT_REGION=us-east-1",
				"AWS_STS_REGIONAL_ENDPOINTS=regional",
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			for k, v := range testCase.env {
				t.Setenv(k, v)
			}
			assert.Equals(t, testCase.want, envprovider.Provide())
		})
	}
}

func TestFormattingEnvironmentVariable(t *testing.T) {
	testCases := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{
			name:  "region",
			key:   "AWS_REGION",
			value: "us-west-1",
			want:  "AWS_REGION=us-west-1",
		},
		{
			name:  "role arn",
			key:   "AWS_ROLE_ARN",
			value: "arn:aws:iam::account:role/csi-driver-role-name",
			want:  "AWS_ROLE_ARN=arn:aws:iam::account:role/csi-driver-role-name",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equals(t, testCase.want, envprovider.Format(testCase.key, testCase.value))
		})
	}
}

func TestRemovingAKeyFromListOfEnvironmentVariables(t *testing.T) {
	testCases := []struct {
		name string
		env  envprovider.Environment
		key  string
		want envprovider.Environment
	}{
		{
			name: "empty environment",
			env:  envprovider.Environment{},
			key:  "AWS_REGION",
			want: envprovider.Environment{},
		},
		{
			name: "remove existing key",
			env:  envprovider.Environment{"AWS_REGION=us-west-1", "AWS_DEFAULT_REGION=us-east-1"},
			key:  "AWS_REGION",
			want: envprovider.Environment{"AWS_DEFAULT_REGION=us-east-1"},
		},
		{
			name: "remove existing key with equals sign",
			env:  envprovider.Environment{"AWS_REGION=us-west-1", "AWS_DEFAULT_REGION=us-east-1"},
			key:  "AWS_REGION=",
			want: envprovider.Environment{"AWS_DEFAULT_REGION=us-east-1"},
		},
		{
			name: "remove non-existing key",
			env:  envprovider.Environment{"AWS_REGION=us-west-1", "AWS_DEFAULT_REGION=us-east-1"},
			key:  "AWS_MAX_ATTEMPTS",
			want: envprovider.Environment{"AWS_REGION=us-west-1", "AWS_DEFAULT_REGION=us-east-1"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equals(t, testCase.want, envprovider.Remove(testCase.env, testCase.key))
		})
	}
}
