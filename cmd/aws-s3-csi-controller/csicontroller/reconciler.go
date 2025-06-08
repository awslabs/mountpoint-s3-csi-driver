package csicontroller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	crdv2beta "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2beta"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/go-logr/logr"
)

const debugLevel = 4

const mountpointCSIDriverName = "s3.csi.aws.com"
const defaultServiceAccount = "default"

const (
	AnnotationServiceAccountRole = "eks.amazonaws.com/role-arn"
	LabelCSIDriverVersion        = "s3.csi.aws.com/created-by-csi-driver-version"
)

const (
	Requeue     = true
	DontRequeue = false
)

// A Reconciler reconciles Mountpoint Pods by watching other workload Pods thats using S3 CSI Driver.
type Reconciler struct {
	mountpointPodConfig  mppod.Config
	mountpointPodCreator *mppod.Creator
	s3paExpectations     *expectations
	client.Client
}

// NewReconciler returns a new reconciler created from `client` and `podConfig`.
func NewReconciler(client client.Client, podConfig mppod.Config, log logr.Logger) *Reconciler {
	creator := mppod.NewCreator(podConfig, log)
	return &Reconciler{Client: client, mountpointPodConfig: podConfig, mountpointPodCreator: creator, s3paExpectations: newExpectations()}
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

		needsRequeue, err := r.spawnOrDeleteMountpointPodIfNeeded(ctx, pod, pvc, pv)
		requeue = requeue || needsRequeue
		if err != nil {
			errs = append(errs, err)
			continue
		}
	}

	err := errors.Join(errs...)
	if err != nil {
		return reconcile.Result{}, nil
	}

	return reconcile.Result{Requeue: requeue}, nil
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
) (bool, error) {
	workloadUID := string(workloadPod.UID)
	roleArn, err := r.findIRSAServiceAccountRole(ctx, workloadPod)
	if err != nil {
		return Requeue, err
	}
	fieldFilters := r.buildFieldFilters(workloadPod, pv, roleArn)
	s3pa, err := r.getExistingS3PodAttachment(ctx, fieldFilters)
	if err != nil {
		return Requeue, err
	}
	log := r.setupLogger(ctx, workloadPod, pvc, workloadUID, fieldFilters, s3pa)

	if !isPodActive(workloadPod) {
		return r.handleInactivePod(ctx, s3pa, workloadUID, log)
	}

	if s3pa != nil {
		return r.handleExistingS3PodAttachment(ctx, workloadPod, pv, s3pa, fieldFilters, log)
	} else {
		return r.handleNewS3PodAttachment(ctx, workloadPod, pv, roleArn, fieldFilters, log)
	}
}

// setupLogger creates and configures logger that includes pod namespace/name, PVC name, and workload UID fields.
// If an S3PodAttachment is provided, its name is added. All fieldFilters are appended as additional key-value pairs.
func (r *Reconciler) setupLogger(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	workloadUID string,
	fieldFilters client.MatchingFields,
	s3pa *crdv2beta.MountpointS3PodAttachment,
) logr.Logger {
	logger := logf.FromContext(ctx).WithValues(
		"workloadPod", types.NamespacedName{Namespace: workloadPod.Namespace, Name: workloadPod.Name},
		"pvc", pvc.Name,
		"workloadUID", workloadUID,
	)

	if s3pa != nil {
		logger = logger.WithValues("s3pa", s3pa.Name)
	}

	var keyValues []interface{}
	for k, v := range fieldFilters {
		keyValues = append(keyValues, k, v)
	}

	if len(keyValues) > 0 {
		logger = logger.WithValues(keyValues...)
	}

	return logger
}

// buildFieldFilters build appropriate matching field filters for List operation on MountpointS3PodAttachments
func (r *Reconciler) buildFieldFilters(workloadPod *corev1.Pod, pv *corev1.PersistentVolume, roleArn string) client.MatchingFields {
	authSource := r.getAuthSource(pv)
	fsGroup := r.getFSGroup(workloadPod)

	fieldFilters := client.MatchingFields{
		crdv2beta.FieldNodeName:             workloadPod.Spec.NodeName,
		crdv2beta.FieldPersistentVolumeName: pv.Name,
		crdv2beta.FieldVolumeID:             pv.Spec.CSI.VolumeHandle,
		crdv2beta.FieldMountOptions:         strings.Join(pv.Spec.MountOptions, ","),
		crdv2beta.FieldWorkloadFSGroup:      fsGroup,
		crdv2beta.FieldAuthenticationSource: authSource,
	}

	if authSource == credentialprovider.AuthenticationSourcePod {
		fieldFilters[crdv2beta.FieldWorkloadNamespace] = workloadPod.Namespace
		fieldFilters[crdv2beta.FieldWorkloadServiceAccountName] = getServiceAccountName(workloadPod)
		fieldFilters[crdv2beta.FieldWorkloadServiceAccountIAMRoleARN] = roleArn
	}

	return fieldFilters
}

// getAuthSource returns authentication source from given PV.
// Defaults to `driver` if `authenticationSource` is not found in volume attributes.
func (r *Reconciler) getAuthSource(pv *corev1.PersistentVolume) string {
	volumeAttributes := mppod.ExtractVolumeAttributes(pv)
	authSource := volumeAttributes[volumecontext.AuthenticationSource]
	if authSource == credentialprovider.AuthenticationSourceUnspecified {
		return credentialprovider.AuthenticationSourceDriver
	}
	return authSource
}

// getFSGroup returns the FSGroup value from the pod's security context as a string.
// If FSGroup is not set, it returns an empty string.
func (r *Reconciler) getFSGroup(workloadPod *corev1.Pod) string {
	if workloadPod.Spec.SecurityContext.FSGroup != nil {
		return strconv.FormatInt(*workloadPod.Spec.SecurityContext.FSGroup, 10)
	}
	return ""
}

// getExistingS3PodAttachment retrieves a MountpointS3PodAttachment resource that matches the provided field filters.
// It returns:
// - The matching MountpointS3PodAttachment if exactly one is found
// - nil if no matching resource is found
// - An error if multiple matching resources are found or if the list operation fails
func (r *Reconciler) getExistingS3PodAttachment(ctx context.Context, fieldFilters client.MatchingFields) (*crdv2beta.MountpointS3PodAttachment, error) {
	s3paList := &crdv2beta.MountpointS3PodAttachmentList{}
	if err := r.List(ctx, s3paList, fieldFilters); err != nil {
		return nil, fmt.Errorf("failed to list MountpointS3PodAttachments: %w", err)
	}

	switch len(s3paList.Items) {
	case 0:
		return nil, nil
	case 1:
		return &s3paList.Items[0], nil
	default:
		return nil, fmt.Errorf("found %d MountpointS3PodAttachments when expecting 0 or 1", len(s3paList.Items))
	}
}

// handleInactivePod handles inactive workload pod.
func (r *Reconciler) handleInactivePod(ctx context.Context, s3pa *crdv2beta.MountpointS3PodAttachment, workloadUID string, log logr.Logger) (bool, error) {
	if s3pa == nil {
		log.Info("Workload pod is not active. Did not find any MountpointS3PodAttachments.")
		return DontRequeue, nil
	}

	return r.removeWorkloadFromS3PodAttachment(ctx, s3pa, workloadUID, log)
}

// handleExistingS3PodAttachment handles existing S3 Pod Attachment.
func (r *Reconciler) handleExistingS3PodAttachment(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	s3pa *crdv2beta.MountpointS3PodAttachment,
	fieldFilters client.MatchingFields,
	log logr.Logger,
) (bool, error) {
	if r.s3paExpectations.isPending(fieldFilters) {
		log.Info("MountpointS3PodAttachment creation is pending, removing from pending")
		r.s3paExpectations.clear(fieldFilters)
	}

	if s3paContainsWorkload(s3pa, string(workloadPod.UID)) {
		log.Info("MountpointS3PodAttachment already has this workload UID")
		return DontRequeue, nil
	}

	return r.addWorkloadToS3PodAttachment(ctx, workloadPod, pv, s3pa, log)
}

// addWorkloadToS3PodAttachment adds workload UID to the first suitable Mountpoint Pod in the map.
// If there aren't any suitable Mountpoint Pods, it creates a new one and assign the workload UID to that Mountpoint Pod.
func (r *Reconciler) addWorkloadToS3PodAttachment(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	s3pa *crdv2beta.MountpointS3PodAttachment,
	log logr.Logger,
) (bool, error) {
	log.Info("Adding workload UID to MountpointS3PodAttachment")

	shouldRequeue, err := r.assignWorkloadToAnExistingMountpointPod(ctx, s3pa, string(workloadPod.UID), log)
	if err == nil {
		// Successfully assigned workload to an existing Mountpoint Pod
		return shouldRequeue, nil
	}

	if !errors.Is(err, errNoSuitableMountpointPodForTheWorkload) {
		// We got an error other than there is no suitable Mountpoint Pod for the workload, just propagate it
		return Requeue, err
	}

	// There is no suitable Mountpoint Pod for the workload, we need to create a new one
	mpPod, err := r.spawnMountpointPod(ctx, workloadPod, pv, log)
	if err != nil {
		log.Error(err, "Failed to spawn Mountpoint Pod")
		return Requeue, err
	}
	s3pa.Spec.MountpointS3PodAttachments[mpPod.Name] = []crdv2beta.WorkloadAttachment{
		{
			WorkloadPodUID: string(workloadPod.UID),
			AttachmentTime: metav1.NewTime(time.Now().UTC()),
		},
	}
	err = r.Update(ctx, s3pa)
	if err != nil {
		log.Error(err, "Failed to update MountpointS3PodAttachment, deleting spawned Mountpoint Pod", "mountpointPodName", mpPod.Name)

		// Clean up spawned Mountpoint Pod
		if deleteErr := r.Delete(ctx, mpPod); deleteErr != nil {
			log.Error(deleteErr, "Failed to cleanup Mountpoint Pod after MountpointS3PodAttachment update failure", "mountpointPodName", mpPod.Name)
		} else {
			log.Info("Successfully cleaned up Mountpoint Pod after S3PodAttachment update failure", "mountpointPodName", mpPod.Name)
		}

		if apierrors.IsConflict(err) {
			log.Info("Failed to update MountpointS3PodAttachment - resource conflict - requeue")
			return Requeue, nil
		}

		return Requeue, err
	}

	log.Info("A new Mountpoint Pod is successfully created for the workload and MountpointS3PodAttachment is successfully updated", "mountpointPodName", mpPod.Name)

	return DontRequeue, nil
}

// errNoSuitableMountpointPodForTheWorkload is returned when there isn't any suitable Mountpoint Pod to assign the workload
// to indicate that a new Mountpoint Pod should be created to assign the workload for.
var errNoSuitableMountpointPodForTheWorkload = errors.New("no suitable Mountpoint Pod found for the workload")

// assignWorkloadToAnExistingMountpointPod tries to assign given `workloadUID` to an existing Mountpoint Pod.
// It returns `errNoSuitableMountpointPodForTheWorkload` if there isn't any suitable Mountpoint Pod to assign this new workload.
func (r *Reconciler) assignWorkloadToAnExistingMountpointPod(ctx context.Context, s3pa *crdv2beta.MountpointS3PodAttachment, workloadUID string, log logr.Logger) (bool, error) {
	log.Info("Trying to assign workload to an existing Mountpoint Pod")

	found := false

	for mpPodName := range s3pa.Spec.MountpointS3PodAttachments {
		mpPodLog := log.WithValues("mountpointPodName", mpPodName)
		mpPod, err := r.getMountpointPod(ctx, mpPodName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				mpPodLog.Info("Mountpoint Pod is not found - not suitable for assigning new workload")
				continue
			}
			return Requeue, err
		}

		if !r.shouldAssignNewWorkloadToMountpointPod(mpPod, mpPodLog) {
			mpPodLog.Info("Mountpoint Pod is not suitable for assigning new workload")
			continue
		}

		s3pa.Spec.MountpointS3PodAttachments[mpPodName] = append(s3pa.Spec.MountpointS3PodAttachments[mpPodName], crdv2beta.WorkloadAttachment{
			WorkloadPodUID: workloadUID,
			AttachmentTime: metav1.NewTime(time.Now().UTC()),
		})
		found = true
		mpPodLog.Info("Found a suitable Mountpoint Pod to assign new workload")
		break
	}

	if !found {
		return DontRequeue, errNoSuitableMountpointPodForTheWorkload
	}

	err := r.Update(ctx, s3pa)
	if err != nil {
		if apierrors.IsConflict(err) {
			log.Info("Failed to update MountpointS3PodAttachment - resource conflict - requeue")
			return Requeue, nil
		}
		log.Error(err, "Failed to update MountpointS3PodAttachment")
		return Requeue, err
	}

	return DontRequeue, nil
}

// removeWorkloadFromS3PodAttachment removes workload UID from MountpointS3PodAttachment map.
// It will delete MountpointS3PodAttachment if map becomes empty.
func (r *Reconciler) removeWorkloadFromS3PodAttachment(ctx context.Context, s3pa *crdv2beta.MountpointS3PodAttachment, workloadUID string, log logr.Logger) (bool, error) {
	// Remove workload UID from mountpoint pods
	for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
		filteredUIDs := []crdv2beta.WorkloadAttachment{}
		found := false
		for _, attachment := range attachments {
			if attachment.WorkloadPodUID == workloadUID {
				found = true
				continue
			}
			filteredUIDs = append(filteredUIDs, attachment)
		}
		if found {
			s3pa.Spec.MountpointS3PodAttachments[mpPodName] = filteredUIDs
			err := r.Update(ctx, s3pa)
			if err != nil {
				if apierrors.IsConflict(err) {
					log.Info("Failed to remove workload pod UID from existing MountpointS3PodAttachment due to resource conflict, requeuing")
					return Requeue, nil
				}
				log.Error(err, "Failed to update MountpointS3PodAttachment")
				return Requeue, err
			}
			log.Info("Successfully removed workload pod UID from MountpointS3PodAttachment")
			break
		}
	}

	// Remove Mountpoint pods with zero workloads
	for mpPodName, uids := range s3pa.Spec.MountpointS3PodAttachments {
		if len(uids) == 0 {
			log.Info("Mountpoint pod has zero workload UIDs. Adding "+mppod.AnnotationNeedsUnmount+" annotation",
				"mountpointPodName", mpPodName)
			err := r.addNeedsUnmountAnnotation(ctx, mpPodName, log)
			if err != nil {
				return Requeue, err
			}

			log.Info("Mountpoint pod has zero workload UIDs. Will remove it from MountpointS3PodAttachment",
				"mountpointPodName", mpPodName)
			delete(s3pa.Spec.MountpointS3PodAttachments, mpPodName)
			err = r.Update(ctx, s3pa)
			if err != nil {
				if apierrors.IsConflict(err) {
					log.Info("Failed to remove Mountpoint pod from MountpointS3PodAttachment due to resource conflict, requeuing",
						"mountpointPodName", mpPodName)
					return Requeue, nil
				}
				log.Error(err, "Failed to update MountpointS3PodAttachment")
				return Requeue, err
			}
		}
	}

	// Delete MountpointS3PodAttachment if map is empty
	if len(s3pa.Spec.MountpointS3PodAttachments) == 0 {
		log.Info("MountpointS3PodAttachment has zero Mountpoint Pods. Will delete it")
		err := r.Delete(ctx, s3pa)
		if err != nil {
			if apierrors.IsConflict(err) {
				log.Info("Failed to delete MountpointS3PodAttachment due to resource conflict, requeuing")
				return Requeue, nil
			}
			log.Error(err, "Failed to delete MountpointS3PodAttachment")
			return Requeue, err
		}
	}

	return DontRequeue, nil
}

// handleNewS3PodAttachment handles new S3 pod attachment in case none were found.
func (r *Reconciler) handleNewS3PodAttachment(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	roleArn string,
	fieldFilters client.MatchingFields,
	log logr.Logger,
) (bool, error) {
	if r.s3paExpectations.isPending(fieldFilters) {
		log.Info("MountpointS3PodAttachment creation is pending, requeuing")
		return Requeue, nil
	}

	if isPodRunning(workloadPod) {
		// Kubernetes guarantees that all volumes were successfully mounted when a pod is Running.
		log.Info("Workload pod is already in Running phase and MountpointS3PodAttachment does not exist. " +
			"This means the S3 volume was already mounted by something else (likely systemd-based mount from CSI Driver v1). " +
			"Skipping creation of MountpointS3PodAttachment and Mountpoint Pod.")
		return DontRequeue, nil
	}

	if err := r.createS3PodAttachmentWithMPPod(ctx, workloadPod, pv, roleArn, log); err != nil {
		return Requeue, err
	}

	r.s3paExpectations.setPending(fieldFilters)
	return Requeue, nil
}

// createS3PodAttachmentWithMPPod creates new MountpointS3PodAttachment resource and Mountpoint Pod for given workload and PV.
func (r *Reconciler) createS3PodAttachmentWithMPPod(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	roleArn string,
	log logr.Logger,
) error {
	authSource := r.getAuthSource(pv)
	mpPod, err := r.spawnMountpointPod(ctx, workloadPod, pv, log)
	if err != nil {
		log.Error(err, "Failed to spawn Mountpoint Pod")
		return err
	}
	s3pa := &crdv2beta.MountpointS3PodAttachment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "s3pa-",
			Labels: map[string]string{
				LabelCSIDriverVersion: r.mountpointPodConfig.CSIDriverVersion,
			},
		},
		Spec: crdv2beta.MountpointS3PodAttachmentSpec{
			NodeName:             workloadPod.Spec.NodeName,
			PersistentVolumeName: pv.Name,
			VolumeID:             pv.Spec.CSI.VolumeHandle,
			MountOptions:         strings.Join(pv.Spec.MountOptions, ","),
			WorkloadFSGroup:      r.getFSGroup(workloadPod),
			AuthenticationSource: authSource,
			MountpointS3PodAttachments: map[string][]crdv2beta.WorkloadAttachment{
				mpPod.Name: {{WorkloadPodUID: string(workloadPod.UID), AttachmentTime: metav1.NewTime(time.Now().UTC())}},
			},
		},
	}
	if authSource == credentialprovider.AuthenticationSourcePod {
		s3pa.Spec.WorkloadNamespace = workloadPod.Namespace
		s3pa.Spec.WorkloadServiceAccountName = getServiceAccountName(workloadPod)
		s3pa.Spec.WorkloadServiceAccountIAMRoleARN = roleArn
	}

	err = r.Create(ctx, s3pa)
	if err != nil {
		log.Error(err, "Failed to create MountpointS3PodAttachment")
		if deleteErr := r.Delete(ctx, mpPod); deleteErr != nil {
			log.Error(deleteErr, "Failed to cleanup Mountpoint Pod after MountpointS3PodAttachment creation failure", "mountpointPodName", mpPod.Name)
		} else {
			log.Info("Successfully cleaned up Mountpoint Pod after S3PodAttachment creation failure", "mountpointPodName", mpPod.Name)
		}
		return err
	}

	log.Info("MountpointS3PodAttachment is created", "s3pa", s3pa.Name)
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
) (*corev1.Pod, error) {
	log.Info("Spawning Mountpoint Pod")

	mpPod, err := r.mountpointPodCreator.Create(workloadPod.Spec.NodeName, pv)
	if err != nil {
		log.Error(err, "Failed to create Mountpoint Pod Spec")
		return nil, err
	}

	err = r.Create(ctx, mpPod)
	if err != nil {
		return nil, err
	}

	log.Info("Mountpoint Pod spawned", "mountpointPodName", mpPod.Name)
	return mpPod, nil
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

// getMountpointPod tries to find Mountpoint Pod with given `name`.
func (r *Reconciler) getMountpointPod(ctx context.Context, name string) (*corev1.Pod, error) {
	mpPod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: r.mountpointPodConfig.Namespace, Name: name}, mpPod)
	if err != nil {
		return nil, err
	}
	return mpPod, nil
}

// shouldAssignNewWorkloadToMountpointPod returns whether a new workload should be assigned to the Mountpoint Pod `mpPod`.
func (r *Reconciler) shouldAssignNewWorkloadToMountpointPod(mpPod *corev1.Pod, log logr.Logger) bool {
	if mpPod.Annotations != nil {
		if mpPod.Annotations[mppod.AnnotationNeedsUnmount] == "true" {
			log.Info("Mountpoint Pod is annotated as 'needs-unmount' - not suitable for a new workload")
			return false
		}

		if mpPod.Annotations[mppod.AnnotationNoNewWorkload] == "true" {
			log.Info("Mountpoint Pod is annotated as 'no-new-workload' - not suitable for a new workload")
			return false
		}
	}

	if mpPod.Labels != nil {
		if mpPod.Labels[mppod.LabelCSIDriverVersion] != r.mountpointPodConfig.CSIDriverVersion {
			log.Info("Mountpoint Pod is created with a different CSI Driver version - not suitable for a new workload",
				"mountpointPodCreatedByCSIDriverVersion", mpPod.Labels[mppod.LabelCSIDriverVersion],
				"currentCSIDriverVersion", r.mountpointPodConfig.CSIDriverVersion)
			return false
		}
	}

	return true
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

// findIRSAServiceAccountRole retrieves the IAM role ARN associated with a pod's service account
// through IRSA (IAM Roles for Service Accounts) annotation ("eks.amazonaws.com/role-arn").
//
// Parameters:
//   - ctx: Context for the request
//   - pod: The Kubernetes pod whose service account role should be retrieved
//
// Returns:
//   - string: The IAM role ARN from the service account's annotation, empty string if not found
//   - error: Error if the service account cannot be retrieved
func (r *Reconciler) findIRSAServiceAccountRole(ctx context.Context, pod *corev1.Pod) (string, error) {
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: getServiceAccountName(pod)}, sa)
	if err != nil {
		return "", fmt.Errorf("Failed to find workload pod's service account %s: %w", getServiceAccountName(pod), err)
	}

	if sa.Annotations == nil {
		return "", nil
	}
	return sa.Annotations[AnnotationServiceAccountRole], nil
}

// addNeedsUnmountAnnotation add "s3.csi.aws.com/needs-unmount" to Mountpoint Pod.
// This will trigger CSI Driver Node to cleanly unmount and Mountpoint Pod will become 'Succeeded'.
func (r *Reconciler) addNeedsUnmountAnnotation(ctx context.Context, mpPodName string, log logr.Logger) error {
	// Get the pod
	mpPod, err := r.getMountpointPod(ctx, mpPodName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Failed to find Mountpoint Pod - ignoring")
			return nil
		}
		log.Error(err, "Failed to get Pod")
		return err
	}

	if mpPod.Annotations == nil {
		mpPod.Annotations = make(map[string]string)
	}
	mpPod.Annotations[mppod.AnnotationNeedsUnmount] = "true"

	// Update the pod
	err = r.Update(ctx, mpPod) // TODO: This probably needs to be a patch as we might've get a stale Mountpoint Pod.
	if err != nil {
		log.Error(err, "Failed to update Mountpoint Pod")
		return err
	}

	return nil
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

// isPodRunning returns whether given Pod phase is `Running`.
func isPodRunning(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodRunning
}

// s3paContainsWorkload checks whether MountpointS3PodAttachment has `workloadUID` in it.
func s3paContainsWorkload(s3pa *crdv2beta.MountpointS3PodAttachment, workloadUID string) bool {
	for _, attachments := range s3pa.Spec.MountpointS3PodAttachments {
		for _, attachment := range attachments {
			if attachment.WorkloadPodUID == workloadUID {
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
