# Installation

## Prerequisites

* Kubernetes Version >= 1.25

## Installation
> [!NOTE]
> To install the Mountpoint for S3 CSI Driver using EKS Add-on (recommended) follow [the guide](https://docs.aws.amazon.com/eks/latest/userguide/s3-csi.html) on EKS.

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

Assuming you have a cluster created, the next step is to ensure the CSI Driver will be able to access Amazon S3.

There's a few different options for providing credentials for the Mountpoint CSI Driver to use when accessing Amazon S3.
See the [AWS credentials section of CONFIGURATION.md](./CONFIGURATION.md#aws-credentials) for instructions on how to setup IAM principals for accessing Amazon S3.

### Deploy driver
You may deploy the Mountpoint for Amazon S3 CSI driver via Kustomize, Helm, or as an [Amazon EKS managed add-on](https://docs.aws.amazon.com/eks/latest/userguide/eks-add-ons.html#workloads-add-ons-available-eks).

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

> [!NOTE]
> For EKS users, you need to pass your Role ARN here if you're using IAM roles for service accounts:
>
> ```bash
> $ helm upgrade --install aws-mountpoint-s3-csi-driver \
>    --namespace kube-system \
>    --set node.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::account:role/csi-driver-role-name" \
>    aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver
> ```

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
