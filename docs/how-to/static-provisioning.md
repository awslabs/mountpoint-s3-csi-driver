# How-To: Static Provisioning of S3 Buckets

This guide demonstrates how to statically provision an existing S3 bucket as a PersistentVolume (PV) in Kubernetes using the Scality S3 CSI Driver.

## Prerequisites

- Scality S3 CSI Driver and S3 credentials secret set up as described in the [Installation Guide](../installation.md)
- An existing S3 bucket

## Example: Static Provisioning

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv
spec:
  capacity:
    storage: 1200Gi # Ignored, required
  accessModes:
    - ReadWriteMany # Supported options: ReadWriteMany / ReadOnlyMany
  storageClassName: "" # Required for static provisioning
  claimRef: # To ensure no other PVCs can claim this PV
    namespace: default # Namespace is required even though it's in "default" namespace.
    name: s3-pvc # Name of your PVC
  mountOptions:
    - allow-delete
    - allow-overwrite
    # - region us-west-2 # Uncomment if needed to override the region specified in driver configuration
    # - prefix some-s3-prefix/ # Uncomment if needed to mount a specific prefix of the bucket, remove if not needed
  csi:
    driver: s3.csi.scality.com # Required
    volumeHandle: s3-csi-driver-volume
    volumeAttributes:
      bucketName: s3-csi-driver
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-pvc
spec:
  accessModes:
    - ReadWriteMany # Supported options: ReadWriteMany / ReadOnlyMany
  storageClassName: "" # Required for static provisioning
  resources:
    requests:
      storage: 1200Gi # Ignored, required
  volumeName: s3-pv # Name of your PV
---
apiVersion: v1
kind: Pod
metadata:
  name: s3-app
spec:
  containers:
    - name: app
      image: ubuntu
      command: ["/bin/sh"]
      args: ["-c", "echo 'Hello from the container!' >> /data/$(date -u).txt; tail -f /dev/null"]
      volumeMounts:
        - name: persistent-storage
          mountPath: /data
  volumes:
    - name: persistent-storage
      persistentVolumeClaim:
        claimName: s3-pvc
```

## 4. Verify the Setup

```bash
kubectl get pv s3-pv
kubectl get pvc s3-pvc
kubectl get pod s3-app
```

## 5. Next Steps

- For advanced configuration, see [Volume Configuration](../configuration/volume-configuration.md)
- For a full example, see [Minimal Helm Example](../../examples/minimal-helm.yaml)
