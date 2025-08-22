#!/usr/bin/env bash
set -euo pipefail
# Uncomment for debugging
# set -x

usage() {
    echo "usage: $0 <command>"
    echo ""
    echo "available commands:"
    echo "  deploy-helm-chart        deploy the Helm chart from source"
    echo "  deploy-containers        build the containers from source, push to ECR, and restart the Csi Driver pods"
    echo "                              --build-mountpoint: use Dockerfile.local instead of Dockerfile to build Mountpoint from source"
    echo "  deploy                   deploy both Helm chart and containers from source"
    echo "  help                     show this help message"
    echo ""
    echo "required environment variables:"
    echo "  MOUNTPOINT_CSI_DEV_ECR_REPOSITORY  ECR repository URL (e.g., 111122223333.dkr.ecr.eu-north-1.amazonaws.com/mp-dev)"
    echo "  MOUNTPOINT_CSI_DEV_REGION          AWS region for the dev stack (e.g., eu-north-1)"
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

deploy_helm_chart() {
    validate_env_vars "MOUNTPOINT_CSI_DEV_ECR_REPOSITORY"

    echo "deploying Helm chart..."
    helm upgrade --install aws-mountpoint-s3-csi-driver \
        --namespace kube-system \
        --set image.repository="${MOUNTPOINT_CSI_DEV_ECR_REPOSITORY}" \
        --set image.pullPolicy=Always \
        --set image.tag=latest \
        --set experimental.dynamicVolumeProvisioningFromExistingBucket=true \
        ./charts/aws-mountpoint-s3-csi-driver
}

deploy_containers() {
    validate_env_vars "MOUNTPOINT_CSI_DEV_ECR_REPOSITORY" "MOUNTPOINT_CSI_DEV_REGION"

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
    local registry="${MOUNTPOINT_CSI_DEV_ECR_REPOSITORY%/*}"
    local image_name="${MOUNTPOINT_CSI_DEV_ECR_REPOSITORY##*/}"

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

main() {
    if [ $# -eq 0 ]; then
        echo "error: no command provided"
        usage
    fi

    case "$1" in
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
