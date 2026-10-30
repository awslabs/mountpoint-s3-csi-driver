package targetpath_test

import (
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/targetpath"
)

// Parse takes the target path from the kubelet and its result names directories on the node, so whatever
// it is given it must not panic, and the IDs it accepts must each be one non-empty path segment.
//
// Not asserted yet: that the IDs come from a literal `/pods/<pod>/volumes/kubernetes.io~csi/<volume>/mount`
// suffix. The regex leaves the `.` in `kubernetes.io~csi` unescaped, so the fuzzer finds it accepting
// `/pods/0/volumes/kubernetes0io~csi/0/mount`. Add that check, and `.`/`..` rejection, with the fix.
//
// `go test` runs the seeds below; `go test -fuzz=FuzzParse ./pkg/driver/node/targetpath/` explores further.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"/var/lib/kubelet/pods/d8c872d7-a29c-4362-81b1-9912370d0813/volumes/kubernetes.io~csi/s3-pv/mount",
		"/var/lib/kubelet/pods/8b40411d/volumes/kubernetes.io~csi/s3-csi~driver/mount",
		"/var/lib/kubelet/pods/../volumes/kubernetes.io~csi/../mount",
		"/var/lib/kubelet/pods//volumes/kubernetes.io~csi//mount",
		"/var/lib/kubelet/pods/a/volumes/kubernetes.io~csi/b/mount/",
		"/var/lib/kubelet/pods/a/b/volumes/kubernetes.io~csi/c/mount",
		"/pods/a/volumes/kubernetes.io~csi/b/mount\n/pods/c/volumes/kubernetes.io~csi/d/mount",
		"pods/a/volumes/kubernetes.io~csi/b/mount",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, path string) {
		parsed, err := targetpath.Parse(path)
		if err != nil {
			return
		}
		for name, id := range map[string]string{"pod ID": parsed.PodID, "volume ID": parsed.VolumeID} {
			if id == "" {
				t.Errorf("Parse(%q) accepted an empty %s", path, name)
			}
			if strings.Contains(id, "/") {
				t.Errorf("Parse(%q) returned a %s spanning several path segments: %q", path, name, id)
			}
		}
		if !strings.HasSuffix(path, "/"+parsed.VolumeID+"/mount") {
			t.Errorf("Parse(%q) = %+v, whose volume ID is not the segment before the trailing /mount", path, parsed)
		}
	})
}
