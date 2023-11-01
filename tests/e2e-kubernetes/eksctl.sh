#!/bin/bash

set -euox pipefail

# does not actually create cluster yet
function eksctl_create_cluster() {
    EKS_CLUSTER_NAME=${1}
    EKS_REGION=${2}
    KUBECONFIG=${3}
    aws eks update-kubeconfig --region ${EKS_REGION} --name ${EKS_CLUSTER_NAME} --kubeconfig=${KUBECONFIG}
}

function eksctl_delete_cluster() {
    echo "eksctl is still using pre-created cluster"
}
