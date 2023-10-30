# Static Provisioning Example
This example shows how to make a static provisioned EFS persistent volume (PV) mounted inside container.

## Configure
### Edit [Persistent Volume](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/examples/kubernetes/static_provisioning/static_provisioning.yaml)
- Bucket name (required): `PersistentVolume -> csi -> volumeHandle`
- Bucket region (if bucket and cluster are in different regions): `PersistentVolume -> csi -> mountOptions`
- [Mountpoint configurations](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md) can be added in the `mountOptions` of the Persistent Volume spec.


## Deploy
```
kubectl apply -f examples/kubernetes/static_provisioning/static_provisioning.yaml
```

## Check the pod is running
```
kubectl get pod fc-app
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