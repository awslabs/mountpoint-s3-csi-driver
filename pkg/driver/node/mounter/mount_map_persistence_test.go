package mounter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
	mountutils "k8s.io/mount-utils"
)

// fakeMountInfoProvider returns a mountInfoProviderFunc that returns the given entries.
func fakeMountInfoProvider(entries []mountutils.MountInfo) mountInfoProviderFunc {
	return func() ([]mountutils.MountInfo, error) {
		return entries, nil
	}
}

// newTestDMWithMountInfo creates a minimal DaemonsetMounter for persistence tests.
// It sets kubeletPath and mountInfoProvider — no clientset/mount needed for RebuildMountMap.
func newTestDMWithMountInfo(kubeletPath string, provider mountInfoProviderFunc) *DaemonsetMounter {
	return &DaemonsetMounter{
		kubeletPath:       kubeletPath,
		mountInfoProvider: provider,
		mountMap:          NewMountMap(),
	}
}

func TestWriteMeta_CreatesFile(t *testing.T) {
	kubeletPath := t.TempDir()

	entry := &MountEntry{
		VolumeID:   "vol-abc123",
		SourcePath: filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt", "vol-abc123"),
		Params: MountParams{
			MountOptions:             []string{"--allow-other", "--region=us-east-1"},
			AuthenticationSource:     "driver",
			ServiceAccountName:       "default",
			ServiceAccountEKSRoleARN: "arn:aws:iam::111111111111:role/my-role",
			PodNamespace:             "default",
			FSGroup:                  "1000",
		},
		RefCount: 1,
		Targets:  []string{"/target-a"},
	}

	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	// Verify file exists at expected path
	metaPath := MetaFileName(kubeletPath, "vol-abc123")
	_, err = os.Stat(metaPath)
	assert.NoError(t, err)

	// Verify content is valid JSON with correct fields
	data, err := os.ReadFile(metaPath)
	assert.NoError(t, err)

	var meta MountMeta
	err = json.Unmarshal(data, &meta)
	assert.NoError(t, err)

	assert.Equals(t, "vol-abc123", meta.VolumeID)
	assert.Equals(t, "driver", meta.AuthenticationSource)
	assert.Equals(t, "default", meta.ServiceAccountName)
	assert.Equals(t, "arn:aws:iam::111111111111:role/my-role", meta.ServiceAccountEKSRoleARN)
	assert.Equals(t, "default", meta.PodNamespace)
	assert.Equals(t, "1000", meta.FSGroup)
	assert.Equals(t, 2, len(meta.MountOptions))
	assert.Equals(t, "--allow-other", meta.MountOptions[0])
	assert.Equals(t, "--region=us-east-1", meta.MountOptions[1])
}

func TestWriteMeta_OverwritesExisting(t *testing.T) {
	kubeletPath := t.TempDir()

	entry1 := &MountEntry{
		VolumeID:   "vol-overwrite",
		SourcePath: "/source-1",
		Params:     MountParams{ServiceAccountName: "sa-first"},
	}
	err := WriteMeta(kubeletPath, entry1)
	assert.NoError(t, err)

	entry2 := &MountEntry{
		VolumeID:   "vol-overwrite",
		SourcePath: "/source-2",
		Params:     MountParams{ServiceAccountName: "sa-second"},
	}
	err = WriteMeta(kubeletPath, entry2)
	assert.NoError(t, err)

	meta, err := readMeta(MetaFileName(kubeletPath, "vol-overwrite"))
	assert.NoError(t, err)
	assert.Equals(t, "sa-second", meta.ServiceAccountName)
}

func TestWriteMeta_CreatesDirectoryIfMissing(t *testing.T) {
	kubeletPath := t.TempDir()

	entry := &MountEntry{VolumeID: "vol-mkdir", SourcePath: "/source"}

	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt")
	_, err := os.Stat(metaDir)
	if !os.IsNotExist(err) {
		t.Fatal("expected meta directory to not exist initially")
	}

	err = WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	info, err := os.Stat(metaDir)
	assert.NoError(t, err)
	if !info.IsDir() {
		t.Fatal("expected meta directory to be a directory")
	}
}

func TestRemoveMeta_RemovesFile(t *testing.T) {
	kubeletPath := t.TempDir()

	entry := &MountEntry{VolumeID: "vol-remove", SourcePath: "/source"}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	metaPath := MetaFileName(kubeletPath, "vol-remove")
	_, err = os.Stat(metaPath)
	assert.NoError(t, err)

	RemoveMeta(kubeletPath, "vol-remove")

	_, err = os.Stat(metaPath)
	if !os.IsNotExist(err) {
		t.Fatal("expected meta file to be removed")
	}
}

func TestRemoveMeta_NoopIfNotExists(t *testing.T) {
	kubeletPath := t.TempDir()
	RemoveMeta(kubeletPath, "vol-nonexistent")
}

func TestReadMeta_ParsesCorrectly(t *testing.T) {
	kubeletPath := t.TempDir()

	entry := &MountEntry{
		VolumeID:   "vol-read",
		SourcePath: "/my/source/path",
		Params: MountParams{
			MountOptions:             []string{"--read-only"},
			AuthenticationSource:     "pod",
			ServiceAccountName:       "my-sa",
			ServiceAccountEKSRoleARN: "arn:aws:iam::222222222222:role/pod-role",
			PodNamespace:             "production",
			FSGroup:                  "2000",
		},
	}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	meta, err := readMeta(MetaFileName(kubeletPath, "vol-read"))
	assert.NoError(t, err)

	assert.Equals(t, "vol-read", meta.VolumeID)
	assert.Equals(t, "pod", meta.AuthenticationSource)
	assert.Equals(t, "my-sa", meta.ServiceAccountName)
	assert.Equals(t, "arn:aws:iam::222222222222:role/pod-role", meta.ServiceAccountEKSRoleARN)
	assert.Equals(t, "production", meta.PodNamespace)
	assert.Equals(t, "2000", meta.FSGroup)
	assert.Equals(t, 1, len(meta.MountOptions))
	assert.Equals(t, "--read-only", meta.MountOptions[0])
}

func TestReadMeta_InvalidJSON(t *testing.T) {
	kubeletPath := t.TempDir()
	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt")
	err := os.MkdirAll(metaDir, 0750)
	assert.NoError(t, err)

	metaPath := filepath.Join(metaDir, "vol-bad.meta.json")
	err = os.WriteFile(metaPath, []byte("not json{{{"), 0640)
	assert.NoError(t, err)

	_, err = readMeta(metaPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestReadMeta_FileNotFound(t *testing.T) {
	_, err := readMeta("/nonexistent/path/vol.meta.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestMetaFileName_Format(t *testing.T) {
	path := MetaFileName("/var/lib/kubelet", "vol-test-123")
	expected := "/var/lib/kubelet/plugins/s3.csi.aws.com/mnt/vol-test-123.meta.json"
	assert.Equals(t, expected, path)
}

func TestWriteMeta_EmptyMountOptions(t *testing.T) {
	kubeletPath := t.TempDir()

	entry := &MountEntry{
		VolumeID: "vol-empty-opts",
		Params:   MountParams{MountOptions: nil, AuthenticationSource: "driver"},
	}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	meta, err := readMeta(MetaFileName(kubeletPath, "vol-empty-opts"))
	assert.NoError(t, err)
	if meta.MountOptions != nil {
		t.Fatalf("expected nil mount options, got: %v", meta.MountOptions)
	}
}

func TestWriteMeta_EmptyEKSRoleARN_PresentInJSON(t *testing.T) {
	kubeletPath := t.TempDir()

	entry := &MountEntry{
		VolumeID: "vol-no-arn",
		Params:   MountParams{AuthenticationSource: "driver", ServiceAccountEKSRoleARN: ""},
	}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	data, err := os.ReadFile(MetaFileName(kubeletPath, "vol-no-arn"))
	assert.NoError(t, err)

	var raw map[string]interface{}
	err = json.Unmarshal(data, &raw)
	assert.NoError(t, err)
	if _, exists := raw["serviceAccountEKSRoleARN"]; !exists {
		t.Fatal("expected serviceAccountEKSRoleARN to be present in JSON even when empty")
	}

	meta, err := readMeta(MetaFileName(kubeletPath, "vol-no-arn"))
	assert.NoError(t, err)
	assert.Equals(t, "", meta.ServiceAccountEKSRoleARN)
}

// --- Tests for mountinfo parsing helpers ---

func TestFindMountByPath_Found(t *testing.T) {
	entries := []mountutils.MountInfo{
		{MountPoint: "/mnt/a", Major: 0, Minor: 100},
		{MountPoint: "/mnt/b", Major: 0, Minor: 101},
		{MountPoint: "/mnt/c", Major: 0, Minor: 102},
	}
	result := findMountByPath(entries, "/mnt/b")
	if result == nil {
		t.Fatal("expected to find entry for /mnt/b")
	}
	assert.Equals(t, "0:101", fmt.Sprintf("%d:%d", result.Major, result.Minor))
}

func TestFindMountByPath_NotFound(t *testing.T) {
	entries := []mountutils.MountInfo{{MountPoint: "/mnt/a", Major: 0, Minor: 100}}
	result := findMountByPath(entries, "/mnt/nonexistent")
	if result != nil {
		t.Fatal("expected nil for nonexistent path")
	}
}

func TestFindMountByPath_EmptyEntries(t *testing.T) {
	result := findMountByPath(nil, "/mnt/a")
	if result != nil {
		t.Fatal("expected nil for empty entries")
	}
}

func TestFindBindMountTargets_MultipleTargets(t *testing.T) {
	entries := []mountutils.MountInfo{
		{MountPoint: "/source", Major: 0, Minor: 50},
		{MountPoint: "/target-a", Major: 0, Minor: 50},
		{MountPoint: "/target-b", Major: 0, Minor: 50},
		{MountPoint: "/other", Major: 0, Minor: 99},
		{MountPoint: "/target-c", Major: 0, Minor: 50},
	}
	targets := findBindMountTargets(entries, "0:50", "/source")
	assert.Equals(t, 3, len(targets))
	for _, tgt := range targets {
		if tgt == "/source" {
			t.Fatal("source path should not be in targets")
		}
	}
}

func TestFindBindMountTargets_NoTargets(t *testing.T) {
	entries := []mountutils.MountInfo{
		{MountPoint: "/source", Major: 0, Minor: 50},
		{MountPoint: "/other", Major: 0, Minor: 99},
	}
	targets := findBindMountTargets(entries, "0:50", "/source")
	assert.Equals(t, 0, len(targets))
}

func TestFindBindMountTargets_EmptyEntries(t *testing.T) {
	targets := findBindMountTargets(nil, "0:50", "/source")
	if targets != nil {
		t.Fatal("expected nil for empty entries")
	}
}

// --- RebuildMountMap tests (using injectable mountInfoProvider on DaemonsetMounter) ---

func TestRebuildMountMap_NoMetaDirectory(t *testing.T) {
	kubeletPath := t.TempDir()
	dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider(nil))

	err := dm.RebuildMountMap()
	assert.NoError(t, err)

	if dm.mountMap.Get("anything") != nil {
		t.Fatal("expected empty mount map")
	}
}

func TestRebuildMountMap_SkipsNonMetaFiles(t *testing.T) {
	kubeletPath := t.TempDir()
	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt")
	err := os.MkdirAll(metaDir, 0750)
	assert.NoError(t, err)

	// Create non-meta files
	os.Mkdir(filepath.Join(metaDir, "vol-123"), 0750)
	os.WriteFile(filepath.Join(metaDir, "something.txt"), []byte("hello"), 0640)

	dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider(nil))
	err = dm.RebuildMountMap()
	assert.NoError(t, err)

	if dm.mountMap.Get("vol-123") != nil {
		t.Fatal("should not create entries for non-meta files")
	}
}

func TestRebuildMountMap_SkipsInvalidMetaJSON(t *testing.T) {
	kubeletPath := t.TempDir()
	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt")
	err := os.MkdirAll(metaDir, 0750)
	assert.NoError(t, err)

	// Invalid meta file
	os.WriteFile(filepath.Join(metaDir, "vol-bad.meta.json"), []byte("not-json"), 0640)

	dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider(nil))
	err = dm.RebuildMountMap()
	assert.NoError(t, err) // should not error, just skip

	if dm.mountMap.Get("vol-bad") != nil {
		t.Fatal("should not create entry for invalid meta")
	}
}

func TestRebuildMountMap_CleansUpDeadSourceMounts(t *testing.T) {
	kubeletPath := t.TempDir()
	sourcePath := SourceMountPath(kubeletPath, "vol-dead")

	entry := &MountEntry{
		VolumeID:   "vol-dead",
		SourcePath: sourcePath,
		Params:     MountParams{AuthenticationSource: "driver", ServiceAccountName: "default"},
	}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	// Mount table has NO entry for sourcePath → source is dead
	dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider([]mountutils.MountInfo{
		{MountPoint: "/some/other/mount", Major: 0, Minor: 99},
	}))
	err = dm.RebuildMountMap()
	assert.NoError(t, err)

	// Entry should NOT be in the mount map (dead source skipped)
	if dm.mountMap.Get("vol-dead") != nil {
		t.Fatal("expected dead source volume to be skipped")
	}

	// Meta file should be cleaned up
	_, err = os.Stat(MetaFileName(kubeletPath, "vol-dead"))
	if !os.IsNotExist(err) {
		t.Fatal("expected meta file to be removed for dead source")
	}
}

func TestRebuildMountMap_RecoversLiveSourceWithBindMounts(t *testing.T) {
	kubeletPath := t.TempDir()
	sourcePath := SourceMountPath(kubeletPath, "vol-live")

	entry := &MountEntry{
		VolumeID:   "vol-live",
		SourcePath: sourcePath,
		Params: MountParams{
			MountOptions:             []string{"--allow-other"},
			AuthenticationSource:     "pod",
			ServiceAccountName:       "my-sa",
			ServiceAccountEKSRoleARN: "arn:aws:iam::123:role/r",
			PodNamespace:             "ns-a",
			FSGroup:                  "1000",
		},
	}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	// Simulate mount table: source + 3 bind mount targets share device ID
	dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider([]mountutils.MountInfo{
		{MountPoint: sourcePath, Major: 0, Minor: 42},
		{MountPoint: "/pods/pod-a/volumes/mount", Major: 0, Minor: 42},
		{MountPoint: "/pods/pod-b/volumes/mount", Major: 0, Minor: 42},
		{MountPoint: "/pods/pod-c/volumes/mount", Major: 0, Minor: 42},
		{MountPoint: "/unrelated", Major: 0, Minor: 99},
	}))
	err = dm.RebuildMountMap()
	assert.NoError(t, err)

	recovered := dm.mountMap.Get("vol-live")
	if recovered == nil {
		t.Fatal("expected recovered entry for vol-live")
	}

	assert.Equals(t, sourcePath, recovered.SourcePath)
	assert.Equals(t, 3, recovered.RefCount)
	assert.Equals(t, 3, len(recovered.Targets))
	assert.Equals(t, true, recovered.initialized)

	// Verify params were restored
	assert.Equals(t, "pod", recovered.Params.AuthenticationSource)
	assert.Equals(t, "my-sa", recovered.Params.ServiceAccountName)
	assert.Equals(t, "arn:aws:iam::123:role/r", recovered.Params.ServiceAccountEKSRoleARN)
	assert.Equals(t, "ns-a", recovered.Params.PodNamespace)
	assert.Equals(t, "1000", recovered.Params.FSGroup)
	assert.Equals(t, 1, len(recovered.Params.MountOptions))
	assert.Equals(t, "--allow-other", recovered.Params.MountOptions[0])
}

func TestRebuildMountMap_SourceWithNoBindMounts(t *testing.T) {
	kubeletPath := t.TempDir()
	sourcePath := SourceMountPath(kubeletPath, "vol-orphan")

	entry := &MountEntry{
		VolumeID:   "vol-orphan",
		SourcePath: sourcePath,
		Params:     MountParams{AuthenticationSource: "driver"},
	}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	// Source exists in mount table but no bind mounts share its device ID
	dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider([]mountutils.MountInfo{
		{MountPoint: sourcePath, Major: 0, Minor: 55},
		{MountPoint: "/unrelated", Major: 0, Minor: 99},
	}))
	err = dm.RebuildMountMap()
	assert.NoError(t, err)

	recovered := dm.mountMap.Get("vol-orphan")
	if recovered == nil {
		t.Fatal("expected recovered entry for vol-orphan")
	}
	assert.Equals(t, 0, recovered.RefCount)
	assert.Equals(t, 0, len(recovered.Targets))
	assert.Equals(t, true, recovered.initialized)
}

func TestRebuildMountMap_MultipleVolumes(t *testing.T) {
	kubeletPath := t.TempDir()

	sourceA := SourceMountPath(kubeletPath, "vol-a")
	sourceB := SourceMountPath(kubeletPath, "vol-b")

	entryA := &MountEntry{
		VolumeID:   "vol-a",
		SourcePath: sourceA,
		Params:     MountParams{ServiceAccountName: "sa-a"},
	}
	entryB := &MountEntry{
		VolumeID:   "vol-b",
		SourcePath: sourceB,
		Params:     MountParams{ServiceAccountName: "sa-b"},
	}
	err := WriteMeta(kubeletPath, entryA)
	assert.NoError(t, err)
	err = WriteMeta(kubeletPath, entryB)
	assert.NoError(t, err)

	dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider([]mountutils.MountInfo{
		{MountPoint: sourceA, Major: 0, Minor: 10},
		{MountPoint: "/target-a1", Major: 0, Minor: 10},
		{MountPoint: "/target-a2", Major: 0, Minor: 10},
		{MountPoint: sourceB, Major: 0, Minor: 20},
		{MountPoint: "/target-b1", Major: 0, Minor: 20},
	}))
	err = dm.RebuildMountMap()
	assert.NoError(t, err)

	recA := dm.mountMap.Get("vol-a")
	recB := dm.mountMap.Get("vol-b")
	if recA == nil || recB == nil {
		t.Fatal("expected both volumes to be recovered")
	}
	assert.Equals(t, 2, recA.RefCount)
	assert.Equals(t, "sa-a", recA.Params.ServiceAccountName)
	assert.Equals(t, 1, recB.RefCount)
	assert.Equals(t, "sa-b", recB.Params.ServiceAccountName)
}

// --- Write → Read round-trip ---

func TestWriteReadMetaCycle(t *testing.T) {
	kubeletPath := t.TempDir()

	entry := &MountEntry{
		VolumeID:   "vol-roundtrip",
		SourcePath: SourceMountPath(kubeletPath, "vol-roundtrip"),
		Params: MountParams{
			MountOptions:             []string{"--allow-other", "--prefix=data/", "--read-only"},
			AuthenticationSource:     "pod",
			ServiceAccountName:       "my-service-account",
			ServiceAccountEKSRoleARN: "arn:aws:iam::123456789012:role/s3-reader",
			PodNamespace:             "my-namespace",
			FSGroup:                  "65534",
		},
	}
	err := WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	meta, err := readMeta(MetaFileName(kubeletPath, "vol-roundtrip"))
	assert.NoError(t, err)

	assert.Equals(t, entry.VolumeID, meta.VolumeID)
	assert.Equals(t, entry.Params.AuthenticationSource, meta.AuthenticationSource)
	assert.Equals(t, entry.Params.ServiceAccountName, meta.ServiceAccountName)
	assert.Equals(t, entry.Params.ServiceAccountEKSRoleARN, meta.ServiceAccountEKSRoleARN)
	assert.Equals(t, entry.Params.PodNamespace, meta.PodNamespace)
	assert.Equals(t, entry.Params.FSGroup, meta.FSGroup)
	assert.Equals(t, len(entry.Params.MountOptions), len(meta.MountOptions))
	for i, opt := range entry.Params.MountOptions {
		assert.Equals(t, opt, meta.MountOptions[i])
	}
}

func TestRemoveMeta_OnlyRemovesTargetVolume(t *testing.T) {
	kubeletPath := t.TempDir()

	for _, volID := range []string{"vol-keep", "vol-remove", "vol-also-keep"} {
		entry := &MountEntry{VolumeID: volID, SourcePath: "/source/" + volID}
		err := WriteMeta(kubeletPath, entry)
		assert.NoError(t, err)
	}

	RemoveMeta(kubeletPath, "vol-remove")

	_, err := os.Stat(MetaFileName(kubeletPath, "vol-remove"))
	if !os.IsNotExist(err) {
		t.Fatal("expected vol-remove meta to be deleted")
	}
	_, err = os.Stat(MetaFileName(kubeletPath, "vol-keep"))
	assert.NoError(t, err)
	_, err = os.Stat(MetaFileName(kubeletPath, "vol-also-keep"))
	assert.NoError(t, err)
}
