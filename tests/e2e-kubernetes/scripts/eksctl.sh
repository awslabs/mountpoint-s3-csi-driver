#!/bin/bash

set -euox pipefail

function eksctl_install() {
  INSTALL_PATH=${1}
  EKSCTL_VERSION=${2}
  if [[ ! -e ${INSTALL_PATH}/eksctl ]]; then
    EKSCTL_DOWNLOAD_URL="https://github.com/weaveworks/eksctl/releases/download/v${EKSCTL_VERSION}/eksctl_$(uname -s)_amd64.tar.gz"
    curl --silent --location "${EKSCTL_DOWNLOAD_URL}" | tar xz -C "${INSTALL_PATH}"
    chmod +x "${INSTALL_PATH}"/eksctl
  fi
}

function eksctl_create_cluster() {
  CLUSTER_NAME=${1}
  REGION=${2}
  KUBECONFIG=${3}
  CLUSTER_FILE=${4}
  BIN=${5}
  KUBECTL_BIN=${6}
  EKSCTL_PATCH_FILE=${7}
  ZONES=${8}
  CI_ROLE_ARN=${9}
  NODE_TYPE=${10}

  eksctl_delete_cluster "$BIN" "$CLUSTER_NAME" "$REGION"

  # CAUTION: this may fail with "the targeted availability zone, does not currently have sufficient capacity to support the cluster" error, we may require a fix for that
  ${BIN} create cluster \
    --name $CLUSTER_NAME \
    --region $REGION \
    --node-type $NODE_TYPE \
    --with-oidc \
    --zones $ZONES \
    --dry-run > $CLUSTER_FILE

  CLUSTER_FILE_TMP="${CLUSTER_FILE}.tmp"
  ${KUBECTL_BIN} patch -f $CLUSTER_FILE --local --type json --patch "$(cat $EKSCTL_PATCH_FILE)" -o yaml > $CLUSTER_FILE_TMP
  mv $CLUSTER_FILE_TMP $CLUSTER_FILE
  ${BIN} create cluster -f "${CLUSTER_FILE}" --kubeconfig "${KUBECONFIG}"

  if [ -n "$CI_ROLE_ARN" ]; then
    ${BIN} create iamidentitymapping --cluster ${CLUSTER_NAME} --region=${REGION} \
      --arn ${CI_ROLE_ARN} --username admin --group system:masters \
      --no-duplicate-arns
  fi
}

function eksctl_delete_cluster() {
  BIN=${1}
  CLUSTER_NAME=${2}
  REGION=${3}
  if eksctl_cluster_exists "${BIN}" "${CLUSTER_NAME}"; then
    ${BIN} delete cluster "${CLUSTER_NAME}"
  fi
  aws cloudformation delete-stack --region ${REGION} --stack-name "eksctl-${CLUSTER_NAME}-cluster"
  aws cloudformation wait stack-delete-complete --region ${REGION} --stack-name "eksctl-${CLUSTER_NAME}-cluster"
}

function eksctl_cluster_exists() {
  BIN=${1}
  CLUSTER_NAME=${2}
  set +e
  if ${BIN} get cluster "${CLUSTER_NAME}"; then
    set -e
    return 0
  else
    set -e
    return 1
  fi
}
