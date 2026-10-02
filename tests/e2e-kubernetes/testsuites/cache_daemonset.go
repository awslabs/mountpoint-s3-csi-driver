package custom_testsuites

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// This suite reconfigures the mounter DaemonSet's cache via Helm, and restarting mounter pods kills every Mountpoint on every
// node: so Serial (`ginkgo -p` runs Serial specs last, alone) and Ordered (the reconfigurations run in a fixed sequence).
// A failed spec skips the rest of its container, but cleanup still restores the cluster, or prints the recovery command.
// The cache blocks set equalSplit; specs that depend on it run on the tmpfs row only, and `none` has its own Context.

const (
	// The DaemonSet the cache volume is attached to, and the container that sees it.
	mounterDaemonSetName  = "s3-csi-daemonset-mounter"
	mounterDaemonSetLabel = "app=s3-csi-daemonset-mounter"
	mounterContainerName  = "mounter"
	csiNodeContainerName  = "s3-plugin"

	// Where the chart mounts the mounter's cache volume; spelled out as the contract between the chart's volumeMounts and `--cache`.
	cacheMountPath = "/cache"
	// The kubelet bind-mounts this from <kubelet>/pods/<uid>/etc-hosts, where a disk emptyDir lives, so it is always the node's disk.
	kubeletHostsFile = "/etc/hosts"

	// Mode of a mount's cache directory. Mountpoint only mkdirs `mountpoint-cache` inside it, so the write bit is what makes caching work.
	cacheDirMode = "drwxrwx---"

	// The directory Mountpoint creates inside the `--cache` directory it is given.
	mountpointCacheDirName = "mountpoint-cache"
)

const (
	// One 1 KiB file is enough to prove the cache served a read.
	cachedFileName = "cached.txt"
	ioFileSize     = 1024
)

// cacheEnabledAttributes is the v3 opt-in: whichever cache the node's mounter has.
var cacheEnabledAttributes = map[string]string{"cache": "enabled"}

const (
	// Long enough for a late Mountpoint cache write to recreate the tree if the unmount ordering is wrong; paid on one row only.
	cacheDirStaysGoneWindow = 15 * time.Second
	// s3-csi-node checks the mounter's socket every 5s and, once it is gone, retries every 1s for 15s; a minute is several attempts.
	mounterRediscoveryTimeout = time.Minute
	// The provisioner rescans its disks every 10s; these cover its image pull, or wiping a released disk, on top of that.
	localPVDiscoveryTimeout = 2 * time.Minute
	localPVReleaseTimeout   = 2 * time.Minute
)

// How much of the node's cache volume one mount may fill under each cacheLimitStrategy, and what happens at the bound. Only a
// cluster proves it: s3-csi-daemonset-mounter computes --max-cache-size after s3-csi-node sent its options, from a capacity the
// chart templated into an env var, and unit and chart tests each see one of the three. Mountpoint's argv is where they meet.
//
// Deliberately absent: the disk-emptyDir eviction cascade. The mounter is `system-node-critical`, so a full node disk makes the
// kubelet evict unrelated pods; the `none` spec pins the cause (sizeLimit is invisible) and the mitigation without the outage.
// Also absent: OOMKill by a tmpfs cache. The chart sets no `resources.limits`, so there is no cgroup ceiling to reach.
const (
	// Fill files are made by `dd` in the pod: util.go's writers base64 the payload into argv, too large for this.
	fillFileSizeMiB = 64

	// A PV's `max-cache-size`, well under the volume it shares, so it rather than free space is the binding constraint.
	pvMaxCacheSizeMiB = 16

	// cacheOvershootKiB is how far above its --max-cache-size a cache may measure. Mountpoint counts the
	// bytes of the blocks it wrote and evicts only after a write has crossed the limit, so it overshoots
	// by part of one 1 MiB block, and `du` then rounds each cached object up to a filesystem block.
	// TODO check overshoot of Mountpoint to verify behaviour.
	cacheOvershootKiB = 4 * 1024
	// duKiBPerCachedMiB is what `du` adds per cached 1 MiB block on top of that: a block file is 1 MiB plus a header, so it takes
	// one more filesystem block. 4.2 KiB per MiB of share was measured on ext4 at 243 and 2432 MiB, past what a fixed margin covers.
	duKiBPerCachedMiB = 4

	// A PV asking for more than the whole cache volume, so a value that survives is unmistakable.
	oversizedPVMaxCacheSizeMiB = 4096
)

// clusterPrerequisite is something a cache configuration needs from the cluster, and doubles as the skip message.
type clusterPrerequisite string

const (
	requiresEBSCSIDriver clusterPrerequisite = "the EBS CSI driver is not installed," +
		" so an ephemeral cache volume cannot be provisioned"
	requiresLocalNVMe clusterPrerequisite = "no node is labelled " + localNVMeNodeLabel + "=true, so the suite" +
		" installed no Local Volume Static Provisioner to offer an instance-store disk in StorageClass " + nvmeCacheStorageClassName
)

// cacheVolume is one node cache configuration and everything that differs by cache type, so the specs read the same for all.
type cacheVolume struct {
	// name completes "with <name>" in the container description.
	name string
	// helm is the daemonsetMounters[0].cache block that creates this volume.
	helm map[string]any

	// cacheFilesystem is what /cache must be relative to the node's disk; docs/CACHING.md promises only emptyDir-on-disk shares it.
	cacheFilesystem string
	// cacheSizeMiB is the size the helm block asks for, and maxVolumesPerNode what the row installs; equalSplit divides one by the other.
	cacheSizeMiB      int
	maxVolumesPerNode int

	// runsRestartSpecs marks one row per restart kind: the only difference is whether a new mounter pod needs a volume provisioned.
	runsRestartSpecs bool
	// assertsCacheStaysGone marks the one row that pays cacheDirStaysGoneWindow; what it guards is not cache-type-specific.
	assertsCacheStaysGone bool
	// runsKilledMountpointSpec marks the one row that kills a Mountpoint; removing its directory reads nothing that differs by cache type.
	runsKilledMountpointSpec bool

	// nodeSelector keeps the driver and the workloads to the nodes this cache can exist on; nil is every node.
	nodeSelector map[string]string
	requires     clusterPrerequisite
}

const (
	// The two emptyDir size limits, in MiB, as numbers because the specs assert `df` against them.
	tmpfsCacheSizeLimitMiB = 128
	diskCacheSizeLimitMiB  = 128
	// The two ephemeral volume sizes, in MiB. 1Gi is the smallest gp3 volume.
	ebsCacheSizeMiB  = 1024
	nvmeCacheSizeMiB = 10240

	// The maxVolumesPerNode every row installs today; a row can set its own to shrink its equalSplit share.
	defaultMaxVolumesPerNode = 4
)

// Fixed, not from f.UniqueName: in an Ordered container that names the first spec's namespace, deleted long before this class.
const daemonsetCacheStorageClassName = "s3-csi-e2e-daemonset-cache-sc"

// The Local Volume Static Provisioner's class for instance-store disks, which its manifest below creates.
const nvmeCacheStorageClassName = "nvme-ssd"

// The label the dev and CI nodegroups with an instance-store disk carry.
const localNVMeNodeLabel = "s3.csi.aws.com/local-nvme"

var localNVMeNodeSelector = map[string]string{localNVMeNodeLabel: "true"}

// The provisioner's DaemonSet in kube-system, as the manifest names it.
const localStaticProvisionerName = "local-static-provisioner"

// localStaticProvisionerManifest is the provisioner for those nodes; its header says what differs from upstream.
//
//go:embed testdata/local-static-provisioner-nvme.yaml
var localStaticProvisionerManifest string

var (
	diskCacheBlock = map[string]any{
		"emptyDir":            map[string]any{"medium": "", "sizeLimit": fmt.Sprintf("%dMi", diskCacheSizeLimitMiB)},
		cacheLimitStrategyKey: "equalSplit",
	}
	tmpfsCacheBlock = map[string]any{
		"emptyDir":            map[string]any{"medium": "Memory", "sizeLimit": fmt.Sprintf("%dMi", tmpfsCacheSizeLimitMiB)},
		cacheLimitStrategyKey: "equalSplit",
	}
)

var cacheVolumes = []cacheVolume{
	{
		name:                     "an emptyDir cache on the node's disk",
		helm:                     diskCacheBlock,
		cacheFilesystem:          "the node's disk",
		cacheSizeMiB:             diskCacheSizeLimitMiB,
		maxVolumesPerNode:        defaultMaxVolumesPerNode,
		runsRestartSpecs:         true,
		assertsCacheStaysGone:    true,
		runsKilledMountpointSpec: true,
	},
	{
		name:              "an emptyDir cache on tmpfs",
		helm:              tmpfsCacheBlock,
		cacheFilesystem:   "tmpfs",
		cacheSizeMiB:      tmpfsCacheSizeLimitMiB,
		maxVolumesPerNode: defaultMaxVolumesPerNode,
	},
	{
		name: "an ephemeral cache on an EBS volume",
		helm: map[string]any{
			"ephemeral":           map[string]any{"storageClassName": daemonsetCacheStorageClassName, "resourceRequests": fmt.Sprintf("%dMi", ebsCacheSizeMiB)},
			cacheLimitStrategyKey: "equalSplit",
		},
		cacheFilesystem:   "its own volume",
		cacheSizeMiB:      ebsCacheSizeMiB,
		maxVolumesPerNode: defaultMaxVolumesPerNode,
		runsRestartSpecs:  true,
		requires:          requiresEBSCSIDriver,
	},
	{
		// Same `ephemeral` type as above but from an instance-store disk, which the driver does not distinguish: this asserts the
		// documented instance-store path caches like any ephemeral volume.
		name: "an ephemeral cache on an NVMe instance store",
		helm: map[string]any{
			"ephemeral":           map[string]any{"storageClassName": nvmeCacheStorageClassName, "resourceRequests": fmt.Sprintf("%dMi", nvmeCacheSizeMiB)},
			cacheLimitStrategyKey: "equalSplit",
		},
		cacheFilesystem:   "its own volume",
		cacheSizeMiB:      nvmeCacheSizeMiB,
		maxVolumesPerNode: defaultMaxVolumesPerNode,
		// A mounter on a node without a free disk stays Pending, so only labelled nodes run the driver and the workloads.
		nodeSelector: localNVMeNodeSelector,
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

	Describe("Local cache on the mounter DaemonSet", Ordered, Serial, func() {
		var (
			config   *storageframework.PerTestConfig
			mounters *mounterValuesConfigurator
		)

		BeforeAll(func(ctx context.Context) {
			// Registered before the restore, so cleanup (LIFO) removes these last, after the restore has removed the cache PVCs using them.
			createEBSCacheSC(ctx, teardownSafeFramework(f), daemonsetCacheStorageClassName)
			installLocalVolumeProvisioner(ctx, teardownSafeFramework(f))
			mounters = setUpMounterReconfiguration(ctx, f)
		})

		BeforeEach(func(ctx context.Context) {
			config = driver.PrepareTest(ctx, f)
		})

		for _, tc := range cacheVolumes {
			// BeforeAll, not BeforeEach: one reconfiguration replaces every mounter pod, so per spec would cost five times the rollouts.
			Context("with "+tc.name, func() {
				BeforeAll(func(ctx context.Context) {
					if !clusterProvides(ctx, f, tc.requires) {
						Skip(string(tc.requires))
					}
					mounters.apply(ctx, f, tc.helm, strategyFromCacheBlock, tc.maxVolumesPerNode, tc.nodeSelector)
					// So a value the chart did not render fails as that, not as a wrong share.
					Expect(maxVolumesPerNodeOf(anyRunningMounterPod(ctx, f))).To(Equal(int64(tc.maxVolumesPerNode)),
						"the mounter does not run with the maxVolumesPerNode this row installed")
				})

				BeforeEach(func() {
					// createPodsOnSameNode places workloads by the volume's config, which PrepareTest makes afresh for every spec.
					config.ClientNodeSelection.Selector = tc.nodeSelector
				})

				It("serves a repeated read from the cache", func(ctx context.Context) {
					// Without an indefinite TTL Mountpoint re-checks S3 after 60s and the post-delete read fails.
					vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, []string{"metadata-ttl indefinite"})
					node, pods := createPodsOnSameNode(ctx, f, 1, vol)
					DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
					mounter := mounterPodOnNode(ctx, f, node)

					By("Writing a file and reading it back (three times) Mountpoint caches its blocks")
					path := filepath.Join(e2epod.VolumeMountPath1, cachedFileName)
					seed := time.Now().UTC().UnixNano()
					checkWriteToPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
					for range 3 {
						checkReadFromPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
					}

					By("Confirming Mountpoint created its own directory inside the mounter's")
					assertCacheDirExists(ctx, f, mounter, vol.Pv.Name)
					assertCacheDirMode(ctx, f, mounter, vol.Pv.Name)
					waitAndAssertMountpointWroteCacheDir(ctx, f, mounter, vol.Pv.Name)
					waitAndAssertMountpointCachedBlocks(ctx, f, mounter, vol.Pv.Name)

					By("Deleting the object from S3, so the next read can only be served by the cache")
					deleteObjectFromS3(ctx, bucketNameFromVolumeResource(vol), cachedFileName)
					checkReadFromPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
				})

				It("gives the mounter the cache filesystem the chart asked for", func(ctx context.Context) {
					mounter := anyRunningMounterPod(ctx, f)
					disk := statfsInMounter(ctx, f, mounter, kubeletHostsFile)
					cache := statfsInMounter(ctx, f, mounter, cacheMountPath)

					By("Comparing /cache with /etc/hosts, which the kubelet serves from the mounter's pod directory")
					Expect(cacheFilesystemOf(cache, disk)).To(Equal(tc.cacheFilesystem),
						"/cache is %s (%s) and /etc/hosts is %s (%s)", cache.device, cache.fsType, disk.device, disk.fsType)

					// df sees sizeLimit only on tmpfs: a disk emptyDir shows the whole node disk, and an EBS filesystem takes some of its 1Gi.
					if tc.cacheFilesystem == "tmpfs" {
						// The kernel sizes a tmpfs from sizeLimit in whole pages (usually 4KiB, sometimes 16KiB/64KiB).
						// We use 128 MiB so it will basically always be whole pages, so the match should be exact.
						Expect(cache.totalKiB).To(Equal(tmpfsCacheSizeLimitMiB*1024),
							"/cache is %d KiB, not the %d MiB the chart asked for", cache.totalKiB, tmpfsCacheSizeLimitMiB)
					}
				})

				It("gives no cache to a PV that does not ask for one", func(ctx context.Context) {
					// Mountpoint refuses `--max-cache-size` without `--cache`, so only a real one shows the mounter kept its share off this mount.
					vol := createVolumeResourceWithAttributes(ctx, config, pattern, nil, nil)
					// Scheduled first and the mounter found from it: the mounter tolerates taints a workload does not.
					node, pods := createPodsOnSameNode(ctx, f, 1, vol)
					DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
					mounter := mounterPodOnNode(ctx, f, node)

					By("Confirming the mount serves I/O and got no cache directory")
					path := filepath.Join(e2epod.VolumeMountPath1, "uncached.txt")
					seed := time.Now().UTC().UnixNano()
					checkWriteToPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
					assertCacheDirAbsent(ctx, f, mounter, vol.Pv.Name)
				})

				// TODO: time the fill spec per row once run; if the EBS/disk rows are too slow, run it on tmpfs only.
				// TODO: the nodes (c5a.xlarge 8 GiB, t3.xlarge 16 GiB) may fit a larger memory request, which would allow a
				// larger maxVolumesPerNode, a smaller EBS share and a faster fill. Check before raising it.
				It("gives a mount an equal share of the cache volume", func(ctx context.Context) {
					vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)
					node, pods := createPodsOnSameNode(ctx, f, 1, vol)
					DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })

					By("Reading the Mountpoint process the mounter started for this mount")
					args := mountpointArgsInMounter(ctx, f, mounterPodOnNode(ctx, f, node), vol.Pv.Name)
					Expect(args).To(ContainSubstring(maxCacheSizeArg(equalSplitShareMiB(tc.cacheSizeMiB, tc.maxVolumesPerNode))),
						"the mounter did not size this mount's cache; its Mountpoint runs with: %s", args)
				})

				It("ignores the max-cache-size a PV asked for", func(ctx context.Context) {
					vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes,
						[]string{fmt.Sprintf("max-cache-size %d", oversizedPVMaxCacheSizeMiB)})
					node, pods := createPodsOnSameNode(ctx, f, 1, vol)
					DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })

					// The mounter replaces the PV's value rather than adding to it, so a mount running with 4096 means injection stopped working.
					By("Confirming the mounter's share replaced the PV's value rather than joining it")
					args := mountpointArgsInMounter(ctx, f, mounterPodOnNode(ctx, f, node), vol.Pv.Name)
					Expect(args).To(ContainSubstring(maxCacheSizeArg(equalSplitShareMiB(tc.cacheSizeMiB, tc.maxVolumesPerNode))),
						"Mountpoint runs with: %s", args)
					Expect(args).NotTo(ContainSubstring(maxCacheSizeArg(oversizedPVMaxCacheSizeMiB)),
						"the PV's own max-cache-size reached Mountpoint: %s", args)
				})

				It("keeps serving correct data when the cache fills", func(ctx context.Context) {
					vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)
					node, pods := createPodsOnSameNode(ctx, f, 1, vol)
					DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
					mounter := mounterPodOnNode(ctx, f, node)

					shareMiB := equalSplitShareMiB(tc.cacheSizeMiB, tc.maxVolumesPerNode)
					By("Writing 1.5x the mount's cache share, then reading all of it twice")
					fillVolume(ctx, f, pods[0], (shareMiB*3/2+fillFileSizeMiB-1)/fillFileSizeMiB) // ceiling division formula :)
					readVolumeAndVerify(ctx, f, pods[0])
					readVolumeAndVerify(ctx, f, pods[0])

					// Without these the spec passes with the cache off: an uncached mount returns the same reads and checksums.
					By("Confirming a cache existed and evicted rather than grew")
					waitAndAssertMountpointWroteCacheDir(ctx, f, mounter, vol.Pv.Name)
					usedKiB := mountCacheUsageKiB(ctx, f, mounter, vol.Pv.Name)
					Expect(usedKiB).To(BeNumerically("<=", shareMiB*(1024+duKiBPerCachedMiB)+cacheOvershootKiB),
						"the cache for volume %s holds %d KiB against a %d MiB share, so nothing evicted",
						vol.Pv.Name, usedKiB, shareMiB)
				})

				if tc.cacheFilesystem == "tmpfs" {
					It("gives the mount an equal share of the memory request the tmpfs cache leaves", func(ctx context.Context) {
						vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)
						node, pods := createPodsOnSameNode(ctx, f, 1, vol)
						DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
						mounter := mounterPodOnNode(ctx, f, node)

						const miB = 1024 * 1024
						maxVolumes := maxVolumesPerNodeOf(mounter)
						requestMiB := mounterContainer(mounter).Resources.Requests.Memory().Value() / miB
						// (requests.memory - tmpfs sizeLimit - 64 MiB for the mounter process) / maxVolumesPerNode.
						want := fmt.Sprintf("--memory-target=%d ", (requestMiB-tmpfsCacheSizeLimitMiB-64)/maxVolumes)

						args := mountpointArgsInMounter(ctx, f, mounter, vol.Pv.Name)
						Expect(args).To(ContainSubstring(want),
							"want is (%d MiB request - %d MiB tmpfs - 64 MiB mounter overhead) / %d; Mountpoint runs with: %s",
							requestMiB, tmpfsCacheSizeLimitMiB, maxVolumes, args)
					})
				}

				It("removes the mount's cache directory when its last consumer unmounts, and keeps the volume root", func(ctx context.Context) {
					vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)
					node, pods := createPodsOnSameNode(ctx, f, 2, vol)
					// For a failure before the deletions below; once they have run it is a no-op.
					DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
					mounter := mounterPodOnNode(ctx, f, node)

					By("Confirming both consumers share one cache directory")
					assertCacheDirExists(ctx, f, mounter, vol.Pv.Name)

					By("Deleting the first consumer, since the cache belongs to the mount and not to a pod")
					framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[0]))
					assertCacheDirExists(ctx, f, mounter, vol.Pv.Name)

					By("Deleting the last consumer, which is what reclaims the cache directory")
					framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[1]))
					waitAndAssertCacheDirReclaimed(ctx, f, mounter, vol.Pv.Name, tc.assertsCacheStaysGone)

					By("Confirming the volume root every other mount shares is still there")
					Expect(pathKindInMounter(ctx, f, mounter, cacheMountPath)).To(Equal("directory"),
						"%s was removed with the mount's own directory", cacheMountPath)
				})

				if tc.runsRestartSpecs {
					// Around 60s
					It("caches a new workload over the dead mount a mounter crash left behind", func(ctx context.Context) {
						vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)
						node, pods := createPodsOnSameNode(ctx, f, 1, vol)
						DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
						oldMounter := mounterPodOnNode(ctx, f, node)
						assertCacheDirExists(ctx, f, oldMounter, vol.Pv.Name)

						By("Killing the mounter pod, which kills every Mountpoint process on the node")
						killMounterPodOnNode(ctx, f, node)
						// Not waitForMounterPodReady: its 3 minutes is under what a new ephemeral cache volume is given.
						// TODO change waitForMounterPodReady so we don't need to use new waitForMounterDaemonSetReady function
						// Or maybe time is used for deleting pod so we could force delete pod?
						waitForMounterDaemonSetReady(ctx, f, mounterRestartTimeout)

						// The cache volume lives under <kubelet>/pods/<mounter UID>, so a new pod is a new, empty cache volume.
						By("Confirming the replacement is a different pod, with a different cache volume")
						newMounter := mounterPodOnNode(ctx, f, node)
						Expect(newMounter.UID).NotTo(Equal(oldMounter.UID))
						// Until then the next mount can fail on the old mounter and be retried as an ordinary fresh mount.
						waitForCSINodeToDiscoverMounter(ctx, f, newMounter)

						// Left running deliberately: its dead map entry makes the next mount take the fresh-mount-over-a-dead-source path,
						// not the unmount-then-mount one a deletion would give.
						By("Confirming the old workload's mount died with the mounter, and leaving it in place")
						assertPodIOFails(ctx, f, pods[0], e2epod.VolumeMountPath1)

						By("Creating a second workload, which must cache under the new mounter pod's volume")
						newPods := createPodOnNode(ctx, f, node, vol)
						DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, newPods) })
						assertCacheDirExists(ctx, f, newMounter, vol.Pv.Name)
						waitAndAssertMountpointWroteCacheDir(ctx, f, newMounter, vol.Pv.Name)
					})

					// Around 30s
					It("keeps the mount's cache across an s3-csi-node restart and still reclaims it on unmount", func(ctx context.Context) {
						vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)
						node, pods := createPodsOnSameNode(ctx, f, 1, vol)
						// For a failure before the deletion below; once it has run it is a no-op.
						DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
						mounter := mounterPodOnNode(ctx, f, node)

						path := filepath.Join(e2epod.VolumeMountPath1, cachedFileName)
						seed := time.Now().UTC().UnixNano()
						checkWriteToPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
						assertCacheDirExists(ctx, f, mounter, vol.Pv.Name)

						By("Killing s3-csi-node, which loses its mount map; the Mountpoint processes live on")
						killCSINodePodOnNode(ctx, f, node)
						waitForCSINodePodReady(ctx, f, node)
						waitForCSINodePodStable(ctx, f, node)

						By("Confirming the running mount still reads and its cache directory was not swept")
						checkReadFromPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
						assertCacheDirExists(ctx, f, mounter, vol.Pv.Name)

						// A rebuilt entry that failed to unmount would leave Mountpoint running, and the mounter removes the directory only once it exits.
						By("Deleting the workload, which the rebuilt entry must still know how to unmount")
						framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[0]))
						waitAndAssertCacheDirReclaimed(ctx, f, mounter, vol.Pv.Name, false)
					})
				}

				if tc.runsKilledMountpointSpec {
					It("reclaims a killed Mountpoint's cache directory while its workload still runs", func(ctx context.Context) {
						vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)
						node, pods := createPodsOnSameNode(ctx, f, 1, vol)
						DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
						mounter := mounterPodOnNode(ctx, f, node)
						assertCacheDirExists(ctx, f, mounter, vol.Pv.Name)

						// SIGKILL, not SIGTERM: a clean exit wipes its own cache, leaving an empty directory to remove.
						// The mounter does not restart it, so the source stays in the mount table and errors on every I/O.
						By("Killing this mount's Mountpoint, which leaves its source mounted but dead")
						killMountpointForMount(ctx, f, mounter, vol.Pv.Name)
						assertPodIOFails(ctx, f, pods[0], e2epod.VolumeMountPath1)

						// The workload stays, so nothing calls NodeUnpublishVolume: only the mounter, seeing Mountpoint exit, removes the directory.
						By("Waiting for the mounter to remove the dead Mountpoint's cache directory")
						waitAndAssertCacheDirReclaimed(ctx, f, mounter, vol.Pv.Name, false)
					})
				}
			})
		}

		Context("with cacheLimitStrategy none and an emptyDir cache on the node's disk", func() {
			BeforeAll(func(ctx context.Context) {
				mounters.apply(ctx, f, diskCacheBlock, "none", 0, nil)
			})

			It("keeps a mount's cache inside the max-cache-size its PV set", func(ctx context.Context) {
				// On the node's disk this is the only bound that holds, Mountpoint's free-space check (for emptyDir disk only) reads
				// the root filesystem, not the cache volume, and the kubelet will not evict the system-node-critical mounter over sizeLimit.
				vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes,
					[]string{fmt.Sprintf("max-cache-size %d", pvMaxCacheSizeMiB)})
				node, pods := createPodsOnSameNode(ctx, f, 1, vol)
				DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })
				mounter := mounterPodOnNode(ctx, f, node)

				// Asserted directly too, so a mounter that silently replaced the value fails here rather than as a size overshoot.
				By("Confirming the PV's own value is what Mountpoint runs with")
				args := mountpointArgsInMounter(ctx, f, mounter, vol.Pv.Name)
				Expect(args).To(ContainSubstring(maxCacheSizeArg(pvMaxCacheSizeMiB)),
					"Mountpoint runs with: %s", args)

				By("Writing 1.5x the PV's max-cache-size, then reading all of it twice")
				fillVolume(ctx, f, pods[0], (pvMaxCacheSizeMiB*3/2+fillFileSizeMiB-1)/fillFileSizeMiB)
				readVolumeAndVerify(ctx, f, pods[0])
				readVolumeAndVerify(ctx, f, pods[0])

				By("Measuring what the mount's cache directory actually holds")
				usedKiB := mountCacheUsageKiB(ctx, f, mounter, vol.Pv.Name)
				Expect(usedKiB).To(BeNumerically("<=", pvMaxCacheSizeMiB*1024+cacheOvershootKiB),
					"the cache for volume %s holds %d KiB against a max-cache-size of %d MiB",
					vol.Pv.Name, usedKiB, pvMaxCacheSizeMiB)
			})
		})

		// Last: it is the negative case and the cheapest reconfiguration, so it sits closest to the restore.
		Context("with no cache volume, as a default install has", func() {
			BeforeAll(func(ctx context.Context) {
				mounters.apply(ctx, f, nil, strategyFromCacheBlock, 0, nil)
			})

			It("rejects a PV that asks for a cache, naming the Helm value that turns one on", func(ctx context.Context) {
				mounter := anyRunningMounterPod(ctx, f)

				By("Confirming the mounter has no /cache at all")
				Expect(pathKindInMounter(ctx, f, mounter, cacheMountPath)).To(Equal("absent"),
					"the mounter should have no %s when the chart renders no cache volume", cacheMountPath)

				vol := createVolumeResourceWithAttributes(ctx, config, pattern, cacheEnabledAttributes, nil)

				// Not pinned to that mounter's node: the mounter tolerates taints a workload does not, and every mounter has the same spec.
				By("Creating a workload whose PV asks for a cache anyway")
				pod := pendingPod(ctx, f, vol)

				By("Waiting for a FailedMount event that names the Helm value to add")
				assertPodFailsToMount(ctx, f, pod, "has no cache volume")
				assertPodFailsToMount(ctx, f, pod, "daemonsetMounters[0].cache")
			})

			It("mounts a PV that does not ask for a cache", func(ctx context.Context) {
				vol := createVolumeResourceWithAttributes(ctx, config, pattern, nil, nil)
				_, pods := createPodsOnSameNode(ctx, f, 1, vol)
				DeferCleanup(func(ctx context.Context) { deletePodsInOrder(ctx, f, pods) })

				path := filepath.Join(e2epod.VolumeMountPath1, "uncached.txt")
				seed := time.Now().UTC().UnixNano()
				checkWriteToPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
				checkReadFromPathSucceed(ctx, f, pods[0], path, ioFileSize, seed)
			})
		})
	})
}

// --- Fixtures -----------------------------------------------------------------------------------

// installLocalVolumeProvisioner installs the vendored provisioner when a node is labelled as having an instance-store disk, and
// registers its removal. One already running (e.g. from dev/) is used as is and left in place.
func installLocalVolumeProvisioner(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	if !clusterHasLocalNVMeNodes(ctx, f) {
		return
	}
	_, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).Get(ctx, localStaticProvisionerName, metav1.GetOptions{})
	if err == nil {
		framework.Logf("Using the Local Volume Static Provisioner already installed for StorageClass %s", nvmeCacheStorageClassName)
		return
	}
	if !apierrors.IsNotFound(err) {
		framework.ExpectNoError(err, "looking for the Local Volume Static Provisioner")
	}
	DeferCleanup(func(ctx context.Context) { uninstallLocalVolumeProvisioner(ctx, f) })
	framework.Logf("Installing the Local Volume Static Provisioner for StorageClass %s", nvmeCacheStorageClassName)
	_, err = e2ekubectl.RunKubectlInput("", localStaticProvisionerManifest, "apply", "-f", "-")
	framework.ExpectNoError(err, "installing the Local Volume Static Provisioner")
}

// clusterProvides reports whether the cluster can host a cache configuration with this prerequisite.
func clusterProvides(ctx context.Context, f *framework.Framework, req clusterPrerequisite) bool {
	GinkgoHelper()
	switch req {
	case requiresEBSCSIDriver:
		// ebsCSIDriverDaemonSet looks for `ebs-csi-node` in kube-system, so this skips on ROSA, where OpenShift names and places it elsewhere.
		return ebsCSIDriverDaemonSet(ctx, f) != nil
	case requiresLocalNVMe:
		if !clusterHasLocalNVMeNodes(ctx, f) {
			return false
		}
		// One PV per disk is made up front; a class with no Available PV leaves the mounter's claim Pending for the whole rollout.
		waitAndAssertAvailablePVInStorageClass(ctx, f, nvmeCacheStorageClassName)
	}
	return true
}

// clusterHasLocalNVMeNodes reports whether any node is labelled as having an instance-store disk.
func clusterHasLocalNVMeNodes(ctx context.Context, f *framework.Framework) bool {
	GinkgoHelper()
	nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: localNVMeNodeLabel + "=true"})
	framework.ExpectNoError(err, "listing nodes labelled %s", localNVMeNodeLabel)
	return len(nodes.Items) > 0
}

// waitAndAssertAvailablePVInStorageClass waits for a PV in this class that a claim can bind to.
func waitAndAssertAvailablePVInStorageClass(ctx context.Context, f *framework.Framework, className string) {
	GinkgoHelper()
	Eventually(ctx, func(ctx context.Context) ([]string, error) {
		return pvsInStorageClass(ctx, f, className, func(phase v1.PersistentVolumePhase) bool { return phase == v1.VolumeAvailable })
	}).WithTimeout(localPVDiscoveryTimeout).WithPolling(5*time.Second).ShouldNot(BeEmpty(),
		"no Available PersistentVolume in StorageClass %s, so the provisioner found no instance-store disk", className)
}

// uninstallLocalVolumeProvisioner removes the provisioner, then the PVs it leaves behind. It runs after the restore, because
// only the provisioner wipes the disk a deleted cache claim released and makes its PV Available again.
func uninstallLocalVolumeProvisioner(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	notAvailable := func(ctx context.Context) ([]string, error) {
		return pvsInStorageClass(ctx, f, nvmeCacheStorageClassName, func(phase v1.PersistentVolumePhase) bool { return phase != v1.VolumeAvailable })
	}
	releaseErr := framework.Gomega().Eventually(ctx, notAvailable).
		WithTimeout(localPVReleaseTimeout).WithPolling(5 * time.Second).Should(BeEmpty())

	// Removed even if a disk was not wiped: a privileged DaemonSet left on the cluster is the worse leak.
	// Foreground, so its pods are gone before the PVs are listed and cannot recreate one afterwards.
	_, err := e2ekubectl.RunKubectlInput("", localStaticProvisionerManifest, "delete", "--ignore-not-found", "--cascade=foreground", "-f", "-")
	framework.ExpectNoError(err, "removing the Local Volume Static Provisioner")
	pvs, err := pvsInStorageClass(ctx, f, nvmeCacheStorageClassName, func(v1.PersistentVolumePhase) bool { return true })
	framework.ExpectNoError(err, "listing the PersistentVolumes in StorageClass %s", nvmeCacheStorageClassName)
	for _, pv := range pvs {
		framework.ExpectNoError(f.ClientSet.CoreV1().PersistentVolumes().Delete(ctx, pv, metav1.DeleteOptions{}), "deleting PV %s", pv)
	}
	framework.ExpectNoError(releaseErr, "PersistentVolumes in StorageClass %s were still claimed or unwiped", nvmeCacheStorageClassName)
}

// pvsInStorageClass names the PVs in this class whose phase matches.
func pvsInStorageClass(ctx context.Context, f *framework.Framework, className string, match func(v1.PersistentVolumePhase) bool) ([]string, error) {
	pvs, err := f.ClientSet.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range pvs.Items {
		if pvs.Items[i].Spec.StorageClassName == className && match(pvs.Items[i].Status.Phase) {
			names = append(names, pvs.Items[i].Name)
		}
	}
	return names, nil
}

// createVolumeResourceWithAttributes creates a PV with these volumeAttributes (nil: no cache opt-in) and registers its cleanup.
func createVolumeResourceWithAttributes(ctx context.Context, config *storageframework.PerTestConfig, pattern storageframework.TestPattern,
	attrs map[string]string, mountOptions []string) *storageframework.VolumeResource {
	GinkgoHelper()
	vol := createVolumeResourceWithMountOptions(contextWithVolumeAttributes(ctx, attrs), config, pattern, mountOptions)
	DeferCleanup(vol.CleanupResource)
	return vol
}

// mounterPodOnNode returns the mounter pod sharing a node with a workload (in multi-node cluster).
func mounterPodOnNode(ctx context.Context, f *framework.Framework, nodeName string) *v1.Pod {
	GinkgoHelper()
	Expect(nodeName).NotTo(BeEmpty(), "workload pod has no node assigned")
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: mounterDaemonSetLabel,
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	framework.ExpectNoError(err)
	Expect(pods.Items).To(HaveLen(1), "expected exactly one mounter pod on node %s", nodeName)
	return &pods.Items[0]
}

// anyRunningMounterPod returns one ready mounter pod, for specs that do not care which node's cache volume they inspect.
func anyRunningMounterPod(ctx context.Context, f *framework.Framework) *v1.Pod {
	GinkgoHelper()
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: mounterDaemonSetLabel,
	})
	framework.ExpectNoError(err, "listing mounter pods")
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == v1.PodRunning && isPodReady(&pods.Items[i]) {
			return &pods.Items[i]
		}
	}
	Fail(fmt.Sprintf("no ready mounter pod out of %d; the cache volume lives on one", len(pods.Items)))
	return nil
}

// mounterContainer returns the mounter pod's container that runs s3-csi-daemonset-mounter.
func mounterContainer(mounter *v1.Pod) *v1.Container {
	GinkgoHelper()
	for i := range mounter.Spec.Containers {
		if mounter.Spec.Containers[i].Name == mounterContainerName {
			return &mounter.Spec.Containers[i]
		}
	}
	Fail(fmt.Sprintf("mounter pod %s has no container named %q", mounter.Name, mounterContainerName))
	return nil
}

// maxVolumesPerNodeOf returns the maxVolumesPerNode the chart rendered into the mounter's arguments.
func maxVolumesPerNodeOf(mounter *v1.Pod) int64 {
	GinkgoHelper()
	args := mounterContainer(mounter).Args
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--max-volumes-per-node="); ok {
			maxVolumes, err := strconv.ParseInt(value, 10, 64)
			framework.ExpectNoError(err, "parsing %q in mounter pod %s", arg, mounter.Name)
			return maxVolumes
		}
	}
	Fail(fmt.Sprintf("mounter pod %s has no --max-volumes-per-node in its args: %v", mounter.Name, args))
	return 0
}

// fillVolume writes fileCount files of fillFileSizeMiB through the mount.
func fillVolume(ctx context.Context, f *framework.Framework, pod *v1.Pod, fileCount int) {
	GinkgoHelper()
	checkExecInPodSucceed(ctx, f, pod, fmt.Sprintf(
		"for i in $(seq 1 %d); do dd if=/dev/urandom of=%s/fill-$i bs=1M count=%d status=none; done; sync",
		fileCount, e2epod.VolumeMountPath1, fillFileSizeMiB))
	// Recorded on the container's own filesystem, so later reads compare against something the mount cannot have changed.
	checkExecInPodSucceed(ctx, f, pod, fmt.Sprintf("cd %s && md5sum fill-* > /tmp/fill.md5", e2epod.VolumeMountPath1))
}

// equalSplitShareMiB mirrors the mounter's equalSplit: 95% of the cache volume over maxVolumesPerNode, in whole MiB.
func equalSplitShareMiB(cacheSizeMiB, maxVolumesPerNode int) int {
	return cacheSizeMiB * 95 / 100 / maxVolumesPerNode
}

// readVolumeAndVerify checks fillVolume's files read back intact; the first call fills the cache, later ones read what survived.
func readVolumeAndVerify(ctx context.Context, f *framework.Framework, pod *v1.Pod) {
	GinkgoHelper()
	checkExecInPodSucceed(ctx, f, pod, fmt.Sprintf("cd %s && md5sum -c /tmp/fill.md5", e2epod.VolumeMountPath1))
}

// waitForCSINodeToDiscoverMounter waits until s3-csi-node on the mounter's node logs this mounter pod's UID as discovered.
func waitForCSINodeToDiscoverMounter(ctx context.Context, f *framework.Framework, mounter *v1.Pod) {
	GinkgoHelper()
	uid := string(mounter.UID)
	Eventually(ctx, func(ctx context.Context) (bool, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + csiDriverDaemonSetName,
			FieldSelector: "spec.nodeName=" + mounter.Spec.NodeName,
		})
		if err != nil || len(pods.Items) != 1 {
			return false, err
		}
		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, csiDriverDaemonSetNamespace, pods.Items[0].Name, csiNodeContainerName)
		return strings.Contains(logs, uid), err
	}).WithTimeout(mounterRediscoveryTimeout).WithPolling(2*time.Second).Should(BeTrue(),
		"s3-csi-node on %s never logged mounter pod %s (uid %s); this relies on its --v=4 or higher discovery log line",
		mounter.Spec.NodeName, mounter.Name, uid)
}

// pendingPod creates a workload without waiting for it to start, and registers its deletion.
func pendingPod(ctx context.Context, f *framework.Framework, vol *storageframework.VolumeResource) *v1.Pod {
	GinkgoHelper()
	pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
	pod, err := createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name, pod)
	framework.ExpectNoError(err)
	DeferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })
	return pod
}

// --- Cache assertions ---------------------------------------------------------------------------

// assertCacheDirExists checks the mounter created this mount's cache directory.
func assertCacheDirExists(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) {
	GinkgoHelper()
	dir := mountCacheDir(pvName)
	Expect(pathKindInMounter(ctx, f, mounter, dir)).To(Equal("directory"),
		"the mounter created no %s in mounter pod %s", dir, mounter.Name)
}

// mountCacheDir is a mount's cache directory inside the mounter pod, which is also what the mounter passes as `--cache`.
func mountCacheDir(pvName string) string {
	return filepath.Join(cacheMountPath, pvName)
}

// waitAndAssertMountpointWroteCacheDir checks Mountpoint created its own directory inside the mounter's: the end-to-end proof
// the permissions are right, since Mountpoint runs as a different user from the one that created the parent.
func waitAndAssertMountpointWroteCacheDir(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) {
	GinkgoHelper()
	dir := filepath.Join(mountCacheHostDir(ctx, f, mounter, pvName), mountpointCacheDirName)
	Eventually(ctx, func(ctx context.Context) (string, error) {
		return pathKindOnNode(ctx, f, mounter, dir), nil
	}).WithTimeout(time.Minute).WithPolling(5*time.Second).Should(Equal("directory"),
		"Mountpoint did not create %s, so it could not write into the cache directory", dir)
}

// waitAndAssertMountpointCachedBlocks waits for a block file, which Mountpoint writes in the background after the read returns.
func waitAndAssertMountpointCachedBlocks(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) {
	GinkgoHelper()
	dir := filepath.Join(mountCacheHostDir(ctx, f, mounter, pvName), mountpointCacheDirName)
	// Any regular file at any depth, whatever the cache layout; `*` skips dotfiles, where Mountpoint writes a block before renaming it.
	findBlock := fmt.Sprintf(`walk() { for e in "$1"/*; do if [ -f "$e" ]; then echo "$e"; return 0; fi; `+
		`if [ -d "$e" ] && walk "$e"; then return 0; fi; done; return 1; }; walk %q || true`, dir)
	Eventually(ctx, func(ctx context.Context) (string, error) {
		return runOnNode(ctx, f, mounter, findBlock)
	}).WithTimeout(time.Minute).WithPolling(5*time.Second).ShouldNot(BeEmpty(),
		"Mountpoint wrote no cache block under %s", dir)
}

// waitAndAssertCacheDirReclaimed waits for the mounter to remove this mount's cache directory, which it does once the mount's
// Mountpoint exits, so possibly after NodeUnpublishVolume has returned. stayGone costs cacheDirStaysGoneWindow.
func waitAndAssertCacheDirReclaimed(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string, stayGone bool) {
	GinkgoHelper()
	dir := mountCacheDir(pvName)
	kind := func(ctx context.Context) (string, error) {
		return pathKindInMounter(ctx, f, mounter, dir), nil
	}
	Eventually(ctx, kind).WithTimeout(time.Minute).WithPolling(2*time.Second).Should(Equal("absent"),
		"%s outlived its Mountpoint", dir)
	if !stayGone {
		return
	}
	Consistently(ctx, kind).WithTimeout(cacheDirStaysGoneWindow).WithPolling(5*time.Second).Should(Equal("absent"),
		"%s came back after the unmount", dir)
}

// assertCacheDirMode checks a mount's cache directory mode; the one site to edit when process isolation makes it 0700 per UID.
func assertCacheDirMode(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) {
	GinkgoHelper()
	dir := mountCacheDir(pvName)
	mode, err := runInMounter(ctx, f, mounter, "stat", "-c", "%A", dir)
	framework.ExpectNoError(err, "stat %s in mounter pod %s", dir, mounter.Name)
	Expect(mode).To(Equal(cacheDirMode), "%s in mounter pod %s is %s", dir, mounter.Name, mode)
}

// cacheFilesystemOf describes /cache the way the table spells it. Its filesystem type comes from the AMI
// or the provisioner, so only whether it shares the node's disk is ours to assert.
func cacheFilesystemOf(cache, disk mounterFilesystem) string {
	switch {
	case cache.device == disk.device:
		return "the node's disk"
	case cache.fsType == "tmpfs":
		return "tmpfs"
	default:
		return "its own volume"
	}
}

// assertCacheDirAbsent checks no cache directory was created for this volume.
func assertCacheDirAbsent(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) {
	GinkgoHelper()
	dir := mountCacheDir(pvName)
	Expect(pathKindInMounter(ctx, f, mounter, dir)).To(Equal("absent"),
		"volume %s should have no cache directory, but %s exists in mounter pod %s", pvName, dir, mounter.Name)
}

// --- Looking inside the mounter pod -------------------------------------------------------------

// pathKindInMounter reports "directory", "file" or "absent"; its command always exits 0, so a failed exec never reads as "absent".
func pathKindInMounter(ctx context.Context, f *framework.Framework, mounter *v1.Pod, path string) string {
	GinkgoHelper()
	out, err := runInMounter(ctx, f, mounter, "/bin/sh", "-c", pathKindCmd(path))
	framework.ExpectNoError(err, "checking %s in mounter pod %s", path, mounter.Name)
	return out
}

func pathKindCmd(path string) string {
	return fmt.Sprintf("if [ -d %s ]; then echo directory; elif [ -e %s ]; then echo file; else echo absent; fi", path, path)
}

func runInMounter(ctx context.Context, f *framework.Framework, mounter *v1.Pod, cmd ...string) (string, error) {
	stdout, stderr, err := execInPodWithNamespace(ctx, f, mounter.Namespace, mounter.Name, mounterContainerName, cmd)
	if err != nil {
		return "", fmt.Errorf("%v in mounter pod %s: %w (stderr: %s)", cmd, mounter.Name, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

// mountpointArgsInMounter returns the command line of the Mountpoint process serving this mount, from the mounter's own /proc:
// the mounter picks --max-cache-size after s3-csi-node sent the options, so mount-s3's argv is the only place it is observable.
func mountpointArgsInMounter(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) string {
	GinkgoHelper()
	// A trailing space per line lets maxCacheSizeArg match a whole token. No grep: a miss exits 1 and reads as an exec failure.
	// Also note: we cannot use meta files since meta files are written by node pod, whereas
	// max-cache-size is injected by mounter pod, after node pod sent mount Options to mounter pod.
	out, err := runInMounter(ctx, f, mounter, "/bin/sh", "-c",
		`for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null; echo; done`)
	framework.ExpectNoError(err, "listing processes in mounter pod %s", mounter.Name)

	cache := "--cache=" + mountCacheDir(pvName)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, cache) {
			return line
		}
	}
	Fail(fmt.Sprintf("no Mountpoint process in mounter pod %s runs with %s; it hosts:\n%s", mounter.Name, cache, out))
	return ""
}

// maxCacheSizeArg is the `=` form pkg/mountpoint renders for a cache of this many MiB; the whole token keeps 30 from matching 4096.
func maxCacheSizeArg(miB int) string {
	return fmt.Sprintf("--max-cache-size=%d ", miB)
}

// mountCacheUsageKiB reports how much a mount's cache directory holds.
func mountCacheUsageKiB(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) int {
	GinkgoHelper()
	path := mountCacheHostDir(ctx, f, mounter, pvName)
	// No `| cut`: a pipeline reports the last command's status, so a du that could not descend would read as a near-empty cache.
	out, err := runOnNode(ctx, f, mounter, fmt.Sprintf("du -sk %q", path))
	framework.ExpectNoError(err, "du %s from the csi-node pod on node %s", path, mounter.Spec.NodeName)
	var kiB int
	_, err = fmt.Sscanf(out, "%d", &kiB)
	framework.ExpectNoError(err, "parsing du output %q for %s", out, path)
	framework.Logf("node %s: %s holds %d KiB", mounter.Spec.NodeName, path, kiB)
	return kiB
}

type mounterFilesystem struct {
	device   string
	fsType   string
	totalKiB int
}

// statfsInMounter reports the filesystem backing a path inside the mounter container.
func statfsInMounter(ctx context.Context, f *framework.Framework, mounter *v1.Pod, path string) mounterFilesystem {
	GinkgoHelper()
	// -P one POSIX line (device, type, total, used, available, capacity, path), -T type, -k KiB so units are locale-independent.
	out, err := runInMounter(ctx, f, mounter, "/bin/sh", "-c", "df -PTk "+path+" | tail -1")
	framework.ExpectNoError(err, "df %s in mounter pod %s", path, mounter.Name)

	fields := strings.Fields(out)
	Expect(len(fields)).To(BeNumerically(">=", 3), "unexpected df output for %s: %q", path, out)
	total, err := strconv.Atoi(fields[2])
	framework.ExpectNoError(err, "parsing the total size from df output %q", out)

	framework.Logf("mounter %s: %s is %s (%s), %d KiB", mounter.Name, path, fields[0], fields[1], total)
	return mounterFilesystem{device: fields[0], fsType: fields[1], totalKiB: total}
}

// killMountpointForMount kills the Mountpoint process serving this mount. What is left is a source the
// kernel still lists and every I/O fails on, which is the state the periodic cleanup probes for.
func killMountpointForMount(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) {
	GinkgoHelper()
	// No pkill in the image, so scan /proc as mountpointArgsInMounter does. The shell skips itself: its own command line carries
	// the pattern, and the glob expands before the pipeline's children exist.
	out, err := runInMounter(ctx, f, mounter, "/bin/sh", "-c", fmt.Sprintf(
		`for p in /proc/[0-9]*; do pid=${p#/proc/}; [ "$pid" = "$$" ] && continue; `+
			`tr '\0' ' ' < $p/cmdline 2>/dev/null | grep -q -- '--cache=%s ' && { kill -9 $pid; echo $pid; }; done`,
		mountCacheDir(pvName)))
	framework.ExpectNoError(err, "killing Mountpoint for volume %s in mounter pod %s", pvName, mounter.Name)
	Expect(out).NotTo(BeEmpty(),
		"no process in mounter pod %s served volume %s, so nothing was killed", mounter.Name, pvName)
}

// --- The mounter's cache volume, from outside the pod -------------------------------------------

// cacheVolumeNameInPodSpec is the mounter's cache volume name, which also names the kubelet's ephemeral claim: <mounter pod>-cache.
const cacheVolumeNameInPodSpec = "cache"

// The kubelet directory as csi-node mounts it, and the subdirectories it puts each volume kind under.
const (
	containerKubeletPath  = "/var/lib/kubelet"
	emptyDirVolumesSubdir = "kubernetes.io~empty-dir"
	csiVolumesSubdir      = "kubernetes.io~csi"
	localVolumesSubdir    = "kubernetes.io~local-volume"
	nfsVolumesSubdir      = "kubernetes.io~nfs"
)

// mountCacheHostDir is mountCacheDir as csi-node sees it: the same directory, reached through the kubelet pod directory.
func mountCacheHostDir(ctx context.Context, f *framework.Framework, mounter *v1.Pod, pvName string) string {
	GinkgoHelper()
	dir := filepath.Join(cacheHostDir(ctx, f, mounter), pvName)
	// Asserted here so a wrong host path fails as that, not as a Mountpoint that wrote nothing.
	Expect(pathKindOnNode(ctx, f, mounter, dir)).To(Equal("directory"),
		"csi-node does not see %s, which is where mounter pod %s mounts %s", dir, mounter.Name, mountCacheDir(pvName))
	return dir
}

// cacheHostDir returns the mounter pod's cache volume on the node, which is what the mounter sees at /cache. Built from the pod
// spec and, for `ephemeral`, the claim the kubelet bound and its PV, independently of the mounter, which only sees /cache.
func cacheHostDir(ctx context.Context, f *framework.Framework, mounter *v1.Pod) string {
	GinkgoHelper()
	volumesDir := filepath.Join(containerKubeletPath, "pods", string(mounter.UID), "volumes")
	for _, v := range mounter.Spec.Volumes {
		if v.Name != cacheVolumeNameInPodSpec {
			continue
		}
		switch {
		case v.EmptyDir != nil:
			// Disk and tmpfs mediums share one path on the node.
			return filepath.Join(volumesDir, emptyDirVolumesSubdir, cacheVolumeNameInPodSpec)
		case v.Ephemeral != nil:
			pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(mounter.Namespace).Get(ctx,
				mounter.Name+"-"+cacheVolumeNameInPodSpec, metav1.GetOptions{})
			framework.ExpectNoError(err, "getting the cache claim of mounter pod %s", mounter.Name)
			Expect(pvc.Spec.VolumeName).NotTo(BeEmpty(), "the cache claim of mounter pod %s is not bound", mounter.Name)
			pv, err := f.ClientSet.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
			framework.ExpectNoError(err, "getting the cache volume %s of mounter pod %s", pvc.Spec.VolumeName, mounter.Name)
			switch {
			case pv.Spec.CSI != nil:
				// Only the CSI plugin adds "/mount" under the PV directory.
				return filepath.Join(volumesDir, csiVolumesSubdir, pv.Name, "mount")
			case pv.Spec.Local != nil:
				return filepath.Join(volumesDir, localVolumesSubdir, pv.Name)
			case pv.Spec.NFS != nil:
				return filepath.Join(volumesDir, nfsVolumesSubdir, pv.Name)
			}
			Fail(fmt.Sprintf("cache volume %s of mounter pod %s has an unsupported source: want csi, local or nfs", pv.Name, mounter.Name))
			return ""
		}
	}
	Fail(fmt.Sprintf("mounter pod %s has no %q volume, so it has no cache", mounter.Name, cacheVolumeNameInPodSpec))
	return ""
}

// pathKindOnNode reports "directory", "file" or "absent" for a host path, read from csi-node.
func pathKindOnNode(ctx context.Context, f *framework.Framework, mounter *v1.Pod, path string) string {
	GinkgoHelper()
	out, err := runOnNode(ctx, f, mounter, pathKindCmd(path))
	framework.ExpectNoError(err, "checking %s from the csi-node pod on node %s", path, mounter.Spec.NodeName)
	return out
}

func runOnNode(ctx context.Context, f *framework.Framework, mounter *v1.Pod, cmd string) (string, error) {
	stdout, err := execInCSINodePod(ctx, f, mounter.Spec.NodeName, cmd)
	if err != nil {
		return "", fmt.Errorf("%q on node %s: %w", cmd, mounter.Spec.NodeName, err)
	}
	return strings.TrimSpace(stdout), nil
}

// TODO: delete everything below when #968 lands; it is copied from that PR's util.go, which declares all of it.

// The csi-node DaemonSet, which the per-mount cache paths are read from.
const csiNodePodLabel = "app=s3-csi-node"

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
