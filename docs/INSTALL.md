# Installing Mountpoint for Amazon S3 CSI Driver

> [!NOTE]
> For the customers using [Amazon Elastic Kubernetes Service (EKS)](https://aws.amazon.com/eks/), we recommend using the official Amazon EKS add-on of Mountpoint for Amazon S3 CSI Driver. See [the Amazon EKS guide](https://docs.aws.amazon.com/eks/latest/userguide/s3-csi.html) for more details.

## Prerequisites

* Kubernetes Version >= 1.25
* A [supported Operating System (OS)](/README.md#distros-support-matrix) with **x86-64** or **arm64** architectures.

## Installation

### Cluster setup (optional)

If you don't have an existing cluster, you can follow these steps to setup an Amazon EKS cluster using [eksctl](https://eksctl.io/).

- Set a cluster-name and a region:

```bash
export CLUSTER_NAME=mountpoint-s3-csi-cluster
export REGION=us-west-2
```

- Create the cluster

```bash
eksctl create cluster \
  --name $CLUSTER_NAME \
  --region $REGION \
  --with-oidc \ # To enable IAM Roles for Service Accounts (IRSA)
  ...
```

See [eksctl documentation](https://eksctl.io/getting-started/) for more details on the configuration you can use while creating your cluster.

- Setup kubectl context

> [!NOTE]
> Ensure that you are using [version 2 of the AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) for this step.

```bash
aws eks update-kubeconfig --region $REGION --name $CLUSTER_NAME
```

### Configure access to Amazon S3

Assuming you have a cluster created, the next step is to ensure the CSI Driver will be able to access Amazon S3.

There's a few different options for providing credentials for the Mountpoint CSI Driver to use when accessing Amazon S3.
See [AWS Credentials](CONFIGURATION.md#aws-credentials) for instructions and different ways of configuring AWS credentials for accessing Amazon S3.

### Deploy Mountpoint for Amazon S3 CSI Driver

You may deploy the Mountpoint for Amazon S3 CSI Driver via [Amazon EKS managed add-on](https://docs.aws.amazon.com/eks/latest/userguide/eks-add-ons.html#workloads-add-ons-available-eks), [Helm](https://helm.sh/), or [Kustomize](https://github.com/kubernetes-sigs/kustomize).

#### Amazon EKS managed add-on

> [!NOTE]
> Mountpoint for Amazon S3 CSI Driver v2.0.0 EKS add-on is not available yet, but we're actively working to release it. In the meantime, if you want to use v2 features, you can consider using our Helm chart or Kustomization manifests.

See [the Amazon EKS guide](https://docs.aws.amazon.com/eks/latest/userguide/s3-csi.html) for more details on installing the Amazon EKS managed add-on of Mountpoint for Amazon S3 CSI Driver.

#### Helm

- Add the `aws-mountpoint-s3-csi-driver` Helm repository.
```sh
helm repo add aws-mountpoint-s3-csi-driver https://awslabs.github.io/mountpoint-s3-csi-driver
helm repo update
```

- Install the latest release of the CSI Driver.
```sh
helm upgrade --install aws-mountpoint-s3-csi-driver \
    --namespace kube-system \
    aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver
```

> [!NOTE]
> For Amazon EKS users, you need to pass your Role ARN here if you're using IAM roles for service accounts:
>
> ```bash
> $ helm upgrade --install aws-mountpoint-s3-csi-driver \
>    --namespace kube-system \
>    --set node.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::account:role/csi-driver-role-name" \
>    aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver
> ```

Review the [configuration values](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/charts/aws-mountpoint-s3-csi-driver/values.yaml) for customizing the Helm chart.

##### Configuring `nodeSelector` for the controller component

Starting from Mountpoint for Amazon S3 CSI Driver v2, the CSI Driver also includes a controller component.
This controller component has a permission to create pods in the `mount-s3` namespace, and its responsible for managing Mountpoint Pods. See [architecture of the controller component](./ARCHITECTURE.md#the-controller-component-aws-s3-csi-controller) for more details.

We recommend customers to separate the controller components from the nodes running workloads using `controller.nodeSelector`, for example:

```sh
helm upgrade --install aws-mountpoint-s3-csi-driver \
    --namespace kube-system \
    --set controller.nodeSelector."kubernetes\.io/role"="control-plane" \
    aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver
```

#### Kustomize

```sh
kubectl apply -k "github.com/awslabs/mountpoint-s3-csi-driver/deploy/kubernetes/overlays/stable/"
```

> [!WARNING]
> Using the main branch to deploy the CSI Driver is not supported. The main branch may contain upcoming features incompatible with the currently released stable version of the CSI Driver.

#### Once Mountpoint for Amazon S3 CSI Driver has been deployed, verify the pods are running:

```sh
$ kubectl get pods -n kube-system -l app.kubernetes.io/name=aws-mountpoint-s3-csi-driver
NAME                                 READY   STATUS    RESTARTS   AGE
s3-csi-controller-79f7fb99c8-7jjjz   1/1     Running   0          26m
s3-csi-node-jj7rz                    3/3     Running   0          26m
s3-csi-node-zwfp6                    3/3     Running   0          26m
```

### Deploying an example application using an Amazon S3 volume

Follow the [README for examples](https://github.com/awslabs/mountpoint-s3-csi-driver/tree/main/examples/kubernetes/static_provisioning) on using the CSI Driver.

### Uninstalling Mountpoint for Amazon S3 CSI Driver

Uninstall the self-managed Mountpoint for Amazon S3 CSI Driver with either Helm or Kustomize, depending on your installation method. If you are using the driver as an Amazon EKS add-on, see the [Amazon EKS documentation](https://docs.aws.amazon.com/eks/latest/userguide/managing-add-ons.html).

#### Helm

```bash
helm uninstall aws-mountpoint-s3-csi-driver --namespace kube-system
```

#### Kustomize

```bash
kubectl delete -k "github.com/awslabs/mountpoint-s3-csi-driver/deploy/kubernetes/overlays/stable/?ref=<YOUR-CSI-DRIVER-VERSION-NUMBER>"
```

> [!WARNING]
> Executing this command will delete a service account `s3-csi-driver-sa` from your cluster, which may cause problems when installing the driver again on a Amazon EKS cluster (the re-created account won't include the `eks.amazonaws.com/role-arn` annotation). Please refer to [eksctl documentation](https://eksctl.io/usage/iamserviceaccounts/) for details of how to re-create the service account in this case.
