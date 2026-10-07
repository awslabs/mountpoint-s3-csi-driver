package custom_testsuites

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/awslabs/mountpoint-s3-csi-driver/tests/e2e-kubernetes/s3client"
)

// Serial and Ordered: each cache type reinstalls the mounter via Helm, which restarts every Mountpoint on every node.
// ContinueOnFailure, so a failed spec does not skip the cache types after it (a failed BeforeAll still does).

const (
	// Where the chart mounts the mounter's cache volume; spelled out as the contract between the chart's volumeMounts and `--cache`.
	cacheMountPath = "/cache"
	// The directory Mountpoint creates inside the `--cache` directory it is given.
	mountpointCacheDirName = "mountpoint-cache"

	// One 1 KiB file is enough to prove the cache served a read.
	cachedFileName = "cached.txt"
	ioFileSize     = 1024
)

// cacheEnabledAttributes is the v3 opt-in: whichever cache the node's mounter has.
var cacheEnabledAttributes = map[string]string{"cache": "enabled"}

// clusterPrerequisite is something a cache configuration needs from the cluster, and doubles as the skip message.
type clusterPrerequisite string

const (
	requiresEBSCSIDriver clusterPrerequisite = "the EBS CSI driver is not installed," +
		" so an ephemeral cache volume cannot be provisioned"
	requiresLocalNVMe clusterPrerequisite = "no node is labelled " + localNVMeNodeLabel + "=true," +
		" so there is no instance-store disk for StorageClass " + nvmeCacheStorageClassName
)

// cacheVolume is one node cache configuration and everything that differs by cache type, so the specs read the same for all.
type cacheVolume struct {
	// name completes "with <name>" in the container description.
	name string
	// helm is the daemonsetMounters[0].cache block that creates this volume.
	helm map[string]any
	// runsExpressSpec adds the local plus Express spec; no code of ours differs by cache type there, so one row runs it.
	runsExpressSpec bool

	// nodeSelector keeps the driver and the workloads to the nodes this cache can exist on; nil is every node.
	nodeSelector map[string]string
	requires     clusterPrerequisite
}

// Fixed, not from f.UniqueName: in an Ordered container that names the first spec's namespace, deleted long before this class.
const daemonsetCacheStorageClassName = "s3-csi-e2e-daemonset-cache-sc"

// The StorageClass a local-volume provisioner must serve from the instance-store disk of each labelled node.
const nvmeCacheStorageClassName = "nvme-ssd"

// The label a node with an instance-store disk must carry for the NVMe row to run.
const localNVMeNodeLabel = "s3.csi.aws.com/local-nvme"

var cacheVolumes = []cacheVolume{
	{
		name: "an emptyDir cache on the node's disk",
		helm: map[string]any{"emptyDir": map[string]any{"medium": "", "sizeLimit": "128Mi"}},
		// Only context to run express test to save time; we can rename this later so more tests only run in disk cache if tests are taking too long.
		runsExpressSpec: true,
	},
	{
		name: "an emptyDir cache on tmpfs",
		helm: map[string]any{"emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "128Mi"}},
	},
	{
		// 1Gi is the smallest gp3 volume.
		name: "an ephemeral cache on an EBS volume",
		helm: map[string]any{
			"ephemeral": map[string]any{"storageClassName": daemonsetCacheStorageClassName, "resourceRequests": "1024Mi"},
		},
		requires: requiresEBSCSIDriver,
	},
	{
		// ! IMPORTANT: This case does NOT currently run in CI, which has no node with an instance-store disk. To run it, deploy a node
		// labelled `s3.csi.aws.com/local-nvme=true` with a local-volume provisioner serving StorageClass `nvme-ssd` (e.g. on a dev cluster).
		// Same `ephemeral` type as above but from an instance-store disk, which the driver does not distinguish: this asserts the
		// documented instance-store path caches like any ephemeral volume.
		name: "an ephemeral cache on an NVMe instance store",
		helm: map[string]any{
			"ephemeral": map[string]any{"storageClassName": nvmeCacheStorageClassName, "resourceRequests": "10240Mi"},
		},
		// A mounter on a node without a free disk stays Pending, so only labelled nodes run the driver and the workloads.
		nodeSelector: map[string]string{localNVMeNodeLabel: "true"},
		requires:     requiresLocalNVMe,
	},
}

type s3CSIDaemonsetCacheTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIDaemonsetCacheTestSuite() storageframework.TestSuite {
	return &s3CSIDaemonsetCacheTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name:         "daemonset-cache",
			TestPatterns: []storageframework.TestPattern{storageframework.DefaultFsPreprovisionedPV},
		},
	}
}

func (t *s3CSIDaemonsetCacheTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIDaemonsetCacheTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSIDaemonsetCacheTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"daemonset-cache", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	Describe("Local cache on the mounter DaemonSet", Ordered, ContinueOnFailure, Serial, func() {
		var (
			config  *storageframework.PerTestConfig
			release *driverRelease
		)

		BeforeAll(func(ctx context.Context) {
			if !isDaemonsetMounterMode(ctx, f) {
				Skip("the cache volume is configured on the mounter DaemonSet, which only exists in daemonset mode")
			}
			// Registered before the restore, so cleanup (LIFO) removes it last, after the restore has removed the cache PVCs using it.
			createEBSCacheSC(ctx, teardownSafeFramework(f), daemonsetCacheStorageClassName)
			release = setUpDriverRelease(ctx, f)
		})

		BeforeEach(func(ctx context.Context) {
			config = driver.PrepareTest(ctx, f)
		})

		for _, tc := range cacheVolumes {
			// BeforeAll, not BeforeEach: one reconfiguration replaces every mounter pod, so per spec would multiply the rollouts by the specs in a row.
			Context("with "+tc.name, func() {
				var provided bool
				BeforeAll(func(ctx context.Context) {
					if provided = clusterProvides(ctx, f, tc.requires); provided {
						release.upgrade(ctx, f, withMounterCache(release, tc.helm, tc.nodeSelector))
					}
				})

				// Not a Skip in BeforeAll: when that skips the last selected specs, Ginkgo runs the restore during a later spec or never.
				BeforeEach(func() {
					if !provided {
						Skip(string(tc.requires))
					}
				})

				It("serves a repeated read from the cache", func(ctx context.Context) {
					// Note: the workload runs as root; a non-root one sees files owned by --uid/--gid, which the cache does not change.
					// Without an indefinite TTL Mountpoint re-checks S3 after 60s and the post-delete read could fail.
					m := mountCachedVolume(ctx, f, config, pattern, tc.nodeSelector, []string{"metadata-ttl indefinite"})

					By("Writing a file and reading it back until Mountpoint has cached its block")
					path := filepath.Join(e2epod.VolumeMountPath1, cachedFileName)
					seed := time.Now().UTC().UnixNano()
					checkWriteToPathSucceed(ctx, f, m.pod, path, ioFileSize, seed)
					waitAndAssertMountpointCachedBlocks(ctx, f, m, path)

					By("Deleting the object from S3, so the next read can only be served by the cache")
					deleteObjectFromS3(ctx, bucketNameFromVolumeResource(m.vol), cachedFileName)
					checkReadFromPathSucceed(ctx, f, m.pod, path, ioFileSize, seed)
				})

				if tc.runsExpressSpec {
					It("serves a read from Express once the local cache is emptied, and from the local cache once Express is", func(ctx context.Context) {
						client := s3client.New()
						expressBucket, deleteExpressBucket := client.CreateDirectoryBucket(ctx)
						DeferCleanup(deleteExpressBucket)
						m := mountCachedVolume(ctx, f, config, pattern, tc.nodeSelector,
							[]string{"metadata-ttl indefinite", "cache-xz " + expressBucket})

						By("Writing a file and reading it back until both caches hold its block")
						path := filepath.Join(e2epod.VolumeMountPath1, cachedFileName)
						seed := time.Now().UTC().UnixNano()
						checkWriteToPathSucceed(ctx, f, m.pod, path, ioFileSize, seed)
						waitAndAssertMountpointCachedBlocks(ctx, f, m, path)
						waitAndAssertExpressCachedBlocks(ctx, expressBucket)
						deleteObjectFromS3(ctx, bucketNameFromVolumeResource(m.vol), cachedFileName)

						By("Emptying the local cache, so only Express can serve the read")
						rm := asMountUID(m, fmt.Sprintf("sh -c 'rm -rf %s/*'", filepath.Join(m.cacheDir, mountpointCacheDirName)))
						_, stderr, err := execInMounterPod(ctx, f, m.node, rm)
						framework.ExpectNoError(err, "%s: %s", rm, stderr)
						checkReadFromPathSucceed(ctx, f, m.pod, path, ioFileSize, seed)
						// Mountpoint copies a block it got from Express into the local cache.
						waitAndAssertMountpointCachedBlocks(ctx, f, m, path)

						By("Emptying Express, so only the local cache can serve the read")
						framework.ExpectNoError(client.WipeoutBucket(ctx, expressBucket))
						checkReadFromPathSucceed(ctx, f, m.pod, path, ioFileSize, seed)
					})
				}

				It("removes the mount's cache directory when its last consumer unmounts", func(ctx context.Context) {
					m := mountCachedVolume(ctx, f, config, pattern, tc.nodeSelector, nil)
					Expect(cacheDirExists(ctx, f, m)).To(BeTrue(), "the mounter created no %s on node %s", m.cacheDir, m.node)

					By("Deleting the workload, which stops its Mountpoint")
					framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, m.pod))
					waitAndAssertCacheDirReclaimed(ctx, f, m)
				})
			})
		}

		// Last: it is the negative case and the cheapest reconfiguration, so it sits closest to the restore.
		Context("with no cache volume, as a default install has", func() {
			BeforeAll(func(ctx context.Context) {
				release.upgrade(ctx, f, withMounterCache(release, nil, nil))
			})

			It("rejects a PV that asks for a cache, naming the Helm value that turns one on", func(ctx context.Context) {
				vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)

				// Not pinned to a node: every mounter has the same spec.
				By("Creating a workload whose PV asks for a cache anyway")
				pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
				pod, err := createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name, pod)
				framework.ExpectNoError(err)
				DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

				By("Waiting for a FailedMount event that names the Helm value to add")
				assertPodFailsToMount(ctx, f, pod, "daemonsetMounters[0].cache")
			})
		})
	})
}

// clusterProvides reports whether the cluster can host a cache configuration with this prerequisite.
func clusterProvides(ctx context.Context, f *framework.Framework, req clusterPrerequisite) bool {
	GinkgoHelper()
	switch req {
	case requiresEBSCSIDriver:
		// ebsCSIDriverDaemonSet looks for `ebs-csi-node` in kube-system, so this skips on ROSA, where OpenShift names and places it elsewhere.
		return ebsCSIDriverDaemonSet(ctx, f) != nil
	case requiresLocalNVMe:
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: localNVMeNodeLabel + "=true"})
		framework.ExpectNoError(err, "listing nodes labelled %s", localNVMeNodeLabel)
		return len(nodes.Items) > 0
	}
	return true
}

// withMounterCache returns the installed values with one complete mounter element carrying cache (nil: none),
// and both driver DaemonSets on nodeSelector's nodes only (nil: every node).
func withMounterCache(r *driverRelease, cache map[string]any, nodeSelector map[string]string) map[string]any {
	// Helm replaces a list whole, so the element starts from the chart's: a partial one renders an absent logLevel as `--v=`.
	chartMounter := r.chart.Values["daemonsetMounters"].([]any)[0].(map[string]any)
	mounter := maps.Clone(chartMounter)
	if installed, _ := r.installed["daemonsetMounters"].([]any); len(installed) > 0 {
		maps.Copy(mounter, installed[0].(map[string]any))
	}
	// The chart reads the key's presence, not its value, as asking for a cache.
	delete(mounter, "cache")
	if cache != nil {
		mounter["cache"] = cache
	}

	// Placement from the chart, not the install: an install pinned to the NVMe nodes would leave unpinned workloads on nodes
	// without s3-csi-node. Both DaemonSets read node.nodeSelector.
	mounter["affinity"] = chartMounter["affinity"]
	installedNode, _ := r.installed["node"].(map[string]any)
	node := maps.Clone(installedNode)
	if node == nil {
		node = map[string]any{}
	}
	node["affinity"] = r.chart.Values["node"].(map[string]any)["affinity"]
	selector := map[string]any{}
	for k, v := range nodeSelector {
		selector[k] = v
	}
	node["nodeSelector"] = selector

	values := maps.Clone(r.installed)
	values["daemonsetMounters"] = []any{mounter}
	values["node"] = node
	return values
}

// cachedMount is a workload using a PV that asks for the cache, and where its Mountpoint caches.
type cachedMount struct {
	vol  *storageframework.VolumeResource
	pod  *v1.Pod
	node string
	// uid is the UID the mount's Mountpoint runs as, and cacheDir the directory in the mounter pod named for it.
	uid      int
	cacheDir string
}

// mountCachedVolume creates a PV that asks for the cache and one workload using it, and registers their cleanup.
func mountCachedVolume(ctx context.Context, f *framework.Framework, config *storageframework.PerTestConfig,
	pattern storageframework.TestPattern, nodeSelector map[string]string, mountOptions []string) cachedMount {
	GinkgoHelper()
	vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, mountOptions)
	pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name,
		e2epod.MakePod(f.Namespace.Name, nodeSelector, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, ""))
	framework.ExpectNoError(err)
	DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })
	// s3-csi-node chowns the mount's credential directory to the UID it allocated, so the UID does not come from the mounter.
	uid := statPath(ctx, f, pod.Spec.NodeName, filepath.Join(commDirHostPath(ctx, f, pod.Spec.NodeName), vol.Pv.Name)).uid
	return cachedMount{vol: vol, pod: pod, node: pod.Spec.NodeName, uid: uid,
		cacheDir: filepath.Join(cacheMountPath, fmt.Sprintf("uid-%d", uid))}
}

// createVolumeResourceWithAttributes creates a PV with these volumeAttributes (nil: no cache opt-in) and registers its cleanup.
func createVolumeResourceWithAttributes(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern,
	attrs map[string]string, mountOptions []string) *storageframework.VolumeResource {
	GinkgoHelper()
	vol := createVolumeResourceWithMountOptions(contextWithVolumeAttributes(ctx, attrs), config, pattern, mountOptions)
	DeferCleanup(vol.CleanupResource)
	return vol
}

// waitAndAssertMountpointCachedBlocks reads path through the mount until Mountpoint has cached a block of it.
func waitAndAssertMountpointCachedBlocks(ctx context.Context, f *framework.Framework, m cachedMount, path string) {
	GinkgoHelper()
	// Mountpoint writes a block after the read returns, under a dot-name it then renames, and `ls` without `-a` lists no dot-names.
	listing := asMountUID(m, "ls -lR "+filepath.Join(m.cacheDir, mountpointCacheDirName))
	Eventually(ctx, func(ctx context.Context) (bool, error) {
		if _, stderr, err := e2epod.ExecShellInPodWithFullOutput(ctx, f, m.pod.Name, "cat "+path+" > /dev/null"); err != nil {
			return false, fmt.Errorf("reading %s: %w (stderr: %s)", path, err, stderr)
		}
		stdout, stderr, err := execInMounterPod(ctx, f, m.node, listing)
		if err != nil {
			return false, fmt.Errorf("%s: %w (stderr: %s)", listing, err, stderr)
		}
		// `ls -l` starts a regular file's line with "-".
		return slices.ContainsFunc(strings.Split(stdout, "\n"), func(line string) bool { return strings.HasPrefix(line, "-") }), nil
	}).WithTimeout(time.Minute).WithPolling(2*time.Second).Should(BeTrue(), "Mountpoint cached no block under %s", m.cacheDir)
}

// asMountUID wraps cmd to run as the mount's UID, the only one that can look inside its cache directory.
func asMountUID(m cachedMount, cmd string) string {
	return fmt.Sprintf("setpriv --reuid=%d --regid=%d --clear-groups %s", m.uid, m.uid, cmd)
}

// waitAndAssertExpressCachedBlocks waits for Mountpoint to put a block in the Express bucket, which it does after the read returns.
func waitAndAssertExpressCachedBlocks(ctx context.Context, bucket string) {
	GinkgoHelper()
	client := s3.NewFromConfig(awsConfig(ctx))
	Eventually(ctx, func(ctx context.Context) (int32, error) {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err != nil {
			return 0, err
		}
		return aws.ToInt32(out.KeyCount), nil
	}).WithTimeout(time.Minute).WithPolling(2*time.Second).Should(BeNumerically(">", 0), "Mountpoint cached no block in %s", bucket)
}

// cacheDirExists reports whether the mount's cache directory exists; its command always exits 0, so a failed exec never
// reads as absent.
func cacheDirExists(ctx context.Context, f *framework.Framework, m cachedMount) (bool, error) {
	cmd := fmt.Sprintf("if [ -e %s ]; then echo present; else echo absent; fi", m.cacheDir)
	stdout, stderr, err := execInMounterPod(ctx, f, m.node, cmd)
	if err != nil {
		return false, fmt.Errorf("%s on node %s: %w (stderr: %s)", cmd, m.node, err, stderr)
	}
	return strings.TrimSpace(stdout) == "present", nil
}

// waitAndAssertCacheDirReclaimed waits for the mounter to remove the mount's cache directory, which it does once the mount's
// Mountpoint exits, so possibly after NodeUnpublishVolume has returned.
func waitAndAssertCacheDirReclaimed(ctx context.Context, f *framework.Framework, m cachedMount) {
	GinkgoHelper()
	Eventually(ctx, func(ctx context.Context) (bool, error) { return cacheDirExists(ctx, f, m) }).
		WithTimeout(time.Minute).WithPolling(2*time.Second).Should(BeFalse(), "%s outlived its Mountpoint", m.cacheDir)
}

// Could add later:
//   - an uncached PV on a no-cache install.
//   - cache growth against the volume's size: the disk emptyDir's sizeLimit does not bound the mounter's cache (the kubelet
//     does not evict the system-node-critical mounter), while tmpfs and ephemeral volumes stop at their size.
//   - a mounter pod crash with leftovers, to test the startup cleanup.
