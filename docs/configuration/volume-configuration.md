# Volume Configuration

When using static provisioning with the Scality S3 CSI Driver, volume-specific configurations are defined within the `PersistentVolume` (PV) manifest. This page details the key attributes and mount options.

!!! note
    This driver primarily supports **static provisioning**. Dynamic provisioning is not supported.

## PersistentVolume (PV) Configuration

The primary way to configure a volume is through the `spec.csi` block and `spec.mountOptions` in your `PersistentVolume` definition.

### `spec.csi` Attributes

These attributes are specific to the CSI driver and control how it interacts with the S3 bucket.

| Attribute (`spec.csi.*`)        | Description                                                                                                                                                                  | Example Value                     | Required                    |
|---------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------|-----------------------------|
| `driver`                        | The name of the CSI driver. Must be `s3.csi.scality.com`.                                                                                                                    | `s3.csi.scality.com`              | **Yes**                     |
| `volumeHandle`                  | A unique identifier for this volume within the driver. Can be any string, but it's common practice to use the bucket name or a descriptive ID.                              | `my-s3-bucket-pv`                 | **Yes**                     |
| `volumeAttributes.bucketName`   | The name of the S3 bucket to mount.                                                                                                                                          | `"my-application-data"`           | **Yes**                     |
| `volumeAttributes.authenticationSource` | Specifies the source of AWS credentials for this volume. If set to `"secret"`, `nodePublishSecretRef` must also be provided. If omitted or set to `"driver"`, global driver credentials are used. | `"secret"` or `"driver"` (or omit) | No                          |
| `nodePublishSecretRef.name`     | The name of the Kubernetes Secret containing S3 credentials (`key_id`, `access_key`) for this specific volume. Used when `authenticationSource` is `"secret"`.                | `"my-volume-credentials"`         | Conditionally Yes           |
| `nodePublishSecretRef.namespace`| The namespace of the Kubernetes Secret specified in `name`. Must be the same namespace as the `PersistentVolumeClaim` that will bind to this PV.                            | `"my-app-namespace"`              | Conditionally Yes           |

**Example `spec.csi` Block:**

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-test
spec:
  capacity:
    storage: 1200Gi # This value is not enforced by S3 but required by Kubernetes
  accessModes:
    - ReadWriteMany
  storageClassName: "" # Required for static provisioning
  claimRef: # To ensure no other PVCs can claim this PV
    namespace: default # Namespace is required even though it's in "default" namespace
    name: s3-pvc-test # Name of your PVC
  mountOptions:
    - allow-delete
    - allow-overwrite
  csi:
    driver: s3.csi.scality.com
    volumeHandle: s3-csi-driver-volume
    volumeAttributes:
      bucketName: "my-application-bucket"
      # Optional: Use specific credentials for this PV
      # authenticationSource: "secret"
    # nodePublishSecretRef: # Required if authenticationSource is "secret"
    #   name: "app-specific-s3-secret"
    #   namespace: "app-namespace" # Must match PVC's namespace
```

And the corresponding PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-pvc-test
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: "" # Required for static provisioning
  resources:
    requests:
      storage: 1Gi
  volumeName: s3-pv-test # References the specific PV above
```

### `spec.mountOptions`

The following mount options are supported by the Scality S3 CSI Driver for standard S3 buckets. Options not listed here are either ignored or stripped by the driver (see note below).

| Mount Option         | Description                                                                                                                                   | Example Value              | Default (if any)         |
|----------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|----------------------------|--------------------------|
| `allow-delete`       | Allows objects to be deleted from the S3 bucket via the mount point.                                                                          | Include as is              | Not set (deletes fail)   |
| `allow-overwrite`    | Allows existing S3 objects to be overwritten. Mountpoint for S3 performs all writes by `PUT`ing new objects. If an object key already exists, this flag allows replacing it.      | Include as is              | Not set (overwrites fail)|
| `allow-other`        | Allow users other than the mounting user (typically root for the CSI driver process) to access the filesystem.                                                           | Include as is              | Not set                  |
| `allow-root`         | Allow the root user to access the filesystem even if `uid` and `gid` are set to non-root values. By default, if `uid`/`gid` are set, root access is restricted.            | Include as is              | Not set                  |
| `uid=<value>`        | Sets the User ID for all files and directories in the mount.                                                                                  | `uid=1000`                 | Process UID (typically root)|
| `gid=<value>`        | Sets the Group ID for all files and directories in the mount.                                                                                 | `gid=1000`                 | Process GID (typically root)|
| `dir-mode=<octal>`   | Sets the permission mode for directories (e.g., `0755`).                                                                                      | `dir-mode=0770`            | `0755`                   |
| `file-mode=<octal>`  | Sets the permission mode for files (e.g., `0644`).                                                                                            | `file-mode=0660`           | `0644`                   |
| `region=<value>`     | Overrides the default S3 region configured for the driver for this specific volume.                                                           | `region=eu-west-1`         | Driver's default region  |
| `prefix=<value>/`    | Mount only a specific "folder" (prefix) within the bucket. The prefix itself becomes the root of the mount. **Must end with a `/`**.                     | `prefix=myapp/data/`      | Bucket root              |
| `cache <path>`       | Enable local disk caching for S3 objects. `<path>` is a directory on the host node's filesystem.                                                                       | `cache /mnt/s3cache/pv1`   | No caching               |
| `metadata-ttl <sec>` | Time-to-live in seconds for cached metadata. Default is Mountpoint's own default (typically low, e.g., 1 second).                                                      | `metadata-ttl 60`          | (Mountpoint default)     |
| `max-cache-size <MB>`| Maximum size in MiB for the local disk cache specified by `cache <path>`.                                                                                             | `max-cache-size 1024`      | (Mountpoint default)     |
| `sse <type>`         | Server-side encryption type (e.g., `aws:kms`).                                                                                                | `sse aws:kms`              | (Mountpoint default)     |
| `sse-kms-key-id <id>`| KMS Key ID for server-side encryption with AWS KMS.                                                                                           | `sse-kms-key-id abc-123`   | (Mountpoint default)     |
| `force-path-style`   | Force S3 client to use path-style addressing.                                                                                                                        | Include as is              | (Mountpoint default, often false for AWS SDK) |
| `debug`              | Enable Mountpoint's debug logging. Logs go to systemd journal on the node.                                      | Include as is              | Not set                  |
| `debug-crt`         | Enable verbose logging for the AWS Common Runtime (CRT) S3 client, which Mountpoint uses internally. Logs also go to systemd journal.                    | Include as is              | Not set                  |

**Ignored/Unsupported Options:**

- `--profile`, `--endpoint-url`, `--storage-class`, `--cache-xz`, `--incremental-upload`, `--foreground`, `-f`, `--help`, `-h`, `--version`, `-v`and any options specific to S3 Express One Zone or
  directory buckets are **ignored or stripped** by the driver.
  These options are supported by the Mountpoint client but are not supported by the Scality S3 CSI Driver.

!!! warning "Unsupported/Stripped Mount Options"
    The CSI driver enforces a strict policy on mount options. Any option not listed above is either ignored or actively stripped for security and compatibility reasons.
    This includes all options related to S3 Express One Zone, directory buckets, per-volume endpoint overrides, AWS profiles, and storage class selection. Only use the options listed above for
    standard S3 volumes.

### Credentials and Access Modes

- **Credentials:** Only static credentials are supported, either via the global driver secret (Helm values) or a per-volume secret (with `authenticationSource: secret`)
- IAM role assumption, AWS profiles, and other advanced credential sources are **not supported**.
- **Access Modes:** The driver supports `ReadWriteMany` access modes for S3 volumes.

### Practical Usage

For advanced examples, see the [How-To Guides](../how-to/static-provisioning.md) and [Minimal Helm Example](../examples/minimal-helm.yaml).

## Configuration Precedence for Mount Options

Mount options are determined by a combination of factors. Understanding their precedence is key:

1. **`PersistentVolume.spec.mountOptions`**: These have the highest precedence for volume-specific behavior. Options defined here will be directly passed to the Mountpoint client for that specific volume.
2. **CSI Driver Defaults**: The Scality S3 CSI Driver may apply certain default options or interpret some PV/PVC parameters to derive mount options. For example:
    - If a volume is marked as `readOnly: true` in the PV or PVC, the driver implicitly adds a read-only behavior (conceptually similar to a `--read-only` flag for Mountpoint,
      although Mountpoint's actual flag might be managed differently by the driver).
    - The driver adds a `--user-agent-prefix` for telemetry.
3. **Mountpoint Client Defaults**: If an option is not specified by the PV or the CSI driver, the Mountpoint S3 client's own internal defaults will apply.

!!! important "Security: Restricted Mount Options"
    Certain Mountpoint S3 options that could pose security risks or conflict with the driver's operational model are **not permitted** or are **ignored** if specified in `PersistentVolume.spec.mountOptions`.
    These include:
    - `--endpoint-url`: The S3 endpoint is a global driver configuration (`node.s3EndpointUrl` in Helm values) and cannot be overridden per volume for security reasons.
    -    `--profile`: The CSI driver manages credentials through Secrets or global configuration, not AWS profiles.
    - `--storage-class`, `--cache-xz`, `--incremental-upload`: These relate to features (like S3 Express One Zone) not typically supported by standard Scality S3 deployments or are outside the scope of
    this driver's current feature set.
    - `--foreground`, `-f`, `--help`, `-h`, `--version`, `-v`: These are CLI-specific flags for running `mount-s3` directly and are not applicable when used via the CSI driver.

  The driver will actively strip or ignore these disallowed options.

## Examples

### Read-Only Mount

```yaml
apiVersion: v1
kind: PersistentVolume
# ...
spec:
  accessModes:
    - ReadOnlyMany # Ensure access mode also reflects read-only
  mountOptions:
    - "--read-only" # Mountpoint flag for read-only
  csi:
    # ...
```

### Non-Root User Access

To allow a pod running as a non-root user (e.g., UID 1001, GID 2002) to access the S3 mount:

```yaml
apiVersion: v1
kind: PersistentVolume
# ...
spec:
  mountOptions:
    - "uid=1001"
    - "gid=2002"
    - "allow-other" # Crucial for non-root access
    - "file-mode=0664" # Example: rw-rw-r--
    - "dir-mode=0775"  # Example: rwxrwxr-x
  csi:
    # ...
```

Your pod's `securityContext` should then specify `runAsUser: 1001` and `runAsGroup: 2002` (or `fsGroup: 2002`).

### Mounting a Bucket Prefix

To mount only the `my-data/raw/` prefix from a bucket:

```yaml
apiVersion: v1
kind: PersistentVolume
# ...
spec:
  mountOptions:
    - "prefix=my-data/raw/" # Note the trailing slash
  csi:
    # ...
```

### Enabling Local Caching

To cache S3 objects on the node's local disk at `/mnt/s3_cache_vol_abc`:

```yaml
apiVersion: v1
kind: PersistentVolume
# ...
spec:
  mountOptions:
    - "cache /mnt/s3_cache_vol_abc"
    - "metadata-ttl 600"      # Cache metadata for 10 minutes
    - "max-cache-size 5120"   # Limit cache to 5 GiB
  csi:
    # ...
```

!!! warning "Cache Path Uniqueness"
    The path specified for `cache` (e.g., `/mnt/s3_cache_vol_abc`) **must be unique on each node for every volume that uses caching**.
    If multiple S3 volumes on the same node attempt to use the same cache path, it will lead to undefined behavior and potential data corruption.
    The CSI driver does not automatically manage the uniqueness or lifecycle of these host cache paths beyond passing the option to Mountpoint.
    Node-level disk space and permissions for the cache path are also your responsibility.

For guidance on filesystem behavior and permissions, see the [Filesystem Semantics](../concepts/filesystem-semantics.md) and [Permissions (How-To)](../how-to/permissions.md) pages.
