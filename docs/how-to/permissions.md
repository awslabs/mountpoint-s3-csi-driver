# Managing File Permissions and Non-Root Access

The Scality S3 CSI Driver allows you to emulate file permissions and enable non-root access for pods using S3 volumes.

## Prerequisites

- Scality S3 CSI Driver and S3 credentials secret set up as described in the [Installation Guide](../installation.md)
- An existing S3 bucket

## Example: PV with Permission Mount Options

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-nonroot
spec:
  capacity:
    storage: 1200Gi
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  claimRef:
    namespace: default
    name: s3-pvc-nonroot
  mountOptions:
    - allow-delete
    - allow-overwrite
    - uid=1000
    - gid=1000
    - allow-other
    - file-mode=0664
    - dir-mode=0775
  csi:
    driver: s3.csi.scality.com
    volumeHandle: s3-pv-nonroot-handle
    volumeAttributes:
      bucketName: my-nonroot-bucket
```

## Example: PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-pvc-nonroot
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  resources:
    requests:
      storage: 1200Gi
  volumeName: s3-pv-nonroot
```

## Example: Pod with Non-Root User

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: s3-app-nonroot
spec:
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
  containers:
    - name: app
      image: ubuntu
      command: ["/bin/sh"]
      args: ["-c", "touch /data/test.txt; ls -l /data; sleep 3600"]
      volumeMounts:
        - name: persistent-storage
          mountPath: /data
  volumes:
    - name: persistent-storage
      persistentVolumeClaim:
        claimName: s3-pvc-nonroot
```

!!! note
    File and directory permissions are emulated by Mountpoint and the CSI driver. S3 does not enforce POSIX permissions. Use IAM and bucket policies for true access control.

## Verification

```bash
kubectl get pod s3-app-nonroot
kubectl exec s3-app-nonroot -- ls -l /data
```

## References

- [Volume Configuration](../configuration/volume-configuration.md)
- [Installation Guide](../installation.md)
