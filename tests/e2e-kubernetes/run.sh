#!/bin/bash

set -euox pipefail

ACTION=${ACTION:-}
REGION=${AWS_REGION}

AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGISTRY=${REGISTRY:-${AWS_ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com}
IMAGE_NAME=${IMAGE_NAME:-}
TAG=${TAG:-}

BASE_DIR=$(dirname "$(realpath "${BASH_SOURCE[0]}")")
source "${BASE_DIR}"/kops.sh
source "${BASE_DIR}"/eksctl.sh
source "${BASE_DIR}"/helm.sh

TEST_DIR=${BASE_DIR}/csi-test-artifacts
BIN_DIR=${TEST_DIR}/bin
KUBECTL_INSTALL_PATH=/usr/local/bin

HELM_BIN=${BIN_DIR}/helm
KOPS_BIN=${BIN_DIR}/kops
EKSCTL_BIN=${BIN_DIR}/eksctl
KUBECTL_BIN=${KUBECTL_INSTALL_PATH}/kubectl

CLUSTER_TYPE=${CLUSTER_TYPE:-kops}
CLUSTER_NAME="s3-csi-cluster.${CLUSTER_TYPE}.k8s.local"
# temporary crutch to make eksctl working with pre-created cluster
if [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
  CLUSTER_NAME=s3-csi-cluster
fi
KUBECONFIG=${KUBECONFIG:-"${TEST_DIR}/${CLUSTER_NAME}.kubeconfig"}

KOPS_VERSION=1.28.0
ZONES=${AWS_AVAILABILITY_ZONES:-us-east-1a,us-east-1b,us-east-1c,us-east-1d}
NODE_COUNT=${NODE_COUNT:-3}
INSTANCE_TYPE=${INSTANCE_TYPE:-c5.large}
AMI_ID=$(aws ssm get-parameters --names /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 --region ${REGION} --query 'Parameters[0].Value' --output text)
CLUSTER_FILE=${TEST_DIR}/${CLUSTER_NAME}.${CLUSTER_TYPE}.yaml
KOPS_PATCH_FILE=${KOPS_PATCH_FILE:-${BASE_DIR}/kops-patch.yaml}
KOPS_PATCH_NODE_FILE=${KOPS_PATCH_NODE_FILE:-${BASE_DIR}/kops-patch-node.yaml}
KOPS_STATE_FILE=${KOPS_STATE_FILE:-s3://mountpoint-s3-csi-driver-kops-state-store}

HELM_RELEASE_NAME=mountpoint-s3-csi-driver

# kops: must include patch version (e.g. 1.19.1)
# eksctl: mustn't include patch version (e.g. 1.19)
K8S_VERSION_KOPS=${K8S_VERSION_KOPS:-${K8S_VERSION:-1.28.2}}

mkdir -p ${TEST_DIR}
mkdir -p ${BIN_DIR}
export PATH="$PATH:${BIN_DIR}"

function kubectl_install() {
  curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
  curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl.sha256"
  echo "$(cat kubectl.sha256)  kubectl" | sha256sum --check
  sudo install -o root -g root -m 0755 kubectl ${KUBECTL_INSTALL_PATH}/kubectl
}

function install_tools() {
  kubectl_install

  helm_install "$BIN_DIR"

  kops_install \
    "${BIN_DIR}" \
    "${KOPS_VERSION}"
}

function create_cluster() {
  if [[ "${CLUSTER_TYPE}" == "kops" ]]; then
    kops_create_cluster \
      "$CLUSTER_NAME" \
      "$KOPS_BIN" \
      "$ZONES" \
      "$NODE_COUNT" \
      "$INSTANCE_TYPE" \
      "$AMI_ID" \
      "$K8S_VERSION_KOPS" \
      "$CLUSTER_FILE" \
      "$KUBECONFIG" \
      "$KOPS_PATCH_FILE" \
      "$KOPS_PATCH_NODE_FILE" \
      "$KOPS_STATE_FILE"
  elif [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    eksctl_create_cluster \
      "$CLUSTER_NAME" \
      "$REGION" \
      "$KUBECONFIG"
  fi
}

function delete_cluster() {
  if [[ "${CLUSTER_TYPE}" == "kops" ]]; then
    kops_delete_cluster \
      "${KOPS_BIN}" \
      "${CLUSTER_NAME}" \
      "${KOPS_STATE_FILE}"
  elif [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    eksctl_delete_cluster
  fi
}

if [[ "${ACTION}" == "install_tools" ]]; then
  install_tools
elif [[ "${ACTION}" == "create_cluster" ]]; then
  create_cluster
elif [[ "${ACTION}" == "install_driver" ]]; then
  helm_install_driver \
    "$HELM_BIN" \
    "$KUBECTL_BIN" \
    "$HELM_RELEASE_NAME" \
    "${REGISTRY}/${IMAGE_NAME}" \
    "${TAG}" \
    "${KUBECONFIG}"
elif [[ "${ACTION}" == "run_tests" ]]; then
  KUBECONFIG=${KUBECONFIG} go test -ginkgo.vv --bucket-region=${REGION} --commit-id=${TAG};
elif [[ "${ACTION}" == "uninstall_driver" ]]; then
  helm_uninstall_driver \
    "$HELM_BIN" \
    "$KUBECTL_BIN" \
    "$HELM_RELEASE_NAME" \
    "${KUBECONFIG}"
elif [[ "${ACTION}" == "delete_cluster" ]]; then
  delete_cluster
else
  echo "ACTION := install_tools|create_cluster|install_driver|run_tests|uninstall_driver|delete_cluster"
  exit 1
fi
