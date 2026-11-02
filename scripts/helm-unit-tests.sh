#!/bin/bash

set -euo pipefail

CHART_DIR="charts/aws-mountpoint-s3-csi-driver"

unit_tests() {
  local helm_command_prefix=$1

  eval "${helm_command_prefix} --show-only templates/controller.yaml" | yq -e ".spec.replicas == 1"
  eval "${helm_command_prefix} --set controller.replicaCount=3 --show-only templates/controller.yaml" | yq -e ".spec.replicas == 3"

  # The node ValidatingAdmissionPolicy. The chart requires Kubernetes 1.31 or later, so the policy's 1.30 version gate
  # can't be tested below it.
  local vap="--show-only templates/validatingadmissionpolicy-csi-node.yaml"
  eval "${helm_command_prefix} --kube-version 1.31.0 --namespace kube-system ${vap}" |
    yq ea -e '[.] | (((map(select(.kind == "ValidatingAdmissionPolicy")) | length) == 1) and ((map(select(.kind == "ValidatingAdmissionPolicyBinding")) | length) == 1))'
  eval "${helm_command_prefix} --kube-version 1.31.0 --namespace kube-system ${vap}" |
    yq -e 'select(.kind == "ValidatingAdmissionPolicy") | ((.spec.failurePolicy == "Fail") and (.spec.matchConditions[0].expression == "request.userInfo.username == \"system:serviceaccount:kube-system:s3-csi-driver-sa\""))'
  eval "${helm_command_prefix} --kube-version 1.31.0 --namespace kube-system ${vap}" |
    yq -e 'select(.kind == "ValidatingAdmissionPolicyBinding") | ((.spec.policyName == "s3-csi-driver-node") and (.spec.validationActions[0] == "Deny"))'
  # the release namespace and the configured name, also for a service account the chart doesn't create
  eval "${helm_command_prefix} --kube-version 1.31.0 --namespace storage --set node.serviceAccount.create=false --set node.serviceAccount.name=my-node-sa ${vap}" |
    yq -e 'select(.kind == "ValidatingAdmissionPolicy") | .spec.matchConditions[0].expression == "request.userInfo.username == \"system:serviceaccount:storage:my-node-sa\""'
  # the off switch
  if eval "${helm_command_prefix} --kube-version 1.31.0 --set node.validatingAdmissionPolicy.enabled=false ${vap}" 2>/dev/null; then
    echo "node.validatingAdmissionPolicy.enabled=false still renders the policy" >&2
    exit 1
  fi
}


unit_tests "helm template ${CHART_DIR} --set isHelmRepo=true"
unit_tests "helm template ${CHART_DIR} --set isEKSAddon=true"
