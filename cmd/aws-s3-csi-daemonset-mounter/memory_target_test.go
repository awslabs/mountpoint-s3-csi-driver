package main

import (
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const gib = 1024 * 1024 * 1024
const mib = 1024 * 1024

func TestParseMemoryLimitStrategy(t *testing.T) {
	testCases := []struct {
		name    string
		value   string
		want    memoryLimitStrategy
		wantErr bool
	}{
		{name: "equalSplit", value: "equalSplit", want: memoryLimitEqualSplit},
		{name: "none", value: "none", want: memoryLimitNone},
		{name: "an empty value is not a strategy", value: "", wantErr: true},
		{name: "the match is case sensitive", value: "equalsplit", wantErr: true},
		{name: "an unknown strategy is refused", value: "proportional", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMemoryLimitStrategy(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected %q to be refused, got strategy %q", tc.value, got)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, tc.want, got)
		})
	}
}

func TestNewMemoryLimitEqualSplit(t *testing.T) {
	testCases := []struct {
		name              string
		requestBytes      int64
		maxVolumesPerNode int64
		wantShareMiB      int64
		wantErr           bool
	}{
		{
			name:              "4Gi across 4 volumes",
			requestBytes:      4 * gib,
			maxVolumesPerNode: 4,
			wantShareMiB:      1008, // (4096 - 64) / 4
		},
		{
			name:              "40Gi across 10 volumes",
			requestBytes:      40 * gib,
			maxVolumesPerNode: 10,
			wantShareMiB:      4089, // (40960 - 64) / 10
		},
		{
			name:              "a single volume gets the whole request less the overhead",
			requestBytes:      4 * gib,
			maxVolumesPerNode: 1,
			wantShareMiB:      4032,
		},
		{
			name:              "the share is floored, never rounded up",
			requestBytes:      2000 * mib,
			maxVolumesPerNode: 3,
			wantShareMiB:      645, // 1936 / 3 = 645.33..
		},
		{
			name:              "a share below Mountpoint's minimum refuses every mount",
			requestBytes:      2 * gib,
			maxVolumesPerNode: 4,
			wantErr:           true, // 1984 / 4 = 496, under 512
		},
		{
			name:              "no memory request leaves nothing to divide",
			maxVolumesPerNode: 4,
			wantErr:           true,
		},
		{
			name:              "a negative memory request leaves nothing to divide",
			requestBytes:      -2 * gib,
			maxVolumesPerNode: 4,
			wantErr:           true,
		},
		{
			name:         "no volume limit leaves nothing to divide by",
			requestBytes: 4 * gib,
			wantErr:      true,
		},
		{
			name:              "a negative volume limit leaves nothing to divide by",
			requestBytes:      4 * gib,
			maxVolumesPerNode: -1,
			wantErr:           true,
		},
		{
			name:              "a request at the mounter's own overhead leaves nothing",
			requestBytes:      64 * mib,
			maxVolumesPerNode: 4,
			wantErr:           true,
		},
		{
			name:              "a request below the mounter's own overhead leaves nothing",
			requestBytes:      32 * mib,
			maxVolumesPerNode: 4,
			wantErr:           true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newMemoryLimit(memoryLimitEqualSplit, tc.requestBytes, tc.maxVolumesPerNode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a configuration that cannot host a mount to be refused, got share %d", got.shareMiB)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, memoryLimitEqualSplit, got.strategy)
			assert.Equals(t, tc.wantShareMiB, got.shareMiB)
		})
	}
}

func TestNewMemoryLimitNone(t *testing.T) {
	// `none` never sizes a mount, so no combination of inputs can misconfigure it.
	testCases := []struct {
		name              string
		requestBytes      int64
		maxVolumesPerNode int64
	}{
		{name: "with a request and a volume limit", requestBytes: 4 * gib, maxVolumesPerNode: 4},
		{name: "with no volume limit", requestBytes: 4 * gib},
		{name: "with no memory request", maxVolumesPerNode: 4},
		{name: "with neither"},
		{name: "with a request too small to host anything", requestBytes: 32 * mib, maxVolumesPerNode: 4},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newMemoryLimit(memoryLimitNone, tc.requestBytes, tc.maxVolumesPerNode)
			assert.NoError(t, err)
			assert.Equals(t, memoryLimitNone, got.strategy)
			assert.Equals(t, int64(0), got.shareMiB)
		})
	}
}

// mustMemoryLimit resolves a configuration the test expects to be valid. An unusable one never produces
// a memoryLimit at all — the mounter exits instead, which TestNewMemoryLimitEqualSplit covers.
func mustMemoryLimit(t *testing.T, strategy memoryLimitStrategy, requestBytes, maxVolumesPerNode int64) memoryLimit {
	t.Helper()
	limit, err := newMemoryLimit(strategy, requestBytes, maxVolumesPerNode)
	assert.NoError(t, err)
	return limit
}

func TestMemoryLimitTargetFor(t *testing.T) {
	testCases := []struct {
		name  string
		limit memoryLimit
		args  []string
		want  int64
	}{
		{
			name:  "equalSplit sizes a volume that declares nothing",
			limit: mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4),
			want:  1008,
		},
		{
			name:  "equalSplit ignores a lower target from the PV",
			limit: mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4),
			args:  []string{"--memory-target=512"},
			want:  1008,
		},
		{
			name:  "equalSplit ignores a higher target from the PV",
			limit: mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4),
			args:  []string{"--memory-target=8192"},
			want:  1008,
		},
		{
			// Nothing parses the value under equalSplit, so a typo cannot fail the mount either.
			name:  "equalSplit ignores an unparseable target from the PV",
			limit: mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4),
			args:  []string{"--memory-target=1Gi"},
			want:  1008,
		},
		{
			name:  "none leaves a volume that declares nothing unsized",
			limit: mustMemoryLimit(t, memoryLimitNone, 4*gib, 4),
			want:  0,
		},
		{
			name:  "none leaves a declared target untouched",
			limit: mustMemoryLimit(t, memoryLimitNone, 4*gib, 4),
			args:  []string{"--memory-target=8192"},
			want:  0,
		},
		{
			// `none` makes no promises about the value, so Mountpoint's own CLI rejects a typo.
			name:  "none passes an unparseable target through to Mountpoint",
			limit: mustMemoryLimit(t, memoryLimitNone, 0, 0),
			args:  []string{"--memory-target=1Gi"},
			want:  0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equals(t, tc.want, tc.limit.targetFor("vol-test", mountpoint.ParseArgs(tc.args)))
		})
	}
}

// Asserted on content because this text is what the operator who owns the config sees in the mounter's
// crash-loop, and is the only place the arithmetic is spelled out.
func TestNewMemoryLimitErrorMessages(t *testing.T) {
	testCases := []struct {
		name              string
		requestBytes      int64
		maxVolumesPerNode int64
		wantMentions      []string
	}{
		{
			name:              "an undersized share names the arithmetic and every remedy",
			requestBytes:      2 * gib,
			maxVolumesPerNode: 4,
			wantMentions: []string{
				"1984", "496", "512", "maxVolumesPerNode=4",
				"--memory-target", "resources.requests.memory",
			},
		},
		{
			name:              "a missing memory request names the strategy and the way out of it",
			maxVolumesPerNode: 4,
			wantMentions:      []string{"equalSplit", "resources.requests.memory", "memoryLimitStrategy=none"},
		},
		{
			name:         "a missing volume limit names the strategy and the way out of it",
			requestBytes: 4 * gib,
			wantMentions: []string{"equalSplit", "maxVolumesPerNode", "memoryLimitStrategy=none"},
		},
		{
			name:              "a request too small to host anything names the overhead",
			requestBytes:      32 * mib,
			maxVolumesPerNode: 4,
			wantMentions:      []string{"64 MiB", "resources.requests.memory"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newMemoryLimit(memoryLimitEqualSplit, tc.requestBytes, tc.maxVolumesPerNode)
			if err == nil {
				t.Fatal("expected the configuration to be refused")
			}
			for _, want := range tc.wantMentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestContainerMemoryRequestBytes(t *testing.T) {
	testCases := []struct {
		name       string
		requestEnv string
		wantBytes  int64
	}{
		{
			name:       "the chart projects a plain byte count",
			requestEnv: "4294967296",
			wantBytes:  4 * gib,
		},
		{
			// The chart projects plain byte counts, but a hand-written manifest can set a quantity.
			name:       "a Kubernetes quantity is also a valid request",
			requestEnv: "4Gi",
			wantBytes:  4 * gib,
		},
		{
			name:      "no request means no budget to divide",
			wantBytes: 0,
		},
		{
			// Ignored rather than fatal: under `none` there is nothing to divide anyway, and under
			// equalSplit the resulting refusal names the request, which a parse panic would not.
			name:       "an unparseable request is ignored",
			requestEnv: "not-a-number",
			wantBytes:  0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(memoryRequestEnvName, tc.requestEnv)
			assert.Equals(t, tc.wantBytes, containerMemoryRequestBytes())
		})
	}
}
