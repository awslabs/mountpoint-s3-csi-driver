# Static Provisioning Example
This example shows how to make a static provisioned Mountpoint for S3 persistent volume (PV) mounted inside container.

## Examples in this folder
- `static_provisioning.yaml` - spawning a pod which creates a file with name as the current date/time
- `non_root.yaml` - same as above, but the pod is spawned as non-root (uid `1000`, gid `2000`)
- `s3_express_specify_az.yaml` - same as above, but this uses a [S3 Express One Zone](https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-express-one-zone.html) directory bucket and shows how to specify the availability zone (AZ) of the [pod assignment](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-affinity) to co-locate the pod with the bucket for lower latency access

## Configure
### Edit [Persistent Volume](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/examples/kubernetes/static_provisioning/static_provisioning.yaml)
> Note: This example assumes your S3 bucket has already been created. If you need to create a bucket, follow the [S3 documentation](https://docs.aws.amazon.com/AmazonS3/latest/userguide/creating-bucket.html) for a general purpose bucket or the [S3 Express One Zone documentation](https://docs.aws.amazon.com/AmazonS3/latest/userguide/directory-bucket-create.html) for a directory bucket.
- Bucket name (required): `PersistentVolume -> csi -> volumeAttributes -> bucketName`
- Bucket region (if bucket and cluster are in different regions): `PersistentVolume -> csi -> mountOptions`
- [Mountpoint configurations](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md) can be added in the `mountOptions` of the Persistent Volume spec.


## Deploy
```
kubectl apply -f examples/kubernetes/static_provisioning/static_provisioning.yaml
```

## Check the pod is running
```
kubectl get pod s3-app
```

## [Optional] Check fc-app created a file in s3
```
$ aws s3 ls <bucket_name>
> 2023-09-18 17:36:17         26 Mon Sep 18 17:36:14 UTC 2023.txt
```

## Cleanup
```
kubectl delete -f examples/kubernetes/static_provisioning/static_provisioning.yaml
```