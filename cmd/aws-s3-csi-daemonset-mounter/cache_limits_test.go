package main

import (
	"bytes"
	"strings"
	"testing"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestParseCacheLimitStrategy(t *testing.T) {
	testCases := []struct {
		name    string
		value   string
		want    cacheLimitStrategy
		wantErr bool
	}{
		{name: "equalSplit", value: "equalSplit", want: cacheLimitEqualSplit},
		{name: "none", value: "none", want: cacheLimitNone},
		{name: "an empty value is not a strategy", value: "", wantErr: true},
		{name: "the match is case sensitive", value: "equalsplit", wantErr: true},
		{name: "an unknown strategy is refused", value: "proportional", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCacheLimitStrategy(tc.value)
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

func TestNewCacheLimitEqualSplit(t *testing.T) {
	testCases := []struct {
		name              string
		capacityBytes     int64
		maxVolumesPerNode int64
		wantShareMiB      int64
		wantErr           bool
	}{
		{
			name:              "4Gi across 4 volumes",
			capacityBytes:     4 * gib,
			maxVolumesPerNode: 4,
			wantShareMiB:      972, // 4096 * 95% / 4
		},
		{
			name:              "a single volume gets the whole cache volume minus the margin",
			capacityBytes:     4 * gib,
			maxVolumesPerNode: 1,
			wantShareMiB:      3891, // 4096 * 95%
		},
		{
			name:              "the share is floored, never rounded up",
			capacityBytes:     2000 * mib,
			maxVolumesPerNode: 3,
			wantShareMiB:      633, // 1900 / 3 = 633.33..
		},
		{
			// Multiple after division so integer conversion floor applied 1 time only.
			name:              "the margin is applied before flooring to MiB",
			capacityBytes:     1536 * 1024,
			maxVolumesPerNode: 1,
			wantShareMiB:      1, // 1.5 MiB * 95% = 1.425 MiB
		},
		{
			name:              "floor function is applied so 1 mib * 95% goes to 0",
			capacityBytes:     1 * mib,
			maxVolumesPerNode: 1,
			wantShareMiB:      0,
		},
		{
			// Served uncached with --max-cache-size=0: exiting would take down every mount on the node, and an
			// unbounded cache is what equalSplit exists to prevent, so we don't fail for this case.
			name:              "no cache volume size serves every mount uncached",
			maxVolumesPerNode: 4,
			wantShareMiB:      0,
		},
		{
			name:              "a cache volume too small for one whole MiB each serves every mount uncached",
			capacityBytes:     1 * mib,
			maxVolumesPerNode: 4,
			wantShareMiB:      0,
		},
		{
			name:              "a negative cache volume size is refused",
			capacityBytes:     -2 * gib,
			maxVolumesPerNode: 4,
			wantErr:           true,
		},
		{
			name:          "no volume limit leaves nothing to divide by",
			capacityBytes: 4 * gib,
			wantErr:       true,
		},
		{
			name:              "a negative volume limit leaves nothing to divide by",
			capacityBytes:     4 * gib,
			maxVolumesPerNode: -1,
			wantErr:           true,
		},
		{
			name:    "no volume limit is refused even with no cache volume size",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newCacheLimit(cacheLimitEqualSplit, tc.capacityBytes, tc.maxVolumesPerNode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a configuration that cannot host a mount to be refused, got share %d", got.shareMiB)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, cacheLimitEqualSplit, got.strategy)
			assert.Equals(t, tc.wantShareMiB, got.shareMiB)
		})
	}
}

func TestNewCacheLimitNone(t *testing.T) {
	// `none` never sizes a mount, so no combination of inputs can misconfigure it.
	testCases := []struct {
		name              string
		capacityBytes     int64
		maxVolumesPerNode int64
	}{
		{name: "with a cache volume and a volume limit", capacityBytes: 4 * gib, maxVolumesPerNode: 4},
		{name: "with no volume limit", capacityBytes: 4 * gib},
		{name: "with no cache volume", maxVolumesPerNode: 4},
		{name: "with neither"},
		{name: "with a cache volume too small to share", capacityBytes: 1 * mib, maxVolumesPerNode: 4},
		{name: "with a negative cache volume size", capacityBytes: -2 * gib, maxVolumesPerNode: 4},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newCacheLimit(cacheLimitNone, tc.capacityBytes, tc.maxVolumesPerNode)
			assert.NoError(t, err)
			assert.Equals(t, cacheLimitNone, got.strategy)
			assert.Equals(t, int64(0), got.shareMiB)
		})
	}
}

// mustCacheLimit resolves a configuration the test expects to be valid. An unusable one never produces
// a cacheLimit at all — the mounter exits instead, which TestNewCacheLimitEqualSplit covers.
func mustCacheLimit(t *testing.T, strategy cacheLimitStrategy, capacityBytes, maxVolumesPerNode int64) cacheLimit {
	t.Helper()
	limit, err := newCacheLimit(strategy, capacityBytes, maxVolumesPerNode)
	assert.NoError(t, err)
	return limit
}

func TestCacheLimitMaxCacheSizeFor(t *testing.T) {
	const cacheArg = "--cache=/cache/vol-test"

	testCases := []struct {
		name   string
		limit  cacheLimit
		args   []string
		want   int64
		wantOK bool
	}{
		{
			name:   "equalSplit sizes a volume that declares nothing",
			limit:  mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4),
			args:   []string{cacheArg},
			want:   972,
			wantOK: true,
		},
		{
			name:   "equalSplit ignores a lower size from the PV",
			limit:  mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4),
			args:   []string{cacheArg, "--max-cache-size=100"},
			want:   972,
			wantOK: true,
		},
		{
			name:   "equalSplit ignores a higher size from the PV",
			limit:  mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4),
			args:   []string{cacheArg, "--max-cache-size=8192"},
			want:   972,
			wantOK: true,
		},
		{
			name:   "equalSplit ignores an unparseable size from the PV",
			limit:  mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4),
			args:   []string{cacheArg, "--max-cache-size=1Gi"}, // we take MiB number only, but we don't parse it
			want:   972,
			wantOK: true,
		},
		{
			// Mountpoint refuses --max-cache-size without --cache, so sizing it would fail the mount later.
			name:  "equalSplit leaves a volume that does not cache unsized",
			limit: mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4),
			want:  0,
		},
		{
			name:   "equalSplit with no cache volume size serves a cached volume uncached",
			limit:  mustCacheLimit(t, cacheLimitEqualSplit, 0, 4),
			args:   []string{cacheArg, "--max-cache-size=8192"},
			want:   0,
			wantOK: true,
		},
		{
			name:  "none leaves a volume that declares nothing unsized",
			limit: mustCacheLimit(t, cacheLimitNone, 4*gib, 4),
			args:  []string{cacheArg},
			want:  0,
		},
		{
			name:  "none leaves a declared size untouched",
			limit: mustCacheLimit(t, cacheLimitNone, 4*gib, 4),
			args:  []string{cacheArg, "--max-cache-size=8192"},
			want:  0,
		},
		{
			name:  "none passes an unparseable size through to Mountpoint",
			limit: mustCacheLimit(t, cacheLimitNone, 4*gib, 4),
			args:  []string{cacheArg, "--max-cache-size=1Gi"},
			want:  0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.limit.maxCacheSizeFor("vol-test", mountpoint.ParseArgs(tc.args))
			assert.Equals(t, tc.want, got)
			assert.Equals(t, tc.wantOK, ok)
		})
	}
}

// captureKlog redirects klog's output into the returned buffer for the rest of t. SetOutput alone
// captures nothing: logtostderr defaults true and klog short-circuits to os.Stderr before the sink.
// The sink is process-global, so no test that uses this may call t.Parallel.
func captureKlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&buf)
	t.Cleanup(func() {
		klog.SetOutput(nil)
		klog.LogToStderr(true)
	})
	return &buf
}

// The warnings are what tells an operator a PV's own size was overridden, or that nothing bounds it.
func TestCacheLimitWarnings(t *testing.T) {
	const cacheArg = "--cache=/cache/vol-test"

	testCases := []struct {
		name     string
		newLimit func(t *testing.T) cacheLimit
		args     []string
		wantWarn string // empty means no warning may fire
	}{
		{
			name:     "equalSplit names the PV's size it ignores",
			newLimit: func(t *testing.T) cacheLimit { return mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4) },
			args:     []string{cacheArg, "--max-cache-size=8192"},
			wantWarn: "Ignoring --max-cache-size=8192 for volume vol-test",
		},
		{
			name:     "equalSplit does not warn when the PV sets no size",
			newLimit: func(t *testing.T) cacheLimit { return mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4) },
			args:     []string{cacheArg},
		},
		{
			name:     "none says nothing bounds a cached volume that sets no size",
			newLimit: func(t *testing.T) cacheLimit { return mustCacheLimit(t, cacheLimitNone, 4*gib, 4) },
			args:     []string{cacheArg},
			wantWarn: "so nothing bounds its share of the cache volume",
		},
		{
			name:     "none does not warn when the PV bounds its own cache",
			newLimit: func(t *testing.T) cacheLimit { return mustCacheLimit(t, cacheLimitNone, 4*gib, 4) },
			args:     []string{cacheArg, "--max-cache-size=1024"},
		},
		{
			name:     "a volume that does not cache is never warned about",
			newLimit: func(t *testing.T) cacheLimit { return mustCacheLimit(t, cacheLimitNone, 4*gib, 4) },
		},
		{
			name:     "equalSplit with no cache volume size says every mount is served uncached",
			newLimit: func(t *testing.T) cacheLimit { return mustCacheLimit(t, cacheLimitEqualSplit, 0, 4) },
			args:     []string{cacheArg},
			wantWarn: "so every mount is served uncached with --max-cache-size=0",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureKlog(t)
			tc.newLimit(t).maxCacheSizeFor("vol-test", mountpoint.ParseArgs(tc.args))
			if tc.wantWarn == "" {
				// klog prefixes each line with its severity; newCacheLimit's own info line is expected.
				for _, line := range strings.Split(logs.String(), "\n") {
					if strings.HasPrefix(line, "W") {
						t.Errorf("expected no warning, got: %s", line)
					}
				}
				return
			}
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, tc.wantWarn) {
					assert.Equals(t, true, strings.HasPrefix(line, "W"))
					return
				}
			}
			t.Errorf("expected a warning containing %q, got:\n%s", tc.wantWarn, logs.String())
		})
	}
}

// Asserted on content because this text is what the operator who owns the config sees in the mounter's
// crash-loop, and is the only place the arithmetic is spelled out.
func TestNewCacheLimitErrorMessages(t *testing.T) {
	testCases := []struct {
		name              string
		capacityBytes     int64
		maxVolumesPerNode int64
		wantMentions      []string
	}{
		{
			name:          "a missing volume limit names the strategy and the way out of it",
			capacityBytes: 4 * gib,
			wantMentions:  []string{"equalSplit", "maxVolumesPerNode", "cache.limitStrategy=none"},
		},
		{
			name:              "a negative cache volume size names it and the values that set it",
			capacityBytes:     -2 * gib,
			maxVolumesPerNode: 4,
			wantMentions:      []string{"-2147483648 bytes", "emptyDir.sizeLimit", "ephemeral.resourceRequests"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newCacheLimit(cacheLimitEqualSplit, tc.capacityBytes, tc.maxVolumesPerNode)
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

func TestTmpfsCacheBytes(t *testing.T) {
	assert.Equals(t, int64(2*gib), tmpfsCacheBytes("Memory", 2*gib))
	assert.Equals(t, int64(0), tmpfsCacheBytes("", 2*gib))
}

func TestCacheCapacityBytes(t *testing.T) {
	testCases := []struct {
		name            string
		capacityEnv     string
		wantBytes       int64
		wantErrContains string
	}{
		{
			name:        "the chart templates the operator's own quantity",
			capacityEnv: "4Gi",
			wantBytes:   4 * gib,
		},
		{
			name:        "a plain byte count is also a valid quantity",
			capacityEnv: "4294967296",
			wantBytes:   4 * gib,
		},
		{
			name:      "no cache volume means no capacity to divide",
			wantBytes: 0,
		},
		{
			name:            "an unparseable capacity is refused",
			capacityEnv:     "not-a-number",
			wantErrContains: `MOUNTER_CACHE_CAPACITY="not-a-number" is not a parseable Kubernetes quantity`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(cacheCapacityEnvName, tc.capacityEnv)
			got, err := cacheCapacityBytes()
			if tc.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected %q to be refused, got %d bytes", tc.capacityEnv, got)
				}
				assert.Contains(t, err.Error(), tc.wantErrContains)
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, tc.wantBytes, got)
		})
	}
}
