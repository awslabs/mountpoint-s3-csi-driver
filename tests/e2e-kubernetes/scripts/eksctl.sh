#!/usr/bin/env bash

set -euox pipefail

# If the cluster is not older than this, it will be re-used.
MAX_CLUSTER_AGE_SECONDS=$((3 * 24 * 60 * 60)) # 3 days
CW_LOG_RETENTION_DAYS=30

function eksctl_install() {
  INSTALL_PATH=${1}

  ARCH=amd64
  PLATFORM=$(uname -s)_$ARCH

  if [[ ! -e ${INSTALL_PATH}/eksctl ]]; then
    curl -sLO "https://github.com/eksctl-io/eksctl/releases/latest/download/eksctl_$PLATFORM.tar.gz"
    curl -sL "https://github.com/eksctl-io/eksctl/releases/latest/download/eksctl_checksums.txt" | grep "eksctl_$PLATFORM.tar.gz$" | sha256sum --check
    tar -xzf eksctl_$PLATFORM.tar.gz -C /tmp && rm eksctl_$PLATFORM.tar.gz
    sudo install -m 0755 /tmp/eksctl ${INSTALL_PATH} && rm /tmp/eksctl
  fi
}

function eksctl_is_cluster_too_old() {
  CLUSTER_NAME=${1}
  REGION=${2}

  CREATED_TIME=$(aws eks describe-cluster --name "${CLUSTER_NAME}" --region "${REGION}" --query 'cluster.createdAt' --output text)
  CURRENT_TIME=$(date +%s)
  CLUSTER_TIME=$(date -d "${CREATED_TIME}" +%s)

  [ $((CURRENT_TIME - CLUSTER_TIME)) -gt ${MAX_CLUSTER_AGE_SECONDS} ]
  return $?
}

function eksctl_compute_cluster_spec_hash() {
  NODE_TYPE=${1}
  ZONES=${2}
  EKSCTL_PATCH_SELINUX_ENFORCING_FILE=${3}
  EKSCTL_PATCH_PROFILE_FILE=${4:-}

  # Hash the profile patch's content, so that changing a profile re-creates its clusters.
  # Clusters without a profile keep their existing hash.
  PROFILE_PATCH_HASH=""
  if [ -n "$EKSCTL_PATCH_PROFILE_FILE" ]; then
    PROFILE_PATCH_HASH="-$(sha256sum < "$EKSCTL_PATCH_PROFILE_FILE" | cut -d' ' -f1)"
  fi

  echo -n "${NODE_TYPE}-${ZONES}-${EKSCTL_PATCH_SELINUX_ENFORCING_FILE}${PROFILE_PATCH_HASH}" | sha256sum | cut -d' ' -f1
}

# Checks whether existing cluster matches with expected specs to decide whether to re-use it.
function eksctl_cluster_matches_specs() {
  CLUSTER_NAME=${1}
  REGION=${2}
  DESIRED_HASH=${3}
  CURRENT_HASH=$(aws eks describe-cluster --name "${CLUSTER_NAME}" --region "${REGION}" --query 'cluster.tags.ClusterSpecHash' --output text)

  [ "${DESIRED_HASH}" = "${CURRENT_HASH}" ]
  return $?
}

function eksctl_create_cluster() {
  CLUSTER_NAME=${1}
  REGION=${2}
  KUBECONFIG=${3}
  CLUSTER_FILE=${4}
  BIN=${5}
  KUBECTL_BIN=${6}
  EKSCTL_PATCH_FILE=${7}
  ZONES=${8}
  CI_ROLE_ARN=${9}
  NODE_TYPE=${10}
  AMI_FAMILY=${11}
  K8S_VERSION=${12}
  EKSCTL_PATCH_SELINUX_ENFORCING_FILE=${13}
  EKSCTL_PATCH_PROFILE_FILE=${14:-}
  PRE_DELETE_HOOK=${15:-}

  CLUSTER_SPEC_HASH=$(eksctl_compute_cluster_spec_hash "${NODE_TYPE}" "${ZONES}" "${EKSCTL_PATCH_SELINUX_ENFORCING_FILE}" "${EKSCTL_PATCH_PROFILE_FILE}")

  # Check if cluster exists and matches our specs
  if eksctl_cluster_exists "${BIN}" "${CLUSTER_NAME}"; then
    if ! eksctl_is_cluster_too_old "${CLUSTER_NAME}" "${REGION}" && \
       eksctl_cluster_matches_specs "${CLUSTER_NAME}" "${REGION}" "${CLUSTER_SPEC_HASH}"; then
      echo "Reusing existing cluster ${CLUSTER_NAME} as it matches specifications and it is not too old"
      return 0
    fi

    echo "Existing cluster ${CLUSTER_NAME} is either too old or doesn't match specifications. Re-creating..."
    eksctl_delete_cluster "$BIN" "$CLUSTER_NAME" "$REGION" "true" "$PRE_DELETE_HOOK"
  fi

  create_log_group_if_absent "${CLUSTER_NAME}" "${REGION}" "${CW_LOG_RETENTION_DAYS}"

  # CAUTION: this may fail with "the targeted availability zone, does not currently have sufficient capacity to support the cluster" error, we may require a fix for that
  ${BIN} create cluster \
    --name $CLUSTER_NAME \
    --region $REGION \
    --node-type $NODE_TYPE \
    --node-ami-family $AMI_FAMILY \
    --with-oidc \
    --zones $ZONES \
    --version $K8S_VERSION \
    --dry-run > $CLUSTER_FILE

  CLUSTER_FILE_TMP="${CLUSTER_FILE}.tmp"
  ${KUBECTL_BIN} patch -f $CLUSTER_FILE --local --type json --patch "$(cat $EKSCTL_PATCH_FILE)" -o yaml > $CLUSTER_FILE_TMP
  mv $CLUSTER_FILE_TMP $CLUSTER_FILE

  if [ -n "$EKSCTL_PATCH_SELINUX_ENFORCING_FILE" ]; then
    ${KUBECTL_BIN} patch -f $CLUSTER_FILE --local --type json --patch "$(cat $EKSCTL_PATCH_SELINUX_ENFORCING_FILE)" -o yaml > $CLUSTER_FILE_TMP
    mv $CLUSTER_FILE_TMP $CLUSTER_FILE
  fi

  if [ -n "$EKSCTL_PATCH_PROFILE_FILE" ]; then
    ${KUBECTL_BIN} patch -f $CLUSTER_FILE --local --type json --patch "$(cat $EKSCTL_PATCH_PROFILE_FILE)" -o yaml > $CLUSTER_FILE_TMP
    mv $CLUSTER_FILE_TMP $CLUSTER_FILE
  fi

  ${BIN} create cluster -f "${CLUSTER_FILE}" --kubeconfig "${KUBECONFIG}"

  if [ -n "$CI_ROLE_ARN" ]; then
    ${BIN} create iamidentitymapping --cluster ${CLUSTER_NAME} --region=${REGION} \
      --arn ${CI_ROLE_ARN} --username admin --group system:masters \
      --no-duplicate-arns
  fi

  # Add read only cluster access to ReadOnly IAM Role
  READ_ONLY_ARN="arn:${AWS_PARTITION}:iam::${AWS_ACCOUNT_ID}:role/ReadOnly"
  ${BIN} create accessentry --cluster ${CLUSTER_NAME} --region ${REGION} --principal-arn ${READ_ONLY_ARN} --type STANDARD
  aws eks associate-access-policy --region ${REGION} --cluster-name ${CLUSTER_NAME} --principal-arn ${READ_ONLY_ARN} \
    --policy-arn arn:${AWS_PARTITION}:eks::aws:cluster-access-policy/AmazonEKSAdminViewPolicy --access-scope type=cluster

  # Only once the cluster is fully created, so that one that failed to come up doesn't match and gets re-created.
  CLUSTER_ARN=$(aws eks describe-cluster --name "${CLUSTER_NAME}" --region "${REGION}" --query 'cluster.arn' --output text)
  aws eks tag-resource --region "${REGION}" --resource-arn "${CLUSTER_ARN}" --tags ClusterSpecHash=${CLUSTER_SPEC_HASH}
}

function eksctl_delete_cluster() {
  BIN=${1}
  CLUSTER_NAME=${2}
  REGION=${3}
  FORCE=${4:-false}
  # Function to run before deleting an existing cluster, e.g. to remove resources that would keep it from being deleted.
  PRE_DELETE_HOOK=${5:-}

  STACK_NAME="eksctl-${CLUSTER_NAME}-cluster"
  if ! eksctl_cluster_exists "${BIN}" "${CLUSTER_NAME}"; then
    # Try to delete CloudFormation stack even if the cluster does not exists just in case
    # if the stack is stuck in `ROLLBACK_COMPLETE` status.
    if eksctl_is_cluster_cf_stack_exists "${STACK_NAME}" "${REGION}"; then
      aws cloudformation update-termination-protection --no-enable-termination-protection --region "${REGION}" --stack-name "${STACK_NAME}"
    fi
    eksctl_delete_cluster_cf_stack "${CLUSTER_NAME}" "${REGION}"
    eksctl_delete_leftover_cf_stacks "${CLUSTER_NAME}" "${REGION}"
    return 0
  fi

  # Skip deletion if cluster is not too old and force flag is not set
  if [ "${FORCE}" != "true" ] && ! eksctl_is_cluster_too_old "${CLUSTER_NAME}" "${REGION}"; then
    echo "Skipping deletion of cluster ${CLUSTER_NAME} to re-use it"
    return 0
  fi

  if [ -n "$PRE_DELETE_HOOK" ]; then
    "$PRE_DELETE_HOOK"
  fi

  aws cloudformation update-termination-protection --no-enable-termination-protection --region "${REGION}" --stack-name "${STACK_NAME}"
  ${BIN} delete cluster "${CLUSTER_NAME}" --wait --disable-nodegroup-eviction
  eksctl_delete_cluster_cf_stack "${CLUSTER_NAME}" "${REGION}"
}

function eksctl_cluster_exists() {
  BIN=${1}
  CLUSTER_NAME=${2}
  set +e
  if ${BIN} get cluster "${CLUSTER_NAME}"; then
    set -e
    return 0
  else
    set -e
    return 1
  fi
}

# CloudWatch Observability addon for EKS creates log groups with no retention policy.
# And it's cumbersome to configure retention via addon configuration.
# We pre-create these log groups if they don't exist and set the retention policy here.
function create_log_group_if_absent() {
  CLUSTER_NAME=${1}
  REGION=${2}
  RETENTION_DAYS=${3}

  container_insights_log_groups=(
    "/aws/containerinsights/${CLUSTER_NAME}/application"
    "/aws/containerinsights/${CLUSTER_NAME}/dataplane"
    "/aws/containerinsights/${CLUSTER_NAME}/host"
    "/aws/containerinsights/${CLUSTER_NAME}/performance"
  )

  for log_group in "${container_insights_log_groups[@]}"; do
    # Check if the log group exists
    if ! aws logs describe-log-groups --region ${REGION} --log-group-name-prefix "$log_group" \
        --query "logGroups[?logGroupName=='$log_group'] | length(@)" \
        --output text | grep -q "1"; then
      aws logs create-log-group --region ${REGION} --log-group-name "$log_group"
      aws logs put-retention-policy --region ${REGION} --log-group-name "$log_group" --retention-in-days "$RETENTION_DAYS"
    fi
  done
}

function eksctl_is_cluster_cf_stack_exists() {
    STACK_NAME=${1}
    REGION=${2}

    if aws cloudformation describe-stacks --region ${REGION} --stack-name ${STACK_NAME} &>/dev/null; then
        return 0
    else
        return 1
    fi
}

function eksctl_delete_cluster_cf_stack() {
    CLUSTER_NAME=${1}
    REGION=${2}

    STACK_NAME="eksctl-${CLUSTER_NAME}-cluster"

    # Check if stack exists before attempting to delete
    if ! eksctl_is_cluster_cf_stack_exists "${STACK_NAME}" "${REGION}"; then
        echo "CloudFormation stack ${STACK_NAME} does not exist, skipping cleanup"
        return 0
    fi

    # Delete any subnets orphaned by a CloudFormation SDK retry during stack creation.
    # CreateSubnet is not idempotent: if the first API call succeeds but the response times out
    # (a transient issue), the SDK retries and gets a CIDR conflict error. CFN sees only the
    # failure, records no PhysicalResourceId, and cannot clean up the subnet during rollback.
    # These orphaned subnets are still tagged with the stack name but are unknown to CFN,
    # and will block VPC deletion.
    SUBNETS=$(aws ec2 describe-subnets --region ${REGION} \
        --filters "Name=tag:aws:cloudformation:stack-name,Values=${STACK_NAME}" \
        --query 'Subnets[*].SubnetId' --output text)
    if [ -n "$SUBNETS" ]; then
        for SUBNET_ID in $SUBNETS; do
            aws ec2 delete-subnet --region ${REGION} --subnet-id ${SUBNET_ID} || true
        done
    fi

    aws cloudformation delete-stack --region ${REGION} --stack-name ${STACK_NAME}

    # GuardDury creates resources (namely an endpoint and a security group), which are not handled by eks cfn stack and prevents it from being deleted
    # https://docs.aws.amazon.com/guardduty/latest/ug/runtime-monitoring-agent-resource-clean-up.html#clean-up-guardduty-agent-resources-process
    VPC_ID=$(aws cloudformation describe-stacks --region ${REGION} --stack-name ${STACK_NAME} | jq -r '.["Stacks"][0]["Outputs"][] | select(.["OutputKey"]=="VPC") | .["OutputValue"]' || true)
    if [ -n "$VPC_ID" ]; then
      ENDPOINT=$(aws ec2 describe-vpc-endpoints --region ${REGION} | jq -r --arg VPC_ID "$VPC_ID" '.["VpcEndpoints"][] | select(.["VpcId"]==$VPC_ID and .["Tags"][0]["Key"]=="GuardDutyManaged" and .["Tags"][0]["Value"]=="true") | .["VpcEndpointId"]')
      if [ -n "$ENDPOINT" ]; then
        aws ec2 delete-vpc-endpoints --region ${REGION} --vpc-endpoint-ids ${ENDPOINT}
      fi

      if [ -n "$VPC_ID" ]; then
        # https://github.com/eksctl-io/eksctl/issues/7589
        ENIS=$(aws ec2 describe-network-interfaces --region ${REGION} | jq -r --arg VPC_ID "$VPC_ID" '.["NetworkInterfaces"][] | select(.["VpcId"]==$VPC_ID) | .NetworkInterfaceId')
        if [ -n "$ENIS" ]; then
          echo "${ENIS}" | while IFS= read -r ENI_ID ; do
            delete_eni ${REGION} ${ENI_ID}
          done
        fi

        SECURITY_GROUP=$(aws ec2 describe-security-groups --region ${REGION} | jq -r --arg VPC_ID "$VPC_ID" '.["SecurityGroups"][] | select(.["VpcId"]==$VPC_ID and .["GroupName"]!="default") | .["GroupId"]')
        if [ -n "$SECURITY_GROUP" ]; then
            # security group deletion only succeeds after a certain step of stack deletion was passed (namely subnets deletion),
            # after which stack deletion is blocked because of the security group, so we retry here until this step is completed
            delete_security_group ${REGION} ${SECURITY_GROUP}
        fi
      fi
    fi

    aws cloudformation wait stack-delete-complete --region ${REGION} --stack-name ${STACK_NAME}
}

# Deletes the cluster's other stacks, which eksctl leaves behind when it fails to delete the cluster's own stack,
# e.g. its Karpenter stack, which it deletes after that one.
function eksctl_delete_leftover_cf_stacks() {
    CLUSTER_NAME=${1}
    REGION=${2}

    STACKS=$(aws cloudformation describe-stacks --region ${REGION} \
        --query "Stacks[?starts_with(StackName, 'eksctl-${CLUSTER_NAME}-')].StackName" --output text)
    for STACK in ${STACKS}; do
        aws cloudformation update-termination-protection --no-enable-termination-protection --region ${REGION} --stack-name ${STACK}
        aws cloudformation delete-stack --region ${REGION} --stack-name ${STACK}
        aws cloudformation wait stack-delete-complete --region ${REGION} --stack-name ${STACK}
    done
}

function delete_security_group() {
  REGION=${1}
  SECURITY_GROUP=${2}

  remaining_attempts=20
  while (( remaining_attempts-- > 0 ))
  do
      if output=$(aws ec2 delete-security-group --region ${REGION} --group-id ${SECURITY_GROUP} 2>&1); then
        return
      fi
      if [[ $output == *"InvalidGroup.NotFound"* ]]; then
        return
      fi
      sleep 30
  done
}

function delete_eni() {
  REGION=${1}
  ENI_ID=${2}

  remaining_attempts=20
  while (( remaining_attempts-- > 0 ))
  do
      if output=$(aws ec2 delete-network-interface --network-interface-id ${ENI_ID} --region ${REGION} 2>&1); then
        return
      fi
      if [[ $output == *"InvalidNetworkInterfaceID.NotFound"* ]]; then
        return
      fi
      sleep 30
  done
}
