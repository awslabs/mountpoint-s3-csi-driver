#!/bin/bash

set -euox pipefail

function helm_install() {
  INSTALL_PATH=${1}
  if [[ ! -e ${INSTALL_PATH}/helm ]]; then
    curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/master/scripts/get-helm-3
    chmod 700 get_helm.sh
    export USE_SUDO=false
    export HELM_INSTALL_DIR=${INSTALL_PATH}
    ./get_helm.sh
    rm get_helm.sh
  fi
}

function helm_uninstall_driver() {
  HELM_BIN=${1}
  KUBECTL_BIN=${2}
  RELEASE_NAME=${3}
  KUBECONFIG=${4}
  if [[ $($HELM_BIN list -A --kubeconfig $KUBECONFIG | grep $RELEASE_NAME) == *deployed* ]]; then
    $HELM_BIN uninstall $RELEASE_NAME --namespace kube-system --kubeconfig $KUBECONFIG
    $KUBECTL_BIN wait --for=delete pod --selector="app=s3-csi-node" -n kube-system --timeout=60s --kubeconfig $KUBECONFIG
  else
    echo "driver does not seem to be installed"
  fi
  $KUBECTL_BIN get pods -A --kubeconfig $KUBECONFIG
  $KUBECTL_BIN get CSIDriver --kubeconfig $KUBECONFIG
}

function helm_install_driver() {
  HELM_BIN=${1}
  KUBECTL_BIN=${2}
  RELEASE_NAME=${3}
  REPOSITORY=${4}
  TAG=${5}
  KUBECONFIG=${6}
  helm_uninstall_driver \
    "$HELM_BIN" \
    "$KUBECTL_BIN" \
    "$RELEASE_NAME" \
    "$KUBECONFIG"
  # temporary crutch to make eksctl working with pre-created cluster
  SA_CREATE=true
  if [[ "${KUBECONFIG}" == *"s3-csi-cluster.kubeconfig"* ]]; then
    SA_CREATE=false
  fi
  $HELM_BIN upgrade --install $RELEASE_NAME --namespace kube-system ./charts/aws-s3-csi-driver --values \
    ./charts/aws-s3-csi-driver/values.yaml \
    --set image.repository=${REPOSITORY} \
    --set image.tag=${TAG} \
    --set image.pullPolicy=Always \
    --set node.serviceAccount.create=${SA_CREATE} \
    --kubeconfig ${KUBECONFIG}
  $KUBECTL_BIN rollout status daemonset s3-csi-node -n kube-system --timeout=60s --kubeconfig $KUBECONFIG
  $KUBECTL_BIN get pods -A --kubeconfig $KUBECONFIG
  echo "s3-csi-node-image: $($KUBECTL_BIN get daemonset s3-csi-node -n kube-system -o jsonpath="{$.spec.template.spec.containers[:1].image}" --kubeconfig $KUBECONFIG)"
}
