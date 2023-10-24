#!/bin/bash

set -euo pipefail

export INSTALL_PATH=/usr/local/bin

function kubectl_install() {
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl.sha256"
    echo "$(cat kubectl.sha256)  kubectl" | sha256sum --check
    sudo install -o root -g root -m 0755 kubectl ${INSTALL_PATH}/kubectl
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
        --set image.repository=${REGISTRY}/s3-csi-driver \
        --set image.tag=${TAG} \
        --set image.pullPolicy=Always
    kubectl rollout status daemonset s3-csi-node -n kube-system --timeout=60s
    kubectl get pods -A
    echo "s3-csi-node-image: $(kubectl get daemonset s3-csi-node -n kube-system -o jsonpath="{$.spec.template.spec.containers[:1].image}")"
}

kubectl_install
helm_install
setup_kubeconfig
ensure_driver_not_installed
install_driver
