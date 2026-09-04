package mounter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	mountutils "k8s.io/mount-utils"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// --- test doubles ---

// errCache is a client.Reader whose List always fails.
type errCache struct {
	err error
}

func (c *errCache) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return c.err
}

func (c *errCache) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return c.err
}

// recordingCredProvider records the last ProvideContext it was given.
type recordingCredProvider struct {
	called  bool
	lastCtx credentialprovider.ProvideContext
}

func (p *recordingCredProvider) Provide(_ context.Context, provideCtx credentialprovider.ProvideContext) (envprovider.Environment, string, error) {
	p.called = true
	p.lastCtx = provideCtx
	return nil, "", nil
}

func (p *recordingCredProvider) Cleanup(_ credentialprovider.CleanupContext) error {
	return nil
}

// newProvideCtx builds a minimal ProvideContext for testing.
func newProvideCtx(volumeID, workloadUID string, authSource credentialprovider.AuthenticationSource) credentialprovider.ProvideContext {
	return credentialprovider.ProvideContext{
		VolumeID:             volumeID,
		WorkloadPodID:        workloadUID,
		AuthenticationSource: authSource,
	}
}

// --- Source mount dir separation ---

func TestV3SourceMountDir(t *testing.T) {
	t.Run("V3 uses separate dir from V2", func(t *testing.T) {
		kubeletPath := "/var/lib/kubelet"
		v2 := SourceMountDir(kubeletPath)
		v3 := V3SourceMountDir(kubeletPath)

		assert.Equals(t, filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt"), v2)
		assert.Equals(t, filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "v3mnt"), v3)

		// "mnt" must not be a path prefix of "v3mnt" (they are siblings).
		if len(v3) > len(v2) && v3[:len(v2)+1] == v2+string(filepath.Separator) {
			t.Fatalf("V3 source dir %q must not be nested under V2 source dir %q", v3, v2)
		}
	})

	t.Run("SourceMountPath uses V3 dir", func(t *testing.T) {
		kubeletPath := "/var/lib/kubelet"
		got := SourceMountPath(kubeletPath, "pv-123")
		want := filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "v3mnt", "pv-123")
		assert.Equals(t, want, got)
	})
}

// --- isV3TrackedTarget ---

func TestIsV3TrackedTarget(t *testing.T) {
	dm := newTestDMWithMountInfo("/var/lib/kubelet", fakeMountInfoProvider(nil))
	target := "/var/lib/kubelet/pods/wl-a/volumes/kubernetes.io~csi/pv-1/mount"

	t.Run("not tracked returns false", func(t *testing.T) {
		assert.Equals(t, false, dm.isV3TrackedTarget(target))
	})

	t.Run("tracked target returns true", func(t *testing.T) {
		seedEntry(dm, "pv-1", SourceMountPath("/var/lib/kubelet", "pv-1"), "", []string{target})
		assert.Equals(t, true, dm.isV3TrackedTarget(target))
	})

	t.Run("different target still not tracked", func(t *testing.T) {
		assert.Equals(t, false, dm.isV3TrackedTarget("/var/lib/kubelet/pods/wl-b/volumes/kubernetes.io~csi/pv-1/mount"))
	})
}

// --- isLegacyV2Mount ---

func TestIsLegacyV2Mount(t *testing.T) {
	kubeletPath := "/var/lib/kubelet"
	target := filepath.Join(kubeletPath, "pods", "wl-a", "volumes", "kubernetes.io~csi", "pv-1", "mount")
	v2Source := filepath.Join(SourceMountDir(kubeletPath), "mp-abc123")
	v3Source := SourceMountPath(kubeletPath, "pv-1")

	t.Run("disabled by env", func(t *testing.T) {
		dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider(nil))
		isV2, err := dm.isLegacyV2Mount("/some/target")
		assert.NoError(t, err)
		assert.Equals(t, false, isV2)
	})

	t.Run("detects V2 source", func(t *testing.T) {
		t.Setenv("SUPPORT_LEGACY_POD_MOUNTS", "true")
		dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider([]mountutils.MountInfo{
			{MountPoint: target, Major: 0, Minor: 42},
			{MountPoint: v2Source, Major: 0, Minor: 42},
		}))
		isV2, err := dm.isLegacyV2Mount(target)
		assert.NoError(t, err)
		assert.Equals(t, true, isV2)
	})

	t.Run("V3 source is not V2", func(t *testing.T) {
		t.Setenv("SUPPORT_LEGACY_POD_MOUNTS", "true")
		dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider([]mountutils.MountInfo{
			{MountPoint: target, Major: 0, Minor: 7},
			{MountPoint: v3Source, Major: 0, Minor: 7},
		}))
		isV2, err := dm.isLegacyV2Mount(target)
		assert.NoError(t, err)
		assert.Equals(t, false, isV2)
	})

	t.Run("target not in mount table", func(t *testing.T) {
		t.Setenv("SUPPORT_LEGACY_POD_MOUNTS", "true")
		dm := newTestDMWithMountInfo(kubeletPath, fakeMountInfoProvider(nil))
		isV2, err := dm.isLegacyV2Mount(target)
		assert.NoError(t, err)
		assert.Equals(t, false, isV2)
	})

	t.Run("mount table error propagates", func(t *testing.T) {
		t.Setenv("SUPPORT_LEGACY_POD_MOUNTS", "true")
		wantErr := errors.New("boom reading mountinfo")
		dm := &DaemonsetMounter{
			kubeletPath:       kubeletPath,
			mountMap:          NewMountMap(),
			mountInfoProvider: func() ([]mountutils.MountInfo, error) { return nil, wantErr },
		}
		isV2, err := dm.isLegacyV2Mount("/some/target")
		assert.Equals(t, false, isV2)
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("expected wrapped mount table error, got: %v", err)
		}
	})
}

// --- findV2S3PodAttachment ---

func TestFindV2S3PodAttachment(t *testing.T) {
	t.Run("nil cache returns nil", func(t *testing.T) {
		dm := newTestDMWithMountInfo("/var/lib/kubelet", fakeMountInfoProvider(nil))
		dm.nodeID = "node-1"

		s3pa, mpPodName, err := dm.findV2S3PodAttachment(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourceDriver), "")
		assert.NoError(t, err)
		assert.Equals(t, "", mpPodName)
		if s3pa != nil {
			t.Fatalf("expected nil S3PA when cache is nil, got %v", s3pa)
		}
	})

	t.Run("matches by workload UID", func(t *testing.T) {
		dm := newTestDMWithMountInfo("/var/lib/kubelet", fakeMountInfoProvider(nil))
		dm.nodeID = "node-1"
		dm.s3paCache = &FakeCache{TestItems: []crdv2.MountpointS3PodAttachment{{
			Spec: crdv2.MountpointS3PodAttachmentSpec{
				NodeName: "node-1", PersistentVolumeName: "pv-1", VolumeID: "vol-1",
				WorkloadServiceAccountIAMRoleARN: "arn:aws:iam::123:role/committed",
				MountpointS3PodAttachments: map[string][]crdv2.WorkloadAttachment{
					"mp-xyz": {{WorkloadPodUID: "wl-1"}},
				},
			},
		}}}

		s3pa, mpPodName, err := dm.findV2S3PodAttachment(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourcePod), "")
		assert.NoError(t, err)
		assert.Equals(t, "mp-xyz", mpPodName)
		if s3pa == nil {
			t.Fatal("expected a matching S3PA")
		}
		assert.Equals(t, "arn:aws:iam::123:role/committed", s3pa.Spec.WorkloadServiceAccountIAMRoleARN)
	})

	t.Run("no matching workload returns nil", func(t *testing.T) {
		dm := newTestDMWithMountInfo("/var/lib/kubelet", fakeMountInfoProvider(nil))
		dm.nodeID = "node-1"
		dm.s3paCache = &FakeCache{TestItems: []crdv2.MountpointS3PodAttachment{{
			Spec: crdv2.MountpointS3PodAttachmentSpec{
				NodeName: "node-1", PersistentVolumeName: "pv-1", VolumeID: "vol-1",
				MountpointS3PodAttachments: map[string][]crdv2.WorkloadAttachment{
					"mp-xyz": {{WorkloadPodUID: "some-other-workload"}},
				},
			},
		}}}

		s3pa, mpPodName, err := dm.findV2S3PodAttachment(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourcePod), "")
		assert.NoError(t, err)
		assert.Equals(t, "", mpPodName)
		if s3pa != nil {
			t.Fatalf("expected nil S3PA, got %v", s3pa)
		}
	})

	t.Run("list error propagates", func(t *testing.T) {
		dm := newTestDMWithMountInfo("/var/lib/kubelet", fakeMountInfoProvider(nil))
		dm.nodeID = "node-1"
		dm.s3paCache = &errCache{err: errors.New("cache list failed")}

		s3pa, mpPodName, err := dm.findV2S3PodAttachment(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourceDriver), "")
		if err == nil {
			t.Fatal("expected error when cache list fails")
		}
		assert.Equals(t, "", mpPodName)
		if s3pa != nil {
			t.Fatalf("expected nil S3PA on list error, got %v", s3pa)
		}
	})
}

// --- handleLegacyV2Refresh ---

func TestHandleLegacyV2Refresh(t *testing.T) {
	t.Run("refreshes credentials for running pod", func(t *testing.T) {
		kubeletPath := t.TempDir()
		mpPodUID := "mp-uid-123"
		credsDir := mppod.PathOnHost(filepath.Join(kubeletPath, "pods", mpPodUID), mppod.KnownPathCredentials)
		assert.NoError(t, os.MkdirAll(credsDir, 0700))

		mpPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "mp-xyz", Namespace: mountpointPodNamespace, UID: types.UID(mpPodUID)},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		rec := &recordingCredProvider{}
		dm := &DaemonsetMounter{
			clientset: fake.NewSimpleClientset(mpPod), nodeID: "node-1", kubeletPath: kubeletPath,
			credProvider: rec, mountMap: NewMountMap(),
			s3paCache: &FakeCache{TestItems: []crdv2.MountpointS3PodAttachment{{
				Spec: crdv2.MountpointS3PodAttachmentSpec{
					NodeName: "node-1", PersistentVolumeName: "pv-1", VolumeID: "vol-1",
					WorkloadServiceAccountIAMRoleARN: "arn:aws:iam::123:role/committed",
					MountpointS3PodAttachments:       map[string][]crdv2.WorkloadAttachment{"mp-xyz": {{WorkloadPodUID: "wl-1"}}},
				},
			}}},
		}

		err := dm.handleLegacyV2Refresh(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourcePod), "")
		assert.NoError(t, err)
		if !rec.called {
			t.Fatal("expected credProvider.Provide to be called")
		}
		assert.Equals(t, "arn:aws:iam::123:role/committed", rec.lastCtx.ServiceAccountEKSRoleARN)
		assert.Equals(t, mpPodUID, rec.lastCtx.MountpointPodID)
	})

	t.Run("no S3PA skips refresh", func(t *testing.T) {
		rec := &recordingCredProvider{}
		dm := &DaemonsetMounter{
			clientset: fake.NewSimpleClientset(), nodeID: "node-1", kubeletPath: t.TempDir(),
			credProvider: rec, mountMap: NewMountMap(),
			s3paCache: &FakeCache{TestItems: nil},
		}

		err := dm.handleLegacyV2Refresh(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourcePod), "")
		assert.NoError(t, err)
		if rec.called {
			t.Fatal("expected no credential refresh when no S3PA matches")
		}
	})

	t.Run("pod not running skips refresh", func(t *testing.T) {
		mpPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "mp-xyz", Namespace: mountpointPodNamespace, UID: "mp-uid-123"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		}
		rec := &recordingCredProvider{}
		dm := &DaemonsetMounter{
			clientset: fake.NewSimpleClientset(mpPod), nodeID: "node-1", kubeletPath: t.TempDir(),
			credProvider: rec, mountMap: NewMountMap(),
			s3paCache: &FakeCache{TestItems: []crdv2.MountpointS3PodAttachment{{
				Spec: crdv2.MountpointS3PodAttachmentSpec{
					NodeName: "node-1", PersistentVolumeName: "pv-1", VolumeID: "vol-1",
					MountpointS3PodAttachments: map[string][]crdv2.WorkloadAttachment{"mp-xyz": {{WorkloadPodUID: "wl-1"}}},
				},
			}}},
		}

		err := dm.handleLegacyV2Refresh(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourcePod), "")
		assert.NoError(t, err)
		if rec.called {
			t.Fatal("expected no credential refresh when pod is not running")
		}
	})

	t.Run("S3PA cache list error propagates", func(t *testing.T) {
		dm := &DaemonsetMounter{
			clientset: fake.NewSimpleClientset(), nodeID: "node-1", kubeletPath: t.TempDir(),
			credProvider: &recordingCredProvider{}, mountMap: NewMountMap(),
			s3paCache: &errCache{err: errors.New("cache list failed")},
		}

		err := dm.handleLegacyV2Refresh(context.Background(), "pv-1", newProvideCtx("vol-1", "wl-1", credentialprovider.AuthenticationSourceDriver), "")
		if err == nil {
			t.Fatal("expected error on cache list failure")
		}
	})
}
