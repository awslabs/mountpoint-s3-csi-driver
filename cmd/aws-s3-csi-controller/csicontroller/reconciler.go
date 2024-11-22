package csicontroller

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

const debugLevel = int(zapcore.DebugLevel)

const mountpointCSIDriverName = "s3.csi.aws.com"

// A Reconciler reconciles Mountpoint Pods by watching other workload Pods thats using S3 CSI Driver.
type Reconciler struct {
	mountpointPodConfig  mppod.Config
	mountpointPodCreator *mppod.Creator

	client.Client
}

// NewReconciler returns a new reconciler created from `client` and `podConfig`.
func NewReconciler(client client.Client, podConfig mppod.Config) *Reconciler {
	creator := mppod.NewCreator(podConfig)
	return &Reconciler{Client: client, mountpointPodConfig: podConfig, mountpointPodCreator: creator}
}

// SetupWithManager configures reconciler to run with given `mgr`.
// It automatically configures reconciler to reconcile Pods in the cluster.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&corev1.Pod{}).
		Complete(r)
}

// Reconcile reconciles either a Mountpoint- or a workload-Pod.
//
// For Mountpoint Pods, it deletes completed Pods and logs each status change.
// For workload Pods, it decides if it needs to spawn a Mountpoint Pod to provide a volume for the workload Pod.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	err := r.Get(ctx, req.NamespacedName, pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Pod not found - ignoring")
			return reconcile.Result{}, nil
		}
		log.Error(err, "Failed to get Pod")
		return reconcile.Result{}, err
	}

	if r.isMountpointPod(pod) {
		return r.reconcileMountpointPod(ctx, pod)
	}

	return r.reconcileWorkloadPod(ctx, pod)
}

// reconcileMountpointPod reconciles given Mountpoint `pod`, and deletes it if its completed.
func (r *Reconciler) reconcileMountpointPod(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("mountpointPod", pod.Name)

	switch pod.Status.Phase {
	case corev1.PodPending:
		log.V(debugLevel).Info("Pod pending to be scheduled")
	case corev1.PodRunning:
		log.V(debugLevel).Info("Pod is running")
	case corev1.PodSucceeded:
		err := r.Delete(ctx, pod)
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete succeeded Pod")
			return reconcile.Result{}, err
		}
		log.Info("Pod succeeded and successfully deleted")
	case corev1.PodFailed:
		// TODO: We should probably delete failed Pods after some time to trigger a retry on the whole operation.
		// 		 Maybe just returning a `reconcile.Result{RequeueAfter: ...}`
		// 	     and deleting in next cycle would be a good way?
		log.Info("Pod failed", "reason", pod.Status.Reason)
	}

	return reconcile.Result{}, nil
}

// reconcileWorkloadPod reconciles given workload `pod` to spawn a Mountpoint Pod to provide a volume for it if needed.
func (r *Reconciler) reconcileWorkloadPod(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("pod", types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})

	if pod.Status.Phase != corev1.PodPending {
		log.V(debugLevel).Info("Pod is not pending - ignoring", "status", pod.Status.Phase)
		return reconcile.Result{}, nil
	}

	if pod.Spec.NodeName == "" {
		log.V(debugLevel).Info("Pod is not scheduled to a node yet - ignoring")
		return reconcile.Result{}, nil
	}

	if len(pod.Spec.Volumes) == 0 {
		log.V(debugLevel).Info("Pod has no volumes - ignoring")
		return reconcile.Result{}, nil
	}

	var requeue bool
	var errs []error

	for _, vol := range pod.Spec.Volumes {
		podPVC := vol.PersistentVolumeClaim
		if podPVC == nil {
			continue
		}

		// If PVC has no bound PVs yet, `getBoundPVForPodClaim` will return `errPVCIsNotBoundToAPV`.
		// In this case we'll just return `reconcile.Result{Requeue: true}` here, which will bubble up to the
		// original `Reconcile` call and will cause a retry for this Pod with an exponential backoff.
		pvc, pv, err := r.getBoundPVForPodClaim(ctx, pod, podPVC)
		if err != nil {
			if errors.Is(err, errPVCIsNotBoundToAPV) {
				requeue = true
			} else {
				errs = append(errs, err)
			}
			continue
		}

		csiSpec := extractCSISpecFromPV(pv)
		if csiSpec == nil {
			continue
		}

		log.V(debugLevel).Info("Found bound PV for PVC", "pvc", pvc.Name, "volumeName", pv.Name)

		err = r.spawnMountpointPodIfNeeded(ctx, pod, pvc, pv, csiSpec)
		if err != nil {
			errs = append(errs, err)
			continue
		}
	}

	return reconcile.Result{Requeue: requeue}, errors.Join(errs...)
}

// spawnMountpointPodIfNeeded spawns a new Mountpoint Pod for given `pod` and volume
// if there isn't already existing Mountpoint Pod.
func (r *Reconciler) spawnMountpointPodIfNeeded(
	ctx context.Context,
	pod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	csiSpec *corev1.CSIPersistentVolumeSource,
) error {
	mpPodName := mppod.MountpointPodNameFor(string(pod.UID), pvc.Spec.VolumeName)

	log := logf.FromContext(ctx).WithValues(
		"pod", types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		"mountpointPod", mpPodName,
		"pvc", pvc.Name, "volumeName", pv.Name)

	err := r.Get(ctx, types.NamespacedName{Namespace: r.mountpointPodConfig.Namespace, Name: mpPodName}, &corev1.Pod{})
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to get Mountpoint Pod")
		return err
	}

	isMpExists := err == nil
	if isMpExists {
		log.V(debugLevel).Info("Mountpoint Pod already exists - ignoring")
		return nil
	}

	if err := r.spawnMountpointPod(ctx, pod, pvc, pv, csiSpec, mpPodName); err != nil {
		log.Error(err, "Failed to spawn Mountpoint Pod")
	}
	return nil
}

// spawnMountpointPod spawns a new Mountpoint Pod for given `pod` and volume.
// The Mountpoint Pod will be spawned into the same node as `pod`, which then the mount operation
// will be continued by the CSI Driver Node component in that node.
func (r *Reconciler) spawnMountpointPod(
	ctx context.Context,
	pod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	_ *corev1.CSIPersistentVolumeSource,
	name string,
) error {
	log := logf.FromContext(ctx).WithValues(
		"pod", types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		"mountpointPod", name,
		"pvc", pvc.Name, "volumeName", pv.Name)

	log.Info("Spawning Mountpoint Pod")

	mpPod := r.mountpointPodCreator.Create(pod, pvc)
	if mpPod.Name != name {
		err := fmt.Errorf("Mountpoint Pod name mismatch %s vs %s", mpPod.Name, name)
		log.Error(err, "Name mismatch on Mountpoint Pod")
		return err
	}

	err := r.Create(ctx, mpPod)
	if err != nil {
		log.Error(err, "Failed to create Mountpoint Pod")
		return err
	}

	log.Info("Mountpoint Pod spawned", "mountpointPodUID", mpPod.UID)
	return nil
}

// errPVCIsNotBoundToAPV is returned when given PVC is not bound to a PV yet.
// This is not a terminal error - as PVCs can be bound to PVs dynamically - and just a transient error
// to be retried later.
var errPVCIsNotBoundToAPV = errors.New("PVC is not bound to a PV yet")

// getBoundPVForPodClaim tries to find bound PV and PVC from given `claim`.
// It `errPVCIsNotBoundToAPV` if PVC is not bound to a PV yet to be eventually retried.
func (r *Reconciler) getBoundPVForPodClaim(
	ctx context.Context,
	pod *corev1.Pod,
	claim *corev1.PersistentVolumeClaimVolumeSource,
) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	log := logf.FromContext(ctx).WithValues("pod", types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, "pvc", claim.ClaimName)

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: claim.ClaimName}, pvc)
	if err != nil {
		log.Error(err, "Failed to get PVC for Pod")
		return nil, nil, fmt.Errorf("Failed to get PVC for Pod: %w", err)
	}

	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		log.V(debugLevel).Info("PVC is not bound to a PV yet or has a empty volume name - ignoring",
			"status", pvc.Status.Phase,
			"volumeName", pvc.Spec.VolumeName)
		return nil, nil, errPVCIsNotBoundToAPV
	}

	pv := &corev1.PersistentVolume{}
	err = r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv)
	if err != nil {
		log.Error(err, "Failed to get PV bound to PVC", "volumeName", pvc.Spec.VolumeName)
		return nil, nil, fmt.Errorf("Failed to get PV bound to PVC: %w", err)
	}

	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name != pvc.Name {
		log.Info("Found the PV but its `ClaimRef` is not bound to the PVC", "volumeName", pvc.Spec.VolumeName)
		return nil, nil, errors.New("The PV has a different `ClaimRef` than the PVC")
	}

	return pvc, pv, nil
}

// isMountpointPod returns whether given `pod` is a Mountpoint Pod.
// It currently checks namespace of `pod`.
func (r *Reconciler) isMountpointPod(pod *corev1.Pod) bool {
	// TODO: Do we need to perform any additional check here?
	return pod.Namespace == r.mountpointPodConfig.Namespace
}

// extractCSISpecFromPV tries to extract `CSIPersistentVolumeSource` from given `pv`.
// It returns nil if the CSI Driver used in the `pv` is not S3 CSI Driver.
func extractCSISpecFromPV(pv *corev1.PersistentVolume) *corev1.CSIPersistentVolumeSource {
	csi := pv.Spec.CSI
	if csi == nil || csi.Driver != mountpointCSIDriverName {
		return nil
	}
	return csi
}
