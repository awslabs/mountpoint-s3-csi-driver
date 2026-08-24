package mounter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
	mountutils "k8s.io/mount-utils"
)

// noopCredProvider is a credential provider that does nothing, for tests that need cleanupMount.
type noopCredProvider struct{}

func (p *noopCredProvider) Provide(_ context.Context, _ credentialprovider.ProvideContext) (envprovider.Environment, string, error) {
	return nil, "", nil
}

func (p *noopCredProvider) Cleanup(_ credentialprovider.CleanupContext) error {
	return nil
}

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

// newTestDMWithMountInfoAndCredProvider creates a DaemonsetMounter with a no-op credential
// provider and fake mounter, suitable for tests that exercise cleanupMount during rebuild.
func newTestDMWithMountInfoAndCredProvider(kubeletPath string, provider mountInfoProviderFunc) *DaemonsetMounter {
	dm, _ := newTestDMWithFakeMounter(kubeletPath, provider)
	return dm
}

// newTestDMWithFakeMounter is like newTestDMWithMountInfoAndCredProvider but also returns
// the underlying FakeMounter, so tests can register healthy mounts that CheckMountpoint sees.
func newTestDMWithFakeMounter(kubeletPath string, provider mountInfoProviderFunc) (*DaemonsetMounter, *mountutils.FakeMounter) {
	fakeMounter := mountutils.NewFakeMounter(nil)
	dm := &DaemonsetMounter{
		kubeletPath:       kubeletPath,
		mountInfoProvider: provider,
		mountMap:          NewMountMap(),
		mount:             mpmounter.NewWithMount(fakeMounter),
		credProvider:      &noopCredProvider{},
	}
	return dm, fakeMounter
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

	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "meta")
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
	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "meta")
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
	expected := "/var/lib/kubelet/plugins/s3.csi.aws.com/meta/vol-test-123.meta.json"
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
	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "meta")
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
	metaDir := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "meta")
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
	dm := newTestDMWithMountInfoAndCredProvider(kubeletPath, fakeMountInfoProvider([]mountutils.MountInfo{
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
	commDir := "/var/lib/kubelet/pods/mounter-uid-abc/volumes/kubernetes.io~empty-dir/comm"

	entry := &MountEntry{
		VolumeID:   "vol-live",
		SourcePath: sourcePath,
		CommDir:    commDir,
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
	assert.Equals(t, commDir, recovered.CommDir)
	assert.Equals(t, 3, recovered.RefCount)
	assert.Equals(t, 3, len(recovered.Targets))
	assert.Equals(t, true, recovered.sourceMounted)

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
	assert.Equals(t, true, recovered.sourceMounted)
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

func TestRebuildMountMap_DeadSourceCleansCredentials(t *testing.T) {
	kubeletPath := t.TempDir()

	// Create a comm dir with credential files that should be cleaned up
	commDir := filepath.Join(kubeletPath, "pods", "mounter-uid", "volumes", "kubernetes.io~empty-dir", "comm")
	credDir := filepath.Join(commDir, "vol-dead-creds")
	err := os.MkdirAll(credDir, 0750)
	assert.NoError(t, err)
	// Simulate a token file in the cred dir
	os.WriteFile(filepath.Join(credDir, "token.jwt"), []byte("secret"), 0600)
	// Simulate an error file
	os.WriteFile(filepath.Join(commDir, "vol-dead-creds.error"), []byte("mount failed"), 0600)

	entry := &MountEntry{
		VolumeID:   "vol-dead-creds",
		SourcePath: SourceMountPath(kubeletPath, "vol-dead-creds"),
		CommDir:    commDir,
		Params:     MountParams{AuthenticationSource: "driver"},
	}
	err = WriteMeta(kubeletPath, entry)
	assert.NoError(t, err)

	// Mount table has NO entry for sourcePath → source is dead
	dm := newTestDMWithMountInfoAndCredProvider(kubeletPath, fakeMountInfoProvider(nil))
	err = dm.RebuildMountMap()
	assert.NoError(t, err)

	// Credential directory should be cleaned up
	_, err = os.Stat(credDir)
	if !os.IsNotExist(err) {
		t.Fatal("expected credential directory to be removed for dead source")
	}

	// Error file should be cleaned up
	_, err = os.Stat(filepath.Join(commDir, "vol-dead-creds.error"))
	if !os.IsNotExist(err) {
		t.Fatal("expected error file to be removed for dead source")
	}

	// Meta file should be cleaned up
	_, err = os.Stat(MetaFileName(kubeletPath, "vol-dead-creds"))
	if !os.IsNotExist(err) {
		t.Fatal("expected meta file to be removed for dead source")
	}
}

// seedEntry inserts a source-mounted entry into the map. i.e pretend a mount already exists
func seedEntry(dm *DaemonsetMounter, volumeID, sourcePath, commDir string, targets []string) *MountEntry {
	entry, _ := dm.mountMap.GetOrCreate(volumeID)
	entry.SourcePath = sourcePath
	entry.CommDir = commDir
	entry.Params = MountParams{AuthenticationSource: "driver"}
	entry.Targets = targets
	entry.RefCount = len(targets)
	entry.sourceMounted = true
	return entry
}

// registerSourceMount registers the source as a mountpoint mount in the fake
// mounter and creates its dir, so teardown's unmount path (cleanupMount ->
// unmountIfMounted -> IsMountPoint/Unmount) has a real mount to act on.
func registerSourceMount(t *testing.T, fakeMounter *mountutils.FakeMounter, sourcePath string) {
	t.Helper()
	assert.NoError(t, os.MkdirAll(sourcePath, 0750))
	assert.NoError(t, fakeMounter.Mount("mountpoint-s3", sourcePath, "fuse", nil))
}

func statErr(path string) error {
	_, err := os.Stat(path)
	return err
}

// TestCleanupOrphans covers the periodic cleanup job's per-entry reconcile logic.
func TestCleanupOrphans(t *testing.T) {
	const volumeID = "vol-1"

	tests := []struct {
		name string
		// setup seeds the map and on-host state for this case.
		setup func(t *testing.T, dm *DaemonsetMounter, fakeMounter *mountutils.FakeMounter, kubeletPath, sourcePath, targetA string)
		// mountInfo is the fake kernel mount table for this case.
		mountInfo func(sourcePath, targetA string) []mountutils.MountInfo

		expectEntryGone bool // map entry removed
		expectMetaGone  bool // meta file removed
		expectRefCount  int  // asserted when the entry survives
	}{
		{
			// Dead source (nothing in the mount table): source, creds, error file,
			// meta, and map entry all removed.
			name: "dead source torn down",
			setup: func(t *testing.T, dm *DaemonsetMounter, _ *mountutils.FakeMounter, kubeletPath, sourcePath, _ string) {
				commDir := filepath.Join(kubeletPath, "comm")
				assert.NoError(t, os.MkdirAll(filepath.Join(commDir, volumeID), 0750))
				os.WriteFile(filepath.Join(commDir, volumeID+".error"), []byte("boom"), 0600)
				entry := seedEntry(dm, volumeID, sourcePath, commDir, nil)
				assert.NoError(t, WriteMeta(kubeletPath, entry))
			},
			mountInfo:       func(_, _ string) []mountutils.MountInfo { return nil },
			expectEntryGone: true,
			expectMetaGone:  true,
		},
		{
			// Healthy source: a tracked target the kernel no longer shows is dropped
			// and the refcount fixed, but the source stays because another target is
			// still live. (Also covers the "healthy mount left alone" path.)
			name: "reconciles refcount",
			setup: func(t *testing.T, dm *DaemonsetMounter, fakeMounter *mountutils.FakeMounter, kubeletPath, sourcePath, targetA string) {
				targetB := filepath.Join(kubeletPath, "pods", "wl-b", "mount")
				registerSourceMount(t, fakeMounter, sourcePath)
				seedEntry(dm, volumeID, sourcePath, "", []string{targetA, targetB})
			},
			mountInfo: func(sourcePath, targetA string) []mountutils.MountInfo {
				return []mountutils.MountInfo{ // targetB is gone from the kernel
					{MountPoint: sourcePath, Major: 0, Minor: 42},
					{MountPoint: targetA, Major: 0, Minor: 42},
				}
			},
			expectRefCount: 1,
		},
		{
			// Teardown's cleanupMount fails (unmount errors): the entry and meta are
			// KEPT so a later tick retries — we must not lose bookkeeping on failure.
			name: "failed cleanup keeps entry and meta",
			setup: func(t *testing.T, dm *DaemonsetMounter, fakeMounter *mountutils.FakeMounter, kubeletPath, sourcePath, _ string) {
				registerSourceMount(t, fakeMounter, sourcePath)
				fakeMounter.UnmountFunc = func(string) error { return fmt.Errorf("unmount failed") }
				entry := seedEntry(dm, volumeID, sourcePath, "", nil) // no live targets -> teardown attempted
				assert.NoError(t, WriteMeta(kubeletPath, entry))
			},
			mountInfo: func(sourcePath, _ string) []mountutils.MountInfo {
				return []mountutils.MountInfo{{MountPoint: sourcePath, Major: 0, Minor: 42}}
			},
			expectRefCount: 0, // entry survives (not torn down)
		},
		{
			// Healthy source, no bind mounts left in the kernel: torn down.
			name: "last consumer gone torn down",
			setup: func(t *testing.T, dm *DaemonsetMounter, fakeMounter *mountutils.FakeMounter, kubeletPath, sourcePath, _ string) {
				registerSourceMount(t, fakeMounter, sourcePath)
				entry := seedEntry(dm, volumeID, sourcePath, "", []string{filepath.Join(kubeletPath, "pods", "gone", "mount")})
				assert.NoError(t, WriteMeta(kubeletPath, entry))
			},
			mountInfo: func(sourcePath, _ string) []mountutils.MountInfo {
				return []mountutils.MountInfo{{MountPoint: sourcePath, Major: 0, Minor: 42}}
			},
			expectEntryGone: true,
			expectMetaGone:  true,
		},
		{
			// The source is healthy and the kernel shows a live bind mount that we are not
			// tracking. Cleanup adopts it, so the refcount becomes 1 and the source is kept.
			name: "untracked live bind mount adopted not torn down",
			setup: func(t *testing.T, dm *DaemonsetMounter, fakeMounter *mountutils.FakeMounter, _, sourcePath, _ string) {
				registerSourceMount(t, fakeMounter, sourcePath) // source is healthy
				seedEntry(dm, volumeID, sourcePath, "", nil)    // we track no targets
			},
			mountInfo: func(sourcePath, targetA string) []mountutils.MountInfo {
				return []mountutils.MountInfo{
					{MountPoint: sourcePath, Major: 0, Minor: 42},
					{MountPoint: targetA, Major: 0, Minor: 42}, // untracked but live
				}
			},
			expectRefCount: 1,
		},
		{
			// A dead source still in the mount table with a live tracked target (mounter
			// crashed, workload pod still running) is torn down: teardown is gated on
			// source health, not on live targets. The stale target's mount is broken
			// anyway (recreate the pod to recover); the meta/cred churn a republishing
			// workload would otherwise cause is stopped in Mount, not here.
			name: "dead source with live target torn down",
			setup: func(t *testing.T, dm *DaemonsetMounter, _ *mountutils.FakeMounter, kubeletPath, sourcePath, targetA string) {
				// Source is not registered as healthy, so the health probe reports it dead.
				entry := seedEntry(dm, volumeID, sourcePath, "", []string{targetA})
				assert.NoError(t, WriteMeta(kubeletPath, entry))
			},
			mountInfo: func(sourcePath, targetA string) []mountutils.MountInfo {
				return []mountutils.MountInfo{
					{MountPoint: sourcePath, Major: 0, Minor: 42},
					{MountPoint: targetA, Major: 0, Minor: 42},
				}
			},
			expectEntryGone: true,
			expectMetaGone:  true,
		},
		{
			// A mid-creation placeholder (empty SourcePath) is skipped, not deleted.
			name: "skips placeholder",
			setup: func(_ *testing.T, dm *DaemonsetMounter, _ *mountutils.FakeMounter, _, _, _ string) {
				dm.mountMap.GetOrCreate(volumeID)
			},
			mountInfo:      func(_, _ string) []mountutils.MountInfo { return nil },
			expectRefCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeletPath := t.TempDir()
			sourcePath := SourceMountPath(kubeletPath, volumeID)
			targetA := filepath.Join(kubeletPath, "pods", "wl-a", "mount")

			dm, fakeMounter := newTestDMWithFakeMounter(kubeletPath, fakeMountInfoProvider(tt.mountInfo(sourcePath, targetA)))
			tt.setup(t, dm, fakeMounter, kubeletPath, sourcePath, targetA)

			dm.CleanupOrphans()

			entry := dm.mountMap.Get(volumeID)
			if tt.expectEntryGone {
				assert.Equals(t, true, entry == nil)
			} else {
				assert.Equals(t, true, entry != nil)
				assert.Equals(t, tt.expectRefCount, entry.RefCount)
			}
			if tt.expectMetaGone {
				assert.Equals(t, true, os.IsNotExist(statErr(MetaFileName(kubeletPath, volumeID))))
			}
		})
	}
}

// TestCleanupOrphans_MountTableReadError verifies that if reading the mount table
// fails, the entry is left untouched rather than acted on with a bad view.
func TestCleanupOrphans_MountTableReadError(t *testing.T) {
	kubeletPath := t.TempDir()
	failingProvider := func() ([]mountutils.MountInfo, error) {
		return nil, fmt.Errorf("read mountinfo failed")
	}
	dm, _ := newTestDMWithFakeMounter(kubeletPath, failingProvider)
	seedEntry(dm, "vol-1", SourceMountPath(kubeletPath, "vol-1"), "", nil)

	dm.CleanupOrphans()

	assert.Equals(t, true, dm.mountMap.Get("vol-1") != nil)
}
