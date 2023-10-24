#!/bin/bash

set -euo pipefail

function kubectl_install() {
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl.sha256"
    echo "$(cat kubectl.sha256)  kubectl" | sha256sum --check
    sudo install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl
}

function setup_kubeconfig() {
    aws eks update-kubeconfig --region ${EKS_REGION} --name ${EKS_CLUSTER_NAME} --kubeconfig=${KUBECONFIG}
}

function check_pods() {
    kubectl get pods -A
}

kubectl_install
setup_kubeconfig
check_pods
