package csicontroller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	crdv1 "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/go-logr/logr"
)

const debugLevel = 4

const mountpointCSIDriverName = "s3.csi.aws.com"
const defaultServiceAccount = "default"

const (
	AnnotationServiceAccountRole = "eks.amazonaws.com/role-arn"
	LabelCSIDriverVersion        = "s3.csi.aws.com/created-by-csi-driver-version"
)

// A Reconciler reconciles Mountpoint Pods by watching other workload Pods thats using S3 CSI Driver.
type Reconciler struct {
	mountpointPodConfig  mppod.Config
	mountpointPodCreator *mppod.Creator
	s3paExpectations     *Expectations

	client.Client
}

// NewReconciler returns a new reconciler created from `client` and `podConfig`.
func NewReconciler(client client.Client, podConfig mppod.Config) *Reconciler {
	creator := mppod.NewCreator(podConfig)
	return &Reconciler{Client: client, mountpointPodConfig: podConfig, mountpointPodCreator: creator, s3paExpectations: NewExpectations()}
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
		// This is not an error situation as sometimes we schedule retries for `req`s,
		// and they might got deleted once we try to re-process them again.
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
		err := r.deleteMountpointPod(ctx, pod)
		if err != nil {
			log.Error(err, "Failed to delete succeeded Pod")
			return reconcile.Result{}, err
		}
		log.Info("Pod succeeded and successfully deleted")
	case corev1.PodFailed:
		// TODO: We should probably delete failed Pods after some time to trigger a retry on the whole operation.
		//       Maybe just returning a `reconcile.Result{RequeueAfter: ...}`
		//       and deleting in next cycle would be a good way?
		log.Info("Pod failed", "reason", pod.Status.Reason)
	}

	return reconcile.Result{}, nil
}

// reconcileWorkloadPod reconciles given workload `pod` to spawn a Mountpoint Pod to provide a volume for it if needed.
func (r *Reconciler) reconcileWorkloadPod(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("pod", types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})

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

		needsRequeue, err := r.spawnOrDeleteMountpointPodIfNeeded(ctx, pod, pvc, pv, csiSpec)
		requeue = requeue || needsRequeue
		if err != nil {
			errs = append(errs, err)
			continue
		}
	}

	return reconcile.Result{Requeue: requeue}, errors.Join(errs...)
}

// spawnOrDeleteMountpointPodIfNeeded spawns or deletes existing Mountpoint Pod for given `workloadPod` and volume if needed.
//
// If `workloadPod` is `Pending` and without any associated Mountpoint Pod, a new Mountpoint Pod will be created for it to provide volume.
//
// If `workloadPod` is `Pending` and scheduled for termination (i.e., `DeletionTimestamp` is non-nil), and there is an existing Mountpoint Pod for it,
// the Mountpoint Pod will be scheduled for termination as well. This is because if `workloadPod` never transition into its `Running` state,
// the Mountpoint Pod might never got a successful mount operation, and thus it might never get unmount operation to cleanly exit
// and might hang there until it reaches its timeout. We just terminate it in this case to prevent unnecessary waits.
func (r *Reconciler) spawnOrDeleteMountpointPodIfNeeded(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	csiSpec *corev1.CSIPersistentVolumeSource,
) (bool, error) {
	workloadUID := string(workloadPod.UID)
	roleArn, err := r.findIRSAServiceAccountRole(ctx, workloadPod)
	if err != nil {
		return false, err
	}
	fieldFilters := r.buildFieldFilters(workloadPod, pv, roleArn)
	log := r.setupLogger(ctx, workloadPod, pvc, pv, workloadUID, fieldFilters)

	s3paList, err := r.getExistingS3PodAttachments(ctx, fieldFilters)
	if err != nil {
		return false, err
	}

	if !isPodActive(workloadPod) {
		return r.handleInactivePod(ctx, workloadPod, s3paList, workloadUID, log)
	}

	if len(s3paList.Items) == 1 {
		return r.handleExistingS3PodAttachment(ctx, s3paList, workloadUID, fieldFilters, log)
	}

	return r.handleNewS3PodAttachment(ctx, workloadPod, pv, fieldFilters, log)
}

func (r *Reconciler) setupLogger(ctx context.Context, workloadPod *corev1.Pod, pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, workloadUID string, fieldFilters client.MatchingFields) logr.Logger {
	logger := logf.FromContext(ctx).WithValues(
		"workloadPod", types.NamespacedName{Namespace: workloadPod.Namespace, Name: workloadPod.Name},
		"pvc", pvc.Name,
		"workloadUID", workloadUID,
	)

	var keyValues []interface{}
	for k, v := range fieldFilters {
		keyValues = append(keyValues, k, v)
	}

	if len(keyValues) > 0 {
		logger = logger.WithValues(keyValues...)
	}

	return logger
}

func (r *Reconciler) buildFieldFilters(workloadPod *corev1.Pod, pv *corev1.PersistentVolume, roleArn string) client.MatchingFields {
	authSource := r.getAuthSource(pv)
	fsGroup := r.getFSGroup(workloadPod)

	fieldFilters := client.MatchingFields{
		crdv1.FieldNodeName:             workloadPod.Spec.NodeName,
		crdv1.FieldPersistentVolumeName: pv.Name,
		crdv1.FieldVolumeID:             pv.Spec.CSI.VolumeHandle,
		crdv1.FieldMountOptions:         strings.Join(pv.Spec.MountOptions, ","),
		crdv1.FieldWorkloadFSGroup:      fsGroup,
		crdv1.FieldAuthenticationSource: authSource,
	}

	if authSource == "pod" {
		fieldFilters[crdv1.FieldWorkloadNamespace] = workloadPod.Namespace
		fieldFilters[crdv1.FieldWorkloadServiceAccountName] = getServiceAccountName(workloadPod)
		fieldFilters[crdv1.FieldWorkloadServiceAccountIAMRoleARN] = roleArn
	}

	return fieldFilters
}

func (r *Reconciler) getAuthSource(pv *corev1.PersistentVolume) string {
	volumeAttributes := mppod.ExtractVolumeAttributes(pv)
	authSource := volumeAttributes[volumecontext.AuthenticationSource]
	if authSource == "" {
		return "driver"
	}
	return authSource
}

func (r *Reconciler) getFSGroup(workloadPod *corev1.Pod) string {
	if workloadPod.Spec.SecurityContext.FSGroup != nil {
		return strconv.FormatInt(*workloadPod.Spec.SecurityContext.FSGroup, 10)
	}
	return ""
}

func (r *Reconciler) getExistingS3PodAttachments(ctx context.Context, fieldFilters client.MatchingFields) (*crdv1.MountpointS3PodAttachmentList, error) {
	s3paList := &crdv1.MountpointS3PodAttachmentList{}
	if err := r.List(ctx, s3paList, fieldFilters); err != nil {
		return nil, err
	}

	if len(s3paList.Items) > 1 {
		return nil, fmt.Errorf("found %d MountpointS3PodAttachments instead of 1", len(s3paList.Items))
	}

	return s3paList, nil
}

func (r *Reconciler) handleInactivePod(ctx context.Context, workloadPod *corev1.Pod, s3paList *crdv1.MountpointS3PodAttachmentList, workloadUID string, log logr.Logger) (bool, error) {
	if len(s3paList.Items) != 1 {
		log.Info("Workload pod is not active. Did not find any MountpointS3PodAttachments.")
		return false, nil
	}

	return r.removeWorkloadFromS3PodAttachment(ctx, &s3paList.Items[0], workloadUID, log)
}

func (r *Reconciler) handleExistingS3PodAttachment(ctx context.Context, s3paList *crdv1.MountpointS3PodAttachmentList, workloadUID string, fieldFilters client.MatchingFields, log logr.Logger) (bool, error) {
	s3pa := &s3paList.Items[0]

	if r.s3paExpectations.IsPending(fieldFilters) {
		log.Info("MountpointS3PodAttachment creation is pending, removing from pending")
		r.s3paExpectations.Clear(fieldFilters)
	}

	if s3paContainsWorkload(s3pa, workloadUID) {
		log.Info("MountpointS3PodAttachment already has this workload UID")
		return false, nil
	}

	return r.addWorkloadToS3PodAttachment(ctx, s3pa, workloadUID, log)
}

func (r *Reconciler) addWorkloadToS3PodAttachment(ctx context.Context, s3pa *crdv1.MountpointS3PodAttachment, workloadUID string, log logr.Logger) (bool, error) {
	log.Info("Adding workload UID to MountpointS3PodAttachment", "workloadUID", workloadUID)

	for key := range s3pa.Spec.MountpointS3PodToWorkloadPodUIDs {
		s3pa.Spec.MountpointS3PodToWorkloadPodUIDs[key] = append(s3pa.Spec.MountpointS3PodToWorkloadPodUIDs[key], workloadUID)
		break
	}

	err := r.Update(ctx, s3pa)
	if apierrors.IsConflict(err) {
		log.Info("Failed to update MountpointS3PodAttachment - resource conflict - requeue", "workloadUID", workloadUID)
		return true, nil
	}

	return false, nil
}

func (r *Reconciler) removeWorkloadFromS3PodAttachment(ctx context.Context, s3pa *crdv1.MountpointS3PodAttachment, workloadUID string, log logr.Logger) (bool, error) {
	// Remove workload UID from mountpoint pods
	for mpPodName, uids := range s3pa.Spec.MountpointS3PodToWorkloadPodUIDs {
		filteredUIDs := []string{}
		found := false
		for _, uid := range uids {
			if uid == workloadUID {
				found = true
				continue
			}
			filteredUIDs = append(filteredUIDs, uid)
		}
		if found {
			s3pa.Spec.MountpointS3PodToWorkloadPodUIDs[mpPodName] = filteredUIDs
			err := r.Update(ctx, s3pa)
			if apierrors.IsConflict(err) {
				log.Info("Failed to remove workload pod UID from existing MountpointS3PodAttachment due to resource conflict, requeueing")
				return true, nil
			}
			log.Info("Successfully removed workload pod UID from MountpointS3PodAttachment")
			break
		}
	}

	// Remove Mountpoint pods with zero workloads
	for mpPodName, uids := range s3pa.Spec.MountpointS3PodToWorkloadPodUIDs {
		if len(uids) == 0 {
			log.Info("Mountpoint pod has zero workload UIDs. Will remove it from MountpointS3PodAttachment",
				"mountpointPodName", mpPodName)
			delete(s3pa.Spec.MountpointS3PodToWorkloadPodUIDs, mpPodName)
			err := r.Update(ctx, s3pa)
			if apierrors.IsConflict(err) {
				log.Info("Failed to remove Mountpoint pod from MountpointS3PodAttachment due to resource conflict, requeueing",
					"mountpointPodName", mpPodName)
				return true, nil
			}
		}
	}

	// Delete MountpointS3PodAttachment if map is empty
	if len(s3pa.Spec.MountpointS3PodToWorkloadPodUIDs) == 0 {
		log.Info("MountpointS3PodAttachment has zero Mountpoint Pods. Will delete it")
		err := r.Delete(ctx, s3pa)
		if apierrors.IsConflict(err) {
			log.Info("Failed to delete MountpointS3PodAttachment due to resource conflict, requeueing")
			return true, nil
		}
	}

	return false, nil
}

func (r *Reconciler) handleNewS3PodAttachment(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	fieldFilters client.MatchingFields,
	log logr.Logger,
) (bool, error) {
	if r.s3paExpectations.IsPending(fieldFilters) {
		log.Info("MountpointS3PodAttachment creation is pending, requeueing")
		return true, nil
	}

	if err := r.createS3PodAttachmentWithMPPod(ctx, workloadPod, pv, log); err != nil {
		return false, err
	}

	r.s3paExpectations.SetPending(fieldFilters)
	return true, nil
}

func (r *Reconciler) createS3PodAttachmentWithMPPod(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	log logr.Logger,
) error {
	authSource := r.getAuthSource(pv)
	mpPodName, err := r.spawnMountpointPod(ctx, workloadPod, pv, log)
	if err != nil {
		log.Error(err, "Failed to spawn Mountpoint Pod")
		return err
	}

	fsGroup := ""
	if workloadPod.Spec.SecurityContext.FSGroup != nil {
		fsGroup = strconv.FormatInt(*workloadPod.Spec.SecurityContext.FSGroup, 10)
	}
	s3pa := &crdv1.MountpointS3PodAttachment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "s3pa-",
			Labels: map[string]string{
				LabelCSIDriverVersion: r.mountpointPodConfig.CSIDriverVersion,
			},
		},
		Spec: crdv1.MountpointS3PodAttachmentSpec{
			NodeName:             workloadPod.Spec.NodeName,
			PersistentVolumeName: pv.Name,
			VolumeID:             pv.Spec.CSI.VolumeHandle,
			MountOptions:         strings.Join(pv.Spec.MountOptions, ","),
			WorkloadFSGroup:      fsGroup,
			AuthenticationSource: authSource,
			MountpointS3PodToWorkloadPodUIDs: map[string][]string{
				mpPodName: {string(workloadPod.UID)},
			},
		},
	}
	if authSource == "pod" {
		s3pa.Spec.WorkloadNamespace = workloadPod.Namespace
		s3pa.Spec.WorkloadServiceAccountName = getServiceAccountName(workloadPod)

		roleARN, err := r.findIRSAServiceAccountRole(ctx, workloadPod)
		if err != nil {
			return err
		}
		s3pa.Spec.WorkloadServiceAccountIAMRoleARN = roleARN
	}

	err = r.Create(ctx, s3pa)
	if err != nil {
		log.Error(err, "Failed to create MountpointS3PodAttachment")
		return err
	}

	log.Info("MountpointS3PodAttachment is created", "s3paName", s3pa.Name)
	return nil
}

// spawnMountpointPod spawns a new Mountpoint Pod for given `workloadPod` and volume.
// The Mountpoint Pod will be spawned into the same node as `workloadPod`, which then the mount operation
// will be continued by the CSI Driver Node component in that node.
func (r *Reconciler) spawnMountpointPod(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	log logr.Logger,
) (string, error) {
	log.Info("Spawning Mountpoint Pod")

	mpPod := r.mountpointPodCreator.Create(workloadPod.Spec.NodeName, pv)

	err := r.Create(ctx, mpPod)
	if err != nil {
		return "", err
	}

	log.Info("Mountpoint Pod spawned", "mountpointPodName", mpPod.Name)
	return mpPod.Name, nil
}

// deleteMountpointPod deletes given `mountpointPod`.
// It does not return an error if `mountpointPod` does not exists in the control plane.
func (r *Reconciler) deleteMountpointPod(ctx context.Context, mountpointPod *corev1.Pod) error {
	log := logf.FromContext(ctx).WithValues("mountpointPod", mountpointPod.Name)

	err := r.Delete(ctx, mountpointPod)
	if err == nil {
		log.Info("Mountpoint Pod deleted")
		return nil
	}

	if apierrors.IsNotFound(err) {
		log.Info("Mountpoint Pod has been deleted already")
		return nil
	}

	log.Error(err, "Failed to delete Mountpoint Pod")
	return err
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

func (r *Reconciler) findIRSAServiceAccountRole(ctx context.Context, pod *corev1.Pod) (string, error) {
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: getServiceAccountName(pod)}, sa)
	if err != nil {
		return "", fmt.Errorf("Failed to find workload pod's service account %s", getServiceAccountName(pod))
	}

	return sa.Annotations[AnnotationServiceAccountRole], nil
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

// isPodActive returns whether given Pod is active and not in the process of termination.
// Copied from https://github.com/kubernetes/kubernetes/blob/8770bd58d04555303a3a15b30c245a58723d0f4a/pkg/controller/controller_utils.go#L1009-L1013.
func isPodActive(p *corev1.Pod) bool {
	return corev1.PodSucceeded != p.Status.Phase &&
		corev1.PodFailed != p.Status.Phase &&
		p.DeletionTimestamp == nil
}

func s3paContainsWorkload(s3pa *crdv1.MountpointS3PodAttachment, workloadUID string) bool {
	for _, workloads := range s3pa.Spec.MountpointS3PodToWorkloadPodUIDs {
		for _, workload := range workloads {
			if workload == workloadUID {
				return true
			}
		}
	}
	return false
}

// getServiceAccountName returns the pod's service account name or "default" if not specified
func getServiceAccountName(pod *corev1.Pod) string {
	if pod.Spec.ServiceAccountName != "" {
		return pod.Spec.ServiceAccountName
	}
	return defaultServiceAccount
}
