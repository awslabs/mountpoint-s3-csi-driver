
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

## Cluster profiles

A cluster can be created from a profile in `scripts/profiles`, which changes how it's set up, e.g. to have an autoscaler provision its nodes. Set `CLUSTER_PROFILE` for every action:

```bash
export CLUSTER_PROFILE=karpenter AMI_FAMILY=AmazonLinux2023 K8S_VERSION=1.36.1

ACTION=create_cluster tests/e2e-kubernetes/scripts/run.sh
ACTION=install_driver tests/e2e-kubernetes/scripts/run.sh
ACTION=run_scheduling_tests tests/e2e-kubernetes/scripts/run.sh
ACTION=delete_cluster FORCE=true tests/e2e-kubernetes/scripts/run.sh
```

The profiles are:
- `karpenter`: Karpenter, installed by eksctl.
- `cas`: Cluster Autoscaler, scaling a managed nodegroup from zero.
- `automode`: EKS Auto Mode.

Each has a pool of nodes its autoscaler provisions only for the pods targeting it, through the `e2e.s3.csi.aws.com/pool=default` label and NoSchedule taint on the pool's nodes. These nodes register with the `s3.csi.aws.com/agent-not-ready:NoExecute` startup taint, and pods can't reach IMDS on them, so the driver gets its credentials from EKS Pod Identity.

A profile is a directory with:
- `profile.sh`, defining `profile_setup`, run after creating or re-using the cluster, and `profile_teardown`, run before deleting it.
- An optional `eksctl-patch.json`, a JSON patch applied to the cluster's eksctl config. Changing it re-creates the profile's clusters.
- An optional `values.yaml`, applied on top of the driver chart's values when installing the driver.

## Scheduling tests

The scheduling tests (`testsuites/scheduling.go`, `run_scheduling_tests`) check that workloads get scheduled and mounted on the nodes an autoscaler provisions for them, on the profiles' pools. In CI, they run on the cells of `.github/matrix.yaml` with `tests: "scheduling"`.

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
