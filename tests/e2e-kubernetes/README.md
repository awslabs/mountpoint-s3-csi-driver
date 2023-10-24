
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
