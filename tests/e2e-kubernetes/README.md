
# Usage
> To recreate EKS cluster used in CI (run locally using CI's AWS account): `ACTION=create_cluster AWS_REGION=us-east-1 CLUSTER_TYPE=eksctl CI_ROLE_ARN=<TEST_IAM_ROLE> tests/e2e-kubernetes/scripts/run.sh`

All of the following commands are expected to be executed from repo root:

```bash
export AWS_REGION=us-east-1
export TAG=20cb9d919704522d93ac40914b760dbd0487bcf3 # CSI Driver image tag to install
export IMAGE_NAME="s3-csi-driver" # repository is inferred from current AWS account and region
export SSH_KEY=/home/csiuser/.ssh/k8s.private.pub # optional
export K8S_VERSION="1.31.0" # optional, must be a full version

ACTION=install_tools tests/e2e-kubernetes/scripts/run.sh
ACTION=create_cluster tests/e2e-kubernetes/scripts/run.sh
ACTION=update_kubeconfig tests/e2e-kubernetes/scripts/run.sh
ACTION=install_driver  tests/e2e-kubernetes/scripts/run.sh

# option 1: kubeconfig inferred from CLUSTER_TYPE and ARCH
ACTION=run_tests tests/e2e-kubernetes/scripts/run.sh
# option 2: kubeconfig set explicitly
export KUBECONFIG=/local/home/vlaad/local/aws-s3-csi-driver/tests/e2e-kubernetes/csi-test-artifacts/s3-csi-cluster.eksctl.1.31.0.k8s.local.kubeconfig
ACTION=run_tests tests/e2e-kubernetes/scripts/run.sh

ACTION=uninstall_driver tests/e2e-kubernetes/scripts/run.sh
ACTION=delete_cluster tests/e2e-kubernetes/scripts/run.sh
```

## Credentials on eksctl clusters

On eksctl clusters, the CSI Driver gets its credentials through [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html),
with the tag-based policy [`docs/iam/cluster-tag-policy.json`](../../docs/iam/cluster-tag-policy.json):

* `create_cluster` creates the IAM role `aws-s3-csi-e2e-<cluster name>` (see `scripts/pod-identity.sh`), sets its
  policies to `CSI_DRIVER_POD_IDENTITY_POLICY_FILES` (by default `docs/iam/cluster-tag-policy.json`) on every run, and
  associates it with `kube-system/s3-csi-driver-sa`. `delete_cluster` deletes the role with the cluster.
* Pods can't reach IMDS (`disablePodIMDS`), and the node role has no S3 permissions, so Mountpoint can't fall back to
  the node role. The EBS CSI Driver and CloudWatch Observability add-ons get their own EKS Pod Identity roles. Tests
  relying on IMDS, such as instance profile credentials, are skipped (`IMDS_AVAILABLE=false`).
* The tests tag every bucket they create with `eks:eks-cluster-name=<cluster name>`, and enable attribute-based
  access control (ABAC) on general purpose buckets, so the policy allows them. The `clustertag` tests check that
  buckets tagged for another cluster, untagged buckets and buckets without ABAC fail to mount.
* The cluster still has an IAM OIDC provider, which only the IRSA credential tests use.

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

The tests also create directory buckets, tagged for the cluster, which needs `s3express:CreateBucket` and
`s3express:TagResource` (`AmazonS3FullAccess` covers `s3:*` only).

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
