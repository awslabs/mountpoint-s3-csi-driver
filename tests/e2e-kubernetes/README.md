
# Usage
## Prerequisites
AWS credentials in ENVs with the following policies attached:
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

## Setting up the environment
> To recreate EKS cluster used in CI (run locally using CI's AWS account): `ACTION=create_cluster AWS_REGION=us-east-1 CLUSTER_TYPE=eksctl CI_ROLE_ARN=arn:aws:iam::239424963615:role/S3CSIDriverE2ETestsRole tests/e2e-kubernetes/run.sh`

All of the following commands are expected to be executed from repo root:

```bash
ACTION=install_tools tests/e2e-kubernetes/run.sh

ACTION=create_cluster AWS_REGION=us-east-1 CLUSTER_TYPE=kops tests/e2e-kubernetes/run.sh # set KOPS_STATE_FILE to your bucket when running locally

ACTION=update_kubeconfig AWS_REGION=us-east-1 CLUSTER_TYPE=kops tests/e2e-kubernetes/run.sh # set KOPS_STATE_FILE to your bucket when running locally

ACTION=install_driver AWS_REGION=us-east-1 CLUSTER_TYPE=kops IMAGE_NAME=s3-csi-driver TAG=v0.1.0 tests/e2e-kubernetes/run.sh

ACTION=uninstall_driver AWS_REGION=us-east-1 CLUSTER_TYPE=kops tests/e2e-kubernetes/run.sh

ACTION=delete_cluster AWS_REGION=us-east-1 CLUSTER_TYPE=kops tests/e2e-kubernetes/run.sh
```

## Running tests
### On cluster created by run.sh
`run_tests` command is expected to be executed from tests/e2e-kubernetes directory:
```bash
pushd tests/e2e-kubernetes
ACTION=run_tests AWS_REGION=us-east-1 CLUSTER_TYPE=kops TAG=v0.1.0 ./run.sh
popd
```

### On existing cluster
From repository root:
```
make e2e E2E_KUBECONFIG=~/.kube/config E2E_REGION=eu-west-1
```
> E2E_REGION specifies where to create bucket for test (should be the same as where cluster is located)

#### Prerequisites
- existing k8s cluster (e.g. EKS)
- `kubectl` in $PATH
- `kubeconfig` setting up access to k8s cluster
- driver deployed in the cluster
- aws credentials with access to s3 (create/delete buckets, read/write/list objects)