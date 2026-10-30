#!/usr/bin/env bash

# Manages the IAM role the CSI Driver uses through EKS Pod Identity on eksctl clusters.
# EKS adds the `eks-cluster-name` and `eks-cluster-arn` session tags that `docs/iam/cluster-tag-policy.json` reads.
# `eksctl.sh` associates this role with the driver's service account when creating the cluster.

set -euox pipefail

# The trust policy names no cluster, so the role can be created before the cluster.
# Prints the role ARN, re-using the role if it already exists.
function pod_identity_create_role() {
  ROLE_NAME=${1}

  if ROLE_ARN=$(aws iam get-role --role-name "${ROLE_NAME}" --query Role.Arn --output text 2>/dev/null); then
    echo "${ROLE_ARN}"
    return 0
  fi

  ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
  TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {"Service": "pods.eks.amazonaws.com"},
      "Action": ["sts:AssumeRole", "sts:TagSession"],
      "Condition": {"StringEquals": {"aws:SourceAccount": "${ACCOUNT_ID}"}}
    }
  ]
}
EOF
)
  aws iam create-role --role-name "${ROLE_NAME}" \
    --assume-role-policy-document "${TRUST_POLICY}" \
    --query Role.Arn --output text
}

# Sets the role's inline policies to the given policy files, and deletes any other inline policy.
function pod_identity_set_policies() {
  ROLE_NAME=${1}
  shift

  WANTED_POLICIES=()
  for POLICY_FILE in "$@"; do
    POLICY_NAME=$(basename "${POLICY_FILE}" .json)
    WANTED_POLICIES+=("${POLICY_NAME}")
    aws iam put-role-policy --role-name "${ROLE_NAME}" --policy-name "${POLICY_NAME}" \
      --policy-document "file://${POLICY_FILE}"
  done

  for POLICY_NAME in $(aws iam list-role-policies --role-name "${ROLE_NAME}" --query 'PolicyNames[]' --output text); do
    if [[ ! " ${WANTED_POLICIES[*]} " == *" ${POLICY_NAME} "* ]]; then
      aws iam delete-role-policy --role-name "${ROLE_NAME}" --policy-name "${POLICY_NAME}"
    fi
  done
}

# The Pod Identity Association is deleted with the cluster.
function pod_identity_delete_role() {
  ROLE_NAME=${1}

  if ! aws iam get-role --role-name "${ROLE_NAME}" >/dev/null 2>&1; then
    return 0
  fi
  for POLICY_NAME in $(aws iam list-role-policies --role-name "${ROLE_NAME}" --query 'PolicyNames[]' --output text); do
    aws iam delete-role-policy --role-name "${ROLE_NAME}" --policy-name "${POLICY_NAME}"
  done
  aws iam delete-role --role-name "${ROLE_NAME}"
}

# EKS injects Pod Identity credentials (`AWS_CONTAINER_CREDENTIALS_FULL_URI`) into a pod when it's created,
# but only once the association has taken effect. A driver pod created right after the cluster can miss it,
# and then has no credentials since pods can't reach IMDS. Restart the driver until all its pods have them.
function pod_identity_wait_for_credentials() {
  KUBECTL_BIN=${1}
  KUBECONFIG=${2}

  for ATTEMPT in 1 2 3 4 5 6; do
    URIS=$(${KUBECTL_BIN} get pods -n kube-system -l app=s3-csi-node --kubeconfig "${KUBECONFIG}" \
      -o jsonpath='{range .items[*]}{.spec.containers[?(@.name=="s3-plugin")].env[?(@.name=="AWS_CONTAINER_CREDENTIALS_FULL_URI")].value}{"\n"}{end}')
    if [[ -n "${URIS}" ]] && ! grep -q '^$' <<<"${URIS}"; then
      return 0
    fi
    echo "CSI Driver pods have no EKS Pod Identity credentials yet (attempt ${ATTEMPT}), restarting them"
    sleep 10
    ${KUBECTL_BIN} rollout restart daemonset s3-csi-node -n kube-system --kubeconfig "${KUBECONFIG}"
    ${KUBECTL_BIN} rollout status daemonset s3-csi-node -n kube-system --timeout=5m --kubeconfig "${KUBECONFIG}"
  done

  echo "CSI Driver pods still have no EKS Pod Identity credentials"
  return 1
}
