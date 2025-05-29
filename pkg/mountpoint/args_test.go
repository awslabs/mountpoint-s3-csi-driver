package mountpoint_test

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestParsingMountpointArgs(t *testing.T) {
	testCases := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name: "no prefix",
			input: []string{
				"allow-delete",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			want: []string{
				"--allow-delete",
				"--aws-max-attempts=5",
				"--region=us-west-2",
			},
		},
		{
			name: "with prefix",
			input: []string{
				"--cache /tmp/s3-cache",
				"--max-cache-size 500",
				"--metadata-ttl 3",
			},
			want: []string{
				"--cache=/tmp/s3-cache",
				"--max-cache-size=500",
				"--metadata-ttl=3",
			},
		},
		{
			name: "with equals but no prefix",
			input: []string{
				"allow-delete",
				"region=us-west-2",
				"sse=aws:kms",
				"sse-kms-key-id=arn:aws:kms:us-west-2:012345678900:key/00000000-0000-0000-0000-000000000000",
			},
			want: []string{
				"--allow-delete",
				"--region=us-west-2",
				"--sse-kms-key-id=arn:aws:kms:us-west-2:012345678900:key/00000000-0000-0000-0000-000000000000",
				"--sse=aws:kms",
			},
		},
		{
			name: "with equals and prefix",
			input: []string{
				"--allow-other",
				"--uid=1000",
				"--gid=2000",
			},
			want: []string{
				"--allow-other",
				"--gid=2000",
				"--uid=1000",
			},
		},
		{
			name: "with multiple spaces",
			input: []string{
				"--allow-other",
				"--uid    1000",
				"--gid  2000",
			},
			want: []string{
				"--allow-other",
				"--gid=2000",
				"--uid=1000",
			},
		},
		{
			name: "with spaces before and after",
			input: []string{
				"--allow-other  ",
				"  --uid    1000",
				"  --gid  2000  ",
			},
			want: []string{
				"--allow-other",
				"--gid=2000",
				"--uid=1000",
			},
		},
		{
			name: "with single dash prefix",
			input: []string{
				"-d",
				"-l logs/",
			},
			want: []string{
				"-d",
				"-l=logs/",
			},
		},
		{
			name: "mixed prefix and equal signs",
			input: []string{
				"--allow-delete",
				"read-only",
				"--cache=/tmp/s3-cache",
				"--region us-east-1",
				"prefix some-s3-prefix/",
				"-d",
				"-l=logs/",
			},
			want: []string{
				"--allow-delete",
				"--cache=/tmp/s3-cache",
				"--prefix=some-s3-prefix/",
				"--read-only",
				"--region=us-east-1",
				"-d",
				"-l=logs/",
			},
		},
		{
			name: "with duplicated options",
			input: []string{
				"--allow-other",
				"--read-only",
				"read-only",
				"--allow-other",
			},
			want: []string{
				"--allow-other",
				"--read-only",
			},
		},
		{
			name: "with unsupported options",
			input: []string{
				"--allow-other",
				"--read-only",
				"--foreground", "-f",
				"--help", "-h",
				"--version", "-v",
			},
			want: []string{
				"--allow-other",
				"--read-only",
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			args := mountpoint.ParseArgs(testCase.input)
			assert.Equals(t, testCase.want, args.SortedList())
		})
	}
}

func TestInsertingArgsToMountpointArgs(t *testing.T) {
	testCases := []struct {
		name         string
		existingArgs []string
		key          string
		value        string
		want         []string
	}{
		{
			name: "new option",
			existingArgs: []string{
				"allow-delete",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			key: mountpoint.ArgReadOnly,
			want: []string{
				"--allow-delete",
				"--aws-max-attempts=5",
				"--read-only",
				"--region=us-west-2",
			},
		},
		{
			name: "existing option",
			existingArgs: []string{
				"allow-delete",
				"read-only",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			key: mountpoint.ArgReadOnly,
			want: []string{
				"--allow-delete",
				"--aws-max-attempts=5",
				"--read-only",
				"--region=us-west-2",
			},
		},
		{
			name: "new arg",
			existingArgs: []string{
				"allow-delete",
				"aws-max-attempts 5",
			},
			key:   mountpoint.ArgRegion,
			value: "us-west-2",
			want: []string{
				"--allow-delete",
				"--aws-max-attempts=5",
				"--region=us-west-2",
			},
		},
		{
			name: "existing arg",
			existingArgs: []string{
				"allow-delete",
				"read-only",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			key:   mountpoint.ArgRegion,
			value: "us-east-1",
			want: []string{
				"--allow-delete",
				"--aws-max-attempts=5",
				"--read-only",
				"--region=us-east-1",
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			args := mountpoint.ParseArgs(testCase.existingArgs)
			args.Set(testCase.key, testCase.value)
			assert.Equals(t, testCase.want, args.SortedList())
		})
	}
}

func TestInsertingArgsToMountpointArgsIfAbsent(t *testing.T) {
	testCases := []struct {
		name         string
		existingArgs []string
		key          string
		value        string
		want         []string
	}{
		{
			name: "new option",
			existingArgs: []string{
				"allow-delete",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			key: mountpoint.ArgAllowOther,
			want: []string{
				"--allow-delete",
				"--allow-other",
				"--aws-max-attempts=5",
				"--region=us-west-2",
			},
		},
		{
			name: "existing option",
			existingArgs: []string{
				"allow-delete",
				"allow-other",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			key: mountpoint.ArgAllowOther,
			want: []string{
				"--allow-delete",
				"--allow-other",
				"--aws-max-attempts=5",
				"--region=us-west-2",
			},
		},
		{
			name: "new arg",
			existingArgs: []string{
				"allow-delete",
				"aws-max-attempts 5",
			},
			key:   mountpoint.ArgGid,
			value: "555",
			want: []string{
				"--allow-delete",
				"--aws-max-attempts=5",
				"--gid=555",
			},
		},
		{
			name: "existing arg",
			existingArgs: []string{
				"allow-delete",
				"read-only",
				"gid 111",
				"aws-max-attempts 5",
			},
			key:   mountpoint.ArgGid,
			value: "555",
			want: []string{
				"--allow-delete",
				"--aws-max-attempts=5",
				"--gid=111",
				"--read-only",
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			args := mountpoint.ParseArgs(testCase.existingArgs)
			args.SetIfAbsent(testCase.key, testCase.value)
			assert.Equals(t, testCase.want, args.SortedList())
		})
	}
}

func TestExtractingAnArgumentsValueFromMountpointArgs(t *testing.T) {
	testCases := []struct {
		name         string
		args         []string
		argToExtract string
		want         string
		exists       bool
	}{
		{
			name: "existing argument",
			args: []string{
				"cache /tmp/s3-cache",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			argToExtract: mountpoint.ArgCache,
			want:         "/tmp/s3-cache",
			exists:       true,
		},
		{
			name: "existing argument with equal",
			args: []string{
				"cache /tmp/s3-cache",
				"region=us-west-2",
				"aws-max-attempts 5",
			},
			argToExtract: mountpoint.ArgRegion,
			want:         "us-west-2",
			exists:       true,
		},
		{
			name: "existing argument queried without prefix",
			args: []string{
				"cache /tmp/s3-cache",
				"region=us-west-2",
				"aws-max-attempts 5",
			},
			argToExtract: "region",
			want:         "us-west-2",
			exists:       true,
		},
		{
			name: "existing argument queried without equals",
			args: []string{
				"cache /tmp/s3-cache",
				"region=us-west-2",
				"aws-max-attempts 5",
			},
			argToExtract: "--region",
			want:         "us-west-2",
			exists:       true,
		},
		{
			name: "non-existent argument",
			args: []string{
				"cache /tmp/s3-cache",
				"aws-max-attempts 5",
			},
			argToExtract: mountpoint.ArgRegion,
			want:         "",
			exists:       false,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			args := mountpoint.ParseArgs(testCase.args)
			gotValue, gotExists := args.Value(testCase.argToExtract)
			assert.Equals(t, testCase.want, gotValue)
			assert.Equals(t, testCase.exists, gotExists)
		})
	}
}

func TestRemovingAnArgumentFromMountpointArgs(t *testing.T) {
	testCases := []struct {
		name        string
		args        []string
		argToRemove string
		want        string
		argsAfter   []string
		exists      bool
	}{
		{
			name: "existing argument",
			args: []string{
				"user-agent-prefix foo/bar",
				"cache /tmp/s3-cache",
				"region us-west-2",
				"aws-max-attempts 5",
			},
			argToRemove: mountpoint.ArgUserAgentPrefix,
			want:        "foo/bar",
			exists:      true,
			argsAfter: []string{
				"--aws-max-attempts=5",
				"--cache=/tmp/s3-cache",
				"--region=us-west-2",
			},
		},
		{
			name: "existing argument without key prefix",
			args: []string{
				"cache /tmp/s3-cache",
				"region=us-west-2",
				"aws-max-attempts=5",
			},
			argToRemove: "aws-max-attempts",
			want:        "5",
			exists:      true,
			argsAfter: []string{
				"--cache=/tmp/s3-cache",
				"--region=us-west-2",
			},
		},
		{
			name: "non-existent argument",
			args: []string{
				"cache /tmp/s3-cache",
				"aws-max-attempts 5",
			},
			argToRemove: mountpoint.ArgRegion,
			want:        "",
			exists:      false,
			argsAfter: []string{
				"--aws-max-attempts=5",
				"--cache=/tmp/s3-cache",
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			args := mountpoint.ParseArgs(testCase.args)
			gotValue, gotExists := args.Remove(testCase.argToRemove)
			assert.Equals(t, testCase.want, gotValue)
			assert.Equals(t, testCase.exists, gotExists)
			assert.Equals(t, testCase.argsAfter, args.SortedList())
		})
	}
}

func TestQueryingExistenceOfAKeyInMountpointArgs(t *testing.T) {
	args := mountpoint.ParseArgs([]string{
		"--allow-other",
		"--cache /tmp/s3-cache",
		"read-only",
	})

	assert.Equals(t, true, args.Has(mountpoint.ArgAllowOther))
	assert.Equals(t, true, args.Has(mountpoint.ArgCache))
	assert.Equals(t, true, args.Has(mountpoint.ArgReadOnly))
	assert.Equals(t, false, args.Has(mountpoint.ArgAllowRoot))
	assert.Equals(t, false, args.Has(mountpoint.ArgRegion))
}

func TestCreatingMountpointArgsFromAlreadyParsedArgs(t *testing.T) {
	args := mountpoint.ParseArgs([]string{
		"--allow-other",
		"--cache /tmp/s3-cache",
		"read-only",
	})
	args.Set("--user-agent-prefix", "s3-csi-driver/1.11.0 credential-source#pod k8s/v1.30.6-eks-7f9249a")

	want := []string{
		"--allow-other",
		"--cache=/tmp/s3-cache",
		"--read-only",
		"--user-agent-prefix=s3-csi-driver/1.11.0 credential-source#pod k8s/v1.30.6-eks-7f9249a",
	}
	assert.Equals(t, want, args.SortedList())

	parsedArgs := mountpoint.ParseArgs(args.SortedList())
	assert.Equals(t, want, parsedArgs.SortedList())
}
