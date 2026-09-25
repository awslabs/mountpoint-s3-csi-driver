package custom_testsuites

import (
	"context"
	"fmt"
	"strconv"
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
	mounterPodLabel      = "app=s3-csi-daemonset-mounter"
	mounterContainerName = "mounter"
	csiNodePodLabel      = "app=s3-csi-node"
	csiNodeContainerName = "s3-plugin"

	// mountpointProcessName is the `comm` of a Mountpoint process, as /proc/<pid>/status reports it.
	mountpointProcessName = "mount-s3"
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
//
// No other suite can observe these properties. Giving every mount the same UID, or skipping the
// chown, leaves all functional tests passing and the isolation gone.
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

		// One node, because that is where two Mountpoint processes share a mounter pod.
		targetNode, pods := createPodsOnSameNode(ctx, f, 1, first)
		pods = append(pods, createPodOnNode(ctx, f, targetNode, second)...)
		defer deletePodsInOrder(ctx, f, pods)

		ginkgo.By("Reading the credentials of every mount-s3 process in the mounter pod")
		var creds []mountpointProcessCreds
		gomega.Eventually(ctx, func(ctx context.Context) (int, error) {
			var err error
			creds, err = mountpointProcessCredentials(ctx, f, targetNode)
			if err != nil {
				return 0, err
			}
			return len(creds), nil
		}).WithTimeout(mountpointProcessTimeout).WithPolling(mountpointProcessPoll).Should(gomega.BeNumerically(">=", 2),
			"expected at least two mount-s3 processes on node %s", targetNode)

		seen := map[int]string{}
		for _, c := range creds {
			ginkgo.By(fmt.Sprintf("Checking Mountpoint pid %s (uid %d)", c.pid, c.uid()))

			// All four IDs, not just the effective one: a process that switched only that could
			// switch back.
			for _, uid := range c.uids {
				gomega.Expect(uid).To(gomega.And(
					gomega.BeNumerically(">=", uidRangeStart),
					gomega.BeNumerically("<=", uidRangeEnd),
				), "Mountpoint pid %s must run under an allocated UID, got %v", c.pid, c.uids)
			}
			for _, gid := range c.gids {
				gomega.Expect(gid).To(gomega.Equal(c.uid()),
					"Mountpoint pid %s must have every GID set to its UID, got %v", c.pid, c.gids)
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

			if other, dup := seen[c.uid()]; dup {
				framework.Failf("Mountpoint pids %s and %s share UID %d, so the kernel cannot isolate them",
					other, c.pid, c.uid())
			}
			seen[c.uid()] = c.pid
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

		// Observed from csi-node, which is privileged and applied this ownership. The mounter holds no
		// CAP_DAC_OVERRIDE, so it cannot read these paths — that is the isolation working.
		commDir := commDirHostPath(ctx, f, targetNode)

		ginkgo.By("Checking the shared comm directory and mount socket are root-owned and closed")
		// No Mountpoint may create entries in /comm for a later mount to inherit, nor reach the socket
		// to issue mount requests of its own.
		comm := statPath(ctx, f, targetNode, commDir)
		gomega.Expect(comm.mode).To(gomega.Equal(expectedCommDirMode))
		gomega.Expect(comm.uid).To(gomega.Equal(0))
		gomega.Expect(comm.gid).To(gomega.Equal(0))

		sock := statPath(ctx, f, targetNode, commDir+"/mount.sock")
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
	})

	// Credential files are covered here, not above: driver-level credentials from the node instance
	// profile write no files, so only pod-level credentials put a token on disk.
	ginkgo.It("should own pod-level credential files by the mount's UID", func(ctx context.Context) {
		oidcProvider := oidcProviderForCluster(ctx, f)
		if oidcProvider == "" {
			ginkgo.Skip("cluster has no OIDC provider, so pod-level credentials via IRSA are unavailable")
		}

		// An assumable role, so the mount succeeds and the token stays on disk.
		sa, removeSA := createServiceAccount(ctx, f)
		ginkgo.DeferCleanup(removeSA)
		role, removeRole := createRole(ctx, f, assumeRoleWithWebIdentityPolicyDocument(ctx, oidcProvider, sa), iamPolicyS3FullAccess)
		ginkgo.DeferCleanup(removeRole)
		sa, _ = overrideServiceAccountRole(ctx, f, sa, *role.Arn)
		waitUntilRoleIsAssumableWithWebIdentity(ctx, f, sa)

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

		// `-A` so an empty directory yields no output rather than "." and "..". An empty directory
		// would mean this spec asserts nothing, hence the guard below.
		listing, err := execInCSINodePod(ctx, f, targetNode, fmt.Sprintf("ls -A %q", credDirPath))
		framework.ExpectNoError(err, "while listing %s", credDirPath)
		files := strings.Fields(listing)
		gomega.Expect(files).ToNot(gomega.BeEmpty(),
			"pod-level credentials must write at least one token into %s", credDirPath)

		ginkgo.By(fmt.Sprintf("Checking the %d credential file(s) belong to UID %d and are read-only", len(files), credDir.uid))
		for _, file := range files {
			st := statPath(ctx, f, targetNode, credDirPath+"/"+file)
			gomega.Expect(st.uid).To(gomega.Equal(credDir.uid),
				"credential file %s must belong to the mount's UID, or Mountpoint cannot read it", file)
			gomega.Expect(st.gid).To(gomega.Equal(credDir.uid))
			gomega.Expect(st.mode).To(gomega.Equal(expectedCredFileMode),
				"credential file %s must be read-only to its owner", file)
		}
	})

	// The modes above mean nothing unless the kernel enforces them. The mounter is root without
	// CAP_DAC_OVERRIDE, so it faces the same checks as any user: refusing it implies refusing any UID.
	ginkgo.It("should deny the mounter itself access to a mount's credential directory", func(ctx context.Context) {
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
		ginkgo.By("Confirming the mounter can reach but not read the credential directory")
		mode, stderr, err := execInMounterPod(ctx, f, targetNode, fmt.Sprintf("stat -c '%%a' /comm/%s", pvName))
		framework.ExpectNoError(err, "the mounter should be able to stat the credential directory via 0711 /comm: %s", stderr)
		gomega.Expect(strings.TrimSpace(mode)).To(gomega.Equal(expectedCredDirMode))

		// The denial itself, not merely an error: a mistyped path or a missing `ls` would also error.
		out, stderr, err := execInMounterPod(ctx, f, targetNode, fmt.Sprintf("ls -A /comm/%s", pvName))
		gomega.Expect(err).To(gomega.HaveOccurred(),
			"the mounter must not be able to list %s; got output %q", pvName, out)
		gomega.Expect(stderr).To(gomega.ContainSubstring("Permission denied"),
			"the mounter must be refused by the kernel, but the failure was: %q", stderr)
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

// podOnNode returns the driver pod matching `label` on `nodeName`.
func podOnNode(ctx context.Context, f *framework.Framework, label, nodeName string) (*v1.Pod, error) {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: label,
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s pods on node %s: %w", label, nodeName, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no %s pod on node %s", label, nodeName)
	}
	return &pods.Items[0], nil
}

// execInPodOnNode runs `cmd` in `container` of the pod matching `label` on `nodeName`, returning its
// stdout and stderr.
//
// stderr matters: a spec asserting that access is *refused* needs to distinguish the kernel denying it
// from the command having failed for any other reason.
func execInPodOnNode(ctx context.Context, f *framework.Framework, label, container, nodeName, cmd string) (string, string, error) {
	pod, err := podOnNode(ctx, f, label, nodeName)
	if err != nil {
		return "", "", err
	}
	return execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, pod.Name, container,
		[]string{"/bin/sh", "-c", cmd})
}

// execInCSINodePod runs `cmd` in the csi-node pod on `nodeName`.
//
// csi-node is the right observer for the per-mount paths: it is privileged, so unlike the mounter it
// can read a directory owned by a mount's UID, and it is the component that applied that ownership.
func execInCSINodePod(ctx context.Context, f *framework.Framework, nodeName, cmd string) (string, error) {
	stdout, _, err := execInPodOnNode(ctx, f, csiNodePodLabel, csiNodeContainerName, nodeName, cmd)
	return stdout, err
}

// execInMounterPod runs `cmd` in the mounter pod on `nodeName`, returning its stdout and stderr.
func execInMounterPod(ctx context.Context, f *framework.Framework, nodeName, cmd string) (string, string, error) {
	return execInPodOnNode(ctx, f, mounterPodLabel, mounterContainerName, nodeName, cmd)
}

// commDirHostPath returns the mounter pod's comm emptyDir as csi-node sees it through the kubelet pod
// directory, which is the same directory the mounter sees at /comm.
func commDirHostPath(ctx context.Context, f *framework.Framework, nodeName string) string {
	pod, err := podOnNode(ctx, f, mounterPodLabel, nodeName)
	framework.ExpectNoError(err)
	return fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~empty-dir/comm", pod.UID)
}

// mustAtoi parses a decimal field, failing the test if it is not a number. Returning a zero on a
// parse failure would pass silently wherever zero is the expected value, as it is for every
// root-owned shared path.
func mustAtoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	framework.ExpectNoError(err, "while parsing %q as a number", s)
	return n
}
