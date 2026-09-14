package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const gib = 1024 * 1024 * 1024
const mib = 1024 * 1024

func TestResolveMemoryTarget(t *testing.T) {
	testCases := []struct {
		name              string
		memoryBudgetBytes int64
		maxVolumesPerNode int64
		want              int64
		wantErr           bool
	}{
		{
			name:              "2Gi across 4 volumes",
			memoryBudgetBytes: 2 * gib,
			maxVolumesPerNode: 4,
			want:              512,
		},
		{
			name:              "40Gi across 10 volumes",
			memoryBudgetBytes: 40 * gib,
			maxVolumesPerNode: 10,
			want:              4096,
		},
		{
			name:              "single volume gets the whole limit",
			memoryBudgetBytes: 4 * gib,
			maxVolumesPerNode: 1,
			want:              4096,
		},
		{
			name:              "share is floored, never rounded up",
			memoryBudgetBytes: 2000 * mib,
			maxVolumesPerNode: 3,
			want:              666, // 2000Mi / 3 = 666.66..MiB
		},
		{
			name:              "no memory budget means no target",
			maxVolumesPerNode: 4,
			want:              0,
		},
		{
			name:              "no volume limit means no target",
			memoryBudgetBytes: 2 * gib,
			want:              0,
		},
		{
			name:              "negative volume limit means no target",
			memoryBudgetBytes: 2 * gib,
			maxVolumesPerNode: -1,
			want:              0,
		},
		{
			name:              "negative memory budget means no target",
			memoryBudgetBytes: -2 * gib,
			maxVolumesPerNode: 4,
			want:              0,
		},
		{
			name:              "share below Mountpoint's minimum is refused",
			memoryBudgetBytes: 2 * gib,
			maxVolumesPerNode: 10, // 2Gi / 10 = 204 MiB
			wantErr:           true,
		},
		{
			name:              "share just below Mountpoint's minimum is refused",
			memoryBudgetBytes: 5111 * mib,
			maxVolumesPerNode: 10, // 5111Mi / 10 = 511 MiB
			wantErr:           true,
		},
		{
			name:              "sub-MiB share is refused",
			memoryBudgetBytes: 1 * mib,
			maxVolumesPerNode: 4,
			wantErr:           true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveMemoryTarget(tc.memoryBudgetBytes, "limits.memory", tc.maxVolumesPerNode)
			if tc.wantErr {
				if got.err == nil {
					t.Fatal("expected an error for an undersized share")
				}
				return
			}
			assert.NoError(t, got.err)
			assert.Equals(t, tc.want, got.miB)
		})
	}
}

func TestMemoryTargetArg(t *testing.T) {
	t.Run("a sized target is Mountpoint's flag value", func(t *testing.T) {
		arg, err := memoryTarget{miB: 512}.arg()
		assert.NoError(t, err)
		assert.Equals(t, "512", arg)
	})

	t.Run("nothing to divide leaves the flag unset", func(t *testing.T) {
		arg, err := memoryTarget{}.arg()
		assert.NoError(t, err)
		assert.Equals(t, "", arg)
	})

	t.Run("an unavailable target surfaces its reason and no value", func(t *testing.T) {
		reason := errors.New("budget cannot afford every volume")
		arg, err := memoryTarget{err: reason}.arg()
		if !errors.Is(err, reason) {
			t.Fatalf("expected the reason back, got: %v", err)
		}
		assert.Equals(t, "", arg)
	})
}

func TestResolveMemoryTargetErrorMessage(t *testing.T) {
	t.Run("undersized share is an error naming the sizing and the remedy", func(t *testing.T) {
		err := resolveMemoryTarget(2*gib, "limits.memory", 10).err
		if err == nil {
			t.Fatal("expected an error for an undersized share")
		}
		// Asserted on content because this text is the workload pod's only signal, and nothing in the
		// user's config literally says "204".
		for _, want := range []string{"204 MiB", "maxVolumesPerNode=10", "512 MiB", "resources.limits.memory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("undersized error names a requests-only budget as the knob to turn", func(t *testing.T) {
		err := resolveMemoryTarget(2*gib, "requests.memory", 10).err
		if err == nil {
			t.Fatal("expected an error for an undersized share")
		}
		if !strings.Contains(err.Error(), "resources.requests.memory") {
			t.Errorf("error %q does not mention %q", err, "resources.requests.memory")
		}
	})
}

func TestContainerMemoryBudgetBytes(t *testing.T) {
	testCases := []struct {
		name       string
		limitEnv   string
		requestEnv string
		wantBytes  int64
		wantField  string
	}{
		{
			name:      "the limit is the budget when set",
			limitEnv:  "2147483648",
			wantBytes: 2 * gib,
			wantField: "limits.memory",
		},
		{
			name:       "the request is the budget when there is no limit",
			requestEnv: "1073741824",
			wantBytes:  1 * gib,
			wantField:  "requests.memory",
		},
		{
			name:       "the limit wins over the request",
			limitEnv:   "2147483648",
			requestEnv: "1073741824",
			wantBytes:  2 * gib,
			wantField:  "limits.memory",
		},
		{
			name:      "neither set means no budget to divide",
			wantBytes: 0,
			wantField: "",
		},
		{
			// The chart projects plain byte counts, but a hand-written manifest can set a quantity.
			name:      "a Kubernetes quantity is a valid budget",
			limitEnv:  "2Gi",
			wantBytes: 2 * gib,
			wantField: "limits.memory",
		},
		{
			// Ignored rather than fatal: running with no target is recoverable, refusing every mount on
			// the node is not.
			name:       "an unparseable limit falls back to the request",
			limitEnv:   "not-a-number",
			requestEnv: "1073741824",
			wantBytes:  1 * gib,
			wantField:  "requests.memory",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(memoryLimitEnvName, tc.limitEnv)
			t.Setenv(memoryRequestEnvName, tc.requestEnv)
			gotBytes, gotField := containerMemoryBudgetBytes()
			assert.Equals(t, tc.wantBytes, gotBytes)
			assert.Equals(t, tc.wantField, gotField)
		})
	}
}
