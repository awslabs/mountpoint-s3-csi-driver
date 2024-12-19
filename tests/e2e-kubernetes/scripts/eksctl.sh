#!/usr/bin/env bash

set -euox pipefail

function eksctl_install() {
  INSTALL_PATH=${1}
  EKSCTL_VERSION=${2}
  if [[ ! -e ${INSTALL_PATH}/eksctl ]]; then
    EKSCTL_DOWNLOAD_URL="https://github.com/weaveworks/eksctl/releases/download/v${EKSCTL_VERSION}/eksctl_$(uname -s)_amd64.tar.gz"
    curl --silent --location "${EKSCTL_DOWNLOAD_URL}" | tar xz -C "${INSTALL_PATH}"
    chmod +x "${INSTALL_PATH}"/eksctl
  fi
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

  eksctl_delete_cluster "$BIN" "$CLUSTER_NAME" "$REGION"

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
  ${BIN} create cluster -f "${CLUSTER_FILE}" --kubeconfig "${KUBECONFIG}"

  if [ -n "$CI_ROLE_ARN" ]; then
    ${BIN} create iamidentitymapping --cluster ${CLUSTER_NAME} --region=${REGION} \
      --arn ${CI_ROLE_ARN} --username admin --group system:masters \
      --no-duplicate-arns
  fi
}

function eksctl_delete_cluster() {
  BIN=${1}
  CLUSTER_NAME=${2}
  REGION=${3}
  if eksctl_cluster_exists "${BIN}" "${CLUSTER_NAME}"; then
    ${BIN} delete cluster "${CLUSTER_NAME}"
  fi
  STACK_NAME="eksctl-${CLUSTER_NAME}-cluster"
  aws cloudformation delete-stack --region ${REGION} --stack-name ${STACK_NAME}

  # GuardDury creates resources (namely an endpoint and a security group), which are not handled by eks cfn stack and prevents it from being deleted
  # https://docs.aws.amazon.com/guardduty/latest/ug/runtime-monitoring-agent-resource-clean-up.html#clean-up-guardduty-agent-resources-process
  VPC_ID=$(aws cloudformation describe-stacks --region ${REGION} --stack-name ${STACK_NAME} | jq -r '.["Stacks"][0]["Outputs"][] | select(.["OutputKey"]=="VPC") | .["OutputValue"]' || true)
  if [ -n "$VPC_ID" ]; then
    ENDPOINT=$(aws ec2 describe-vpc-endpoints --region ${REGION} | jq -r --arg VPC_ID "$VPC_ID" '.["VpcEndpoints"][] | select(.["VpcId"]==$VPC_ID and .["Tags"][0]["Key"]=="GuardDutyManaged" and .["Tags"][0]["Value"]=="true") | .["VpcEndpointId"]')
    if [ -n "$ENDPOINT" ]; then
      aws ec2 delete-vpc-endpoints --region ${REGION} --vpc-endpoint-ids ${ENDPOINT}
    fi

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

  aws cloudformation wait stack-delete-complete --region ${REGION} --stack-name ${STACK_NAME}
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

function delete_security_group() {
  REGION=${1}
  SECURITY_GROUP=${2}

  remaining_attemps=20
  while (( remaining_attemps-- > 0 ))
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

  remaining_attemps=20
  while (( remaining_attemps-- > 0 ))
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
