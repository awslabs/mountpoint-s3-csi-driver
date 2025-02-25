#!/usr/bin/env bash

set -euox pipefail

# If the cluster is not older than this, it will be re-used.
MAX_CLUSTER_AGE_SECONDS=$((3 * 24 * 60 * 60)) # 3 days

OS_ARCH=$(go env GOOS)-amd64

function kops_install() {
  INSTALL_PATH=${1}
  KOPS_VERSION=${2}
  if [[ -e "${INSTALL_PATH}"/kops ]]; then
    INSTALLED_KOPS_VERSION=$("${INSTALL_PATH}"/kops version)
    if [[ "$INSTALLED_KOPS_VERSION" == *"$KOPS_VERSION"* ]]; then
      echo "KOPS $INSTALLED_KOPS_VERSION already installed!"
      return
    fi
  fi
  KOPS_DOWNLOAD_URL=https://github.com/kubernetes/kops/releases/download/v${KOPS_VERSION}/kops-${OS_ARCH}
  curl -L -X GET "${KOPS_DOWNLOAD_URL}" -o "${INSTALL_PATH}"/kops
  chmod +x "${INSTALL_PATH}"/kops
}

function kops_is_cluster_too_old() {
  CLUSTER_NAME=${1}
  BIN=${2}
  KOPS_STATE_FILE=${3}

  CREATED_TIME=$(${BIN} get cluster --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}" -o json | jq -r '.metadata.creationTimestamp')
  CURRENT_TIME=$(date +%s)
  CLUSTER_TIME=$(date -d "${CREATED_TIME}" +%s)

  [ $((CURRENT_TIME - CLUSTER_TIME)) -gt ${MAX_CLUSTER_AGE_SECONDS} ]
  return $?
}

function kops_compute_cluster_spec_hash() {
  INSTANCE_TYPE=${1}
  ZONES=${2}
  AMI_ID=${3}
  KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE=${4}

  echo -n "${INSTANCE_TYPE}-${ZONES}-${AMI_ID}-${KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE}" | sha256sum | cut -d' ' -f1
}

# Checks whether existing cluster matches with expected specs to decide whether to re-use it.
function kops_cluster_matches_specs() {
  CLUSTER_NAME=${1}
  BIN=${2}
  KOPS_STATE_FILE=${3}
  DESIRED_HASH=${4}
  CURRENT_HASH=$(${BIN} get cluster --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}" -o json | jq -r '.spec.cloudLabels.ClusterSpecHash // empty')

  [ "${DESIRED_HASH}" = "${CURRENT_HASH}" ]
  return $?
}

function kops_create_cluster() {
  CLUSTER_NAME=${1}
  BIN=${2}
  ZONES=${3}
  NODE_COUNT=${4}
  INSTANCE_TYPE=${5}
  AMI_ID=${6}
  K8S_VERSION=${7}
  CLUSTER_FILE=${8}
  KUBECONFIG=${9}
  KOPS_PATCH_FILE=${10}
  KOPS_PATCH_NODE_FILE=${11}
  KOPS_STATE_FILE=${12}
  SSH_KEY=${13}
  KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE=${14}

  CLUSTER_SPEC_HASH=$(kops_compute_cluster_spec_hash "${INSTANCE_TYPE}" "${ZONES}" "${AMI_ID}" "${KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE}")

  # Check if cluster exists and matches our specs
  if kops_cluster_exists "${CLUSTER_NAME}" "${BIN}" "${KOPS_STATE_FILE}"; then
    if ! kops_is_cluster_too_old "${CLUSTER_NAME}" "${BIN}" "${KOPS_STATE_FILE}" && \
       kops_cluster_matches_specs "${CLUSTER_NAME}" "${BIN}" "${KOPS_STATE_FILE}" "${CLUSTER_SPEC_HASH}"; then
      echo "Reusing existing cluster ${CLUSTER_NAME} as it matches specifications and it is not too old"
      return 0
    fi

    echo "Existing cluster ${CLUSTER_NAME} is either too old or doesn't match specifications. Re-creating..."
    kops_delete_cluster "$BIN" "$CLUSTER_NAME" "$KOPS_STATE_FILE" "true"
  fi

  ARGS=()
  if [ -n "$SSH_KEY" ]; then
    ARGS+=('--ssh-public-key' $SSH_KEY)
  fi

  ${BIN} create cluster --state "${KOPS_STATE_FILE}" \
    --zones "${ZONES}" \
    --node-count="${NODE_COUNT}" \
    --node-size="${INSTANCE_TYPE}" \
    --image="${AMI_ID}" \
    --kubernetes-version="${K8S_VERSION}" \
    --dry-run \
    --cloud aws \
    --cloud-labels="ClusterSpecHash=${CLUSTER_SPEC_HASH}" \
    -o yaml \
    ${ARGS[@]+"${ARGS[@]}"} \
    "${CLUSTER_NAME}" > "${CLUSTER_FILE}"

  kops_patch_cluster_file "$CLUSTER_FILE" "$KOPS_PATCH_FILE" "Cluster" ""
  kops_patch_cluster_file "$CLUSTER_FILE" "$KOPS_PATCH_NODE_FILE" "InstanceGroup" "Node"

  if [ -n "$KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE" ]; then
    kops_patch_cluster_file "$CLUSTER_FILE" "$KOPS_PATCH_NODE_SELINUX_ENFORCING_FILE" "InstanceGroup" "Node"
  fi

  ${BIN} create --state "${KOPS_STATE_FILE}" -f "${CLUSTER_FILE}"
  ${BIN} update cluster --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}" --yes
  ${BIN} export kubecfg --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}" --admin --kubeconfig "${KUBECONFIG}"
  ${BIN} validate cluster --state "${KOPS_STATE_FILE}" --wait 15m --kubeconfig "${KUBECONFIG}"
}

function kops_cluster_exists() {
  CLUSTER_NAME=${1}
  BIN=${2}
  KOPS_STATE_FILE=${3}
  set +e
  if ${BIN} get cluster --state "${KOPS_STATE_FILE}" "${CLUSTER_NAME}"; then
    set -e
    return 0
  else
    set -e
    return 1
  fi
}

function kops_delete_cluster() {
  BIN=${1}
  CLUSTER_NAME=${2}
  KOPS_STATE_FILE=${3}
  FORCE=${4:-false}

  if ! kops_cluster_exists "${CLUSTER_NAME}" "${BIN}" "${KOPS_STATE_FILE}"; then
    return 0
  fi

  # Skip deletion if cluster is not too old and force flag is not set
  if [ "${FORCE}" != "true" ] && ! kops_is_cluster_too_old "${CLUSTER_NAME}" "${BIN}" "${KOPS_STATE_FILE}"; then
    echo "Skipping deletion of cluster ${CLUSTER_NAME} to re-use it"
    return 0
  fi

  echo "Deleting cluster ${CLUSTER_NAME}"
  ${BIN} delete cluster --name "${CLUSTER_NAME}" --state "${KOPS_STATE_FILE}" --yes
}

# TODO switch this to python, work exclusively with yaml, use kops toolbox
# template/kops set?, all this hacking with jq stinks!
function kops_patch_cluster_file() {
  CLUSTER_FILE=${1}    # input must be yaml
  KOPS_PATCH_FILE=${2} # input must be yaml
  KIND=${3}            # must be either Cluster or InstanceGroup
  ROLE=${4}            # must be either Master or Node

  echo "Patching cluster $CLUSTER_NAME with $KOPS_PATCH_FILE"

  # Temporary intermediate files for patching, don't mutate CLUSTER_FILE until
  # the end
  CLUSTER_FILE_JSON=$CLUSTER_FILE.json
  CLUSTER_FILE_0=$CLUSTER_FILE.0
  CLUSTER_FILE_1=$CLUSTER_FILE.1

  # HACK convert the multiple yaml documents to an array of json objects
  yaml_to_json "$CLUSTER_FILE" "$CLUSTER_FILE_JSON"

  # Find the json objects to patch
  FILTER=".[] | select(.kind==\"$KIND\")"
  if [ -n "$ROLE" ]; then
    FILTER="$FILTER | select(.spec.role==\"$ROLE\")"
  fi
  jq "$FILTER" "$CLUSTER_FILE_JSON" > "$CLUSTER_FILE_0"

  # Patch only the json objects
  kubectl patch -f "$CLUSTER_FILE_0" --local --type merge --patch "$(cat "$KOPS_PATCH_FILE")" -o json > "$CLUSTER_FILE_1"
  mv "$CLUSTER_FILE_1" "$CLUSTER_FILE_0"

  # Delete the original json objects, add the patched
  # TODO Cluster must always be first?
  jq "del($FILTER)" "$CLUSTER_FILE_JSON" | jq ". + \$patched | sort" --slurpfile patched "$CLUSTER_FILE_0" > "$CLUSTER_FILE_1"
  mv "$CLUSTER_FILE_1" "$CLUSTER_FILE_0"

  # HACK convert the array of json objects to multiple yaml documents
  json_to_yaml "$CLUSTER_FILE_0" "$CLUSTER_FILE_1"
  mv "$CLUSTER_FILE_1" "$CLUSTER_FILE_0"

  # Done patching, overwrite original yaml CLUSTER_FILE
  mv "$CLUSTER_FILE_0" "$CLUSTER_FILE" # output is yaml

  # Clean up
  rm "$CLUSTER_FILE_JSON"
}

function yaml_to_json() {
  IN=${1}
  OUT=${2}
  kubectl patch -f "$IN" --local -p "{}" --type merge -o json | jq '.' -s > "$OUT"
}

function json_to_yaml() {
  IN=${1}
  OUT=${2}
  for ((i = 0; i < $(jq length "$IN"); i++)); do
    echo "---" >> "$OUT"
    jq ".[$i]" "$IN" | kubectl patch -f - --local -p "{}" --type merge -o yaml >> "$OUT"
  done
}
