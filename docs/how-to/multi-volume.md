# Mounting Multiple S3 Buckets in a Single Pod

The Scality S3 CSI Driver allows you to mount multiple S3 buckets as separate volumes in a single Kubernetes pod.

## Prerequisites

- Scality S3 CSI Driver and S3 credentials secret(s) set up as described in the [Installation Guide](../installation.md)
- Two or more S3 buckets

## Example: Two PVs, Two PVCs, One Pod

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-bucket1
spec:
  capacity:
    storage: 1200Gi
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  claimRef:
    namespace: default
    name: s3-pvc-bucket1
  mountOptions:
    - allow-delete
    - allow-overwrite
  csi:
    driver: s3.csi.scality.com
    volumeHandle: s3-pv-bucket1-handle
    volumeAttributes:
      bucketName: bucket1-name
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-bucket2
spec:
  capacity:
    storage: 1200Gi
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  claimRef:
    namespace: default
    name: s3-pvc-bucket2
  mountOptions:
    - allow-delete
    - allow-overwrite
  csi:
    driver: s3.csi.scality.com
    volumeHandle: s3-pv-bucket2-handle
    volumeAttributes:
      bucketName: bucket2-name
```

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-pvc-bucket1
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  resources:
    requests:
      storage: 1200Gi
  volumeName: s3-pv-bucket1
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-pvc-bucket2
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  resources:
    requests:
      storage: 1200Gi
  volumeName: s3-pv-bucket2
```

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: multi-s3-volumes-pod
spec:
  containers:
    - name: app
      image: ubuntu
      command: ["/bin/sh"]
      args: ["-c", "ls /data/volume1; ls /data/volume2; sleep 3600"]
      volumeMounts:
        - name: s3-data-volume
          mountPath: /data/volume1
        - name: s3-logs-volume
          mountPath: /data/volume2
  volumes:
    - name: s3-data-volume
      persistentVolumeClaim:
        claimName: s3-pvc-bucket1
    - name: s3-logs-volume
      persistentVolumeClaim:
        claimName: s3-pvc-bucket2
```

!!! note
    If using different credentials for each bucket, create a secret for each and reference them in the PVs as described in the [Installation Guide](../installation.md).

## Verification

```bash
kubectl get pod multi-s3-volumes-pod
```

## References

- [Volume Configuration](../configuration/volume-configuration.md)
- [Installation Guide](../installation.md)
