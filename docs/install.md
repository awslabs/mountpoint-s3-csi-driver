# Installation

## Prerequisites

* Kubernetes Version >= 1.20 

* If you are using a self managed cluster, ensure the flag `--allow-privileged=true` for `kube-apiserver`.

## Installation

### Cluster setup (optional)
If you don't have an existing cluster, you can follow these steps to setup an EKS cluster.

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

> Ensure that you are using aws cli v2 before executing

```
aws eks update-kubeconfig --region $REGION --name $CLUSTER_NAME
```

### Set up driver permissions
The driver requires IAM permissions to talk to Amazon S3 to manage the volume on user's behalf. AWS maintains a managed policy, available at ARN `arn:aws:iam::aws:policy/AmazonS3FullAccess`. 

For more information, review ["Creating the Amazon Mountpoint for S3 CSI driver IAM role for service accounts" from the EKS User Guide.](TODO: add AWS docs link)

### Deploy driver
You may deploy the Mountpoint for S3 CSI driver via Kustomize, Helm, or as an [Amazon EKS managed add-on].

#### Kustomize
```sh
kubectl apply -k "github.com/awslabs/mountpoint-s3-csi-driver/deploy/kubernetes/overlays/stable"
```
*Note: Using the main branch to deploy the driver is not supported as the main branch may contain upcoming features incompatible with the currently released stable version of the driver.*

#### Helm
- Add the `aws-mountpoint-s3-csi-driver` Helm repository.
```sh
helm repo add aws-mountpoint-s3-csi-driver ???
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

### Uninstalling the driver

Uninstall the self-managed Mountpoint for S3 CSI Driver with either Helm or Kustomize, depending on your installation method. If you are using the driver as an EKS add-on, see the [EKS documentation](https://docs.aws.amazon.com/eks/latest/userguide/managing-add-ons.html).

#### Helm

```
helm uninstall aws-mountpoint-s3-csi-driver --namespace kube-system
```

#### Kustomize

```
kubectl delete -k "github.com/awslabs/aws-mountpoint-s3-csi-driver/deploy/kubernetes/overlays/stable/?ref=release-<YOUR-CSI-DRIVER-VERION-NUMBER>"
```

### Cleanup
#### Kustomize
Delete the pod
```
kubectl delete -f examples/kubernetes/static_provisioning/static_provisioning.yaml
```

Note: If you use `kubectl delete -k deploy/kubernetes/overlays/dev` to delete the driver itself, it will also delete the service account. You can change the `node-serviceaccount.yaml` file to this to prevent having to re-connect it when deploying the driver next
```
---

apiVersion: v1
kind: ServiceAccount
metadata:
  name: s3-csi-driver-sa
  labels:
    app.kubernetes.io/name: aws-mountpoint-s3-csi-driver
    app.kubernetes.io/managed-by: eksctl
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::151381207180:role/AmazonS3CSIDriverFullAccess # CHANGE THIS ARN
```

#### Helm
Uninstall the driver
```
helm uninstall aws-mountpoint-s3-csi-driver --namespace kube-system
```
Note: This will not delete the service account.
