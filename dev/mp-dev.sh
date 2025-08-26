#!/usr/bin/env bash
#
# mp-dev.sh provides utilities for day-to-day development of the CSI Driver.
#
# It allows developers to setup a development environment (an ECR repository and an EKS cluster) with "setup" command,
# and also allows termination of the deveopment environment with "teradown" command.
#
# It also allows developers to deploy Helm chart and containers from source
# with "deploy-helm-chart" and "deploy-containers" commands respectively.
#
# == Requirements ==
# This script requires a functioning AWS CLI that can access to an AWS account
# with permissions to create ECR repositories and EKS clusters.
#
# This script also uses the following binaries: "aws", "eksctl", "kubectl", "helm" and "docker".
set -euo pipefail
# Uncomment for debugging
# set -x

# Default environment variables
: "${MOUNTPOINT_CSI_DEV_REGION:=eu-north-1}"
: "${MOUNTPOINT_CSI_DEV_ECR_REPO_NAME:=mp-dev}"
: "${MOUNTPOINT_CSI_DEV_CLUSTER_NAME:=mp-dev-cluster}"

usage() {
    echo "usage: $0 <command>"
    echo ""
    echo "available commands:"
    echo "  setup                    create ECR repository, EKS cluster, and deploy everything"
    echo "  teardown                 delete ECR repository and EKS cluster"
    echo "  deploy-helm-chart        deploy the Helm chart from source using the ECR repository"
    echo "  deploy-containers        build the containers images from source, push to ECR, and restart the CSI Driver pods"
    echo "                              --build-mountpoint: use Dockerfile.local instead of Dockerfile to build Mountpoint from source"
    echo "  deploy                   deploy both Helm chart and containers from source"
    echo "  help                     show this help message"
    echo ""
    echo "optional environment variables (with defaults):"
    echo "  MOUNTPOINT_CSI_DEV_REGION          AWS region for the dev stack (default: eu-north-1)"
    echo "  MOUNTPOINT_CSI_DEV_ECR_REPO_NAME   ECR repository name (default: mp-dev)"
    echo "  MOUNTPOINT_CSI_DEV_CLUSTER_NAME    EKS cluster name (default: mp-dev-cluster)"
    exit 1
}

validate_env_vars() {
    local missing_vars=()
    local required_vars=("$@")

    for var in "${required_vars[@]}"; do
        if [[ -z "${!var:-}" ]]; then
            missing_vars+=("$var")
        fi
    done

    if [[ ${#missing_vars[@]} -gt 0 ]]; then
        echo "error: missing required environment variables:"
        for var in "${missing_vars[@]}"; do
            echo "  $var"
        done
        echo ""
        exit 1
    fi
}

get_ecr_repository_url() {
    local repo_name="${MOUNTPOINT_CSI_DEV_ECR_REPO_NAME}"
    local region="${MOUNTPOINT_CSI_DEV_REGION}"
    local account_id="$(aws sts get-caller-identity --query Account --output text)"
    echo "${account_id}.dkr.ecr.${region}.amazonaws.com/${repo_name}"
}

create_ecr_repository() {
    local repo_name="${MOUNTPOINT_CSI_DEV_ECR_REPO_NAME}"
    local region="${MOUNTPOINT_CSI_DEV_REGION}"

    echo "checking if ECR repository '${repo_name}' exists in region '${region}'..."

    if aws ecr describe-repositories --repository-names "${repo_name}" --region "${region}" >/dev/null 2>&1; then
        echo "ECR repository '${repo_name}' already exists"
    else
        echo "creating ECR repository '${repo_name}'..."
        aws ecr create-repository \
            --repository-name "${repo_name}" \
            --region "${region}"
        echo "ECR repository '${repo_name}' created successfully"
    fi

    local ecr_url=$(get_ecr_repository_url)
    echo "ECR repository URL: ${ecr_url}"
}

delete_ecr_repository() {
    local repo_name="${MOUNTPOINT_CSI_DEV_ECR_REPO_NAME}"
    local region="${MOUNTPOINT_CSI_DEV_REGION}"

    echo "checking if ECR repository '${repo_name}' exists in region '${region}'..."

    # Check if repository exists
    if aws ecr describe-repositories --repository-names "${repo_name}" --region "${region}" >/dev/null 2>&1; then
        echo "deleting ECR repository '${repo_name}'..."
        aws ecr delete-repository \
            --repository-name "${repo_name}" \
            --region "${region}" \
            --force
        echo "ECR repository '${repo_name}' deleted successfully"
    else
        echo "ECR repository '${repo_name}' does not exist"
    fi
}

create_eks_cluster() {
    local cluster_name="${MOUNTPOINT_CSI_DEV_CLUSTER_NAME}"
    local region="${MOUNTPOINT_CSI_DEV_REGION}"

    echo "checking if EKS cluster '${cluster_name}' exists in region '${region}'..."

    if aws eks describe-cluster --name "${cluster_name}" --region "${region}" >/dev/null 2>&1; then
        echo "EKS cluster '${cluster_name}' already exists"

        # Update kubeconfig to make sure we can connect
        echo "updating kubeconfig for cluster '${cluster_name}'..."
        aws eks update-kubeconfig --name "${cluster_name}" --region "${region}"
    else
        echo "creating EKS cluster '${cluster_name}' using eksctl, this might take 15-20 minutes..."
        eksctl create cluster -f dev/mp-dev-cluster.yaml --timeout 1h
        echo "EKS cluster '${cluster_name}' created successfully"
    fi
}

delete_eks_cluster() {
    local cluster_name="${MOUNTPOINT_CSI_DEV_CLUSTER_NAME}"
    local region="${MOUNTPOINT_CSI_DEV_REGION}"

    echo "checking if EKS cluster '${cluster_name}' exists in region '${region}'..."

    # Check if cluster exists
    if aws eks describe-cluster --name "${cluster_name}" --region "${region}" >/dev/null 2>&1; then
        echo "deleting EKS cluster '${cluster_name}' using eksctl..."
        eksctl delete cluster --disable-nodegroup-eviction -f dev/mp-dev-cluster.yaml
        echo "EKS cluster '${cluster_name}' deleted successfully"
    else
        echo "EKS cluster '${cluster_name}' does not exist"
    fi
}

deploy_helm_chart() {
    local ecr_repository_url=$(get_ecr_repository_url)

    echo "deploying Helm chart..."
    helm upgrade --install aws-mountpoint-s3-csi-driver \
        --namespace kube-system \
        --set image.repository="${ecr_repository_url}" \
        --set image.pullPolicy=Always \
        --set image.tag=latest \
        --set experimental.dynamicVolumeProvisioningFromExistingBucket=true \
        ./charts/aws-mountpoint-s3-csi-driver
}

deploy_containers() {
    local ecr_repository_url=$(get_ecr_repository_url)
    local dockerfile="Dockerfile"

    while [[ $# -gt 0 ]]; do
        case $1 in
            --build-mountpoint)
                dockerfile="Dockerfile.local"
                echo "will build Mountpoint from source, customize Dockerfile.local for specifying the branch and the build arguments"
                shift
                ;;
            *)
                echo "error: unknown argument '$1' for deploy-containers"
                exit 1
                ;;
        esac
    done

    echo "deploying containers..."

    # Extract registry and image name from the ECR repository URL
    local registry="${ecr_repository_url%/*}"
    local image_name="${ecr_repository_url##*/}"

    # Clean up the marker file to ensure we build again each time
    rm -f .image-latest-linux-amd64-amazon

    # Build and push the image
    REGISTRY="${registry}" \
    REGION="${MOUNTPOINT_CSI_DEV_REGION}" \
    IMAGE_NAME="${image_name}" \
    ALL_ARCH_linux="amd64" \
    TAG="latest" \
    DOCKERFILE="${dockerfile}" \
      make login_registry all-push

    # Restart the node and controller pods
    kubectl -n kube-system delete po -lapp=s3-csi-controller
    kubectl -n kube-system delete po -lapp=s3-csi-node
}

deploy() {
    echo "running full deployment..."
    deploy_helm_chart
    deploy_containers "$@"
    echo "deployment complete!"
}

setup() {
    echo "running full setup..."

    create_ecr_repository
    create_eks_cluster
    deploy "$@"

    echo ""
    echo "setup complete!"
    echo "AWS region: ${MOUNTPOINT_CSI_DEV_REGION}"
    echo "ECR repository: $(get_ecr_repository_url)"
    echo "EKS cluster: ${MOUNTPOINT_CSI_DEV_CLUSTER_NAME}"
    echo ""
}

teardown() {
    echo "running teardown..."
    echo "WARNING: This will delete the ECR repository and EKS cluster!"
    echo "ECR repository: $(get_ecr_repository_url)"
    echo "EKS cluster: ${MOUNTPOINT_CSI_DEV_CLUSTER_NAME}"
    echo ""
    read -p "Are you sure you want to proceed? (yes/no): " -r
    echo
    if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
        echo "Teardown cancelled."
        exit 0
    fi

    delete_eks_cluster
    delete_ecr_repository

    echo ""
    echo "teardown complete!"
}

main() {
    if [ $# -eq 0 ]; then
        echo "error: no command provided"
        usage
    fi

    case "$1" in
        setup)
            shift
            setup "$@"
            ;;
        teardown)
            shift
            teardown "$@"
            ;;
        deploy-helm-chart)
            deploy_helm_chart
            ;;
        deploy-containers)
            shift
            deploy_containers "$@"
            ;;
        deploy)
            shift
            deploy "$@"
            ;;
        help|--help|-h)
            usage
            ;;
        *)
            echo "error: unknown command '$1'"
            usage
            ;;
    esac
}
main "$@"
