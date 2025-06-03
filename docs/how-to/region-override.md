# Overriding S3 Region

The Scality S3 CSI Driver allows you to specify the S3 region at two levels: globally for the driver instance and on a per-volume basis.

## Global S3 Region (Driver Level)

When you install the Scality S3 CSI Driver using Helm, you can set a default S3 region that will be used for all S3 operations unless overridden at the volume level.
This is configured via the `node.s3Region` parameter in your `values.yaml` file.

**Example `values.yaml`:**

```yaml
node:
  s3EndpointUrl: "https://your-s3-endpoint.example.com"
  s3Region: "us-west-2" # All volumes will use us-west-2 by default

s3CredentialSecret:
  # ... your credential configuration
```

If `node.s3Region` is not specified, it defaults to `us-east-1`.

## Per-Volume Region Override

You can override the region for a specific PersistentVolume (PV) by adding the `region` mount option in the PV manifest. This is useful if you have buckets in different regions.

**Example PV manifest with region override:**

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-region-override
spec:
  capacity:
    storage: 1200Gi
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  claimRef:
    namespace: default
    name: s3-pvc-region-override
  mountOptions:
    - allow-delete
    - allow-overwrite
    - region eu-central-1 # This overrides the global region for this volume
  csi:
    driver: s3.csi.scality.com
    volumeHandle: s3-csi-driver-volume
    volumeAttributes:
      bucketName: my-eu-bucket
```

!!! note
    The `region` mount option takes precedence over the global `s3Region` setting for that volume.
    Only standard S3 regions are supported.

## Verification

After applying your manifests, verify the region override is in effect by checking the pod's access to the correct bucket/region.

## References

- [Volume Configuration](../configuration/volume-configuration.md)
- [Installation Guide](../installation.md)
