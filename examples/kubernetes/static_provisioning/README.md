# Static Provisioning Example
This example shows how to make a static provisioned Mountpoint for S3 persistent volume (PV) mounted inside a container.

## Examples in this folder
- `static_provisioning.yaml` - spawning a pod which creates a file with name as the current date/time
- `non_root.yaml` - same as above, but the pod is spawned as non-root (uid `1000`, gid `2000`)
- `s3_express_specify_az.yaml` - same as above, but this uses a [S3 Express One Zone](https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-express-one-zone.html) directory bucket and shows how to specify the availability zone (AZ) of the [persistent volume](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-affinity) to co-locate the pods with the bucket for lower latency access
- `multiple_buckets_one_pod.yaml` - same as above, with multiple buckets mounted in one pod. Note: when mounting multiple buckets in the same pod, the `volumeHandle` must be unique as specified in the [CSI documentation](https://kubernetes.io/docs/concepts/storage/volumes/#csi).
- `multiple_pods_one_pv.yaml` - same as above, with multiple pods mounting the same persistent volume, in Deployment kind, that can be used to scale pods, using HPA or other method to create more replicas. This example also demonstrates the Pod Sharing feature, where multiple workloads can share a single Mountpoint instance for better resource utilization. See [Pod Sharing](../../../docs/MOUNTPOINT_POD_SHARING.md) for more details.
- `caching.yaml` - shows how to configure mountpoint to use a cache directory. See the [Mountpoint documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#caching-configuration) for more details on caching options. Please thumbs up [#11](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/141) or add details about your use case if you want improvements in this area.
- `kms_sse.yaml` - demonstrates using SSE-KMS encryption with a customer supplied key id. See the [Mountpoint documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#data-encryption) for more details.
- `aws_max_attempts.yaml` - configure the number of retries for requests to S3. This option is passed to Mountpoint as the `AWS_MAX_ATTEMPTS` environment variable. See the [Mountpoint configuration documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#other-s3-bucket-configuration) for more details.
## Configure
### Edit [Persistent Volume](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/examples/kubernetes/static_provisioning/static_provisioning.yaml)
> Note: This example assumes your S3 bucket has already been created. If you need to create a bucket, follow the [S3 documentation](https://docs.aws.amazon.com/AmazonS3/latest/userguide/creating-bucket.html) for a general purpose bucket or the [S3 Express One Zone documentation](https://docs.aws.amazon.com/AmazonS3/latest/userguide/directory-bucket-create.html) for a directory bucket.
- Bucket name (required): `PersistentVolume -> csi -> volumeAttributes -> bucketName`
- Bucket region (if bucket and cluster are in different regions): `PersistentVolume -> csi -> mountOptions`
- [Mountpoint configurations](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md) can be added in the `mountOptions` of the Persistent Volume spec.

See [Static Provisioning](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#static-provisioning) configuration page for more details.

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
