#!/bin/bash

set -euox pipefail

OS_ARCH=$(go env GOOS)-amd64

function kops_install() {
  INSTALL_PATH=${1}
  KOPS_VERSION=${2}
  if [[ -e "${INSTALL_PATH}"/kops ]]; then
    INSTALLED_KOPS_VERSION=$("${INSTALL_PATH}"/kops version)
    if [[ "$INSTALLED_KOPS_VERSION" == *"$KOPS_VERSION"* ]]; then
      echo "KOPS $INSTALLED_KOPS_VERSION already installed!"
      return
    fi
  fi
  KOPS_DOWNLOAD_URL=https://github.com/kubernetes/kops/releases/download/v${KOPS_VERSION}/kops-${OS_ARCH}
  curl -L -X GET "${KOPS_DOWNLOAD_URL}" -o "${INSTALL_PATH}"/kops
  chmod +x "${INSTALL_PATH}"/kops
}

function kops_create_cluster() {
  CLUSTER_NAME=${1}
  BIN=${2}
  ZONES=${3}
  NODE_COUNT=${4}
  INSTANCE_TYPE=${5}
  AMI_ID=${6}
  K8S_VERSION=${7}
  CLUSTER_FILE=${8}
  KUBECONFIG=${9}
  KOPS_PATCH_NODE_FILE=${10}
  KOPS_STATE_FILE=${11}

  if kops_cluster_exists "${CLUSTER_NAME}" "${BIN}" "${KOPS_STATE_FILE}"; then
    kops_delete_cluster "${BIN}" \
      "${CLUSTER_NAME}" \
      "${KOPS_STATE_FILE}"
    # fail if cluster already exists
    exit 1
  fi
  # ${BIN} create cluster --state "${KOPS_STATE_FILE}" \
  #   --zones "${ZONES}" \
  #   --node-count="${NODE_COUNT}" \
  #   --node-size="${INSTANCE_TYPE}" \
  #   --image="${AMI_ID}" \
  #   --kubernetes-version="${K8S_VERSION}" \
  #   --dry-run \
  #   -o yaml \
  #   "${CLUSTER_NAME}" > "${CLUSTER_FILE}"

  # todo: patch node iam policies

  # ${BIN} create --state "${KOPS_STATE_FILE}" -f "${CLUSTER_FILE}"

  ${BIN} update cluster --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}" --yes
  ${BIN} export kubecfg --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}" --admin --kubeconfig "${KUBECONFIG}"
  ${BIN} validate cluster --state "${KOPS_STATE_FILE}" --wait 10m --kubeconfig "${KUBECONFIG}"
}

function kops_cluster_exists() {
  CLUSTER_NAME=${1}
  BIN=${2}
  KOPS_STATE_FILE=${3}
  set +e
  if ${BIN} get cluster --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}"; then
    set -e
    return 0
  else
    set -e
    return 1
  fi
}

function kops_delete_cluster() {
  BIN=${1}
  CLUSTER_NAME=${2}
  KOPS_STATE_FILE=${3}
  echo "Deleting cluster ${CLUSTER_NAME}"
  ${BIN} delete cluster --name "${CLUSTER_NAME}" --state "${KOPS_STATE_FILE}" --yes
}