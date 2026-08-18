# Note that all dependencies are compiled here, since isolated regions
# won't be able to pull dependencies over internet at run time.

ARG GO_VERSION

FROM public.ecr.aws/eks-distro-build-tooling/golang:${GO_VERSION}-al23 AS builder
ARG GINKGO_VERSION

ENV GOPROXY=direct

WORKDIR /src
COPY . .

# Compile the e2e test binary
RUN cd tests/e2e-kubernetes && go test -c -o /e2e.test .

# Install ginkgo CLI
RUN GOBIN=/usr/local/bin go install github.com/onsi/ginkgo/v2/ginkgo@${GINKGO_VERSION}

# --- Runtime stage ---
FROM public.ecr.aws/amazonlinux/amazonlinux:2023

# Install AWS CLI and basic utilities
RUN dnf install -y aws-cli && \
    dnf clean all

# Install kubectl
ARG KUBECTL_VERSION=1.33.3
RUN curl -LO "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl" && \
    chmod +x kubectl && mv kubectl /usr/local/bin/

# Copy pre-compiled binaries from builder
COPY --from=builder /e2e.test /usr/local/bin/e2e.test
COPY --from=builder /usr/local/bin/ginkgo /usr/local/bin/ginkgo

COPY <<'EOF' /entrypoint.sh
#!/bin/bash
set -euo pipefail

# Use service-specific endpoint env var for EKS beta/gamma endpoint override.
# This avoids --endpoint-url CLI flag which is unreliable across AWS CLI versions.
# AWS_ENDPOINT_URL_EKS only affects EKS calls, not S3/STS/CloudWatch/etc.
if [[ -n "${EKS_ENDPOINT:-}" ]]; then
    export AWS_ENDPOINT_URL_EKS="${EKS_ENDPOINT}"
    echo "EKS Endpoint: ${EKS_ENDPOINT} (via AWS_ENDPOINT_URL_EKS)"
fi
if [[ -n "${EKS_AUTH_ENDPOINT:-}" ]]; then
    export AWS_ENDPOINT_URL_EKS_AUTH="${EKS_AUTH_ENDPOINT}"
    echo "EKS Auth Endpoint: ${AWS_ENDPOINT_URL_EKS_AUTH} (via AWS_ENDPOINT_URL_EKS_AUTH)"
fi
if [[ -n "${TEST_IMAGE_URI:-}" ]]; then
    export TEST_POD_IMAGE="${TEST_IMAGE_URI}"
    echo "TEST_POD_IMAGE: ${TEST_POD_IMAGE}"
fi

export KUBECONFIG="/tmp/kubeconfig"
echo "${KUBECONFIG_BASE64}" | base64 -d > "${KUBECONFIG}"

aws eks create-addon --addon-name aws-mountpoint-s3-csi-driver \
	    --cluster-name "$CLUSTER_NAME" --region "$REGION" \
	    --addon-version "$ADDON_VERSION"

# Wait for addons to become active
echo "Waiting for aws-mountpoint-s3-csi-driver addon to become active..."
if ! aws eks wait addon-active --addon-name aws-mountpoint-s3-csi-driver \
    --cluster-name "$CLUSTER_NAME" --region "$REGION"; then
  echo "ERROR: aws-mountpoint-s3-csi-driver addon did not become active"
  aws eks describe-addon --addon-name aws-mountpoint-s3-csi-driver \
      --cluster-name "$CLUSTER_NAME" --region "$REGION" 2>&1 || true
  kubectl get pods -n kube-system -l app.kubernetes.io/name=aws-mountpoint-s3-csi-driver -o wide 2>&1 || true
  exit 1
fi

exec ginkgo -vv -timeout 60m \
    /usr/local/bin/e2e.test \
    -- \
    --bucket-region="${REGION}" \
    --commit-id=latest \
    --bucket-prefix="${CLUSTER_NAME}" \
    --imds-available=true \
    --cluster-name="${CLUSTER_NAME}" \
    --cluster-type=eksctl

EOF
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
