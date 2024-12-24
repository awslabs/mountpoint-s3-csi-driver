package regionprovider_test

import (
	"errors"
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/regionprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGettingRegionForSTS(t *testing.T) {
	testCases := []struct {
		name           string
		volumeContext  map[string]string
		args           mountpoint.Args
		env            map[string]string
		regionFromIMDS func() (string, error)
		want           string
		wantError      error
	}{
		{
			name:           "region from volume context",
			volumeContext:  map[string]string{volumecontext.STSRegion: "us-west-1"},
			args:           mountpoint.ParseArgs(nil),
			regionFromIMDS: func() (string, error) { return "", nil },
			want:           "us-west-1",
			wantError:      nil,
		},
		{
			name:           "region from bucket region",
			volumeContext:  map[string]string{},
			args:           mountpoint.ParseArgs([]string{"region us-east-1"}),
			regionFromIMDS: func() (string, error) { return "", nil },
			want:           "us-east-1",
			wantError:      nil,
		},
		{
			name:           "region from environment variable",
			volumeContext:  map[string]string{},
			args:           mountpoint.ParseArgs(nil),
			env:            map[string]string{"AWS_REGION": "us-west-2"},
			regionFromIMDS: func() (string, error) { return "", nil },
			want:           "us-west-2",
			wantError:      nil,
		},
		{
			name:           "region from IMDS",
			volumeContext:  map[string]string{},
			args:           mountpoint.ParseArgs(nil),
			regionFromIMDS: func() (string, error) { return "us-east-2", nil },
			want:           "us-east-2",
			wantError:      nil,
		},
		{
			name:           "unknown region",
			volumeContext:  map[string]string{},
			args:           mountpoint.ParseArgs(nil),
			regionFromIMDS: func() (string, error) { return "", nil },
			want:           "",
			wantError:      regionprovider.ErrUnknownRegion,
		},
		{
			name:           "IMDS error",
			volumeContext:  map[string]string{},
			args:           mountpoint.ParseArgs(nil),
			regionFromIMDS: func() (string, error) { return "", errors.New("IMDS error") },
			want:           "",
			wantError:      regionprovider.ErrUnknownRegion,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.env != nil {
				for key, val := range testCase.env {
					t.Setenv(key, val)
				}
			}

			provider := regionprovider.New(testCase.regionFromIMDS)
			region, err := provider.SecurityTokenService(testCase.volumeContext, testCase.args)

			assert.Equals(t, testCase.want, region)
			if testCase.wantError != nil {
				assert.Equals(t, err, testCase.wantError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
