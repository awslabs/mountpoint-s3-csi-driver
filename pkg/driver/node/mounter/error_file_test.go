package mounter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/errorfile"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// With worst-case Kubernetes names and a full-length reason, the FailedMount event the kubelet builds
// around mountFailedError still carries the whole reason and the hint, within 2 KiB.
func TestMountFailedError_WorstCaseNamesStayReadable(t *testing.T) {
	saved := mounterNamespace
	mounterNamespace = strings.Repeat("n", 63)
	t.Cleanup(func() { mounterNamespace = saved })

	pv := strings.Repeat("p", 253)
	pod := strings.Repeat("m", 253)
	bucket := strings.Repeat("b", 63)
	commDir := "/var/lib/kubelet/pods/" + strings.Repeat("u", 36) + "/volumes/kubernetes.io~empty-dir/comm"
	dm := &DaemonsetMounter{nodeID: "node"}
	dm.mounter.Store(&mounterPod{name: pod, commDir: commDir})

	reason := errorfile.Format("Mountpoint exited with code 1: Error: " + strings.Repeat("cause ", 200) + "ROOT")
	assert.Equals(t, true, len(reason) > errorfile.MaxBytes-5 && len(reason) <= errorfile.MaxBytes)

	target := "/var/lib/kubelet/pods/" + strings.Repeat("w", 36) + "/volumes/kubernetes.io~csi/" + pv + "/mount"
	// pkg/driver/node/node.go wraps Mount's error; the kubelet adds its own prefix.
	nodeErr := fmt.Sprintf("Could not mount %q at %q: %v", bucket, target, dm.mountFailedError(commDir, pv, reason))
	event := fmt.Sprintf("MountVolume.SetUp failed for volume %q : rpc error: code = Internal desc = %s", pv, nodeErr)

	if len(event) > 2048 {
		t.Fatalf("event is %d bytes, want at most 2048", len(event))
	}
	assert.Contains(t, event, reason)
	assert.Contains(t, event, "`kubectl logs -n "+mounterNamespace+" "+pod+" -c mounter | grep -F '["+pv+"]'`")
	t.Logf("worst-case event: %d bytes", len(event))
}

func TestMountFailedError_EmptyReason(t *testing.T) {
	dm := &DaemonsetMounter{nodeID: "node"}
	msg := dm.mountFailedError("/comm", "pv", "").Error()
	assert.Contains(t, msg, "the mounter reported no reason. You can check")
}

// A file that cannot be opened (e.g. EMFILE, EACCES) is not taken as a reasonless failure: the poller retries.
func TestConsumeErrorFile_UnopenableFileIsNotFound(t *testing.T) {
	assert.Equals(t, false, os.Geteuid() == 0)
	path := filepath.Join(t.TempDir(), "pv.error")
	assert.NoError(t, os.WriteFile(path, []byte("Error: x"), 0000))

	_, found, err := consumeErrorFile(path)
	assert.Equals(t, false, found)
	assert.NoError(t, err)

	assert.NoError(t, os.Chmod(path, 0600))
	reason, found, err := consumeErrorFile(path)
	assert.Equals(t, true, found)
	assert.NoError(t, err)
	assert.Equals(t, "Error: x", reason)
}
