#!/usr/bin/env bash
# Deletes this environment's ROSA HCP cluster when it exists in OpenShift Cluster Manager (OCM)
# but is missing from Terraform state.
#
# A cancelled `terraform apply` can leave the cluster in OCM but not in state. `terraform destroy`
# then cannot delete it, nor the subnets and OIDC config the cluster still uses: it fails with
# "DependencyViolation" on the subnets and "there are clusters using OIDC config". The ROSA e2e
# workflow's next `terraform apply` fails too, with "Duplicate cluster name". The ROSA e2e workflow
# (e2e-rosa-tests.yaml) runs this script before `terraform apply` when state has no cluster, and
# the "Manual ROSA cluster cleanup" workflow (delete-rosa-cluster.yaml) runs it before
# `terraform destroy`.
#
# Run it from tests/e2e-kubernetes/rosa/ after `terraform init`, with AWS credentials for the
# cluster's account. It needs `terraform`, `rosa` and `jq` on PATH and these variables:
#   CLUSTER_NAME  this environment's cluster name, for example s3-csi-rosa-untrusted
#   ROSATOKEN     Red Hat offline token, as the "Get ROSA Token" step sets it. It is passed to
#                 `rosa login` as ROSA_TOKEN in that command's environment, never on its command
#                 line; later rosa commands use the session that `rosa login` saves.
#
# It only deletes a cluster that is named CLUSTER_NAME, uses the OIDC config in this
# environment's Terraform state, and belongs to the AWS account of the current credentials
# (`rosa list clusters` lists only that account's clusters, in every region). If a cluster has
# the name but the script cannot prove it is this environment's, it fails and says why.

set -euo pipefail
shopt -s inherit_errexit

: "${CLUSTER_NAME:?CLUSTER_NAME must be set}"
: "${ROSATOKEN:?ROSATOKEN must be set, as the Get ROSA Token step does}"

DELETE_TIMEOUT_MINUTES=60
# A poll that fails this many times in a row stops the script. A failed poll never counts as the
# cluster being gone.
MAX_CONSECUTIVE_POLL_FAILURES=3
# OCM reports state `error` when an uninstall fails. Fail after this many polls in a row in state
# `error`, counted only once the cluster has left the state it was in before the delete request:
# an orphan left by a failed install is often in state `error` already.
ERROR_POLLS_BEFORE_FAILING=3

# Keep only these non-sensitive facts: `terraform show -json` also prints secret values.
# Both resources live in nested modules (module.hcp.module.*), hence the recursion.
state_facts=$(terraform show -json | jq -c '
  [.values.root_module // {} | recurse(.child_modules[]?) | .resources[]? | select(.mode == "managed")] as $resources
  | ([$resources[] | select(.type == "rhcs_rosa_oidc_config") | .values] | first // {}) as $oidc
  | {
      cluster_in_state: any($resources[]; .type == "rhcs_cluster_rosa_hcp"),
      oidc_config_id: ($oidc.id // ""),
      oidc_endpoint_url: ($oidc.oidc_endpoint_url // "")
    }')

cluster_in_state=$(jq -r '.cluster_in_state' <<<"$state_facts")
if [[ "$cluster_in_state" == "true" ]]; then
  echo "Cluster $CLUSTER_NAME is in Terraform state, terraform destroy deletes it"
  exit 0
fi

# `rosa list clusters -o json` always prints a JSON array, so anything else is an error.
list_clusters() {
  rosa list clusters -o json |
    jq -c 'if type == "array" then . else error("rosa list clusters did not print a JSON array") end'
}

# The cluster's id and state, and the OIDC config it uses, read with `rosa describe cluster`:
# the command Red Hat documents for finding a cluster's OIDC endpoint URL.
describe_cluster() { # CLUSTER_ID
  rosa describe cluster -c "$1" -o json </dev/null | jq -c '
    if type == "object" then . else error("rosa describe cluster did not print a JSON object") end
    | {
        id,
        state,
        oidc_config_id: (.aws.sts.oidc_config.id // ""),
        oidc_endpoint_url: (.aws.sts.oidc_endpoint_url // "")
      }'
}

if ! ROSA_TOKEN="$ROSATOKEN" rosa login >/dev/null; then
  echo "::error::rosa login failed"
  exit 1
fi

if ! clusters=$(list_clusters); then
  echo "::error::Failed to list OCM clusters"
  exit 1
fi

match_ids=$(jq -r --arg name "$CLUSTER_NAME" '.[] | select(.name == $name) | .id' <<<"$clusters")
if [[ -z "$match_ids" ]]; then
  echo "No OCM cluster named $CLUSTER_NAME in this AWS account, nothing to delete"
  exit 0
fi

matches='[]'
while read -r match_id; do
  if ! described=$(describe_cluster "$match_id"); then
    echo "::error::Failed to describe OCM cluster $match_id ($CLUSTER_NAME)"
    exit 1
  fi
  matches=$(jq -c --argjson cluster "$described" '. + [$cluster]' <<<"$matches")
done <<<"$match_ids"
match_list=$(jq -r 'map("\(.id) (state \(.state), OIDC config \(.oidc_config_id | if . == "" then "none" else . end))") | join(", ")' <<<"$matches")

state_oidc_config_id=$(jq -r '.oidc_config_id' <<<"$state_facts")
state_oidc_endpoint_url=$(jq -r '.oidc_endpoint_url' <<<"$state_facts")
if [[ -z "$state_oidc_config_id" && -z "$state_oidc_endpoint_url" ]]; then
  echo "::error::Terraform state has no OIDC config, so the script cannot prove that the OCM cluster named $CLUSTER_NAME belongs to this environment: $match_list. If it is the orphan, delete it by hand with 'rosa delete cluster -c <id>'."
  exit 1
fi

# The module stores the endpoint URL without a scheme and OCM returns it with one, so compare
# them normalised, and never match on an empty value.
owned=$(jq -c --arg id "$state_oidc_config_id" --arg url "$state_oidc_endpoint_url" '
  def normalise: ltrimstr("https://") | rtrimstr("/");
  ($url | normalise) as $state_url
  | map(select(
      ($id != "" and .oidc_config_id == $id)
      or ($state_url != "" and (.oidc_endpoint_url | normalise) == $state_url)))' <<<"$matches")

owned_count=$(jq 'length' <<<"$owned")
if ((owned_count == 0)); then
  echo "::error::No OCM cluster named $CLUSTER_NAME uses the OIDC config in Terraform state (${state_oidc_config_id:-$state_oidc_endpoint_url}), so none is deleted. Found: $match_list. If one is the orphan, delete it by hand with 'rosa delete cluster -c <id>'."
  exit 1
fi
if ((owned_count > 1)); then
  owned_ids=$(jq -r 'map(.id) | join(", ")' <<<"$owned")
  echo "::error::Found $owned_count OCM clusters named $CLUSTER_NAME that use the OIDC config in Terraform state ($owned_ids). Delete the orphaned one by hand."
  exit 1
fi

cluster_id=$(jq -r '.[0].id' <<<"$owned")
cluster_state=$(jq -r '.[0].state' <<<"$owned")
echo "Deleting OCM cluster $cluster_id ($CLUSTER_NAME, state $cluster_state), which is missing from Terraform state"
if ! rosa delete cluster -c "$cluster_id" --yes; then
  echo "::error::rosa delete cluster failed for OCM cluster $cluster_id"
  exit 1
fi

# Poll instead of `rosa delete cluster --watch`, which reports success on any error reading the
# cluster state.
poll_failures=0
error_polls=0
state=$cluster_state
left_pre_delete_state=false
for ((minute = 0; ; minute++)); do
  if clusters=$(list_clusters); then
    poll_failures=0
    remaining=$(jq -c --arg id "$cluster_id" 'map(select(.id == $id))' <<<"$clusters")
    if [[ "$remaining" == "[]" ]]; then
      echo "OCM cluster $cluster_id deleted after about $minute minutes"
      exit 0
    fi
    state=$(jq -r '.[0].state' <<<"$remaining")
    echo "OCM cluster $cluster_id is $state ($minute of $DELETE_TIMEOUT_MINUTES minutes)"
    if [[ "$state" != "$cluster_state" ]]; then
      left_pre_delete_state=true
    fi
    if [[ "$state" == "error" ]] && $left_pre_delete_state; then
      error_polls=$((error_polls + 1))
      if ((error_polls >= ERROR_POLLS_BEFORE_FAILING)); then
        echo "::error::The uninstall of OCM cluster $cluster_id ($CLUSTER_NAME) failed: it left state $cluster_state after the delete request and is now in state error. See 'rosa logs uninstall -c $cluster_id'."
        exit 1
      fi
    else
      error_polls=0
    fi
  else
    poll_failures=$((poll_failures + 1))
    if ((poll_failures >= MAX_CONSECUTIVE_POLL_FAILURES)); then
      echo "::error::Failed to list OCM clusters $poll_failures times in a row while waiting for $cluster_id to be deleted"
      exit 1
    fi
    echo "::warning::Failed to list OCM clusters while waiting for $cluster_id to be deleted, trying again in a minute"
  fi
  if ((minute >= DELETE_TIMEOUT_MINUTES)); then
    break
  fi
  sleep 60
done

echo "::error::OCM cluster $cluster_id ($CLUSTER_NAME) still exists after $DELETE_TIMEOUT_MINUTES minutes, last seen in state $state"
exit 1
