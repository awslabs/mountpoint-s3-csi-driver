#!/bin/bash
# build_test_image.sh --output <buildx output spec>
# Builds the e2e test container image.
# Go and Ginkgo versions are auto-discovered from the source.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

OUTPUT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) OUTPUT="$2"; shift 2 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

if [ -z "$OUTPUT" ]; then
  echo "Usage: $0 --output <buildx output spec>]"
  echo "Example: $0 --output 'type=oci,dest=out.tar'"
  echo "         $0 --output 'type=docker'"
  exit 1
fi

# Discover versions from source
GO_VERSION=$(grep '^go ' "$REPO_ROOT/go.mod" | awk '{print $2}')
GINKGO_VERSION=$(grep 'github.com/onsi/ginkgo/v' "$REPO_ROOT/tests/e2e-kubernetes/go.mod" | awk '{print $2}')

echo "Discovered GO_VERSION=${GO_VERSION}, GINKGO_VERSION=${GINKGO_VERSION}"

docker buildx create --use --driver docker-container
docker buildx build --platform "linux/amd64" \
  --build-arg "GO_VERSION=${GO_VERSION}" \
  --build-arg "GINKGO_VERSION=${GINKGO_VERSION}" \
  --output "$OUTPUT" \
  -f "$REPO_ROOT/tests/e2e-kubernetes/scripts/eks-addon/Dockerfile" \
  "$REPO_ROOT"
