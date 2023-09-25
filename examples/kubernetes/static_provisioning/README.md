# Static Provisioning Example

# Configure
- bucket name: `PersistentVolume -> csi -> volumeHandle`
- bucket region (if bucket and cluster are in different regions): `PersistentVolume -> csi -> volumeAttributes -> bucketRegion`

# Deploy
```
kubectl apply -f examples/kubernetes/static_provisioning/static_provisioning.yaml
```

# Check the pod is running
```
kubectl get pod fc-app
```

# [Optional] Check fc-app created a file in s3
```
$ aws s3 ls <bucket_name>
> 2023-09-18 17:36:17         26 Mon Sep 18 17:36:14 UTC 2023.txt
```

# Cleanup
```
kubectl delete -f examples/kubernetes/static_provisioning/static_provisioning.yaml
```