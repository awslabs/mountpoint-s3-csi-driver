package mounter

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestResolveCacheTypeAndDir(t *testing.T) {
	const mounterDir = "/var/lib/kubelet/pods/mounter-uid"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "s3-csi-daemonset-mounter-abcde", Namespace: "kube-system"}}
	emptyDirPath := filepath.Join(mounterDir, volumesSubdir, emptyDirVolumesSubdir, CacheVolumeName)

	comm := corev1.Volume{Name: CommVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	withCacheVolume := func(source corev1.VolumeSource) []corev1.Volume {
		return []corev1.Volume{comm, {Name: CacheVolumeName, VolumeSource: source}}
	}

	boundPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name + "-" + CacheVolumeName, Namespace: pod.Namespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "my-static-cache-pv"},
	}
	unboundPVC := boundPVC.DeepCopy()
	unboundPVC.Spec.VolumeName = ""

	testCases := []struct {
		name            string
		volumes         []corev1.Volume
		objects         []runtime.Object
		wantType        CacheType
		wantDir         string
		wantErrContains string
	}{
		{
			name:     "a pod with no volumes at all",
			wantType: CacheNone,
		},
		{
			name:     "a pod with only a comm volume",
			volumes:  []corev1.Volume{comm},
			wantType: CacheNone,
		},
		{
			name:     "a hostPath volume that happens to be named cache",
			volumes:  withCacheVolume(corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/mnt/cache"}}),
			wantType: CacheNone,
		},
		{
			name:     "a disk-backed emptyDir cache volume",
			volumes:  withCacheVolume(corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}),
			wantType: CacheEmptyDirDisk,
			wantDir:  emptyDirPath,
		},
		{
			name:     "a tmpfs emptyDir cache volume",
			volumes:  withCacheVolume(corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}),
			wantType: CacheEmptyDirMemory,
			wantDir:  emptyDirPath,
		},
		{
			name:     "an ephemeral volume resolves through the PVC the kubelet bound",
			volumes:  withCacheVolume(corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}),
			objects:  []runtime.Object{boundPVC},
			wantType: CacheEphemeral,
			wantDir:  filepath.Join(mounterDir, volumesSubdir, csiVolumesSubdir, "my-static-cache-pv", "mount"),
		},
		{
			name:            "an ephemeral volume with no PVC at all is an error",
			volumes:         withCacheVolume(corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}),
			wantErrContains: "failed to get cache volume PVC",
		},
		{
			name:            "an ephemeral volume whose PVC is not bound is an error",
			volumes:         withCacheVolume(corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}),
			objects:         []runtime.Object{unboundPVC},
			wantErrContains: "is not bound",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			pod := pod.DeepCopy()
			pod.Spec.Volumes = testCase.volumes

			gotType, gotDir, err := resolveCacheTypeAndDir(context.Background(),
				fake.NewSimpleClientset(testCase.objects...), pod, mounterDir)

			if testCase.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %s at %q", testCase.wantErrContains, gotType, gotDir)
				}
				assert.Contains(t, err.Error(), testCase.wantErrContains)
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, testCase.wantType, gotType)
			assert.Equals(t, testCase.wantDir, gotDir)
		})
	}
}

func TestConfigureCache(t *testing.T) {
	// Note we also assert --cache=/cache/<volumeID> instead of MountOptionCacheDir
	const volumeID = "test-volume-id"
	const cacheArg = "--cache=/cache/" + volumeID
	const deprecatedAttrs = "sets the deprecated volume attributes "

	type testCase struct {
		name         string
		mountOptions []string
		volumeCtx    map[string]string
		// mounterCacheType is the cache type the mounter pod has.
		mounterCacheType CacheType
		expectedArgs     []string
		// expectedErrContains: Rejected with InvalidArgument if set.
		expectedErrContains string
		// expectedWarnContains: Empty means no warning may fire at all.
		expectedWarnContains []string
	}

	groups := []struct {
		name  string
		cases []testCase
	}{
		{
			name: "no cache",
			cases: []testCase{
				{
					name:             "caches nothing when no volume attribute or mount option asks for it",
					volumeCtx:        map[string]string{},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{},
				},
				{
					name:             "`cache: false` caches nothing",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheDisabled},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{},
				},
				{
					name:             "a deprecated attribute alone does not enable the cache, and warns it is deprecated",
					volumeCtx:        map[string]string{volumecontext.CacheEmptyDirMedium: "Memory"},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{},
					expectedWarnContains: []string{
						deprecatedAttrs + `["cacheEmptyDirMedium"]`,
						`a PV opts in with "cache" set to "true"`,
					},
				},
				{
					name:                "rejects a value that is not a cache value, rather than serve it uncached",
					volumeCtx:           map[string]string{volumecontext.Cache: "ture"},
					mounterCacheType:    CacheEmptyDirDisk,
					expectedErrContains: `has an invalid cache setting: "ture" is not a supported cache value`,
				},
			},
		},
		{
			name: "v3 `cache: true`",
			cases: []testCase{
				{
					name:             "takes the node's disk emptyDir",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{cacheArg},
				},
				{
					name:             "takes the node's tmpfs emptyDir",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheType: CacheEmptyDirMemory,
					expectedArgs:     []string{cacheArg},
				},
				{
					name:             "takes the node's ephemeral volume",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheType: CacheEphemeral,
					expectedArgs:     []string{cacheArg},
				},
				{
					name:                "rejects the mount when the node has no cache volume",
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheType:    CacheNone,
					expectedErrContains: "has no cache volume",
				},
			},
		},
		{
			name: "v3 `cache: true` with leftover v2 attributes",
			cases: []testCase{
				{
					name:         "warns once, naming every deprecated attribute it sets",
					mountOptions: []string{"max-cache-size 1024"},
					volumeCtx: map[string]string{
						volumecontext.Cache:                                volumecontext.CacheEnabled,
						volumecontext.CacheEmptyDirMedium:                  "Memory",
						volumecontext.CacheEmptyDirSizeLimit:               "2Gi",
						volumecontext.CacheEphemeralStorageClassName:       "gp3",
						volumecontext.CacheEphemeralStorageResourceRequest: "10Gi",
					},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{cacheArg, "--max-cache-size=1024"},
					expectedWarnContains: []string{
						deprecatedAttrs + `["cacheEmptyDirMedium" "cacheEmptyDirSizeLimit" "cacheEphemeralStorageClassName" "cacheEphemeralStorageResourceRequest"]`,
					},
				},
				{
					// `cache: true` still reads a v2 size, for safety, ONLY IF cache type matches.
					name: "bounds the cache from a deprecated size limit",
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheEnabled,
						volumecontext.CacheEmptyDirSizeLimit: "2Gi",
					},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{cacheArg, "--max-cache-size=1945"},
					expectedWarnContains: []string{
						deprecatedAttrs + `["cacheEmptyDirSizeLimit"]`,
						"driver injects 1945 MiB",
					},
				},
				{
					// LIMITATION
					name: "does not bound an ephemeral node's cache from an emptyDir size limit",
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheEnabled,
						volumecontext.CacheEmptyDirSizeLimit: "2Gi",
					},
					mounterCacheType:     CacheEphemeral,
					expectedArgs:         []string{cacheArg},
					expectedWarnContains: []string{deprecatedAttrs + `["cacheEmptyDirSizeLimit"]`},
				},
				{
					name: "does not bound a tmpfs node's cache from a disk emptyDir size limit",
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheEnabled,
						volumecontext.CacheEmptyDirSizeLimit: "2Gi",
					},
					mounterCacheType:     CacheEmptyDirMemory,
					expectedArgs:         []string{cacheArg},
					expectedWarnContains: []string{deprecatedAttrs + `["cacheEmptyDirSizeLimit"]`},
				},
				// TODO this test does not work today (we don't check mismatch for ephemeral storageClassNames yet)
				// {
				// 	name: "does not bound an ephemeral node's cache from an ephemeral storage request with different storageClassName",
				// 	volumeCtx: map[string]string{
				// 		volumecontext.Cache: volumecontext.CacheEnabled,
				// 		volumecontext.CacheEphemeralStorageClassName:       "nvme-ssd",
				// 		volumecontext.CacheEphemeralStorageResourceRequest: "10Gi",
				// 	},
				// 	mounterCacheType:         CacheEphemeral,
				// 	mounterStorageClassName:  "gp3",
				// 	expectedArgs:             []string{cacheArg},
				// 	expectedWarnContains:     []string{deprecatedAttrs + `["cacheEphemeralStorageClassName" "cacheEphemeralStorageResourceRequest"]`},
				// },
			},
		},
		{
			name: "v1 `cache` mount option",
			cases: []testCase{
				{
					name:             "discards the path, takes the node's cache whatever its type, and warns it is deprecated",
					mountOptions:     []string{"cache /tmp/customer-path"},
					volumeCtx:        map[string]string{},
					mounterCacheType: CacheEmptyDirMemory,
					expectedArgs:     []string{cacheArg},
					expectedWarnContains: []string{
						"deprecated `cache` mount option", `set the "cache" volume attribute to "true" instead`,
					},
				},
				{
					name:                "rejects the mount when the node has no cache volume",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{},
					mounterCacheType:    CacheNone,
					expectedErrContains: "has no cache volume",
				},
				{
					name:                "rejects a cache configured with both a mount option and a volume attribute",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
					mounterCacheType:    CacheEmptyDirDisk,
					expectedErrContains: "configured with both",
				},
				{
					name:                "rejects `cache: false` alongside the mount option, as the two disagree",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheDisabled},
					mounterCacheType:    CacheEmptyDirDisk,
					expectedErrContains: "configured with both",
				},
			},
		},
		{
			name: "v2 cache type",
			cases: []testCase{
				{
					name:             "takes the node's disk emptyDir when it names it, and warns the type is deprecated",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{cacheArg},
					expectedWarnContains: []string{
						`deprecated "cache" value "emptyDir"`,
						`Set "cache" to "true" to use whichever cache this node has`,
					},
				},
				{
					name: "takes the node's tmpfs when its emptyDir medium matches",
					volumeCtx: map[string]string{
						volumecontext.Cache:               volumecontext.CacheTypeEmptyDir,
						volumecontext.CacheEmptyDirMedium: "Memory",
					},
					mounterCacheType: CacheEmptyDirMemory,
					expectedArgs:     []string{cacheArg},
					expectedWarnContains: []string{
						`deprecated "cache" value "emptyDir"`,
						deprecatedAttrs + `["cacheEmptyDirMedium"]`,
					},
				},
				{
					name:                 "takes the node's ephemeral volume when it names it",
					volumeCtx:            map[string]string{volumecontext.Cache: volumecontext.CacheTypeEphemeral},
					mounterCacheType:     CacheEphemeral,
					expectedArgs:         []string{cacheArg},
					expectedWarnContains: []string{`deprecated "cache" value "ephemeral"`},
				},
				{
					name: "rejects a tmpfs request on a disk node",
					volumeCtx: map[string]string{
						volumecontext.Cache:               volumecontext.CacheTypeEmptyDir,
						volumecontext.CacheEmptyDirMedium: "Memory",
					},
					mounterCacheType:     CacheEmptyDirDisk,
					expectedErrContains:  `requests an emptyDir (medium Memory) cache, but s3-csi-daemonset-mounter on this node provides emptyDir. Set "cache" to "true"`,
					expectedWarnContains: []string{deprecatedAttrs + `["cacheEmptyDirMedium"]`},
				},
				{
					name:                "rejects an emptyDir request on an ephemeral node",
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
					mounterCacheType:    CacheEphemeral,
					expectedErrContains: "provides ephemeral",
				},
				// TODO this test does not work today (we don't check mismatch for ephemeral storageClassNames yet)
				// {
				// 	name: "rejects an ephemeral request naming another storage class than the node's",
				// 	volumeCtx: map[string]string{
				// 		volumecontext.Cache:                          volumecontext.CacheTypeEphemeral,
				// 		volumecontext.CacheEphemeralStorageClassName: "nvme-ssd",
				// 	},
				// 	mounterCacheType:        CacheEphemeral,
				// 	mounterStorageClassName: "gp3",
				// 	expectedErrContains:     `requests storage class "nvme-ssd"`,
				// 	expectedWarnContains:    []string{deprecatedAttrs + `["cacheEphemeralStorageClassName"]`},
				// },
				{
					name: "rejects an emptyDir medium that cannot hold cache blocks",
					volumeCtx: map[string]string{
						volumecontext.Cache:               volumecontext.CacheTypeEmptyDir,
						volumecontext.CacheEmptyDirMedium: string(corev1.StorageMediumHugePages),
					},
					mounterCacheType:     CacheEmptyDirDisk,
					expectedErrContains:  `"cacheEmptyDirMedium" "HugePages" is not supported`,
					expectedWarnContains: []string{deprecatedAttrs + `["cacheEmptyDirMedium"]`},
				},
				{
					name:                "rejects the mount when the node has no cache volume",
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
					mounterCacheType:    CacheNone,
					expectedErrContains: "has no cache volume",
				},
			},
		},
		{
			name: "max-cache-size",
			cases: []testCase{
				{
					name:             "keeps max-cache-size for a cached volume",
					mountOptions:     []string{"max-cache-size 1024"},
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{cacheArg, "--max-cache-size=1024"},
				},
				{
					// v2 rejected a max-cache-size above the size limit; v3 prioritises max-cache-size instead.
					name:         "prefers max-cache-size over a deprecated size limit, even when set above it",
					mountOptions: []string{"max-cache-size 5000"},
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheEnabled,
						volumecontext.CacheEmptyDirSizeLimit: "2Gi",
					},
					mounterCacheType:     CacheEmptyDirDisk,
					expectedArgs:         []string{cacheArg, "--max-cache-size=5000"},
					expectedWarnContains: []string{deprecatedAttrs + `["cacheEmptyDirSizeLimit"]`},
				},
				{
					name: "bounds a disk emptyDir from the deprecated size limit, at 95%",
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
						volumecontext.CacheEmptyDirSizeLimit: "2Gi",
					},
					mounterCacheType:     CacheEmptyDirDisk,
					expectedArgs:         []string{cacheArg, "--max-cache-size=1945"}, // 2048 less a 5% margin
					expectedWarnContains: []string{"driver injects 1945 MiB", "Under cacheLimitStrategy=none, set --max-cache-size"},
				},
				{
					name: "bounds a tmpfs from the deprecated size limit",
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
						volumecontext.CacheEmptyDirMedium:    "Memory",
						volumecontext.CacheEmptyDirSizeLimit: "2Gi",
					},
					mounterCacheType:     CacheEmptyDirMemory,
					expectedArgs:         []string{cacheArg, "--max-cache-size=1945"},
					expectedWarnContains: []string{"driver injects 1945 MiB"},
				},
				{
					name: "bounds an ephemeral cache from the deprecated storage request",
					volumeCtx: map[string]string{
						volumecontext.Cache: volumecontext.CacheTypeEphemeral,
						volumecontext.CacheEphemeralStorageResourceRequest: "10Gi",
					},
					mounterCacheType:     CacheEphemeral,
					expectedArgs:         []string{cacheArg, "--max-cache-size=9728"}, // 10240 * 95%
					expectedWarnContains: []string{`95% of its deprecated "cacheEphemeralStorageResourceRequest" volume attribute ("10Gi")`},
				},
				{
					name:                 "bounds a mount-option cache from the deprecated size limit too",
					mountOptions:         []string{"cache /tmp/customer-path"},
					volumeCtx:            map[string]string{volumecontext.CacheEmptyDirSizeLimit: "2Gi"},
					mounterCacheType:     CacheEmptyDirDisk,
					expectedArgs:         []string{cacheArg, "--max-cache-size=1945"},
					expectedWarnContains: []string{"driver injects 1945 MiB"},
				},
				{
					// Note not multiplying before dividing fails this test case, as all division floors.
					// e.g.: boundMiB := quantity.Value() / bytesPerMiB * cacheSizeMarginPercent / 100
					name: "a size limit of 1500Ki injects 1",
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheEnabled,
						volumecontext.CacheEmptyDirSizeLimit: "1500Ki",
					},
					mounterCacheType:     CacheEmptyDirDisk,
					expectedArgs:         []string{cacheArg, "--max-cache-size=1"},
					expectedWarnContains: []string{"driver injects 1 MiB"},
				},
				{
					name: "a size limit at 1 MiB injects 0, which silently disables the cache",
					volumeCtx: map[string]string{
						volumecontext.Cache:                  volumecontext.CacheEnabled,
						volumecontext.CacheEmptyDirSizeLimit: "1Mi",
					},
					mounterCacheType:     CacheEmptyDirDisk,
					expectedArgs:         []string{cacheArg, "--max-cache-size=0"},
					expectedWarnContains: []string{"driver injects 0 MiB"},
				},
				{
					name: "does not fail on a deprecated storage request that is not a quantity",
					volumeCtx: map[string]string{
						volumecontext.Cache: volumecontext.CacheEnabled,
						volumecontext.CacheEphemeralStorageResourceRequest: "10 gigabytes",
					},
					mounterCacheType:     CacheEphemeral,
					expectedArgs:         []string{cacheArg},
					expectedWarnContains: []string{`sets the "cacheEphemeralStorageResourceRequest" volume attribute to "10 gigabytes"`},
				},
				{
					// Match v2.
					name:                "rejects max-cache-size when the cache is not enabled",
					mountOptions:        []string{"max-cache-size 1024"},
					volumeCtx:           map[string]string{},
					mounterCacheType:    CacheEmptyDirDisk,
					expectedErrContains: `sets --max-cache-size in its mountOptions but does not enable the cache. Set the "cache" volume attribute to "true"`,
				},
			},
		},
		{
			name: "misc",
			cases: []testCase{
				{
					name:             "leaves an S3 Express cache-xz mount option alone on a node with no cache volume",
					mountOptions:     []string{"cache-xz test-bucket--usw2-az1--x-s3"},
					volumeCtx:        map[string]string{},
					mounterCacheType: CacheNone,
					expectedArgs:     []string{"--cache-xz=test-bucket--usw2-az1--x-s3"},
				},
				{
					name:             "leaves unrelated mount options alone",
					mountOptions:     []string{"region us-west-2"},
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheType: CacheEmptyDirDisk,
					expectedArgs:     []string{cacheArg, "--region=us-west-2"},
				},
			},
		},
	}

	for _, group := range groups {
		t.Run(group.name, func(t *testing.T) {
			for _, testCase := range group.cases {
				t.Run(testCase.name, func(t *testing.T) {
					args := mountpoint.ParseArgs(testCase.mountOptions)
					logs := captureKlog(t)

					err := configureCache(&args, testCase.volumeCtx, volumeID, testCase.mounterCacheType)

					if len(testCase.expectedWarnContains) == 0 && logs.Len() != 0 {
						t.Errorf("expected no warning, got:\n%s", logs.String())
					}
					for _, warning := range testCase.expectedWarnContains {
						assert.Contains(t, logs.String(), warning)
					}
					if testCase.expectedErrContains != "" {
						if err == nil {
							t.Fatalf("expected an error, got args %v", args.SortedList())
						}
						assert.Equals(t, codes.InvalidArgument, status.Code(err))
						assert.Contains(t, err.Error(), testCase.expectedErrContains)
						return
					}
					assert.NoError(t, err)
					assert.Equals(t, testCase.expectedArgs, args.SortedList())
				})
			}
		})
	}
}

// Note: MountOptionCacheDir tested via daemonset_mounter_test.go "Discovery resolves the mounter pod's comm directory".

func TestCacheTypeFromPV(t *testing.T) {
	testCases := []struct {
		name                  string
		pvCache               string
		pvCacheEmptyDirMedium string
		want                  CacheType
		wantErrContains       string
	}{
		{
			name: "a PV that asks for no cache (unset cache in PV)",
			want: CacheNone,
		},
		{
			name:    "`cache: false`, which is no cache said explicitly",
			pvCache: volumecontext.CacheDisabled,
			want:    CacheNone,
		},
		{
			name:    "`cache: true`, which opts in and names no type",
			pvCache: volumecontext.CacheEnabled,
			want:    CacheNone,
		},
		{
			name:                  "an ephemeral cache, whose medium is ignored",
			pvCache:               volumecontext.CacheTypeEphemeral,
			pvCacheEmptyDirMedium: string(corev1.StorageMediumMemory),
			want:                  CacheEphemeral,
		},
		{
			name:    "an emptyDir cache with no medium, which is the node's disk",
			pvCache: volumecontext.CacheTypeEmptyDir,
			want:    CacheEmptyDirDisk,
		},
		{
			name:                  "an emptyDir cache on medium Memory, which is a tmpfs",
			pvCache:               volumecontext.CacheTypeEmptyDir,
			pvCacheEmptyDirMedium: string(corev1.StorageMediumMemory),
			want:                  CacheEmptyDirMemory,
		},
		{
			name:                  "an emptyDir cache on a medium that cannot hold cache blocks",
			pvCache:               volumecontext.CacheTypeEmptyDir,
			pvCacheEmptyDirMedium: string(corev1.StorageMediumHugePages),
			wantErrContains:       `"cacheEmptyDirMedium" "HugePages" is not supported`,
		},
		{
			name:            "a value that is not a cache value at all",
			pvCache:         "yes",
			wantErrContains: `Use "true" for this node's cache`,
		},
		{
			// The accepted values are matched exactly, as `emptyDir` and `Memory` already are.
			name:            "`True`, because the match is case sensitive",
			pvCache:         "True",
			wantErrContains: "is not a supported cache value",
		},
		{
			name:            "`1`, which only a boolean parser would take",
			pvCache:         "1",
			wantErrContains: "is not a supported cache value",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := cacheTypeFromPV(testCase.pvCache, testCase.pvCacheEmptyDirMedium)

			if testCase.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got cache type %q", testCase.wantErrContains, got)
				}
				assert.Contains(t, err.Error(), testCase.wantErrContains)
				assert.Equals(t, CacheNone, got)
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, testCase.want, got)
		})
	}
}

func TestCreateCacheDir(t *testing.T) {
	t.Run("creates the mount's directory group-writable", func(t *testing.T) {
		cacheDir := t.TempDir()
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))

		fi, err := os.Stat(filepath.Join(cacheDir, "s3-pv"))
		assert.NoError(t, err)
		// TODO: Updated permissions when we enforce process isolation
		assert.Equals(t, fs.FileMode(0770), fi.Mode().Perm())
	})

	t.Run("sets the permissions on a directory that already exists with the wrong one", func(t *testing.T) {
		cacheDir := t.TempDir()
		mountCacheDir := filepath.Join(cacheDir, "s3-pv")
		assert.NoError(t, os.Mkdir(mountCacheDir, 0700))

		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))

		fi, err := os.Stat(mountCacheDir)
		assert.NoError(t, err)
		assert.Equals(t, fs.FileMode(0770), fi.Mode().Perm())
	})

	t.Run("is idempotent, so a re-publish does not fail", func(t *testing.T) {
		cacheDir := t.TempDir()
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))
	})

	t.Run("refuses a symlink rather than chmod-ing its target", func(t *testing.T) {
		cacheDir := t.TempDir()
		victim := t.TempDir()
		before, err := os.Stat(victim)
		assert.NoError(t, err)
		assert.NoError(t, os.Symlink(victim, filepath.Join(cacheDir, "s3-pv")))

		err = createCacheDir(cacheDir, "s3-pv")
		if err == nil {
			// Expect mode Lrwxrwxrwx, which fails.
			t.Fatal("expected createCacheDir to refuse a symlink")
		}
		assert.Contains(t, err.Error(), "is not a directory")

		// The symlink's target keep the mode it had.
		after, statErr := os.Stat(victim)
		assert.NoError(t, statErr)
		assert.Equals(t, before.Mode().Perm(), after.Mode().Perm())
	})

	t.Run("fails when the mounter has no cache volume", func(t *testing.T) {
		err := createCacheDir("", "s3-pv")
		assert.Equals(t, true, err != nil)
	})

	t.Run("fails when the cache volume cannot be written to", func(t *testing.T) {
		// Assert, not skip: CI is unprivileged, so a root run must fail loudly rather than lose this case. (Considered skipping if root run)
		assert.Equals(t, false, os.Geteuid() == 0)
		// A read-only cache volume: the kubelet has mounted it, but nothing can be created inside.
		// e.g. HugePages emptyDir medium which we should reject.
		cacheDir := t.TempDir()
		assert.NoError(t, os.Chmod(cacheDir, 0500))
		t.Cleanup(func() { os.Chmod(cacheDir, 0700) })

		err := createCacheDir(cacheDir, "s3-pv")
		if err == nil {
			t.Fatal("expected createCacheDir to fail on a read-only cache volume")
		}
		assert.Contains(t, err.Error(), "failed to create cache directory")
	})
}

func TestRemoveCacheDir(t *testing.T) {
	t.Run("removes the mount's directory and its contents", func(t *testing.T) {
		cacheDir := t.TempDir()
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))
		assert.NoError(t, os.WriteFile(filepath.Join(cacheDir, "s3-pv", "block"), []byte("x"), 0600))

		assert.NoError(t, removeCacheDir(cacheDir, "s3-pv"))
		_, err := os.Stat(filepath.Join(cacheDir, "s3-pv"))
		assert.Equals(t, true, os.IsNotExist(err))
	})

	t.Run("is idempotent, so a retried cleanup does not fail", func(t *testing.T) {
		cacheDir := t.TempDir()
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))

		assert.NoError(t, removeCacheDir(cacheDir, "s3-pv"))
		assert.NoError(t, removeCacheDir(cacheDir, "s3-pv"))
	})

	t.Run("succeeds when the whole cache volume is gone, as a replaced mounter pod leaves it", func(t *testing.T) {
		cacheDir := t.TempDir()
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))
		assert.NoError(t, os.RemoveAll(cacheDir))

		assert.NoError(t, removeCacheDir(cacheDir, "s3-pv"))
	})

	t.Run("fails when the mounter has no cache volume", func(t *testing.T) {
		if err := removeCacheDir("", "s3-pv"); err == nil {
			t.Fatal("expected removeCacheDir to refuse an empty cache volume")
		}
	})

	t.Run("fails when there is a problem with the os.RemoveAll path", func(t *testing.T) {
		testCases := []struct {
			name     string
			volumeID string
		}{
			{name: "an unnamed volume", volumeID: ""},
			{name: "the volume root itself", volumeID: "."},
			{name: "the parent of the volume root", volumeID: ".."},
			{name: "a path escaping the volume root", volumeID: "../sibling"},
			{name: "a subdirectory of a mount's cache", volumeID: "s3-pv/mountpoint-cache/V2"},
		}

		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				cacheDir := t.TempDir()
				sibling := filepath.Join(filepath.Dir(cacheDir), "sibling")
				assert.NoError(t, os.Mkdir(sibling, 0770))
				cachedBlocks := filepath.Join(cacheDir, "s3-pv", "mountpoint-cache", "V2")
				assert.NoError(t, os.MkdirAll(cachedBlocks, 0770))

				if err := removeCacheDir(cacheDir, testCase.volumeID); err == nil {
					t.Fatal("expected removeCacheDir to refuse a volumeID that is not a plain directory name")
				}

				// Check that it did not run os.RemoveAll after returning error, and that
				// the cache volume root and its sibling are still there.
				_, err := os.Stat(cacheDir)
				assert.NoError(t, err)
				_, err = os.Stat(filepath.Dir(cacheDir))
				assert.NoError(t, err)
				_, err = os.Stat(sibling)
				assert.NoError(t, err)
				_, err = os.Stat(cachedBlocks)
				assert.NoError(t, err)
			})
		}
	})
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
