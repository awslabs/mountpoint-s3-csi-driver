package mounter

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestMounterPodHasCacheVolume(t *testing.T) {
	emptyDir := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	ephemeral := corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}

	for _, testCase := range []struct {
		name     string
		volumes  []corev1.Volume
		expected bool
	}{
		{name: "A pod with only its comm volume has none", volumes: []corev1.Volume{{Name: "comm", VolumeSource: emptyDir}}},
		{name: "An emptyDir named cache is one", volumes: []corev1.Volume{{Name: "comm", VolumeSource: emptyDir}, {Name: "cache", VolumeSource: emptyDir}}, expected: true},
		{name: "An ephemeral volume named cache is one", volumes: []corev1.Volume{{Name: "cache", VolumeSource: ephemeral}}, expected: true},
		// Match by exact name: the chart names it `cache`.
		{name: "A volume whose name only contains cache is not one", volumes: []corev1.Volume{{Name: "cache-2", VolumeSource: emptyDir}}},
		{name: "A pod with no volumes has none"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pod := corev1.Pod{Spec: corev1.PodSpec{Volumes: testCase.volumes}}
			assert.Equals(t, testCase.expected, mounterPodHasCacheVolume(pod))
		})
	}
}

func TestValidateCacheRequest(t *testing.T) {
	const volumeID = "test-volume-id"
	const bothSurfaces = "both `mountOptions` and `volumeAttributes`"

	type testCase struct {
		name            string
		mountOptions    []string
		volumeCtx       map[string]string
		mounterHasCache bool
		expectedCaches  bool
		// expectedErrContains: Rejected with InvalidArgument if set.
		expectedErrContains string
	}

	groups := []struct {
		name  string
		cases []testCase
	}{
		{
			name: "no cache",
			cases: []testCase{
				{
					name:            "caches nothing when no volume attribute or mount option asks for it",
					volumeCtx:       map[string]string{},
					mounterHasCache: true,
					expectedCaches:  false,
				},
				{
					name:            "`cache: Disabled` caches nothing, as disabled is matched in any case",
					volumeCtx:       map[string]string{volumecontext.Cache: "Disabled"},
					mounterHasCache: true,
					expectedCaches:  false,
				},
				{
					name:            "a deprecated attribute alone does not enable the cache",
					volumeCtx:       map[string]string{volumecontext.CacheEmptyDirMedium: "Memory"},
					mounterHasCache: true,
					expectedCaches:  false,
				},
				{
					name:                "rejects an unrecognised cache value, naming it",
					volumeCtx:           map[string]string{volumecontext.Cache: "emptyDrr"},
					mounterHasCache:     true,
					expectedErrContains: `"emptyDrr"`,
				},
			},
		},
		{
			name: "v3 `cache: enabled`",
			cases: []testCase{
				{
					name:            "takes the node's cache",
					volumeCtx:       map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterHasCache: true,
					expectedCaches:  true,
				},
				{
					name:            "`cache: Enabled` takes it too, as enabled is matched in any case",
					volumeCtx:       map[string]string{volumecontext.Cache: "Enabled"},
					mounterHasCache: true,
					expectedCaches:  true,
				},
				{
					name:                "rejects the mount when the node has no cache volume",
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterHasCache:     false,
					expectedErrContains: "has no cache volume",
				},
			},
		},
		{
			name: "v1 `cache` mount option",
			cases: []testCase{
				{
					name:            "opts in and takes the node's cache, whatever path it names",
					mountOptions:    []string{"cache /tmp/customer-path"},
					volumeCtx:       map[string]string{},
					mounterHasCache: true,
					expectedCaches:  true,
				},
				{
					name:                "rejects the mount when the node has no cache volume",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{},
					mounterHasCache:     false,
					expectedErrContains: "has no cache volume",
				},
				{
					name:                "rejects a cache configured with both a mount option and a volume attribute",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterHasCache:     true,
					expectedErrContains: bothSurfaces,
				},
				{
					name:                "rejects `cache: disabled` alongside the mount option, as the two disagree",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheDisabled},
					mounterHasCache:     true,
					expectedErrContains: bothSurfaces,
				},
			},
		},
		{
			name: "v2 cache type",
			cases: []testCase{
				{
					name:            "`cache: emptyDir` takes the node's cache",
					volumeCtx:       map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
					mounterHasCache: true,
					expectedCaches:  true,
				},
				{
					name:            "`cache: ephemeral` takes the node's cache whatever its type",
					volumeCtx:       map[string]string{volumecontext.Cache: volumecontext.CacheTypeEphemeral},
					mounterHasCache: true,
					expectedCaches:  true,
				},
				{
					name: "accepts the v2-only cache and container resource attributes",
					volumeCtx: map[string]string{
						volumecontext.Cache:                                      volumecontext.CacheTypeEmptyDir,
						volumecontext.CacheEmptyDirMedium:                        "Memory",
						volumecontext.CacheEmptyDirSizeLimit:                     "2Gi",
						volumecontext.CacheEphemeralStorageClassName:             "gp3",
						volumecontext.CacheEphemeralStorageResourceRequest:       "10Gi",
						volumecontext.MountpointContainerResourcesRequestsCpu:    "100m",
						volumecontext.MountpointContainerResourcesRequestsMemory: "128Mi",
						volumecontext.MountpointContainerResourcesLimitsCpu:      "500m",
						volumecontext.MountpointContainerResourcesLimitsMemory:   "1Gi",
					},
					mounterHasCache: true,
					expectedCaches:  true,
				},
			},
		},
		{
			name: "max-cache-size",
			cases: []testCase{
				{
					name:            "max-cache-size alone does not opt in, leaving it for Mountpoint to reject",
					mountOptions:    []string{"max-cache-size 1024"},
					volumeCtx:       map[string]string{},
					mounterHasCache: true,
					expectedCaches:  false,
				},
			},
		},
		{
			name: "misc",
			cases: []testCase{
				{
					name:            "an S3 Express cache-xz mount option is not a local cache, so it mounts on a node with no cache volume",
					mountOptions:    []string{"cache-xz test-bucket--usw2-az1--x-s3"},
					volumeCtx:       map[string]string{},
					mounterHasCache: false,
					expectedCaches:  false,
				},
			},
		},
	}

	for _, group := range groups {
		t.Run(group.name, func(t *testing.T) {
			for _, testCase := range group.cases {
				t.Run(testCase.name, func(t *testing.T) {
					args := mountpoint.ParseArgs(testCase.mountOptions)

					caches, err := validateCacheRequest(args, testCase.volumeCtx, volumeID, testCase.mounterHasCache)

					if testCase.expectedErrContains != "" {
						if err == nil {
							t.Fatalf("expected an error, got caches=%v", caches)
						}
						assert.Equals(t, codes.InvalidArgument, status.Code(err))
						assert.Contains(t, err.Error(), testCase.expectedErrContains)
						return
					}
					assert.NoError(t, err)
					assert.Equals(t, testCase.expectedCaches, caches)
				})
			}
		})
	}
}
