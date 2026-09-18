package controller_test

import (
	"context"
	"errors"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/kubectl/pkg/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/awslabs/mountpoint-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
)

// These tests check that leader election allows us to stay consistent with multiple controllers.
// We run with a custom envtest API server to avoid mutating the global one which does not run
// with leader election

// Shortened leader-election timings so the failover spec settles in seconds rather than the
// controller-runtime defaults (15s/10s/2s), keeping the suite fast.
const (
	leaseDuration = 4 * time.Second
	renewDeadline = 3 * time.Second
	retryPeriod   = 1 * time.Second
)

// Leader election and reconciliation are eventually-consistent, so use longer timers.
const (
	electionTimeout  = 30 * time.Second
	reconcileTimeout = 30 * time.Second
	pollInterval     = 200 * time.Millisecond
	// stabiliseWindow is how long we assert the non-leader does NOT duplicate side effects.
	stabiliseWindow = 3 * time.Second
)

const (
	managerAIdentity = "manager-a"
	managerBIdentity = "manager-b"
)

const (
	// the latest a new leader can be elected
	latestAcquire = leaseDuration + 2*retryPeriod

	// the soonest the lease can expire after the leader is cancelled
	expiryFloor = leaseDuration - renewDeadline
	// Defined loosely - this prevents tests from hanging.
	expiryCeiling = 2 * latestAcquire
)

var _ = Describe("Leader Election", func() {
	var (
		env       *envtest.Environment
		restCfg   *rest.Config
		apiClient client.Client
		// ctx is the parent context for every manager a spec starts; cancelling it in AfterEach
		// stops any manager the spec did not stop itself.
		ctx          context.Context
		cancel       context.CancelFunc
		managerDones []<-chan struct{}
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		managerDones = nil

		By("Bootstrapping a dedicated envtest environment for the leader-election spec")
		env, restCfg, apiClient = createClient()
		bootstrapCluster(ctx, apiClient)
	})

	AfterEach(func() {
		By("Tearing down the leader-election envtest environment")
		cancel()
		// Wait for everything to be cancelled before moving to the next test.
		for _, done := range managerDones {
			Eventually(done, electionTimeout, pollInterval).Should(BeClosed())
		}
		Expect(env.Stop()).To(Succeed())
	})

	It("creates a Lease held by exactly one of two managers, and only the leader reconciles", func() {
		_, _, done1 := startManager(ctx, restCfg, managerAIdentity, true)
		_, _, done2 := startManager(ctx, restCfg, managerBIdentity, true)
		managerDones = append(managerDones, done1, done2)

		By("Waiting for one of the two managers to hold the Lease")
		waitForLeaseHolder(ctx, apiClient, managerAIdentity, managerBIdentity)

		By("Scheduling a single workload pod backed by an S3 volume")
		vol := createBoundVolumeWithClient(ctx, apiClient)
		node := generateRandomNodeName()
		scheduleWorkloadWithClient(ctx, apiClient, node, vol)

		By("Asserting the leader creates exactly one S3PA and one Mountpoint Pod")
		validateCreatesWorkloadAndS3PA(ctx, apiClient)
	})

	It("fails over to the standby manager when the leader's context is cancelled", func() {
		_, cancelA, doneA := startManager(ctx, restCfg, managerAIdentity, true)
		_, cancelB, doneB := startManager(ctx, restCfg, managerBIdentity, true)
		managerDones = append(managerDones, doneA, doneB)
		cancels := map[string]context.CancelFunc{managerAIdentity: cancelA, managerBIdentity: cancelB}

		By("Waiting for an initial leader and recording its identity and the transition count")
		initialHolder := waitForLeaseHolder(ctx, apiClient, managerAIdentity, managerBIdentity)
		standby := managerBIdentity
		if initialHolder == managerBIdentity {
			standby = managerAIdentity
		}

		// Verify a transition happens to the standby after cancelling the initial holder.
		transitionsBefore := leaseTransitions(ctx, apiClient)
		By("Cancelling the leader's context to release its Lease (LeaderElectionReleaseOnCancel)")
		cancels[initialHolder]()
		By("Asserting leadership transfers to the named standby")
		waitForLeaseHolder(ctx, apiClient, standby)
		By("Asserting the Lease recorded exactly one new transition")
		Expect(leaseTransitions(ctx, apiClient)).To(Equal(transitionsBefore + 1))

		// Verify it still works properly.
		By("Asserting the new leader resumes reconciliation")
		vol := createBoundVolumeWithClient(ctx, apiClient)
		node := generateRandomNodeName()
		scheduleWorkloadWithClient(ctx, apiClient, node, vol)

		By("Asserting the leader creates exactly one S3PA and one Mountpoint Pod")
		validateCreatesWorkloadAndS3PA(ctx, apiClient)
	})

	It("fails over only after the lease expires when the leader cannot release it", func() {
		_, cancelA, doneA := startManager(ctx, restCfg, managerAIdentity, false)
		_, cancelB, doneB := startManager(ctx, restCfg, managerBIdentity, false)
		managerDones = append(managerDones, doneA, doneB)
		cancels := map[string]context.CancelFunc{managerAIdentity: cancelA, managerBIdentity: cancelB}

		By("Waiting for an initial leader and recording the transition count")
		initialHolder := waitForLeaseHolder(ctx, apiClient, managerAIdentity, managerBIdentity)
		standby := managerBIdentity
		if initialHolder == managerBIdentity {
			standby = managerAIdentity
		}

		// Verify a transition happens to the standby after the initial holder is cancelled.
		transitionsBefore := leaseTransitions(ctx, apiClient)
		By("Cancelling the leader WITHOUT releasing the lease, forcing the standby to wait out expiry")
		cancels[initialHolder]()
		By("Asserting the standby does NOT take over before the lease can expire (lower bound)")
		Consistently(func(g Gomega) {
			g.Expect(leaseHolderIdentity(ctx, apiClient)).To(Equal(initialHolder))
		}, expiryFloor, pollInterval).Should(Succeed())
		By("Asserting the standby then acquires within expiryCeiling (upper bound)")
		Eventually(func(g Gomega) {
			g.Expect(leaseHolderIdentity(ctx, apiClient)).To(Equal(standby), "the standby should acquire the expired lease")
		}, expiryCeiling, pollInterval).Should(Succeed())
		By("Asserting the Lease recorded exactly one new transition")
		Expect(leaseTransitions(ctx, apiClient)).To(Equal(transitionsBefore + 1))

		// Verify it still works properly.
		By("Asserting the new leader resumes reconciliation")
		vol := createBoundVolumeWithClient(ctx, apiClient)
		node := generateRandomNodeName()
		scheduleWorkloadWithClient(ctx, apiClient, node, vol)

		By("Asserting the leader creates exactly one S3PA and one Mountpoint Pod")
		validateCreatesWorkloadAndS3PA(ctx, apiClient)
	})

	It("recovers a duplicate MountpointS3PodAttachment on the leader", func() {
		_, _, done1 := startManager(ctx, restCfg, managerAIdentity, true)
		_, _, done2 := startManager(ctx, restCfg, managerBIdentity, true)
		managerDones = append(managerDones, done1, done2)

		By("Waiting for one of the two managers to hold the Lease")
		waitForLeaseHolder(ctx, apiClient, managerAIdentity, managerBIdentity)

		By("Seeding two empty duplicate S3PAs sharing the field-tuple of the workload to come")
		vol := createBoundVolumeWithClient(ctx, apiClient)
		node := generateRandomNodeName()
		duplicateA := newEmptyS3PodAttachment(node, vol)
		duplicateB := newEmptyS3PodAttachment(node, vol)
		Expect(apiClient.Create(ctx, duplicateA)).To(Succeed())
		Expect(apiClient.Create(ctx, duplicateB)).To(Succeed())
		Expect(listS3PodAttachmentsWithClient(ctx, apiClient)).To(HaveLen(2))

		By("Scheduling a workload so the leader reconciles the duplicated field-tuple")
		pod := scheduleWorkloadWithClient(ctx, apiClient, node, vol)

		By("Asserting the leader collapses the duplicates to exactly one S3PA holding the workload")
		Eventually(func(g Gomega) {
			s3paList := listS3PodAttachmentsWithClient(ctx, apiClient)
			g.Expect(s3paList).To(HaveLen(1))
			g.Expect(findMountpointPodNameForWorkload(&s3paList[0], string(pod.UID))).NotTo(BeEmpty())
		}, reconcileTimeout, pollInterval).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(listS3PodAttachmentsWithClient(ctx, apiClient)).To(HaveLen(1))
		}, stabiliseWindow, pollInterval).Should(Succeed())
	})

	Context("Stale Attachment Cleaner", func() {
		It("should reclaim an S3PodAttachment whose only workload attachment is stale", func() {
			mgr, reconciler := buildManager(restCfg, func(opts *manager.Options) {})
			_, done := runManager(ctx, mgr)
			managerDones = append(managerDones, done)

			cleanerCtx, cancelCleaner := context.WithCancel(ctx)
			defer cancelCleaner()
			cleaner := csicontroller.NewStaleAttachmentCleaner(reconciler, csicontroller.WithCleanupInterval(1*time.Second))
			cleanerDone := make(chan struct{})
			managerDones = append(managerDones, cleanerDone)
			go func() {
				defer GinkgoRecover()
				defer close(cleanerDone)
				Expect(cleaner.Start(cleanerCtx)).To(Succeed())
			}()

			testNode := generateRandomNodeName()
			pv := createBoundVolumeWithClient(ctx, apiClient)

			// A Mountpoint Pod that the stale S3PodAttachment references. It stays Pending so
			// the reconciler does not delete it as a completed Pod, leaving the periodic cleaner
			// as the only component that can act on it.
			mpPod := &corev1.Pod{
				GenerateName: "mp-stale-",
				Namespace:    mountpointNamespace,
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "mountpoint", Image: mountpointImage}},
				},
			}
			Expect(apiClient.Create(ctx, mpPod)).To(Succeed())
			waitForObjectWithClient(ctx, apiClient, mpPod)

			// Create a pod attachment whose only workload reference is already stale.
			s3pa := newEmptyS3PodAttachment(testNode, pv)
			s3pa.Spec.MountpointS3PodAttachments = map[string][]crdv2.WorkloadAttachment{
				mpPod.Name: {{
					WorkloadPodUID: uuid.New().String(),
					AttachmentTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				}},
			}
			Expect(apiClient.Create(ctx, s3pa)).To(Succeed())
			waitForObjectWithClient(ctx, apiClient, s3pa)

			// Verify the unmount annotation gets added by the attachment cleaner
			waitForObjectWithClient(ctx, apiClient, mpPod, func(g Gomega, pod *corev1.Pod) {
				g.Expect(pod.Annotations).To(HaveKeyWithValue(mppod.AnnotationNeedsUnmount, "true"))
			})
			waitForObjectToDisappearWithClient(ctx, apiClient, s3pa)
		})
	})
})

func validateCreatesWorkloadAndS3PA(ctx context.Context, client client.Client) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		g.Expect(listS3PodAttachmentsWithClient(ctx, client)).To(HaveLen(1))
		g.Expect(listMountpointPodsWithClient(ctx, client)).To(HaveLen(1))
	}, reconcileTimeout, pollInterval).Should(Succeed())

	Consistently(func(g Gomega) {
		g.Expect(listS3PodAttachmentsWithClient(ctx, client)).To(HaveLen(1))
		g.Expect(listMountpointPodsWithClient(ctx, client)).To(HaveLen(1))
	}, stabiliseWindow, pollInterval).Should(Succeed())
}

func startManager(parent context.Context, cfg *rest.Config, identity string, releaseOnCancel bool) (manager.Manager, context.CancelFunc, <-chan struct{}) {
	GinkgoHelper()

	mgr, _ := buildManager(cfg, leaderElectionConfigurer(cfg, identity, releaseOnCancel))
	cancel, done := runManager(parent, mgr)
	return mgr, cancel, done
}

func buildManager(cfg *rest.Config, configure func(*manager.Options)) (manager.Manager, *csicontroller.Reconciler) {
	GinkgoHelper()

	opts := manager.Options{
		Scheme: scheme.Scheme,
		// Disable the metrics server; two managers in one process would otherwise contend for the
		// same port.
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Skip the controller runtime name check as we're deliberately running with 2 of the same controller.
		Controller:              config.Controller{SkipNameValidation: new(true)},
		LeaderElectionNamespace: mountpointNamespace,
	}
	configure(&opts)

	mgr, err := manager.New(cfg, opts)
	Expect(err).NotTo(HaveOccurred())
	Expect(crdv2.SetupManagerIndices(mgr)).To(Succeed())

	reconciler := csicontroller.NewReconciler(mgr.GetClient(), reconcilerConfig(), logf.Log)
	Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

	return mgr, reconciler
}

// start a new context to run a manager in. Return a cancellation function, and a channel which closes when complete.
func runManager(parent context.Context, mgr manager.Manager) (context.CancelFunc, <-chan struct{}) {
	GinkgoHelper()

	mgrCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		if err := mgr.Start(mgrCtx); err != nil && !errors.Is(err, context.Canceled) {
			Expect(err).NotTo(HaveOccurred(), "manager exited with an unexpected error")
		}
	}()

	return cancel, done
}

func leaderElectionConfigurer(cfg *rest.Config, identity string, releaseOnCancel bool) func(*manager.Options) {
	GinkgoHelper()

	clientset, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: csicontroller.Name, Namespace: mountpointNamespace},
		Client:     clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}

	return func(opts *manager.Options) {
		csicontroller.ConfigureLeaderElection(opts)
		csicontroller.SetLeaderElectionTimings(opts, leaseDuration, renewDeadline, retryPeriod)
		opts.LeaderElectionReleaseOnCancel = releaseOnCancel
		opts.LeaderElectionResourceLockInterface = lock
	}
}

// leaseHolderIdentity returns the current leaseholder without waiting for it.
func leaseHolderIdentity(ctx context.Context, c client.Client) string {
	GinkgoHelper()
	lease, err := getLease(ctx, c)
	Expect(err).NotTo(HaveOccurred())
	return *lease.Spec.HolderIdentity
}

// waitForLeaseHolder waits until the leader-election Lease is held by a known manager.
func waitForLeaseHolder(ctx context.Context, c client.Client, identities ...string) string {
	GinkgoHelper()

	var holder string
	Eventually(func(g Gomega) {
		lease, err := getLease(ctx, c)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
		g.Expect(*lease.Spec.HolderIdentity).To(BeElementOf(identities), "lease should be held by a known manager")
		holder = *lease.Spec.HolderIdentity
	}, electionTimeout, pollInterval).Should(Succeed())

	return holder
}

// leaseTransitions returns how many times the lease has changed owner.
func leaseTransitions(ctx context.Context, c client.Client) int32 {
	GinkgoHelper()
	lease, err := getLease(ctx, c)
	Expect(err).NotTo(HaveOccurred())
	if lease.Spec.LeaseTransitions == nil {
		return 0
	}
	return *lease.Spec.LeaseTransitions
}

// getLease reads the leader-election Lease created by controller-runtime.
func getLease(ctx context.Context, c client.Client) (*coordinationv1.Lease, error) {
	lease := &coordinationv1.Lease{}
	err := c.Get(ctx, client.ObjectKey{Name: csicontroller.Name, Namespace: mountpointNamespace}, lease)
	return lease, err
}

// create the cluster-scoped fixtures the reconciler needs

// createBoundVolumeWithClient creates a bound PV/PVC pair backed by the S3 CSI driver and returns the PV.
func createBoundVolumeWithClient(ctx context.Context, c client.Client) *corev1.PersistentVolume {
	GinkgoHelper()

	accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	capacity := corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}

	pv := &corev1.PersistentVolume{
		GenerateName: "le-test-pv",
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: "",
			AccessModes:      accessModes,
			Capacity:         capacity,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       s3CSIDriver,
					VolumeHandle: "le-test-csi-volume",
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		GenerateName: "le-test-pvc", Namespace: defaultNamespace,
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: new(""),
			AccessModes:      accessModes,
			Resources:        corev1.VolumeResourceRequirements{Requests: capacity},
		},
	}
	Expect(c.Create(ctx, pv)).To(Succeed())
	Expect(c.Create(ctx, pvc)).To(Succeed())

	// Bind PV and PVC to each other.
	pv.Spec.ClaimRef = &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace}
	Expect(c.Update(ctx, pv)).To(Succeed())
	pv.Status.Phase = corev1.VolumeBound
	Expect(c.Status().Update(ctx, pv)).To(Succeed())

	pvc.Spec.VolumeName = pv.Name
	Expect(c.Update(ctx, pvc)).To(Succeed())
	pvc.Status.Phase = corev1.ClaimBound
	Expect(c.Status().Update(ctx, pvc)).To(Succeed())

	return pv
}

// scheduleWorkloadWithClient creates a workload pod using `pv`'s claim and binds it to `node`.
func scheduleWorkloadWithClient(ctx context.Context, c client.Client, node string, pv *corev1.PersistentVolume) *corev1.Pod {
	GinkgoHelper()

	pod := &corev1.Pod{
		GenerateName: "le-test-pod-", Namespace: defaultNamespace,
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "test-container", Image: defaultContainerImage}},
			Volumes: []corev1.Volume{{
				Name: "vol",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pv.Spec.ClaimRef.Name,
				},
			}},
		},
	}
	Expect(c.Create(ctx, pod)).To(Succeed())

	binding := &corev1.Binding{Target: corev1.ObjectReference{Name: node}}
	Expect(c.SubResource("binding").Create(ctx, pod, binding)).To(Succeed())
	waitForObjectWithClient(ctx, c, pod, func(g Gomega, p *corev1.Pod) {
		g.Expect(p.Spec.NodeName).To(Equal(node))
	})

	return pod
}

func listS3PodAttachmentsWithClient(ctx context.Context, c client.Client) []crdv2.MountpointS3PodAttachment {
	GinkgoHelper()

	list := &crdv2.MountpointS3PodAttachmentList{}
	Expect(c.List(ctx, list)).To(Succeed())
	return list.Items
}

func listMountpointPodsWithClient(ctx context.Context, c client.Client) []corev1.Pod {
	GinkgoHelper()

	list := &corev1.PodList{}
	Expect(c.List(ctx, list, client.InNamespace(mountpointNamespace))).To(Succeed())
	return list.Items
}
