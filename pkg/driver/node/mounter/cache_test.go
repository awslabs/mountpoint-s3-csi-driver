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

func TestResolveCacheDir(t *testing.T) {
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
	boundPV := func(source corev1.PersistentVolumeSource) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "my-static-cache-pv"},
			Spec:       corev1.PersistentVolumeSpec{PersistentVolumeSource: source},
		}
	}
	ephemeral := withCacheVolume(corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}})

	testCases := []struct {
		name            string
		volumes         []corev1.Volume
		objects         []runtime.Object
		wantDir         string
		wantErrContains string
	}{
		{
			name: "a pod with no volumes at all",
		},
		{
			name:    "a pod with only a comm volume",
			volumes: []corev1.Volume{comm},
		},
		{
			name:    "a hostPath volume that happens to be named cache",
			volumes: withCacheVolume(corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/mnt/cache"}}),
		},
		{
			name:    "a disk-backed emptyDir cache volume",
			volumes: withCacheVolume(corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}),
			wantDir: emptyDirPath,
		},
		{
			name:    "a tmpfs emptyDir cache volume resolves to the same path as disk",
			volumes: withCacheVolume(corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}),
			wantDir: emptyDirPath,
		},
		{
			name:    "an ephemeral volume on a CSI PV resolves under kubernetes.io~csi, with /mount",
			volumes: ephemeral,
			objects: []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com"}})},
			wantDir: mounterDir + "/volumes/kubernetes.io~csi/my-static-cache-pv/mount",
		},
		{
			name:    "an ephemeral volume on an in-tree EBS PV (gp2) resolves under kubernetes.io~csi, as the CSI plugin mounts it",
			volumes: ephemeral,
			objects: []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{VolumeID: "vol-0123"}})},
			wantDir: mounterDir + "/volumes/kubernetes.io~csi/my-static-cache-pv/mount",
		},
		{
			name:    "an ephemeral volume on a local PV (instance-store NVMe) resolves under kubernetes.io~local-volume",
			volumes: ephemeral,
			objects: []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/mnt/disks/nvme0"}})},
			wantDir: mounterDir + "/volumes/kubernetes.io~local-volume/my-static-cache-pv",
		},
		{
			name:    "an ephemeral volume on an NFS PV resolves under kubernetes.io~nfs",
			volumes: ephemeral,
			objects: []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs", Path: "/"}})},
			wantDir: mounterDir + "/volumes/kubernetes.io~nfs/my-static-cache-pv",
		},
		{
			name:    "an ephemeral volume on an iSCSI PV resolves under kubernetes.io~iscsi",
			volumes: ephemeral,
			objects: []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{ISCSI: &corev1.ISCSIPersistentVolumeSource{TargetPortal: "10.0.0.1:3260", IQN: "iqn.2026-10.com.example:cache", Lun: 0}})},
			wantDir: mounterDir + "/volumes/kubernetes.io~iscsi/my-static-cache-pv",
		},
		{
			name:    "an ephemeral volume on an FC PV resolves under kubernetes.io~fc",
			volumes: ephemeral,
			objects: []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{FC: &corev1.FCVolumeSource{WWIDs: []string{"3600508b400105e210000900000490000"}}})},
			wantDir: mounterDir + "/volumes/kubernetes.io~fc/my-static-cache-pv",
		},
		{
			name:    "an ephemeral volume on a flex PV resolves under its driver name, with \"/\" escaped as \"~\"",
			volumes: ephemeral,
			objects: []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{FlexVolume: &corev1.FlexPersistentVolumeSource{Driver: "example.com/cachefs"}})},
			wantDir: mounterDir + "/volumes/example.com~cachefs/my-static-cache-pv",
		},
		{
			name:            "an ephemeral volume on a hostPath PV is an error, as s3-csi-node cannot reach its host path",
			volumes:         ephemeral,
			objects:         []runtime.Object{boundPVC, boundPV(corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/mnt/cache"}})},
			wantErrContains: "unsupported source",
		},
		{
			name:            "an ephemeral volume whose bound PV cannot be read is an error",
			volumes:         ephemeral,
			objects:         []runtime.Object{boundPVC},
			wantErrContains: "failed to get cache volume PV",
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

			gotDir, err := resolveCacheDir(context.Background(),
				fake.NewSimpleClientset(testCase.objects...), pod, mounterDir)

			if testCase.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got dir %q", testCase.wantErrContains, gotDir)
				}
				assert.Contains(t, err.Error(), testCase.wantErrContains)
				return
			}
			assert.NoError(t, err)
			assert.Equals(t, testCase.wantDir, gotDir)
		})
	}
}

func TestCacheDirForMount(t *testing.T) {
	const volumeID = "test-volume-id"
	const mounterCacheDir = "/var/lib/kubelet/pods/mounter-uid/volumes/kubernetes.io~empty-dir/cache"
	const bothSurfaces = "both `mountOptions` and `volumeAttributes`"

	type testCase struct {
		name         string
		mountOptions []string
		volumeCtx    map[string]string
		// mounterCacheDir is where the mounter pod's cache lives, or "" when it has no cache volume.
		mounterCacheDir string
		// expectedCacheDir is the cache volume root the mount records, "" when it does not cache.
		expectedCacheDir string
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
					name:             "caches nothing when no volume attribute or mount option asks for it",
					volumeCtx:        map[string]string{},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: "",
				},
				{
					name:             "`cache: Disabled` caches nothing, as disabled is matched in any case",
					volumeCtx:        map[string]string{volumecontext.Cache: "Disabled"},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: "",
				},
				{
					name:             "a deprecated attribute alone does not enable the cache",
					volumeCtx:        map[string]string{volumecontext.CacheEmptyDirMedium: "Memory"},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: "",
				},
				{
					name:                "rejects an unrecognised cache value, naming it",
					volumeCtx:           map[string]string{volumecontext.Cache: "emptyDrr"},
					mounterCacheDir:     mounterCacheDir,
					expectedErrContains: `"emptyDrr"`,
				},
			},
		},
		{
			name: "v3 `cache: enabled`",
			cases: []testCase{
				{
					name:             "takes the node's cache",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: mounterCacheDir,
				},
				{
					name:             "`cache: Enabled` takes it too, as enabled is matched in any case",
					volumeCtx:        map[string]string{volumecontext.Cache: "Enabled"},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: mounterCacheDir,
				},
				{
					name:                "rejects the mount when the node has no cache volume",
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheDir:     "",
					expectedErrContains: "has no cache volume",
				},
			},
		},
		{
			name: "v1 `cache` mount option",
			cases: []testCase{
				{
					name:             "opts in and takes the node's cache, whatever path it names",
					mountOptions:     []string{"cache /tmp/customer-path"},
					volumeCtx:        map[string]string{},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: mounterCacheDir,
				},
				{
					name:                "rejects the mount when the node has no cache volume",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{},
					mounterCacheDir:     "",
					expectedErrContains: "has no cache volume",
				},
				{
					name:                "rejects a cache configured with both a mount option and a volume attribute",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheEnabled},
					mounterCacheDir:     mounterCacheDir,
					expectedErrContains: bothSurfaces,
				},
				{
					name:                "rejects `cache: disabled` alongside the mount option, as the two disagree",
					mountOptions:        []string{"cache /tmp/customer-path"},
					volumeCtx:           map[string]string{volumecontext.Cache: volumecontext.CacheDisabled},
					mounterCacheDir:     mounterCacheDir,
					expectedErrContains: bothSurfaces,
				},
			},
		},
		{
			name: "v2 cache type",
			cases: []testCase{
				{
					name:             "`cache: emptyDir` takes the node's cache",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEmptyDir},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: mounterCacheDir,
				},
				{
					name:             "`cache: ephemeral` takes the node's cache whatever its type",
					volumeCtx:        map[string]string{volumecontext.Cache: volumecontext.CacheTypeEphemeral},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: mounterCacheDir,
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
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: mounterCacheDir,
				},
			},
		},
		{
			name: "max-cache-size",
			cases: []testCase{
				{
					name:             "max-cache-size alone does not opt in, leaving it for Mountpoint to reject",
					mountOptions:     []string{"max-cache-size 1024"},
					volumeCtx:        map[string]string{},
					mounterCacheDir:  mounterCacheDir,
					expectedCacheDir: "",
				},
			},
		},
		{
			name: "misc",
			cases: []testCase{
				{
					name:             "an S3 Express cache-xz mount option is not a local cache, so it mounts on a node with no cache volume",
					mountOptions:     []string{"cache-xz test-bucket--usw2-az1--x-s3"},
					volumeCtx:        map[string]string{},
					mounterCacheDir:  "",
					expectedCacheDir: "",
				},
			},
		},
	}

	for _, group := range groups {
		t.Run(group.name, func(t *testing.T) {
			for _, testCase := range group.cases {
				t.Run(testCase.name, func(t *testing.T) {
					args := mountpoint.ParseArgs(testCase.mountOptions)

					cacheDir, err := cacheDirForMount(args, testCase.volumeCtx, volumeID, testCase.mounterCacheDir)

					if testCase.expectedErrContains != "" {
						if err == nil {
							t.Fatalf("expected an error, got cache dir %q", cacheDir)
						}
						assert.Equals(t, codes.InvalidArgument, status.Code(err))
						assert.Contains(t, err.Error(), testCase.expectedErrContains)
						return
					}
					assert.NoError(t, err)
					assert.Equals(t, testCase.expectedCacheDir, cacheDir)
				})
			}
		})
	}
}

// Note: MountOptionCacheDir tested via daemonset_mounter_test.go "Discovery resolves the mounter pod's comm directory".

func TestCreateCacheDir(t *testing.T) {
	t.Run("creates the mount's directory group-writable", func(t *testing.T) {
		cacheDir := t.TempDir()
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))

		fi, err := os.Stat(filepath.Join(cacheDir, "s3-pv"))
		assert.NoError(t, err)
		// TODO: Updated permissions when we enforce process isolation
		assert.Equals(t, fs.FileMode(0770), fi.Mode().Perm())
	})

	t.Run("replaces a directory that already exists, dropping what it held", func(t *testing.T) {
		cacheDir := t.TempDir()
		mountCacheDir := filepath.Join(cacheDir, "s3-pv")
		assert.NoError(t, os.MkdirAll(filepath.Join(mountCacheDir, "mountpoint-cache"), 0700))

		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))

		fi, err := os.Stat(mountCacheDir)
		assert.NoError(t, err)
		assert.Equals(t, fs.FileMode(0770), fi.Mode().Perm())
		assert.FileNotExists(t, filepath.Join(mountCacheDir, "mountpoint-cache"))
	})

	t.Run("is idempotent, so a re-publish does not fail", func(t *testing.T) {
		cacheDir := t.TempDir()
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))
		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))
	})

	t.Run("replaces a symlink with a directory, leaving the symlink's target untouched", func(t *testing.T) {
		cacheDir := t.TempDir()
		victim := t.TempDir()
		before, err := os.Stat(victim)
		assert.NoError(t, err)
		assert.NoError(t, os.Symlink(victim, filepath.Join(cacheDir, "s3-pv")))

		assert.NoError(t, createCacheDir(cacheDir, "s3-pv"))

		fi, err := os.Lstat(filepath.Join(cacheDir, "s3-pv"))
		assert.NoError(t, err)
		assert.Equals(t, true, fi.Mode().IsDir())
		after, err := os.Stat(victim)
		assert.NoError(t, err)
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
