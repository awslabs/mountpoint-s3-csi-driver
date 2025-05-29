package envprovider_test

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
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

func TestProvidingDefaultEnvironmentVariables(t *testing.T) {
	testutil.CleanRegionEnv(t)

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
				"AWS_DEFAULT_REGION=us-east-1",
				"AWS_REGION=us-west-1",
				"AWS_STS_REGIONAL_ENDPOINTS=regional",
			},
		},
		{
			name: "additional env variables shouldn't be passed",
			env: map[string]string{
				"AWS_REGION":       "us-west-1",
				"AWS_MAX_ATTEMPTS": "10",
			},
			want: []string{
				"AWS_REGION=us-west-1",
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			for k, v := range testCase.env {
				t.Setenv(k, v)
			}
			assert.Equals(t, testCase.want, envprovider.Default().List())
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
			env:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_DEFAULT_REGION": "us-east-1"},
			key:  "AWS_REGION",
			want: envprovider.Environment{"AWS_DEFAULT_REGION": "us-east-1"},
		},
		{
			name: "remove non-existing key",
			env:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_DEFAULT_REGION": "us-east-1"},
			key:  "AWS_MAX_ATTEMPTS",
			want: envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_DEFAULT_REGION": "us-east-1"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.env.Delete(testCase.key)
			assert.Equals(t, testCase.want, testCase.env)
		})
	}
}

func TestSettingKeyValueInEnvironmentVariables(t *testing.T) {
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
			key:   "AWS_DEFAULT_REGION",
			value: "us-east-1",
			want:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_DEFAULT_REGION": "us-east-1"},
		},
		{
			name:  "set empty value",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			key:   "AWS_DEFAULT_REGION",
			value: "",
			want:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_DEFAULT_REGION": ""},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.env.Set(testCase.key, testCase.value)
			assert.Equals(t, testCase.want, testCase.env)
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
				"AWS_REGION":                 "us-west-1",
				"AWS_DEFAULT_REGION":         "us-east-1",
				"AWS_STS_REGIONAL_ENDPOINTS": "regional",
			},
			want: []string{
				"AWS_DEFAULT_REGION=us-east-1",
				"AWS_REGION=us-west-1",
				"AWS_STS_REGIONAL_ENDPOINTS=regional",
			},
		},
		{
			name: "environment variables with empty values",
			env: envprovider.Environment{
				"AWS_REGION":         "",
				"AWS_DEFAULT_REGION": "us-east-1",
			},
			want: []string{
				"AWS_DEFAULT_REGION=us-east-1",
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

func TestMergingEnvironments(t *testing.T) {
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
			other: envprovider.Environment{"AWS_DEFAULT_REGION": "us-east-1"},
			want:  envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_DEFAULT_REGION": "us-east-1"},
		},
		{
			name:  "merge with overlapping keys",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1", "AWS_PROFILE": "default"},
			other: envprovider.Environment{"AWS_REGION": "us-east-1", "AWS_DEFAULT_REGION": "us-east-2"},
			want:  envprovider.Environment{"AWS_REGION": "us-east-1", "AWS_PROFILE": "default", "AWS_DEFAULT_REGION": "us-east-2"},
		},
		{
			name:  "merge with empty values",
			env:   envprovider.Environment{"AWS_REGION": "us-west-1"},
			other: envprovider.Environment{"AWS_REGION": "", "AWS_DEFAULT_REGION": "us-east-1"},
			want:  envprovider.Environment{"AWS_REGION": "", "AWS_DEFAULT_REGION": "us-east-1"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.env.Merge(testCase.other)
			assert.Equals(t, testCase.want, testCase.env)
		})
	}
}
