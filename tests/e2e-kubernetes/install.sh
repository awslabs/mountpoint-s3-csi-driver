#!/bin/bash

set -euox pipefail

REGION=${AWS_REGION:-us-east-1}

AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGISTRY=${REGISTRY:-${AWS_ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com}
IMAGE_NAME=${IMAGE_NAME:-}
TAG=${TAG:-}

BASE_DIR=$(dirname "$(realpath "${BASH_SOURCE[0]}")")
source "${BASE_DIR}"/kops.sh

TEST_DIR=${BASE_DIR}/csi-test-artifacts
BIN_DIR=${TEST_DIR}/bin
KUBECTL_INSTALL_PATH=/usr/local/bin
CLUSTER_NAME=s3-csi-cluster.k8s.local

CLUSTER_TYPE=${CLUSTER_TYPE:-kops}
export KUBECONFIG=${KUBECONFIG:-"${TEST_DIR}/${CLUSTER_NAME}.${CLUSTER_TYPE}.kubeconfig"}

KOPS_VERSION=1.28.0
ZONES=${AWS_AVAILABILITY_ZONES:-us-east-1a,us-east-1b,us-east-1c,us-east-1d}
NODE_COUNT=${NODE_COUNT:-3}
INSTANCE_TYPE=${INSTANCE_TYPE:-c5.large}
AMI_ID=$(aws ssm get-parameters --names /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 --region ${REGION} --query 'Parameters[0].Value' --output text)
CLUSTER_FILE=${TEST_DIR}/${CLUSTER_NAME}.${CLUSTER_TYPE}.yaml
KOPS_PATCH_FILE=${KOPS_PATCH_FILE:-${BASE_DIR}/kops-patch.yaml}
KOPS_STATE_FILE=s3://vlaad-kops-state-store

# kops: must include patch version (e.g. 1.19.1)
# eksctl: mustn't include patch version (e.g. 1.19)
K8S_VERSION_KOPS=${K8S_VERSION_KOPS:-${K8S_VERSION:-1.28.2}}

mkdir -p ${TEST_DIR}
mkdir -p ${BIN_DIR}

function kubectl_install() {
  curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
  curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl.sha256"
  echo "$(cat kubectl.sha256)  kubectl" | sha256sum --check
  sudo install -o root -g root -m 0755 kubectl ${KUBECTL_INSTALL_PATH}/kubectl
}

function helm_install() {
  if [[ ! -e ${INSTALL_PATH}/helm ]]; then
    curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/master/scripts/get-helm-3
    chmod 700 get_helm.sh
    export USE_SUDO=false
    export HELM_INSTALL_DIR=${INSTALL_PATH}
    ./get_helm.sh
    rm get_helm.sh
  fi
}

function setup_kubeconfig() {
    aws eks update-kubeconfig --region ${EKS_REGION} --name ${EKS_CLUSTER_NAME} --kubeconfig=${KUBECONFIG}
}

function ensure_driver_not_installed() {
  if [[ $(helm list -A | grep aws-s3-csi-driver) == *deployed* ]]; then
    helm uninstall aws-s3-csi-driver --namespace kube-system
    sleep 10 # nice to have: a better way to wait for driver removed
  fi
  kubectl get pods -A
  kubectl get CSIDriver
}

function install_driver() {
  helm upgrade --install aws-s3-csi-driver --namespace kube-system ./charts/aws-s3-csi-driver --values \
    ./charts/aws-s3-csi-driver/values.yaml \
    --set image.repository=${REGISTRY}/${IMAGE_NAME} \
    --set image.tag=${TAG} \
    --set image.pullPolicy=Always
  kubectl rollout status daemonset s3-csi-node -n kube-system --timeout=60s
  kubectl get pods -A
  echo "s3-csi-node-image: $(kubectl get daemonset s3-csi-node -n kube-system -o jsonpath="{$.spec.template.spec.containers[:1].image}")"
}

# kubectl_install
# helm_install
# setup_kubeconfig
# ensure_driver_not_installed
# install_driver

# kops_install "${BIN_DIR}" "${KOPS_VERSION}"
# kops_create_cluster \
#   "$CLUSTER_NAME" \
#   "$BIN_DIR"/kops \
#   "$ZONES" \
#   "$NODE_COUNT" \
#   "$INSTANCE_TYPE" \
#   "$AMI_ID" \
#   "$K8S_VERSION_KOPS" \
#   "$CLUSTER_FILE" \
#   "$KUBECONFIG" \
#   "$KOPS_PATCH_FILE" \
#   "$KOPS_STATE_FILE"
install_driver