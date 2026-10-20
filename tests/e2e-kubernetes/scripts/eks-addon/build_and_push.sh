#!/bin/bash
# Builds the e2e test image and pushes it to ECR
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --account) AWS_ACCOUNT="$2"; shift 2 ;;
        --region)  AWS_REGION="$2"; shift 2 ;;
        --repo)    ECR_REPO="$2"; shift 2 ;;
        --tag)     IMAGE_TAG="$2"; shift 2 ;;
        --help)
            echo "Usage: $0 [--account ID] [--region REGION] [--repo NAME] [--tag TAG]"
            echo ""
            echo "Options:"
            echo "  --account   AWS account ID"
            echo "  --region    AWS region"
            echo "  --repo      ECR repository name"
            echo "  --tag       Image tag"
            exit 0
            ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

ECR_URI="${AWS_ACCOUNT}.dkr.ecr.${AWS_REGION}.amazonaws.com"
FULL_IMAGE="${ECR_URI}/${ECR_REPO}:${IMAGE_TAG}"
LOCAL_IMAGE="mp-s3-csi-e2e:${IMAGE_TAG}"

echo "=== Configuration ==="
echo "  Account:  ${AWS_ACCOUNT}"
echo "  Region:   ${AWS_REGION}"
echo "  Image:    ${FULL_IMAGE}"
echo ""

# Authenticate to ECR
echo "=== Logging in to ECR ==="
aws ecr get-login-password --region "${AWS_REGION}" | \
    docker login --username AWS --password-stdin "${ECR_URI}"

# Build
echo "=== Building image ==="
"${SCRIPT_DIR}/build_test_image.sh" --output "type=docker,name=${LOCAL_IMAGE}"

# Tag and push
echo "=== Pushing to ECR ==="
docker tag "${LOCAL_IMAGE}" "${FULL_IMAGE}"
docker push "${FULL_IMAGE}"

echo ""
echo "=== Done ==="
echo "Image pushed: ${FULL_IMAGE}"
echo ""
echo "To use locally, set testImageUris in your run definition:"
echo "  \"testImageUris\": \"{\\\"${IMAGE_TAG}\\\":\\\"${FULL_IMAGE}\\\"}\""
