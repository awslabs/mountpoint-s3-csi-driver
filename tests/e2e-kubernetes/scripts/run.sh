#!/usr/bin/env bash

set -euox pipefail

export AWS_RETRY_MODE=standard
export AWS_MAX_ATTEMPTS=10

ACTION=${ACTION:-}
REGION=${AWS_REGION}

AWS_PARTITION=$(aws sts get-caller-identity --query Arn --output text | cut -d: -f2)
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGISTRY=${REGISTRY:-${AWS_ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com}
IMAGE_NAME=${IMAGE_NAME:-}
TAG=${TAG:-}
export REPOSITORY="${REGISTRY}/${IMAGE_NAME}"

BASE_DIR=$(dirname "$(realpath "${BASH_SOURCE[0]}")")
source "${BASE_DIR}"/eksctl.sh
source "${BASE_DIR}"/helm.sh

TEST_DIR=${BASE_DIR}/../csi-test-artifacts
BIN_DIR=${TEST_DIR}/bin
KUBECTL_INSTALL_PATH=/usr/local/bin

HELM_BIN=${BIN_DIR}/helm
EKSCTL_BIN=${BIN_DIR}/eksctl
KUBECTL_BIN=${KUBECTL_INSTALL_PATH}/kubectl

CLUSTER_TYPE=${CLUSTER_TYPE:-eksctl}
IMDS_AVAILABLE=${IMDS_AVAILABLE:-true}
ARCH=${ARCH:-x86}
AMI_FAMILY=${AMI_FAMILY:-AmazonLinux2}
SELINUX_MODE=${SELINUX_MODE:-}

# eksctl: mustn't include patch version (e.g. 1.19)
# 'K8S_VERSION' variable must be a full version (e.g. 1.19.1)
K8S_VERSION=${K8S_VERSION:-1.30.4}
K8S_VERSION_EKSCTL=${K8S_VERSION_EKSCTL:-${K8S_VERSION%.*}}

# We need to ensure that we're using all testing matrix variables in the cluster name
# because they all run in parallel and conflicting name would break other tests.
if [[ "${CLUSTER_TYPE}" == "openshift" ]]; then
    CLUSTER_NAME=${CLUSTER_NAME:-"s3-csi-rosa"}
elif [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    # EKS does not allow cluster names with ".", we're replacing them with "-".
    CLUSTER_NAME="s3-csi-cluster-${CLUSTER_TYPE}-${AMI_FAMILY,,}-${ARCH}-${K8S_VERSION_EKSCTL/./-}"
else
    echo "Unsupported cluster type: ${CLUSTER_TYPE}."
    exit 1
fi

KUBECONFIG=${KUBECONFIG:-"${TEST_DIR}/${CLUSTER_NAME}.kubeconfig"}

ZONES=${AWS_AVAILABILITY_ZONES:-$(aws ec2 describe-availability-zones --region ${REGION} | jq -c '.AvailabilityZones[].ZoneName' | grep -v "us-east-1e" | tr '\n' ',' | sed 's/"//g' | sed 's/.$//')} # excluding us-east-1e, see: https://github.com/eksctl-io/eksctl/issues/817
NODE_COUNT=${NODE_COUNT:-3}
if [[ "${ARCH}" == "x86" ]]; then
  INSTANCE_TYPE_DEFAULT=c5.large
else
  INSTANCE_TYPE_DEFAULT=m7g.large
fi
INSTANCE_TYPE=${INSTANCE_TYPE:-$INSTANCE_TYPE_DEFAULT}
CLUSTER_FILE=${TEST_DIR}/${CLUSTER_NAME}.${CLUSTER_TYPE}.yaml

SSH_KEY=${SSH_KEY:-""}
HELM_RELEASE_NAME=mountpoint-s3-csi-driver

EKSCTL_PATCH_FILE=${EKSCTL_PATCH_FILE:-${BASE_DIR}/eksctl-patch.json}
EKSCTL_PATCH_SELINUX_ENFORCING_FILE=${EKSCTL_PATCH_SELINUX_ENFORCING_FILE:-${BASE_DIR}/eksctl-patch-selinux-enforcing.json}
if [[ "${SELINUX_MODE}" != "enforcing" ]]; then
    EKSCTL_PATCH_SELINUX_ENFORCING_FILE=""
fi

CI_ROLE_ARN=${CI_ROLE_ARN:-""}
CSI_DRIVER_IRSA_ROLE_ARN=${CSI_DRIVER_IRSA_ROLE_ARN:-}

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

  eksctl_install "${BIN_DIR}"

  go install github.com/onsi/ginkgo/v2/ginkgo
}

function create_cluster() {
  if [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
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
  if [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
    eksctl_delete_cluster \
      "$EKSCTL_BIN" \
      "$CLUSTER_NAME" \
      "$REGION" \
      "${FORCE:-}"
  fi
}

function update_kubeconfig() {
  if [[ "${CLUSTER_TYPE}" == "eksctl" ]]; then
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
}

# Delete test buckets older than 7 days
function delete_old_buckets() {
  current_date=$(date +%s)
  seven_days_ago=$((current_date - 7*24*60*60))
  bucket_name_prefix="^s3-csi-k8s-e2e-"

  # Clean up standard S3 buckets
  echo "Cleaning up standard S3 buckets..."
  aws s3 ls --region ${REGION} | while read -r date_part time_part bucket_name; do
    if [[ "$bucket_name" =~ $bucket_name_prefix ]]; then
      # Convert bucket date to seconds since epoch
      bucket_date=$(date -d "$date_part $time_part" +%s 2>/dev/null || echo "0")

      # Delete if bucket is older than 7 days
      if [[ "$bucket_date" -lt "$seven_days_ago" ]]; then
        echo "Deleting old standard bucket: $bucket_name (created: $date_part $time_part)"
        aws s3 rb "s3://${bucket_name}" --force --region ${REGION}
      fi
    fi
  done

  # Clean up S3 Express (Directory) buckets
  echo "Cleaning up S3 Express buckets..."
  aws s3api list-directory-buckets --region ${REGION} --query 'Buckets[*].[Name,CreationDate]' --output text 2>/dev/null | while read -r bucket_name creation_date; do
    if [[ "$bucket_name" =~ $bucket_name_prefix ]]; then
      # Convert bucket date to seconds since epoch
      bucket_date=$(date -d "$creation_date" +%s 2>/dev/null || echo "0")

      # Delete if bucket is older than 7 days
      if [[ "$bucket_date" -lt "$seven_days_ago" ]]; then
        echo "Deleting old S3 Express bucket: $bucket_name (created: $creation_date)"
        aws s3 rm s3://"${bucket_name}"/ --recursive --region ${REGION}
        aws s3api delete-bucket --bucket "${bucket_name}" --region ${REGION}
      fi
    fi
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
    "${CSI_DRIVER_IRSA_ROLE_ARN}" \
    "${CLUSTER_TYPE}"
elif [[ "${ACTION}" == "run_tests" ]]; then
  set +e
  pushd tests/e2e-kubernetes
  KUBECONFIG=${KUBECONFIG} ginkgo -p -vv --github-output -timeout 60m -- --bucket-region=${REGION} --commit-id=${TAG} --bucket-prefix=${CLUSTER_NAME} --imds-available=${IMDS_AVAILABLE} --cluster-name=${CLUSTER_NAME} --cluster-type=${CLUSTER_TYPE}
  EXIT_CODE=$?
  print_cluster_info
  exit $EXIT_CODE
elif [[ "${ACTION}" == "run_upgrade_tests" ]]; then
  set +e
  pushd tests/e2e-kubernetes
  KUBECONFIG=${KUBECONFIG} ginkgo -vv --github-output -timeout 10h -- --bucket-region=${REGION} --commit-id=${TAG} --bucket-prefix=${CLUSTER_NAME} --imds-available=${IMDS_AVAILABLE} --cluster-name=${CLUSTER_NAME} --cluster-type=${CLUSTER_TYPE} --run-upgrade-tests
  EXIT_CODE=$?
  print_cluster_info
  exit $EXIT_CODE
elif [[ "${ACTION}" == "run_perf" ]]; then
  set +e
  pushd tests/e2e-kubernetes
  KUBECONFIG=${KUBECONFIG} go test -ginkgo.vv --bucket-region=${REGION} --commit-id=${TAG} --bucket-prefix=${CLUSTER_NAME} --performance=true --imds-available=${IMDS_AVAILABLE} --cluster-name=${CLUSTER_NAME} --cluster-type=${CLUSTER_TYPE}
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
    "${KUBECONFIG}" \
    "${CLUSTER_TYPE}"
elif [[ "${ACTION}" == "delete_cluster" ]]; then
  delete_cluster
elif [[ "${ACTION}" == "e2e_cleanup" ]]; then
  e2e_cleanup || true
elif [[ "${ACTION}" == "delete_old_buckets" ]]; then
  delete_old_buckets
else
  echo "ACTION := install_tools|create_cluster|install_driver|update_kubeconfig|run_tests|run_upgrade_tests|run_perf|e2e_cleanup|uninstall_driver|delete_cluster|delete_old_buckets"
  exit 1
fi
