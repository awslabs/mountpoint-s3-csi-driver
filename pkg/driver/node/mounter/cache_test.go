package mounter

import (
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

	testCases := []struct {
		name         string
		mountOptions []string
		volumeCtx    map[string]string
		// mounterCacheType is the cache type the mounter pod has.
		mounterCacheType    CacheType
		expectedArgs        []string
		expectError         bool
		expectedErrContains string
	}{
		// Happy Cases + edge cases
		{
			name:             "requests a cache when the attribute matches the node's emptyDir",
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{cacheArg},
		},
		{
			name: "requests a cache when the attribute matches the node's tmpfs",
			volumeCtx: map[string]string{
				volumecontext.Cache:               volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirMedium: "Memory",
			},
			mounterCacheType: CacheEmptyDirMemory,
			expectedArgs:     []string{cacheArg},
		},
		{
			name:             "requests a cache when the attribute matches the node's ephemeral volume",
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEphemeral},
			mounterCacheType: CacheEphemeral,
			expectedArgs:     []string{cacheArg},
		},
		{
			// Note: We also give a warning for this case.
			name: "ignores the medium when the type is ephemeral",
			volumeCtx: map[string]string{
				volumecontext.Cache:               volumecontext.CacheTypeEphemeral,
				volumecontext.CacheEmptyDirMedium: "Memory",
			},
			mounterCacheType: CacheEphemeral,
			expectedArgs:     []string{cacheArg},
		},
		{
			name:             "does not enable the cache when only the medium is set",
			volumeCtx:        map[string]string{volumecontext.CacheEmptyDirMedium: "Memory"},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{},
		},
		// Rejection cases
		{
			name: "rejects a tmpfs request on a disk-backed node",
			volumeCtx: map[string]string{
				volumecontext.Cache:               volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirMedium: "Memory",
			},
			mounterCacheType: CacheEmptyDirDisk,
			expectError:      true,
		},
		{
			name:             "rejects a disk request on a tmpfs node",
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
			mounterCacheType: CacheEmptyDirMemory,
			expectError:      true,
		},
		{
			name:             "rejects an emptyDir request on an ephemeral node",
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
			mounterCacheType: CacheEphemeral,
			expectError:      true,
		},
		{
			name:             "rejects an ephemeral request on an emptyDir node",
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEphemeral},
			mounterCacheType: CacheEmptyDirDisk,
			expectError:      true,
		},
		{
			name: "rejects an emptyDir medium that is not a supported cache type",
			volumeCtx: map[string]string{
				volumecontext.Cache:               volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirMedium: string(corev1.StorageMediumHugePages),
			},
			mounterCacheType:    CacheEmptyDirDisk,
			expectError:         true,
			expectedErrContains: `must be "" or "Memory"`,
		},
		{
			name:                "rejects a cache request when the node has no cache volume",
			volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
			mounterCacheType:    CacheNone,
			expectError:         true,
			expectedErrContains: "has no cache volume",
		},
		// Opting in via mountOptions
		{
			name:                "rejects the deprecated mount option when the node has no cache volume",
			mountOptions:        []string{"cache /tmp/customer-path"},
			volumeCtx:           map[string]string{},
			mounterCacheType:    CacheNone,
			expectError:         true,
			expectedErrContains: "has no cache volume",
		},

		{
			// Opting in via mount options will automatically accept whatever mounter pod cache type is.
			name:             "the deprecated mount option accepts the node's tmpfs",
			mountOptions:     []string{"cache /tmp/customer-path"},
			volumeCtx:        map[string]string{},
			mounterCacheType: CacheEmptyDirMemory,
			expectedArgs:     []string{cacheArg},
		},
		{
			name:             "does not request a cache when no volume attribute or mount option asks for one",
			volumeCtx:        map[string]string{},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{},
		},
		{
			name:             "discards the path of a `cache` mount option",
			mountOptions:     []string{"cache /tmp/customer-path"},
			volumeCtx:        map[string]string{},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{cacheArg},
		},
		{
			name:             "rejects a cache configured with both a mount option and a volume attribute",
			mountOptions:     []string{"cache /tmp/customer-path"},
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
			mounterCacheType: CacheEmptyDirDisk,
			expectError:      true,
		},

		// --max-cache-size handling
		{
			name:             "keeps max-cache-size for a cached volume",
			mountOptions:     []string{"max-cache-size 1024"},
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{cacheArg, "--max-cache-size=1024"},
		},
		{
			name:             "drops max-cache-size when the cache is not enabled",
			mountOptions:     []string{"max-cache-size 1024"},
			volumeCtx:        map[string]string{},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{},
		},
		// Misc
		{
			name:             "leaves an S3 Express cache-xz mount option alone on a node with no cache volume",
			mountOptions:     []string{"cache-xz test-bucket--usw2-az1--x-s3"},
			volumeCtx:        map[string]string{},
			mounterCacheType: CacheNone,
			expectedArgs:     []string{"--cache-xz=test-bucket--usw2-az1--x-s3"},
		},
		{
			name: "accepts the v2-only cache and container resource attributes without leaking them into args",
			volumeCtx: map[string]string{
				volumecontext.Cache:                                      volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit:                     "2Gi",
				volumecontext.CacheEphemeralStorageClassName:             "gp3",
				volumecontext.CacheEphemeralStorageResourceRequest:       "10Gi",
				volumecontext.MountpointContainerResourcesRequestsCpu:    "100m",
				volumecontext.MountpointContainerResourcesRequestsMemory: "128Mi",
				volumecontext.MountpointContainerResourcesLimitsCpu:      "500m",
				volumecontext.MountpointContainerResourcesLimitsMemory:   "1Gi",
			},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{cacheArg},
		},
		{
			name:             "leaves unrelated mount options alone",
			mountOptions:     []string{"region us-west-2"},
			volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
			mounterCacheType: CacheEmptyDirDisk,
			expectedArgs:     []string{cacheArg, "--region=us-west-2"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			args := mountpoint.ParseArgs(testCase.mountOptions)

			err := configureCache(&args, testCase.volumeCtx, volumeID, testCase.mounterCacheType)

			if testCase.expectError {
				if err == nil {
					t.Fatalf("expected an error, got args %v", args.SortedList())
				}
				assert.Equals(t, codes.InvalidArgument, status.Code(err))
				if testCase.expectedErrContains != "" {
					assert.Contains(t, err.Error(), testCase.expectedErrContains)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, testCase.expectedArgs, args.SortedList())
		})
	}
}

// Note: MountOptionCacheDir tested via daemosnet_mounter_test.go "Discovery resolves the mounter pod's comm directory".

func TestCacheTypeFromPV(t *testing.T) {
	testCases := []struct {
		name                  string
		pvCache               string
		pvCacheEmptyDirMedium string
		want                  CacheType
		wantErrContains       string
	}{
		{
			name: "a PV that asks for no cache",
			want: CacheNone,
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
			wantErrContains:       `must be "" or "Memory"`,
		},
		{
			// `true` is not accepted - we require the PV to specify the cache type
			name:            "a value that is not a cache type",
			pvCache:         "true",
			wantErrContains: `must be "emptyDir" or "ephemeral"`,
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
