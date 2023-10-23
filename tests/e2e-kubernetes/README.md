
## Usage
From repository root:
```
make e2e E2E_KUBECONFIG=~/.kube/config E2E_REGION=eu-west-1
```

## Prerequisites
- existing k8s cluster (e.g. EKS)
- `kubectl` in $PATH
- `kubeconfig` setting up access to k8s cluster
- driver deployed in the cluster
- aws credentials with access to s3 (create/delete buckets, read/write/list objects)

## Kernel pacthes
We're not spawning clusters for tests invocations, but are using an existent EKS cluster. This cluster requires **kernel patches** to be applied from time to time, which [must be invoked manually](https://docs.aws.amazon.com/eks/latest/userguide/update-managed-node-group.html#mng-update) until we implement an automation for it. To apply kernel patches:
- login to <> account;
- run:

```
eksctl upgrade nodegroup \
  --name=node-group-name \
  --cluster=my-cluster \
  --region=us-east-1
```