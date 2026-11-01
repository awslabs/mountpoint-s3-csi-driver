#!/bin/bash

set -euo pipefail


PREFIX="aws-s3-csi-e2e-"
MAX_AGE_DAYS=30
DRY_RUN=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --delete) DRY_RUN=0; shift ;;
    --max-age-days) MAX_AGE_DAYS="$2"; shift 2 ;;
    --prefix) PREFIX="$2"; shift 2 ;;
    *) echo "ERROR: Unknown argument: $1" >&2; exit 1 ;;
  esac
done

if ! command -v aws &> /dev/null; then
  echo "ERROR: 'aws' CLI is not installed or not in PATH" >&2
  exit 1
fi

if ! command -v jq &> /dev/null; then
  echo "ERROR: 'jq' is not installed or not in PATH" >&2
  exit 1
fi

if ! [[ "${MAX_AGE_DAYS}" =~ ^[0-9]+$ ]]; then
  echo "ERROR: --max-age-days must be a non-negative integer, got: ${MAX_AGE_DAYS}" >&2
  exit 1
fi

# Cut-off epoch: roles created strictly before this instant are stale.
NOW_EPOCH=$(date -u +%s)
CUTOFF_EPOCH=$(( NOW_EPOCH - MAX_AGE_DAYS * 86400 ))
CUTOFF_HUMAN=$(date -u -d "@${CUTOFF_EPOCH}" +"%Y-%m-%d %H:%M:%S UTC" 2>/dev/null \
  || date -u -r "${CUTOFF_EPOCH}" +"%Y-%m-%d %H:%M:%S UTC")

if [[ ${DRY_RUN} -eq 1 ]]; then
  echo "Mode:    DRY RUN (no changes will be made; pass --delete to delete)"
else
  echo "Mode:    DELETE"
fi
echo "Prefix:  ${PREFIX}"
echo "Max age: ${MAX_AGE_DAYS} days (delete roles created before ${CUTOFF_HUMAN})"
echo ""

# Convert an ISO-8601 timestamp (e.g. 2025-01-08T11:01:12+00:00) to epoch seconds.
to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -jf "%Y-%m-%dT%H:%M:%S%z" "${1/+00:00/+0000}" +%s
}

# Collect matching roles (name + createDate) across all pages.
echo "Listing IAM roles with prefix '${PREFIX}'..."
ROLES_JSON=$(aws iam list-roles \
  --query "Roles[?starts_with(RoleName, \`${PREFIX}\`)].{Name:RoleName,Created:CreateDate}" \
  --output json)

TOTAL=$(echo "${ROLES_JSON}" | jq 'length')
echo "Found ${TOTAL} role(s) matching the prefix."
echo ""

delete_role() {
  local role="$1"

  # Detach managed policies.
  local arn
  for arn in $(aws iam list-attached-role-policies --role-name "${role}" \
                 --query 'AttachedPolicies[].PolicyArn' --output text); do
    echo "    detach managed policy: ${arn}"
    aws iam detach-role-policy --role-name "${role}" --policy-arn "${arn}"
  done

  # Delete inline policies.
  local pol
  for pol in $(aws iam list-role-policies --role-name "${role}" \
                 --query 'PolicyNames[]' --output text); do
    echo "    delete inline policy: ${pol}"
    aws iam delete-role-policy --role-name "${role}" --policy-name "${pol}"
  done

  aws iam delete-role --role-name "${role}"
}

MATCHED=0
DELETED=0
FAILED=0

while IFS=$'\t' read -r name created; do
  [[ -z "${name}" ]] && continue
  created_epoch=$(to_epoch "${created}")
  if (( created_epoch < CUTOFF_EPOCH )); then
    MATCHED=$((MATCHED + 1))
    age_days=$(( (NOW_EPOCH - created_epoch) / 86400 ))
    if [[ ${DRY_RUN} -eq 1 ]]; then
      echo "[DRY RUN] would delete ${name} (created ${created}, ${age_days} days old)"
    else
      echo "Deleting ${name} (created ${created}, ${age_days} days old)"
      if delete_role "${name}"; then
        echo "    deleted"
        DELETED=$((DELETED + 1))
      else
        echo "    ERROR: failed to delete ${name}" >&2
        FAILED=$((FAILED + 1))
      fi
    fi
  fi
done < <(echo "${ROLES_JSON}" | jq -r '.[] | [.Name, .Created] | @tsv')

echo ""
echo "========================================"
echo "Matched (older than ${MAX_AGE_DAYS} days): ${MATCHED}"
if [[ ${DRY_RUN} -eq 1 ]]; then
  echo "Dry run complete. No roles were deleted. Re-run with --delete to delete them."
else
  echo "Deleted: ${DELETED}"
  [[ ${FAILED} -gt 0 ]] && echo "Failed:  ${FAILED}"
fi

[[ ${FAILED} -gt 0 ]] && exit 1
exit 0
