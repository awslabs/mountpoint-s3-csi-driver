#!/bin/bash

set -euo pipefail

# Script to verify that all container images referenced in the Helm chart
# are available in public ECR before publishing the chart.
# This prevents publishing a Helm chart that references non-existent images.
#
# Images verified:
#   - CSI driver images (multi-arch, amd64, arm64)
#   - Node driver registrar sidecar
#   - Liveness probe sidecar
#   - Headroom pod image (pause container)
#
# Prerequisites:
#   - yq: YAML processor (https://github.com/mikefarah/yq)
#   - crane: Container registry tool (https://github.com/google/go-containerregistry/tree/main/cmd/crane)
#
# Note: AWS credentials are required, though not specific ones.

CHART_DIR="charts/aws-mountpoint-s3-csi-driver"
VALUES_FILE="${CHART_DIR}/values.yaml"
EKS_REGISTRY="602401143452.dkr.ecr.us-east-1.amazonaws.com"

echo "Checking prerequisites..."

if ! command -v yq &> /dev/null; then
  echo "ERROR: 'yq' is not installed or not in PATH"
  echo "Install from: https://github.com/mikefarah/yq"
  echo ""
  echo "On macOS: brew install yq"
  echo "On Linux: wget https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -O /usr/local/bin/yq && chmod +x /usr/local/bin/yq"
  exit 1
fi

if ! command -v crane &> /dev/null; then
  echo "ERROR: 'crane' is not installed or not in PATH"
  echo "Install from: https://github.com/google/go-containerregistry/tree/main/cmd/crane"
  echo ""
  echo "On macOS: brew install crane"
  echo "On Linux: go install github.com/google/go-containerregistry/cmd/crane@latest"
  echo "Or download binary from: https://github.com/google/go-containerregistry/releases"
  exit 1
fi

echo "✓ Prerequisites satisfied"
echo ""

if [[ ! -f "${VALUES_FILE}" ]]; then
  echo "ERROR: values.yaml not found at ${VALUES_FILE}"
  exit 1
fi

echo "Verifying all images referenced in ${VALUES_FILE}..."
echo ""

if ! aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin "${EKS_REGISTRY}"; then
  echo "ERROR: Failed to authenticate with ECR"
  exit 1
fi

FAILED=0

# Function to verify public ECR image exists using crane (no AWS credentials needed)
verify_public_ecr_image() {
  local full_image=$1
  
  echo "  Image: ${full_image}"
  
  # Use crane to get the image digest
  # crane handles authentication automatically for public ECR repositories
  if digest=$(crane digest "${full_image}" 2>&1); then
    echo "  Digest: ${digest}"
    echo "  Status: ✅ Found"
    echo ""
  else
    echo "  Status: ❌ NOT FOUND"
    echo ""
    FAILED=1
  fi
}

# Verify CSI driver images (the ones we build and publish)
MAIN_REPO=$(yq eval '.image.repository' "${VALUES_FILE}")
MAIN_TAG=$(yq eval '.image.tag' "${VALUES_FILE}")

echo "=== CSI Driver Image (Multi-arch Manifest) ==="
verify_public_ecr_image "${MAIN_REPO}:${MAIN_TAG}"

echo "=== CSI Driver Image (AMD64) ==="
verify_public_ecr_image "${MAIN_REPO}:${MAIN_TAG}-amd64"

echo "=== CSI Driver Image (ARM64) ==="
verify_public_ecr_image "${MAIN_REPO}:${MAIN_TAG}-arm64"

verify_sidecar_images_in_chart() {
  RENDERED_CHART=$(eval "$1")

  # Verify sidecar images
  NODE_REGISTRAR=$(echo "$RENDERED_CHART" | yq '.spec.template.spec.containers[] | select(.name == "node-driver-registrar") | .image')
  echo "=== Node Driver Registrar Sidecar ==="
  verify_public_ecr_image "${NODE_REGISTRAR}"

  LIVENESS_PROBE=$(echo "$RENDERED_CHART" | yq '.spec.template.spec.containers[] | select(.name == "liveness-probe") | .image')
  echo "=== Liveness Probe Sidecar ==="
  verify_public_ecr_image "${LIVENESS_PROBE}"
}

echo "== Verifying standard images =="
verify_sidecar_images_in_chart "helm template $CHART_DIR"

echo "== Verifying EKS Addon images =="
verify_sidecar_images_in_chart "helm template $CHART_DIR --set isEKSAddon=true --set sidecars.livenessProbe.image.containerRegistry=$EKS_REGISTRY --set sidecars.nodeDriverRegistrar.image.containerRegistry=$EKS_REGISTRY"

# Verify headroom pod image (used when experimental.reserveHeadroomForMountpointPods is enabled)
HEADROOM_IMAGE=$(yq eval '.experimental.headroomPodImage' "${VALUES_FILE}")

echo "=== Headroom Pod Image (Pause Container) ==="
verify_public_ecr_image "${HEADROOM_IMAGE}"

# Summary
echo "========================================"
if [[ ${FAILED} -eq 0 ]]; then
  echo "✅ SUCCESS: All images verified successfully!"
  echo "The Helm chart is safe to publish."
  exit 0
else
  echo "❌ FAILURE: One or more images are missing!"
  echo "DO NOT publish the Helm chart until all images are available."
  exit 1
fi
