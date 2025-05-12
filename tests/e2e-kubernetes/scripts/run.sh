#!/usr/bin/env bash

set -euox pipefail

ACTION=${ACTION:-}
REGION=${AWS_REGION}

AWS_PARTITION=$(aws sts get-caller-identity --query Arn --output text | cut -d: -f2)
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

# kops: must include patch version (e.g. 1.19.1)
# eksctl: mustn't include patch version (e.g. 1.19)
# 'K8S_VERSION' variable must be a full version (e.g. 1.19.1)
K8S_VERSION=${K8S_VERSION:-1.30.4}
K8S_VERSION_KOPS=${K8S_VERSION_KOPS:-${K8S_VERSION}}
K8S_VERSION_EKSCTL=${K8S_VERSION_EKSCTL:-${K8S_VERSION%.*}}

# We need to ensure that we're using all testing matrix variables in the cluster name
# because they all run in parallel and conflicting name would break other tests.
CLUSTER_NAME="s3-csi-cluster-${CLUSTER_TYPE}-${AMI_FAMILY,,}-${ARCH}"

if [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    # EKS does not allow cluster names with ".", we're replacing them with "-".
    CLUSTER_NAME="${CLUSTER_NAME}-${K8S_VERSION_EKSCTL/./-}"
else
    # In kops, cluster names must end with ".k8s.local" to use Gossip DNS.
    # See https://kops.sigs.k8s.io/gossip/#configuring-a-cluster-to-use-gossip
    # They also need to be valid domain names, that's why we're lowercasing "CLUSTER_NAME" and replacing "." with "-".
    CLUSTER_NAME="${CLUSTER_NAME,,}-${K8S_VERSION_KOPS//./-}.k8s.local"
fi

KUBECONFIG=${KUBECONFIG:-"${TEST_DIR}/${CLUSTER_NAME}.kubeconfig"}

KOPS_VERSION=1.28.5
ZONES=${AWS_AVAILABILITY_ZONES:-$(aws ec2 describe-availability-zones --region ${REGION} | jq -c '.AvailabilityZones[].ZoneName' | grep -v "us-east-1e" | tr '\n' ',' | sed 's/"//g' | sed 's/.$//')} # excluding us-east-1e, see: https://github.com/eksctl-io/eksctl/issues/817
NODE_COUNT=${NODE_COUNT:-3}
if [[ "${ARCH}" == "x86" ]]; then
  INSTANCE_TYPE_DEFAULT=c5.large
  AMI_ID_DEFAULT=$(aws ssm get-parameters --names /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 --region ${REGION} --query 'Parameters[0].Value' --output text)
else
  INSTANCE_TYPE_DEFAULT=m7g.large
  AMI_ID_DEFAULT=$(aws ssm get-parameters --names /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --region ${REGION} --query 'Parameters[0].Value' --output text)
fi
INSTANCE_TYPE=${INSTANCE_TYPE:-$INSTANCE_TYPE_DEFAULT}
AMI_ID=${AMI_ID:-$AMI_ID_DEFAULT}
CLUSTER_FILE=${TEST_DIR}/${CLUSTER_NAME}.${CLUSTER_TYPE}.yaml
KOPS_PATCH_FILE=${KOPS_PATCH_FILE:-${BASE_DIR}/kops-patch.yaml}
KOPS_PATCH_NODE_FILE=${KOPS_PATCH_NODE_FILE:-${BASE_DIR}/kops-patch-node.yaml}
KOPS_STATE_FILE=${KOPS_STATE_FILE:-"s3://mountpoint-s3-csi-driver-kops-state-store"}
KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE=${KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE:-${BASE_DIR}/kops-patch-node-selinux-enforcing.yaml}
if [[ "${SELINUX_MODE}" != "enforcing" ]]; then
    KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE=""
fi

SSH_KEY=${SSH_KEY:-""}
HELM_RELEASE_NAME=mountpoint-s3-csi-driver

EKSCTL_VERSION=${EKSCTL_VERSION:-0.202.0}
EKSCTL_PATCH_FILE=${EKSCTL_PATCH_FILE:-${BASE_DIR}/eksctl-patch.json}
EKSCTL_PATCH_SELINUX_ENFORCING_FILE=${EKSCTL_PATCH_SELINUX_ENFORCING_FILE:-${BASE_DIR}/eksctl-patch-selinux-enforcing.json}
if [[ "${SELINUX_MODE}" != "enforcing" ]]; then
    EKSCTL_PATCH_SELINUX_ENFORCING_FILE=""
fi

CI_ROLE_ARN=${CI_ROLE_ARN:-""}

MOUNTER_KIND=${MOUNTER_KIND:-systemd}
if [ "$MOUNTER_KIND" = "pod" ]; then
  USE_POD_MOUNTER=true
else
  USE_POD_MOUNTER=false
fi

mkdir -p ${TEST_DIR}
mkdir -p ${BIN_DIR}
export PATH="$PATH:${BIN_DIR}"

function kubectl_install() {
  curl -LO "https://dl.k8s.io/release/v$K8S_VERSION/bin/linux/amd64/kubectl"
  curl -LO "https://dl.k8s.io/release/v$K8S_VERSION/bin/linux/amd64/kubectl.sha256"
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

  go install github.com/onsi/ginkgo/v2/ginkgo
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
      "$AMI_FAMILY" \
      "$K8S_VERSION_EKSCTL" \
      "$EKSCTL_PATCH_SELINUX_ENFORCING_FILE"
  fi
}

function delete_cluster() {
  if [[ "${CLUSTER_TYPE}" == "kops" ]]; then
    kops_delete_cluster \
      "${KOPS_BIN}" \
      "${CLUSTER_NAME}" \
      "${KOPS_STATE_FILE}" \
      "${FORCE:-}"
  elif [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    eksctl_delete_cluster \
      "$EKSCTL_BIN" \
      "$CLUSTER_NAME" \
      "$REGION" \
      "${FORCE:-}"
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
    "${KUBECONFIG}" \
    "${MOUNTER_KIND}"
elif [[ "${ACTION}" == "run_tests" ]]; then
  set +e
  pushd tests/e2e-kubernetes
  KUBECONFIG=${KUBECONFIG} ginkgo -p -vv -timeout 60m -- --bucket-region=${REGION} --commit-id=${TAG} --bucket-prefix=${CLUSTER_NAME} --imds-available=true --pod-mounter=${USE_POD_MOUNTER} --cluster-name=${CLUSTER_NAME}
  EXIT_CODE=$?
  print_cluster_info
  exit $EXIT_CODE
elif [[ "${ACTION}" == "run_perf" ]]; then
  set +e
  pushd tests/e2e-kubernetes
  KUBECONFIG=${KUBECONFIG} go test -ginkgo.vv --bucket-region=${REGION} --commit-id=${TAG} --bucket-prefix=${CLUSTER_NAME} --performance=true --imds-available=true --pod-mounter=${USE_POD_MOUNTER} --cluster-name=${CLUSTER_NAME}
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
