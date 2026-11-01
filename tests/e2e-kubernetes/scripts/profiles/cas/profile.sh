#!/usr/bin/env bash
# Cluster Autoscaler profile, sourced by `run.sh`.
#
# Cluster Autoscaler runs on the `system` nodegroup, and scales the `pool-default` nodegroup in `eksctl-patch.json`
# from zero for the pods targeting it. The driver gets its credentials from EKS Pod Identity.

set -euox pipefail

CLUSTER_AUTOSCALER_CHART_VERSION=9.59.0
# Must have the cluster's Kubernetes minor version.
CLUSTER_AUTOSCALER_VERSION=v1.36.1

function profile_setup() {
  # `startup-taint`: the driver removes the taint once it's ready on the node, so it doesn't keep workloads off the
  #   nodes Cluster Autoscaler would provision.
  # `ignore-daemonsets-utilization`: otherwise the mounter's memory request alone keeps a node above the
  #   utilization threshold for scaling it down.
  # The scale-down delays are shortened so that empty nodes don't linger after the tests, including those that never
  # got ready (with the startup taint, Cluster Autoscaler considers them unready).
  $HELM_BIN upgrade --install cluster-autoscaler cluster-autoscaler \
    --repo https://kubernetes.github.io/autoscaler \
    --version "${CLUSTER_AUTOSCALER_CHART_VERSION}" \
    --namespace kube-system \
    --set fullnameOverride=cluster-autoscaler \
    --set autoDiscovery.clusterName="${CLUSTER_NAME}" \
    --set awsRegion="${REGION}" \
    --set image.tag="${CLUSTER_AUTOSCALER_VERSION}" \
    --set rbac.serviceAccount.create=false \
    --set rbac.serviceAccount.name=cluster-autoscaler \
    --set extraArgs.startup-taint=s3.csi.aws.com/agent-not-ready \
    --set extraArgs.ignore-daemonsets-utilization=true \
    --set extraArgs.scale-down-unneeded-time=1m \
    --set extraArgs.scale-down-delay-after-add=1m \
    --set extraArgs.scale-down-unready-time=2m \
    --wait \
    --kubeconfig "${KUBECONFIG}"
}

# Everything Cluster Autoscaler scales belongs to the cluster's CloudFormation stacks.
function profile_teardown() {
  :
}

# Prints what Cluster Autoscaler says about the nodegroups it scales, when the tests fail.
function profile_print_info() {
  $KUBECTL_BIN get configmap cluster-autoscaler-status -n kube-system -o jsonpath='{.data.status}{"\n"}' \
    --kubeconfig "${KUBECONFIG}"
  $KUBECTL_BIN logs -n kube-system deployment/cluster-autoscaler --tail=200 --kubeconfig "${KUBECONFIG}"
}
