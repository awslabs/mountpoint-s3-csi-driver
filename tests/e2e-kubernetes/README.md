
# Usage
> To recreate EKS cluster used in CI (run locally using CI's AWS account): `ACTION=create_cluster AWS_REGION=us-east-1 CLUSTER_TYPE=eksctl CI_ROLE_ARN=<TEST_IAM_ROLE> tests/e2e-kubernetes/scripts/run.sh`

All of the following commands are expected to be executed from repo root:

```bash
export AWS_REGION=us-east-1
export TAG=20cb9d919704522d93ac40914b760dbd0487bcf3 # CSI Driver image tag to install
export IMAGE_NAME="s3-csi-driver" # repository is inferred from current AWS account and region
export SSH_KEY=/home/csiuser/.ssh/k8s.private.pub # optional
export K8S_VERSION="1.30.0" # optional, must be a full version

ACTION=install_tools tests/e2e-kubernetes/scripts/run.sh
ACTION=create_cluster tests/e2e-kubernetes/scripts/run.sh
ACTION=update_kubeconfig tests/e2e-kubernetes/scripts/run.sh
ACTION=install_driver  tests/e2e-kubernetes/scripts/run.sh

# option 1: kubeconfig inferred from CLUSTER_TYPE and ARCH
ACTION=run_tests tests/e2e-kubernetes/scripts/run.sh
# option 2: kubeconfig set explicitly
export KUBECONFIG=/local/home/vlaad/local/aws-s3-csi-driver/tests/e2e-kubernetes/csi-test-artifacts/s3-csi-cluster.eksctl.1.28.0.k8s.local.kubeconfig
ACTION=run_tests tests/e2e-kubernetes/scripts/run.sh

ACTION=uninstall_driver tests/e2e-kubernetes/scripts/run.sh
ACTION=delete_cluster tests/e2e-kubernetes/scripts/run.sh
```

## Prerequisites
To run command above its expected that you have AWS credentials in ENVs with the following policies attached:
```
AmazonEC2FullAccess
AmazonRoute53FullAccess
AmazonS3FullAccess
IAMFullAccess
AmazonVPCFullAccess
AmazonSQSFullAccess
AmazonEventBridgeFullAccess
AmazonSSMReadOnlyAccess
AWSCloudFormationFullAccess
EksAllAccess (non-managed see below, and https://eksctl.io/usage/minimum-iam-policies/)
```

### EksAllAccess
```
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": "eks:*",
            "Resource": "*"
        },
        {
             "Action": [
               "kms:CreateGrant",
               "kms:DescribeKey"
             ],
             "Resource": "*",
             "Effect": "Allow"
        },
        {
             "Action": [
               "logs:PutRetentionPolicy"
             ],
             "Resource": "*",
             "Effect": "Allow"
        }
    ]
}
```
