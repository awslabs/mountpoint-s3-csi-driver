# Installation

## Prerequisites

* Kubernetes Version >= 1.23

## Installation

### Cluster setup (optional)
If you don't have an existing cluster, you can follow these steps to setup an EKS cluster. Clusters using the driver must use a supported OS (see [README](/README.md#distros-support-matrix)) on either x86_64 or ARM64.

#### Set cluster-name and a region:
```
export CLUSTER_NAME=mountpoint-s3-csi-cluster
export REGION=us-west-2
```

#### Create cluster

```
eksctl create cluster \
  --name $CLUSTER_NAME \
  --region $REGION \
  --with-oidc \
  --ssh-access \
  --ssh-public-key <my-key>
```

#### Setup kubectl context

> [!NOTE]
> Ensure that you are using [version 2 of the AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) for this step.

```
aws eks update-kubeconfig --region $REGION --name $CLUSTER_NAME
```

### Configure access to S3

The driver requires IAM permissions to access your Amazon S3 bucket. We recommend using [Mountpoint's suggested IAM permission policy](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#iam-permissions). Alternatively, you can use the AWS managed policy AmazonS3FullAccess, available at ARN `arn:aws:iam::aws:policy/AmazonS3FullAccess`, but this managed policy grants more permissions than needed for the Mountpoint CSI driver. For more details on creating a policy and an IAM role, review ["Creating an IAM policy"](https://docs.aws.amazon.com/eks/latest/userguide/s3-csi.html#s3-create-iam-policy) and ["Creating an IAM role"](https://docs.aws.amazon.com/eks/latest/userguide/s3-csi.html#s3-create-iam-role) from the EKS User Guide.

The policy ARN will be referred to as `$ROLE_ARN` in the setup instructions and the name of the role will be `$ROLE_NAME`.

There are several methods to grant these IAM permissions to the driver:

- Using an IAM [instance profile](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_switch-role-ec2_instance-profiles.html): attach the policy to the instance profile IAM role and turn on access to [instance metadata](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-metadata.html) for the instance(s) on which the driver will run.
- EKS only: Using [IAM roles for service accounts](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html).
- Using a secret object: create an IAM user, attach the policy to it, then create a generic secret in the `kube-system` namespace with the IAM user's credentials. We don't recommend this option because it requires long-lived credentials.

#### Service Account configuration for EKS Clusters

EKS allows [using Kubernetes service accounts to authenticate requests to S3](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html). To set this up follow these steps:

##### Create a Kubernetes service account for the driver and attach the policy to the service account

> [!NOTE]
> The same service account name (`s3-csi-driver-sa`) must be specified both in this command and when creating a drivers pod (in the pod spec `deploy/kubernetes/base/node-daemonset.yaml`).

```
eksctl create iamserviceaccount \
    --name s3-csi-driver-sa \
    --namespace kube-system \
    --cluster $CLUSTER_NAME \
    --attach-policy-arn $ROLE_ARN \
    --approve \
    --role-name $ROLE_NAME \
    --region $REGION
```
##### [Optional] Validate the account was succesfully created
```
kubectl describe sa s3-csi-driver-sa --namespace kube-system
```

For more validation steps see the [EKS documentation](https://docs.aws.amazon.com/eks/latest/userguide/associate-service-account-role.html).

#### Secret Object setup

The CSI driver will read k8s secrets at `aws-secret.key_id` and `aws-secret.access_key` to pass keys to the driver. These keys are only read on startup, so must be in place before the driver starts. The following snippet can be used to create these secrets in the cluster:

```
kubectl create secret generic aws-secret \
    --namespace kube-system \
    --from-literal "key_id=${AWS_ACCESS_KEY_ID}" \
    --from-literal "access_key=${AWS_SECRET_ACCESS_KEY}"
```

### Deploy driver
You may deploy the Mountpoint for Amzon S3 CSI driver via Kustomize, Helm, or as an [Amazon EKS managed add-on](https://docs.aws.amazon.com/eks/latest/userguide/eks-add-ons.html#workloads-add-ons-available-eks).

#### Kustomize
```sh
kubectl apply -k "github.com/awslabs/mountpoint-s3-csi-driver/deploy/kubernetes/overlays/stable/"
```
> [!WARNING]
> Using the main branch to deploy the driver is not supported. The main branch may contain upcoming features incompatible with the currently released stable version of the driver.

#### Helm
- Add the `aws-mountpoint-s3-csi-driver` Helm repository.
```sh
helm repo add aws-mountpoint-s3-csi-driver https://awslabs.github.io/mountpoint-s3-csi-driver
helm repo update
```

- Install the latest release of the driver.
```sh
helm upgrade --install aws-mountpoint-s3-csi-driver \
    --namespace kube-system \
    aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver
```

Review the [configuration values](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/charts/aws-mountpoint-s3-csi-driver/values.yaml) for the Helm chart.

#### Once the driver has been deployed, verify the pods are running:
```sh
kubectl get pods -n kube-system -l app.kubernetes.io/name=aws-mountpoint-s3-csi-driver
```

### Volume Configuration Example
Follow the [README for examples](https://github.com/awslabs/mountpoint-s3-csi-driver/tree/main/examples/kubernetes/static_provisioning) on using the driver.

### Uninstalling the driver

Uninstall the self-managed Mountpoint for Amazon S3 CSI Driver with either Helm or Kustomize, depending on your installation method. If you are using the driver as an EKS add-on, see the [EKS documentation](https://docs.aws.amazon.com/eks/latest/userguide/managing-add-ons.html).

#### Helm

```
helm uninstall aws-mountpoint-s3-csi-driver --namespace kube-system
```

#### Kustomize

```
kubectl delete -k "github.com/awslabs/mountpoint-s3-csi-driver/deploy/kubernetes/overlays/stable/?ref=<YOUR-CSI-DRIVER-VERION-NUMBER>"
```

> [!WARNING]
> Executing this command will delete a service account `s3-csi-driver-sa` from your cluster, which may cause problems when installing the driver again on a EKS cluster (the re-created account won't include the `eks.amazonaws.com/role-arn` annotation). Please refer to [eksctl documentation](https://eksctl.io/usage/iamserviceaccounts/) for details of how to re-create the service account in this case.
