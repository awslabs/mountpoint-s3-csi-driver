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

TEST_DIR=${BASE_DIR}/../csi-test-artifacts
BIN_DIR=${TEST_DIR}/bin
KUBECTL_INSTALL_PATH=/usr/local/bin

HELM_BIN=${BIN_DIR}/helm
KOPS_BIN=${BIN_DIR}/kops
EKSCTL_BIN=${BIN_DIR}/eksctl
KUBECTL_BIN=${KUBECTL_INSTALL_PATH}/kubectl

CLUSTER_TYPE=${CLUSTER_TYPE:-kops}
ARCH=${ARCH:-x86}
AMI_FAMILY=${AMI_FAMILY:-AmazonLinux2}
SELINUX_MODE=${SELINUX_MODE:-}

# We don't actually create clusters with eksctl as part of our GitHub workflow, we run our tests on pre-existing clusters.
# Therefore, we need to make sure we're setting correct cluster names here.
if [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    if [[ "${AMI_FAMILY}" == "Bottlerocket" ]]; then
        CLUSTER_NAME="s3-csi-cluster-bottlerocket"
    elif [[ "${ARCH}" == "arm" ]]; then
        CLUSTER_NAME="s3-csi-cluster-arm"
    else
        CLUSTER_NAME="s3-csi-cluster"
    fi
else
    # In kops, cluster names must end with ".k8s.local" to use Gossip DNS.
    # See https://kops.sigs.k8s.io/gossip/#configuring-a-cluster-to-use-gossip
    # They also need to be valid domain names, that's why we're lowercasing "AMI_FAMILY".
    CLUSTER_NAME="s3-csi-cluster-${CLUSTER_TYPE}-${AMI_FAMILY,,}-${ARCH}.k8s.local"
fi

KUBECONFIG=${KUBECONFIG:-"${TEST_DIR}/${CLUSTER_NAME}.kubeconfig"}

# kops: must include patch version (e.g. 1.19.1)
# eksctl: mustn't include patch version (e.g. 1.19)
K8S_VERSION=${K8S_VERSION:-1.28.2}
K8S_VERSION_MAJOR_MINOR=${K8S_VERSION_MAJOR_MINOR:-1.28}
K8S_VERSION_KOPS=${K8S_VERSION_KOPS:-${K8S_VERSION}}

KOPS_VERSION=1.28.0
ZONES=${AWS_AVAILABILITY_ZONES:-$(aws ec2 describe-availability-zones --region ${REGION} | jq -c '.AvailabilityZones[].ZoneName' | grep -v "us-east-1e" | tr '\n' ',' | sed 's/"//g' | sed 's/.$//')} # excluding us-east-1e, see: https://github.com/eksctl-io/eksctl/issues/817
NODE_COUNT=${NODE_COUNT:-3}

# "AMI_ID" is only used on kops (eksctl directly uses "AMI_FAMILY").
declare -A AMI_IDS
AMI_IDS["AmazonLinux2-x86"]="/aws/service/ami-amazon-linux-latest/amzn2-ami-kernel-5.10-hvm-x86_64-gp2"
AMI_IDS["AmazonLinux2-arm"]="/aws/service/ami-amazon-linux-latest/amzn2-ami-kernel-5.10-hvm-arm64-gp2"
AMI_IDS["AmazonLinux2023-x86"]="/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
AMI_IDS["AmazonLinux2023-arm"]="/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
AMI_IDS["Bottlerocket-x86"]="/aws/service/bottlerocket/aws-k8s-${K8S_VERSION_MAJOR_MINOR}/x86_64/latest/image_id"
AMI_IDS["Bottlerocket-arm"]="/aws/service/bottlerocket/aws-k8s-${K8S_VERSION_MAJOR_MINOR}/arm64/latest/image_id"
AMI_IDS["Ubuntu2004-x86"]="/aws/service/canonical/ubuntu/server/20.04/stable/current/amd64/hvm/ebs-gp2/ami-id"
AMI_IDS["Ubuntu2004-arm"]="/aws/service/canonical/ubuntu/server/20.04/stable/current/arm64/hvm/ebs-gp2/ami-id"

AMI_ID_DEFAULT=$(aws ssm get-parameters --names "${AMI_IDS["${AMI_FAMILY}-${ARCH}"]}" --region ${REGION} --query 'Parameters[0].Value' --output text)
AMI_ID=${AMI_ID:-$AMI_ID_DEFAULT}
if [[ "${CLUSTER_TYPE}" == "kops" ]]; then
    echo "Using AMI $AMI_ID for $AMI_FAMILY"
fi

if [[ "${ARCH}" == "x86" ]]; then
  INSTANCE_TYPE_DEFAULT=c5.large
  KOPS_STATE_FILE_DEFAULT=s3://mountpoint-s3-csi-driver-kops-state-store
else
  INSTANCE_TYPE_DEFAULT=m7g.medium
  KOPS_STATE_FILE_DEFAULT=s3://mountpoint-s3-csi-driver-kops-arm-state-store
fi
INSTANCE_TYPE=${INSTANCE_TYPE:-$INSTANCE_TYPE_DEFAULT}
CLUSTER_FILE=${TEST_DIR}/${CLUSTER_NAME}.${CLUSTER_TYPE}.yaml
KOPS_PATCH_FILE=${KOPS_PATCH_FILE:-${BASE_DIR}/kops-patch.yaml}
KOPS_PATCH_NODE_FILE=${KOPS_PATCH_NODE_FILE:-${BASE_DIR}/kops-patch-node.yaml}
KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE=${KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE:-${BASE_DIR}/kops-patch-node-selinux-enforcing.yaml}
if [[ "${SELINUX_MODE}" != "enforcing" ]]; then
    KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE=""
fi
KOPS_STATE_FILE=${KOPS_STATE_FILE:-$KOPS_STATE_FILE_DEFAULT}
SSH_KEY=${SSH_KEY:-""}
HELM_RELEASE_NAME=mountpoint-s3-csi-driver

EKSCTL_VERSION=${EKSCTL_VERSION:-0.161.0}
EKSCTL_PATCH_FILE=${EKSCTL_PATCH_FILE:-${BASE_DIR}/eksctl-patch.yaml}
CI_ROLE_ARN=${CI_ROLE_ARN:-""}

mkdir -p ${TEST_DIR}
mkdir -p ${BIN_DIR}
export PATH="$PATH:${BIN_DIR}"

function kubectl_install() {
  curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
  curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl.sha256"
  echo "$(cat kubectl.sha256)  kubectl" | sha256sum --check
  sudo install -o root -g root -m 0755 kubectl ${KUBECTL_INSTALL_PATH}/kubectl
}

function print_cluster_info() {
  $KUBECTL_BIN logs -l app=s3-csi-node -n kube-system --kubeconfig ${KUBECONFIG}
  $KUBECTL_BIN version --kubeconfig ${KUBECONFIG}
  $KUBECTL_BIN get nodes -o wide --kubeconfig ${KUBECONFIG}
}

function install_tools() {
  kubectl_install

  helm_install "$BIN_DIR"

  kops_install \
    "${BIN_DIR}" \
    "${KOPS_VERSION}"

  eksctl_install \
    "${BIN_DIR}" \
    "${EKSCTL_VERSION}"
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
      "$KOPS_STATE_FILE" \
      "$SSH_KEY" \
      "$KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE"
  elif [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    eksctl_create_cluster \
      "$CLUSTER_NAME" \
      "$REGION" \
      "$KUBECONFIG" \
      "$CLUSTER_FILE" \
      "$EKSCTL_BIN" \
      "$KUBECTL_BIN" \
      "$EKSCTL_PATCH_FILE" \
      "$ZONES" \
      "$CI_ROLE_ARN" \
      "$INSTANCE_TYPE" \
      "$AMI_FAMILY"
  fi
}

function delete_cluster() {
  if [[ "${CLUSTER_TYPE}" == "kops" ]]; then
    kops_delete_cluster \
      "${KOPS_BIN}" \
      "${CLUSTER_NAME}" \
      "${KOPS_STATE_FILE}"
  elif [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    eksctl_delete_cluster \
      "$EKSCTL_BIN" \
      "$CLUSTER_NAME" \
      "$REGION"
  fi
}

function update_kubeconfig() {
  if [[ "${CLUSTER_TYPE}" == "kops" ]]; then
    ${KOPS_BIN} export kubecfg --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}" --admin --kubeconfig "${KUBECONFIG}"
  elif [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    aws eks update-kubeconfig --name ${CLUSTER_NAME} --region ${REGION} --kubeconfig=${KUBECONFIG}
  fi
}

function e2e_cleanup() {
  set -e
  if driver_installed ${HELM_BIN} ${HELM_RELEASE_NAME} ${KUBECONFIG}; then
    for ns in $($KUBECTL_BIN get namespaces -o custom-columns=":metadata.name" --kubeconfig "${KUBECONFIG}" | grep -E "^aws-s3-csi-e2e-.*|^volume-.*"); do
      $KUBECTL_BIN delete all --all -n $ns --timeout=2m --kubeconfig "${KUBECONFIG}"
      $KUBECTL_BIN delete namespace $ns --timeout=2m --kubeconfig "${KUBECONFIG}"
    done
  fi
  set +e

  for bucket in $(aws s3 ls --region ${REGION} | awk '{ print $3 }' | grep "^${CLUSTER_NAME}-e2e-kubernetes-.*"); do
    aws s3 rb "s3://${bucket}" --force --region ${REGION}
  done
}

function print_cluster_info() {
  $KUBECTL_BIN logs -l app=s3-csi-node -n kube-system --kubeconfig ${KUBECONFIG}
  $KUBECTL_BIN version --kubeconfig ${KUBECONFIG}
  $KUBECTL_BIN get nodes -o wide --kubeconfig ${KUBECONFIG}
}

if [[ "${ACTION}" == "install_tools" ]]; then
  install_tools
elif [[ "${ACTION}" == "create_cluster" ]]; then
  create_cluster
elif [[ "${ACTION}" == "update_kubeconfig" ]]; then
  update_kubeconfig
elif [[ "${ACTION}" == "install_driver" ]]; then
  helm_install_driver \
    "$HELM_BIN" \
    "$KUBECTL_BIN" \
    "$HELM_RELEASE_NAME" \
    "${REGISTRY}/${IMAGE_NAME}" \
    "${TAG}" \
    "${KUBECONFIG}"
elif [[ "${ACTION}" == "run_tests" ]]; then
  set +e
  pushd tests/e2e-kubernetes
  KUBECONFIG=${KUBECONFIG} go test -ginkgo.vv --bucket-region=${REGION} --commit-id=${TAG} --bucket-prefix=${CLUSTER_NAME}
  EXIT_CODE=$?
  print_cluster_info
  exit $EXIT_CODE
elif [[ "${ACTION}" == "run_perf" ]]; then
  set +e
  pushd tests/e2e-kubernetes
  KUBECONFIG=${KUBECONFIG} go test -ginkgo.vv --bucket-region=${REGION} --commit-id=${TAG} --bucket-prefix=${CLUSTER_NAME} --performance=true
  EXIT_CODE=$?
  print_cluster_info
  popd
  cat tests/e2e-kubernetes/csi-test-artifacts/output.json
  exit $EXIT_CODE
elif [[ "${ACTION}" == "uninstall_driver" ]]; then
  helm_uninstall_driver \
    "$HELM_BIN" \
    "$KUBECTL_BIN" \
    "$HELM_RELEASE_NAME" \
    "${KUBECONFIG}"
elif [[ "${ACTION}" == "delete_cluster" ]]; then
  delete_cluster
elif [[ "${ACTION}" == "e2e_cleanup" ]]; then
  e2e_cleanup || true
else
  echo "ACTION := install_tools|create_cluster|install_driver|update_kubeconfig|run_tests|run_perf|e2e_cleanup|uninstall_driver|delete_cluster"
  exit 1
fi
