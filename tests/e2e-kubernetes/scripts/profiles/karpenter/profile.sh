#!/usr/bin/env bash
# Karpenter profile, sourced by `run.sh`.
#
# eksctl installs Karpenter (the version in `eksctl-patch.json`, which must support the cluster's Kubernetes version:
# https://karpenter.sh/docs/upgrading/compatibility/) on the `system` nodegroup, and Karpenter provisions the nodes of
# the NodePool in `nodepools.yaml` for the pods targeting it. The driver gets its credentials from EKS Pod Identity.

set -euox pipefail

function profile_setup() {
  NODE_ROLE_NAME=$(karpenter_node_role_name)
  CLUSTER_NAME=${CLUSTER_NAME} NODE_ROLE_NAME=${NODE_ROLE_NAME} envsubst '${CLUSTER_NAME} ${NODE_ROLE_NAME}' < "${PROFILE_DIR}/nodepools.yaml" | \
    $KUBECTL_BIN apply --kubeconfig "${KUBECONFIG}" -f -
}

# Removes what Karpenter created outside of the cluster's CloudFormation stacks, which would otherwise keep them from
# being deleted: its instances keep the VPC in use, and its instance profile the `system` nodegroup's role.
function profile_teardown() {
  # Best effort: whatever is left blocks the deletion, which then reports it.
  set +e
  aws eks update-kubeconfig --name "${CLUSTER_NAME}" --region "${REGION}" --kubeconfig "${KUBECONFIG}"
  # Karpenter drains and terminates the nodes,
  $KUBECTL_BIN delete nodepools --all --timeout=2m --kubeconfig "${KUBECONFIG}"
  $KUBECTL_BIN delete nodeclaims --all --wait=false --kubeconfig "${KUBECONFIG}"
  $KUBECTL_BIN wait --for=delete nodeclaims --all --timeout=10m --kubeconfig "${KUBECONFIG}"
  # and deletes the EC2NodeClass's instance profile.
  $KUBECTL_BIN delete ec2nodeclasses --all --timeout=5m --kubeconfig "${KUBECONFIG}"

  # Clean up anything Karpenter didn't, e.g. because it wasn't running.
  karpenter_terminate_instances
  karpenter_delete_instance_profiles
  set -e
}

function karpenter_node_role_name() {
  aws eks describe-nodegroup --cluster-name "${CLUSTER_NAME}" --region "${REGION}" --nodegroup-name system \
    --query 'nodegroup.nodeRole' --output text | awk -F/ '{print $NF}'
}

function karpenter_terminate_instances() {
  INSTANCE_IDS=$(aws ec2 describe-instances --region "${REGION}" \
    --filters "Name=tag:eks:eks-cluster-name,Values=${CLUSTER_NAME}" "Name=tag-key,Values=karpenter.sh/nodepool" \
      "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId' --output text)
  if [ -n "${INSTANCE_IDS}" ]; then
    aws ec2 terminate-instances --region "${REGION}" --instance-ids ${INSTANCE_IDS}
    aws ec2 wait instance-terminated --region "${REGION}" --instance-ids ${INSTANCE_IDS}
  fi
}

# Karpenter creates its instance profiles under `/karpenter/<region>/<cluster>/`.
function karpenter_delete_instance_profiles() {
  for PROFILE in $(aws iam list-instance-profiles --path-prefix "/karpenter/${REGION}/${CLUSTER_NAME}/" \
      --query 'InstanceProfiles[].InstanceProfileName' --output text); do
    for ROLE in $(aws iam get-instance-profile --instance-profile-name "${PROFILE}" \
        --query 'InstanceProfile.Roles[].RoleName' --output text); do
      aws iam remove-role-from-instance-profile --instance-profile-name "${PROFILE}" --role-name "${ROLE}"
    done
    aws iam delete-instance-profile --instance-profile-name "${PROFILE}"
  done
}
