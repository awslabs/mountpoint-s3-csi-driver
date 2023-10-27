# Installation

## Prerequisites

* Kubernetes Version >= 1.20 

* If you are using a self managed cluster, ensure the flag `--allow-privileged=true` for `kube-apiserver`.

## Installation
### Set up driver permissions
The driver requires IAM permissions to talk to Amazon S3 to manage the volume on user's behalf. AWS maintains a managed policy, available at ARN `arn:aws:iam::aws:policy/AmazonS3FullAccess`. 

For more information, review ["Creating the Amazon Mountpoint for S3 CSI driver IAM role for service accounts" from the EKS User Guide.]

### Deploy driver
You may deploy the EBS CSI driver via Kustomize, Helm, or as an [Amazon EKS managed add-on].

### Kustomize

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
    aws-mountpoint-s3-csi-driver/aws-ebs-csi-driver
```