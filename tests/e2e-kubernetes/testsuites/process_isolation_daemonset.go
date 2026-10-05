package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// The UID range the driver allocates from, mirroring pkg/driver/node/mounter.UIDRange{Start,End}.
const (
	uidRangeStart = 65536
	uidRangeEnd   = 131071
)

// Expected modes on the paths the mounter and Mountpoint share, mirroring the driver's constants.
const (
	expectedCommDirMode  = "711"
	expectedSockMode     = "600"
	expectedCredDirMode  = "700"
	expectedCredFileMode = "400"

	// mountpointProcessName is the `comm` of a Mountpoint process, as /proc/<pid>/status reports it.
	mountpointProcessName = "mount-s3"

	// mountSockName is the socket csi-node sends mount requests to, inside the comm directory.
	mountSockName = "mount.sock"
)

// How long to wait for Mountpoint processes and mounts to appear after the pods are running.
const (
	mountpointProcessTimeout = 2 * time.Minute
	mountpointProcessPoll    = 5 * time.Second
)

type s3CSIProcessIsolationDaemonsetTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitS3CSIProcessIsolationDaemonsetTestSuite verifies that the kernel isolates each Mountpoint
// process: its own UID, no groups, no capabilities, and credentials only it can reach.
func InitS3CSIProcessIsolationDaemonsetTestSuite() storageframework.TestSuite {
	return &s3CSIProcessIsolationDaemonsetTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "processisolationdaemonset",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIProcessIsolationDaemonsetTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIProcessIsolationDaemonsetTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIProcessIsolationDaemonsetTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var l local

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"isolation-ds", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	// The property the feature rests on: two mounts on a node get two UIDs. Sharing one leaves the
	// kernel no basis to separate them.
	ginkgo.It("should run each Mountpoint process under a distinct non-root UID", func(ctx context.Context) {
		first := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, first)
		second := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, second)

		// Both pods must land on one node, since that is where two Mountpoint processes share a mounter
		// pod. createPodsOnSameNode places pods for a single volume, so it picks the node with the first
		// and the second volume's pod is then placed on that same node.
		targetNode, pods := createPodsOnSameNode(ctx, f, 1, first)
		pods = append(pods, createPodOnNode(ctx, f, targetNode, second)...)
		defer deletePodsInOrder(ctx, f, pods)

		// Each mount is identified by the UID csi-node chowned its credential directory to. Counting
		// mount-s3 processes instead would be satisfied by another spec's mounts, since specs run in
		// parallel and share this node's mounter pod.
		ginkgo.By("Reading the UID each of the two mounts was given")
		commDir := commDirHostPath(ctx, f, targetNode)
		uids := []int{
			statPath(ctx, f, targetNode, filepath.Join(commDir, first.Pv.Name)).uid,
			statPath(ctx, f, targetNode, filepath.Join(commDir, second.Pv.Name)).uid,
		}
		gomega.Expect(uids[0]).ToNot(gomega.Equal(uids[1]),
			"two mounts on one node must be given different UIDs, both got %d", uids[0])

		for _, uid := range uids {
			c := mountpointProcessRunningAs(ctx, f, targetNode, uid)
			ginkgo.By(fmt.Sprintf("Checking Mountpoint pid %s (uid %d)", c.pid, c.uid()))

			gomega.Expect(c.uid()).To(gomega.And(
				gomega.BeNumerically(">=", uidRangeStart),
				gomega.BeNumerically("<=", uidRangeEnd),
			), "Mountpoint pid %s must run under an allocated UID, got %v", c.pid, c.uids)

			// Every UID and GID, not just the effective ones: a process that switched only those keeps
			// the ones it came from and could switch back.
			for _, id := range append(append([]int{}, c.uids...), c.gids...) {
				gomega.Expect(id).To(gomega.Equal(c.uid()),
					"Mountpoint pid %s must have every UID and GID set to %d, got uids=%v gids=%v",
					c.pid, c.uid(), c.uids, c.gids)
			}

			// Inheriting the mounter's GID 0 would give every Mountpoint anything group-root-readable.
			gomega.Expect(c.groups).To(gomega.BeEmpty(),
				"Mountpoint pid %s must have no supplementary groups, got %q", c.pid, c.groups)

			// The kernel clears these on the UID 0 -> non-0 transition. A non-zero set means the
			// mounter never transitioned from root.
			gomega.Expect(c.capPrm).To(gomega.Equal("0000000000000000"),
				"Mountpoint pid %s must hold no permitted capabilities", c.pid)
			gomega.Expect(c.capEff).To(gomega.Equal("0000000000000000"),
				"Mountpoint pid %s must hold no effective capabilities", c.pid)
			gomega.Expect(c.capAmb).To(gomega.Equal("0000000000000000"),
				"Mountpoint pid %s must hold no ambient capabilities", c.pid)
		}
	})

	// The UID isolates nothing unless the filesystem agrees: shared paths root-owned and closed,
	// per-mount paths owned by the mount.
	ginkgo.It("should own the shared and per-mount paths correctly", func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		pvName := resource.Pv.Name

		targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
		defer deletePodsInOrder(ctx, f, pods)

		ginkgo.By("Waiting for the mount to be serving before inspecting its paths")
		gomega.Eventually(ctx, func(ctx context.Context) (int, error) {
			return countFuseMountsForVolume(ctx, f, targetNode, pvName), nil
		}).WithTimeout(mountpointProcessTimeout).WithPolling(mountpointProcessPoll).Should(gomega.Equal(1))

		// Observed from csi-node, which is privileged and applied this ownership. WITHOUT A CACHE the mounter
		// cannot read these paths itself, holding no CAP_DAC_OVERRIDE. (But with a cache, it can)
		commDir := commDirHostPath(ctx, f, targetNode)

		ginkgo.By("Checking the shared comm directory and mount socket are root-owned and closed")
		comm := statPath(ctx, f, targetNode, commDir)
		gomega.Expect(comm.mode).To(gomega.Equal(expectedCommDirMode))
		gomega.Expect(comm.uid).To(gomega.Equal(0))
		gomega.Expect(comm.gid).To(gomega.Equal(0))

		sock := statPath(ctx, f, targetNode, filepath.Join(commDir, mountSockName))
		gomega.Expect(sock.mode).To(gomega.Equal(expectedSockMode))
		gomega.Expect(sock.uid).To(gomega.Equal(0))
		gomega.Expect(sock.gid).To(gomega.Equal(0))

		ginkgo.By("Checking the per-mount credential directory belongs to the mount's UID")
		credDirPath := commDir + "/" + pvName
		credDir := statPath(ctx, f, targetNode, credDirPath)
		gomega.Expect(credDir.mode).To(gomega.Equal(expectedCredDirMode))
		gomega.Expect(credDir.uid).To(gomega.And(
			gomega.BeNumerically(">=", uidRangeStart),
			gomega.BeNumerically("<=", uidRangeEnd),
		), "credential directory for %s must belong to an allocated UID", pvName)
		gomega.Expect(credDir.gid).To(gomega.Equal(credDir.uid))

		ginkgo.By("Confirming a mount's UID can neither write to /comm nor open the mount socket")
		asMount := fmt.Sprintf("setpriv --reuid=%d --regid=%d --clear-groups", credDir.uid, credDir.uid)

		out, stderr, err := execInMounterPod(ctx, f, targetNode, asMount+" touch /comm/planted")
		gomega.Expect(err).To(gomega.HaveOccurred(),
			"UID %d must not be able to create entries in /comm; got output %q", credDir.uid, out)
		gomega.Expect(stderr).To(gomega.ContainSubstring("Permission denied"),
			"UID %d must be refused by the kernel, but the failure was: %q", credDir.uid, stderr)

		// The mount's UID must get "Permission denied" on the socket.
		sockPath := filepath.Join("/comm", mountSockName)
		out, stderr, err = execInMounterPod(ctx, f, targetNode, asMount+" cat "+sockPath)
		gomega.Expect(err).To(gomega.HaveOccurred(),
			"UID %d must not be able to open %s; got output %q", credDir.uid, sockPath, out)
		gomega.Expect(stderr).To(gomega.ContainSubstring("Permission denied"),
			"UID %d must be refused by the kernel, but the failure was: %q", credDir.uid, stderr)
	})

	// Credential files are covered here, not above: driver-level credentials from the node instance
	// profile write no files, so only pod-level credentials put a token on disk.
	ginkgo.It("should own pod-level credential files by the mount's UID", func(ctx context.Context) {
		oidcProvider := oidcProviderForCluster(ctx, f)
		if oidcProvider == "" {
			ginkgo.Skip("cluster has no OIDC provider, so pod-level credentials via IRSA are unavailable")
		}

		// An assumable role, so the mount succeeds and the token stays on disk.
		sa := createServiceAccountWithAssumableRole(ctx, f, oidcProvider, iamPolicyS3FullAccess)

		podLevelCtx := contextWithVolumeAttributes(ctx, map[string]string{"authenticationSource": "pod"})
		resource := createVolumeResourceWithMountOptions(podLevelCtx, l.config, pattern,
			[]string{fmt.Sprintf("region %s", DefaultRegion)})
		l.resources = append(l.resources, resource)
		pvName := resource.Pv.Name

		ginkgo.By("Creating a pod using the service account, so csi-node writes a token")
		pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name,
			[]*v1.PersistentVolumeClaim{resource.Pvc}, sa.Name)
		framework.ExpectNoError(err)
		defer deletePodsInOrder(ctx, f, []*v1.Pod{pod})
		targetNode := pod.Spec.NodeName

		credDirPath := commDirHostPath(ctx, f, targetNode) + "/" + pvName
		credDir := statPath(ctx, f, targetNode, credDirPath)

		// `-A` so a credential file whose name begins with a dot is listed too, without picking up "."
		// and "..". An empty listing would mean this spec asserts nothing, hence the guard below.
		listing, err := execInCSINodePod(ctx, f, targetNode, fmt.Sprintf("ls -A %q", credDirPath))
		framework.ExpectNoError(err, "while listing %s", credDirPath)
		files := strings.Fields(listing)
		gomega.Expect(files).ToNot(gomega.BeEmpty(),
			"pod-level credentials must write at least one token into %s", credDirPath)

		ginkgo.By(fmt.Sprintf("Checking the %d credential file(s) belong to UID %d and are read-only", len(files), credDir.uid))
		for _, file := range files {
			st := statPath(ctx, f, targetNode, filepath.Join(credDirPath, file))
			gomega.Expect(st.uid).To(gomega.Equal(credDir.uid),
				"credential file %s must belong to the mount's UID, or Mountpoint cannot read it", file)
			gomega.Expect(st.gid).To(gomega.Equal(credDir.uid))
			gomega.Expect(st.mode).To(gomega.Equal(expectedCredFileMode),
				"credential file %s must be read-only to its owner", file)
		}
	})

	// The modes asserted above only matter if the kernel enforces them. The mounter runs as root, so it is subject to
	// the same permission checks as any other user unless it holds CAP_DAC_OVERRIDE, which the chart grants with a cache.
	ginkgo.It("should deny the mounter access to a mount's credential directory unless it holds CAP_DAC_OVERRIDE", func(ctx context.Context) {
		mounter, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).Get(ctx, mounterDaemonSetName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		dacOverride := slices.Contains(mounter.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add, v1.Capability("DAC_OVERRIDE"))

		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)
		pvName := resource.Pv.Name

		targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
		defer deletePodsInOrder(ctx, f, pods)

		ginkgo.By("Waiting for the mount to be serving")
		gomega.Eventually(ctx, func(ctx context.Context) (int, error) {
			return countFuseMountsForVolume(ctx, f, targetNode, pvName), nil
		}).WithTimeout(mountpointProcessTimeout).WithPolling(mountpointProcessPoll).Should(gomega.Equal(1))

		// /comm is 0711 so root may traverse it; the directory itself is 0700 and owned by the mount.
		ginkgo.By("Confirming the mounter can reach the credential directory")
		mode, stderr, err := execInMounterPod(ctx, f, targetNode, fmt.Sprintf("stat -c '%%a' /comm/%s", pvName))
		framework.ExpectNoError(err, "the mounter should be able to stat the credential directory via 0711 /comm: %s", stderr)
		gomega.Expect(strings.TrimSpace(mode)).To(gomega.Equal(expectedCredDirMode))

		out, stderr, err := execInMounterPod(ctx, f, targetNode, fmt.Sprintf("ls -A /comm/%s", pvName))
		if dacOverride {
			ginkgo.By("Confirming the mounter, which holds CAP_DAC_OVERRIDE, can list the credential directory")
			framework.ExpectNoError(err, "the mounter holds CAP_DAC_OVERRIDE, so it should be able to list %s: %s", pvName, stderr)
		} else {
			ginkgo.By("Confirming the mounter, which holds no CAP_DAC_OVERRIDE, cannot list the credential directory")
			// The denial itself, not merely an error: a mistyped path or a missing `ls` would also error.
			gomega.Expect(err).To(gomega.HaveOccurred(),
				"the mounter must not be able to list %s; got output %q", pvName, out)
			gomega.Expect(stderr).To(gomega.ContainSubstring("Permission denied"),
				"the mounter must be refused by the kernel, but the failure was: %q", stderr)
		}
	})
}

// mountpointProcessCreds is what /proc/<pid>/status reports for one mount-s3 process.
type mountpointProcessCreds struct {
	pid string
	// uids and gids hold the real, effective, saved and filesystem IDs, all four of which must be
	// the allocated one: a process that switched only some of them could switch back.
	uids   []int
	gids   []int
	groups string
	capPrm string
	capEff string
	capAmb string
}

// uid returns the effective UID, which equals the others once the assertions below have run.
func (c mountpointProcessCreds) uid() int { return c.uids[1] }

// pathStat is the owner and mode of one path inside the mounter pod.
type pathStat struct {
	mode string
	uid  int
	gid  int
}

// mountpointProcessRunningAs returns the mount-s3 process on `nodeName` whose UID is `uid`, waiting
// for it to appear.
//
// csi-node chowned that mount's credentials to `uid`, so a Mountpoint must end up running as `uid`:
// if none does, the credentials were handed to an identity nothing is using and the mount cannot
// read them. The failure lists the UIDs actually seen so a mismatch names both sides.
func mountpointProcessRunningAs(ctx context.Context, f *framework.Framework, nodeName string, uid int) mountpointProcessCreds {
	var match mountpointProcessCreds
	gomega.Eventually(ctx, func(ctx context.Context) error {
		creds, err := mountpointProcessCredentials(ctx, f, nodeName)
		if err != nil {
			return err
		}
		seen := make([]int, 0, len(creds))
		for _, c := range creds {
			if c.uid() == uid {
				match = c
				return nil
			}
			seen = append(seen, c.uid())
		}
		return fmt.Errorf("no mount-s3 process on node %s runs as UID %d, which owns its credentials; running UIDs are %v",
			nodeName, uid, seen)
	}).WithTimeout(mountpointProcessTimeout).WithPolling(mountpointProcessPoll).Should(gomega.Succeed())

	return match
}

// mountpointProcessCredentials reads the credentials and capabilities of every mount-s3 process in
// the mounter pod on `nodeName`. The mounter's PID namespace contains its children, so no host PID
// access is needed.
func mountpointProcessCredentials(ctx context.Context, f *framework.Framework, nodeName string) ([]mountpointProcessCreds, error) {
	// `-H` prefixes every line with its /proc path, so each line carries the pid it belongs to and
	// can be parsed on its own. Processes come and go while the glob expands, so a non-zero exit is
	// not treated as fatal; an empty result is, via the caller's retry.
	const cmd = `grep -H -E '^(Name|Uid|Gid|Groups|CapPrm|CapEff|CapAmb):' /proc/[0-9]*/status 2>/dev/null`

	stdout, stderr, err := execInMounterPod(ctx, f, nodeName, cmd)
	if err != nil && stdout == "" {
		return nil, fmt.Errorf("reading process credentials on node %s: %w (stderr: %s)", nodeName, err, stderr)
	}

	// Keyed by pid, since the lines for one process are contiguous but nothing depends on that.
	byPid := map[string]*mountpointProcessCreds{}
	names := map[string]string{}
	var order []string

	for line := range strings.Lines(stdout) {
		path, rest, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		// "/proc/1234/status" -> "1234"
		pid := strings.TrimSuffix(strings.TrimPrefix(path, "/proc/"), "/status")

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		cur, seen := byPid[pid]
		if !seen {
			cur = &mountpointProcessCreds{pid: pid}
			byPid[pid] = cur
			order = append(order, pid)
		}

		switch fields[0] {
		case "Name:":
			if len(fields) > 1 {
				names[pid] = fields[1]
			}
		case "Uid:":
			// Real, effective, saved and filesystem UID, in that order.
			cur.uids = []int{mustAtoi(fields[1]), mustAtoi(fields[2]), mustAtoi(fields[3]), mustAtoi(fields[4])}
		case "Gid:":
			cur.gids = []int{mustAtoi(fields[1]), mustAtoi(fields[2]), mustAtoi(fields[3]), mustAtoi(fields[4])}
		case "Groups:":
			cur.groups = strings.Join(fields[1:], " ")
		case "CapPrm:":
			cur.capPrm = fields[1]
		case "CapEff:":
			cur.capEff = fields[1]
		case "CapAmb:":
			cur.capAmb = fields[1]
		}
	}

	// `Name` is the process' comm, truncated to 15 characters by the kernel; "mount-s3" is well
	// within that.
	var out []mountpointProcessCreds
	for _, pid := range order {
		if names[pid] == mountpointProcessName {
			out = append(out, *byPid[pid])
		}
	}
	return out, nil
}

// statPath returns the mode and owner of `path`, read from the csi-node pod.
func statPath(ctx context.Context, f *framework.Framework, nodeName, path string) pathStat {
	stdout, err := execInCSINodePod(ctx, f, nodeName, fmt.Sprintf("stat -c '%%a %%u %%g' %q", path))
	framework.ExpectNoError(err, "while stat'ing %s from the csi-node pod on node %s", path, nodeName)

	fields := strings.Fields(strings.TrimSpace(stdout))
	gomega.Expect(fields).To(gomega.HaveLen(3), "unexpected stat output for %s: %q", path, stdout)
	return pathStat{mode: fields[0], uid: mustAtoi(fields[1]), gid: mustAtoi(fields[2])}
}

// commDirHostPath returns the mounter pod's comm emptyDir as csi-node sees it through the kubelet pod
// directory, which is the same directory the mounter sees at /comm.
func commDirHostPath(ctx context.Context, f *framework.Framework, nodeName string) string {
	pod, err := podOnNode(ctx, f, mounterPodLabel, nodeName)
	framework.ExpectNoError(err)
	return fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~empty-dir/comm", pod.UID)
}
