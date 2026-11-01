#!/usr/bin/env bash
# EKS Auto Mode profile, sourced by `run.sh`.
#
# EKS Auto Mode provisions every node: the built-in NodePools' for the cluster's add-ons and other pods, and the
# NodePool in `nodepools.yaml` for the pods targeting it. Pods can't reach IMDS on its nodes, so the driver and the
# CloudWatch agent get their credentials from EKS Pod Identity (see `eksctl-patch.json`).

set -euox pipefail

function profile_setup() {
  $KUBECTL_BIN apply --kubeconfig "${KUBECONFIG}" -f "${PROFILE_DIR}/nodepools.yaml"
}

# Removes the NodePool's nodes before the cluster is deleted.
function profile_teardown() {
  # Best effort: whatever is left blocks the deletion, which then reports it.
  set +e
  aws eks update-kubeconfig --name "${CLUSTER_NAME}" --region "${REGION}" --kubeconfig "${KUBECONFIG}"
  $KUBECTL_BIN delete nodepools s3-csi-e2e-default --ignore-not-found --timeout=2m --kubeconfig "${KUBECONFIG}"
  $KUBECTL_BIN wait --for=delete nodeclaims -l e2e.s3.csi.aws.com/pool --timeout=10m --kubeconfig "${KUBECONFIG}"
  set -e
}

# Prints what Auto Mode says about the nodes it provisions, when the tests fail. Its node provisioning runs outside of
# the cluster, so there are no logs to print.
function profile_print_info() {
  $KUBECTL_BIN get nodepools,nodeclaims -o wide --kubeconfig "${KUBECONFIG}"
}
