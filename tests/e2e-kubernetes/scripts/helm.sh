#!/usr/bin/env bash

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
  if driver_installed ${HELM_BIN} ${RELEASE_NAME} ${KUBECONFIG}; then
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
  MOUNTER_KIND=${7}

  if [ "$MOUNTER_KIND" = "pod" ]; then
    USE_POD_MOUNTER=true
  else
    USE_POD_MOUNTER=false
  fi

  helm_uninstall_driver \
    "$HELM_BIN" \
    "$KUBECTL_BIN" \
    "$RELEASE_NAME" \
    "$KUBECONFIG"
  $HELM_BIN upgrade --install $RELEASE_NAME --namespace kube-system ./charts/aws-mountpoint-s3-csi-driver --values \
    ./charts/aws-mountpoint-s3-csi-driver/values.yaml \
    --set image.repository=${REPOSITORY} \
    --set image.tag=${TAG} \
    --set image.pullPolicy=Always \
    --set node.serviceAccount.create=true \
    --set node.podInfoOnMountCompat.enable=true \
    --set experimental.podMounter=${USE_POD_MOUNTER} \
    --kubeconfig ${KUBECONFIG}
  $KUBECTL_BIN rollout status daemonset s3-csi-node -n kube-system --timeout=60s --kubeconfig $KUBECONFIG
  $KUBECTL_BIN get pods -A --kubeconfig $KUBECONFIG
  echo "s3-csi-node-image: $($KUBECTL_BIN get daemonset s3-csi-node -n kube-system -o jsonpath="{$.spec.template.spec.containers[:1].image}" --kubeconfig $KUBECONFIG)"
}

function driver_installed() {
  HELM_BIN=${1}
  RELEASE_NAME=${2}
  KUBECONFIG=${3}
  set +e
  if [[ $($HELM_BIN list -A --kubeconfig $KUBECONFIG | grep $RELEASE_NAME) == *deployed* ]]; then
    set -e
    return 0
  else
    set -e
    return 1
  fi
}
