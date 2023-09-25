# Cluster setup

#### Set cluster-name and a region:
```
export CLUSTER_NAME=s3-csi-cluster-2
export REGION=eu-west-1
```

#### Create cluster

```
eksctl create cluster \
  --name $CLUSTER_NAME \
  --region $REGION \
  --with-oidc \
  --ssh-access \
  --ssh-public-key my-key
```

#### Setup kubectl context

> Ensure that you are using aws cli v2 before executing

```
aws eks update-kubeconfig --region $REGION --name $CLUSTER_NAME
```

# Configure access to S3

## From Amazon EKS

EKS allows to use kubernetes service accounts to authenticate requests to S3, read more [here](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html). To set this up follow the steps:

#### Create a Kubernetes service account for the driver and attach the AmazonS3FullAccess AWS-managed policy to the service account:
> Notice that the same service account name `s3-csi-driver-sa` must be specified when creating a drivers pod (already in pod spec `deploy/kubernetes/base/node-daemonset.yaml`)

```
eksctl create iamserviceaccount \
    --name s3-csi-driver-sa \
    --namespace kube-system \
    --cluster $CLUSTER_NAME \
    --attach-policy-arn arn:aws:iam::aws:policy/AmazonS3FullAccess \
    --approve \
    --role-name AmazonS3CSIDriverFullAccess \
    --region $REGION
```
#### [Optional] validate account was succesfully created
```
kubectl describe sa s3-csi-driver-sa --namespace kube-system
```

For more validation steps read more [here](https://docs.aws.amazon.com/eks/latest/userguide/associate-service-account-role.html).

## From on-premises k8s cluster

For development purposes [long-term access keys](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_access-keys.html) may be used. Those may be delivered  as a k8s secret through kustomize: put your access keys in `deploy/kubernetes/overlays/dev/credentials` file and use `kubectl apply -k deploy/kubernetes/overlays/dev` to deploy the driver.

Usage of long-term credentials for production accounts/workloads is discouraged in favour of temporary credentials obtained through X.509 authentication scheme, read more [here](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_common-scenarios_non-aws.html).

## Deploy the Driver
#### Stable
```
kubectl apply -k deploy/kubernetes/overlays/stable
```
#### Development
- this will use `:latest` tag which is pulled on every container recreation
- this will provide aws credentials specified in `deploy/kubernetes/overlays/dev/credentials` (file should exists, even if empty) to the driver
```
kubectl apply -k deploy/kubernetes/overlays/dev
```
To redeploy driver with an updated image:
```
kubectl rollout restart daemonset s3-csi-node -n kube-system
```
Verify new version was deployed:
```
$ kubectl get pods -A
NAMESPACE     NAME                       READY   STATUS        RESTARTS   AGE
<...>
kube-system   s3-csi-node-94mdh          3/3     Running       0          57s
kube-system   s3-csi-node-vtgnq          3/3     Running       0          55s

$ kubectl logs -f s3-csi-node-94mdh -n kube-system
<...>
I0922 12:11:20.465762       1 driver.go:51] Driver version: 0.1.0, Git commit: b36c8a52b999a48ca8b88f985ed862d54585f0dd, build date: 2023-09-22T11:58:15Z
<...>
```