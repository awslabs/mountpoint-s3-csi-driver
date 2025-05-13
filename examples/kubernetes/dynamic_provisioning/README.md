# Dynamic Provisioning Example
This example shows how to use dynamic provisioning with the Mountpoint for Amazon S3 CSI Driver to create a persistent volume that mounts an S3 bucket.

## Prerequisites

- An existing S3 bucket (the bucket must already exist as the driver doesn't create buckets)
- Kubernetes cluster with the Mountpoint for Amazon S3 CSI Driver installed
- Proper IAM permissions to access the S3 bucket

## Configure

Edit [dynamic_provisioning.yaml](dynamic_provisioning.yaml):

- `parameters.bucketName`: Change to the name of your existing S3 bucket
- If you need to specify the region or other mount options, you can add them later to the PV that gets created

## Deploy

```
kubectl apply -f examples/kubernetes/dynamic_provisioning/dynamic_provisioning.yaml
```

## Check the pod is running

```
kubectl get pod s3-app
```

## Verify the volume is mounted

```
kubectl exec -it s3-app -- ls -la /data
```

You should see the contents of your S3 bucket mounted at `/data`.

## Cleanup

```
kubectl delete -f examples/kubernetes/dynamic_provisioning/dynamic_provisioning.yaml
```

Note: Deleting the resources will not delete the S3 bucket, as the CSI driver does not manage bucket lifecycle.