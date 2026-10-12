#!/bin/bash

set -euo pipefail

CHART_DIR="charts/aws-mountpoint-s3-csi-driver"

unit_tests() {
  local helm_command_prefix=$1

  eval "${helm_command_prefix} --show-only templates/controller.yaml" | yq -e ".spec.replicas == 1"
  eval "${helm_command_prefix} --set controller.replicaCount=3 --show-only templates/controller.yaml" | yq -e ".spec.replicas == 3"
}


unit_tests "helm template ${CHART_DIR} --set isHelmRepo=true"
unit_tests "helm template ${CHART_DIR} --set isEKSAddon=true"
