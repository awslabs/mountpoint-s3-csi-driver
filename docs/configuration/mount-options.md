# Mount Options Deep Dive

The Scality S3 CSI Driver allows you to customize how S3 buckets are mounted by specifying mount options in the `PersistentVolume` (PV) specification.
These options are passed directly to the underlying Mountpoint for Amazon S3 client.

## How Mount Options are Applied

Mount options are defined in the `spec.mountOptions` array within a `PersistentVolume` manifest.

**Example:**

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: my-s3-volume
spec:
  capacity:
    storage: 1Pi
  accessModes:
    - ReadWriteMany
  storageClassName: "" # For static provisioning
  persistentVolumeReclaimPolicy: Retain
  mountOptions:
    - "allow-delete"         # Allows deleting objects
    - "uid=1000"             # Sets mounted files/dirs UID to 1000
    - "gid=1000"             # Sets mounted files/dirs GID to 1000
    - "allow-other"          # Allows non-root users to access the mount
    - "file-mode=0640"       # Sets file permissions to rw-r-----
    - "dir-mode=0750"        # Sets directory permissions to rwxr-x---
    - "region=eu-west-1"     # Overrides S3 region for this PV
    - "prefix=app-data/"     # Mounts only the 'app-data/' prefix from the bucket
    - "cache /mnt/host_cache/my-s3-volume" # Enables caching to a host path
    - "debug"                # Enables Mountpoint debug logging
  csi:
    driver: s3.csi.scality.com
    volumeHandle: "my-s3-bucket-unique-handle"
    volumeAttributes:
      bucketName: "my-application-bucket"
```

## Common Mount Options

Here's a list of commonly used Mountpoint S3 options relevant for the CSI driver:

| Option               | Description                                                                                                                                                            | Notes                                                                                                                                                              |
|----------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `allow-delete`       | Permit `unlink` and `rmdir` operations. Without this, operations that would delete S3 objects will fail.                                                               | Crucial for read-write workloads that need to delete files/objects.                                                                                                |
| `allow-overwrite`    | Permit overwriting existing S3 objects. Mountpoint for S3 performs all writes by `PUT`ing new objects. If an object key already exists, this flag allows replacing it.      | Required if your application updates files in place.                                                                                                               |
| `allow-other`        | Allow users other than the mounting user (typically root for the CSI driver process) to access the filesystem.                                                           | **Essential** for pods running as non-root users. Must be used with appropriate `uid` and `gid` options.                                                           |
| `allow-root`         | Allow the root user to access the filesystem even if `uid` and `gid` are set to non-root values. By default, if `uid`/`gid` are set, root access is restricted.            | Useful in specific scenarios; `allow-other` is more common for general non-root access.                                                                            |
| `uid=<ID>`           | Set the User ID for all files and directories in the mount.                                                                                                            | Must match the `runAsUser` of your pod's container if `allow-other` is not used, or the user your application expects.                                             |
| `gid=<ID>`           | Set the Group ID for all files and directories in the mount.                                                                                                           | Must match the `runAsGroup` or `fsGroup` of your pod's container if `allow-other` is not used, or the group your application expects.                            |
| `file-mode=<octal>`  | Set the permission bits for files (e.g., `0644`).                                                                                                                      | Default is `0644`.                                                                                                                                                 |
| `dir-mode=<octal>`   | Set the permission bits for directories (e.g., `0755`).                                                                                                                | Default is `0755`.                                                                                                                                                 |
| `region=<value>`     | Specify the S3 region for this bucket. Overrides the driver's global `s3Region` setting.                                                                               | Ensure this matches the actual region of your bucket.                                                                                                              |
| `prefix=<value>/`    | Mount only a specific "folder" (prefix) within the bucket. The prefix itself becomes the root of the mount. **Must end with a `/`**.                                       | Example: `prefix=myapp/data/`.                                                                                                                                   |
| `cache <path>`       | Enable local disk caching for S3 objects. `<path>` is a directory on the host node's filesystem.                                                                       | `<path>` **must be unique per volume on each node**. Performance and consistency implications should be understood. Requires disk space on the node.                  |
| `metadata-ttl <sec>` | Time-to-live (in seconds) for cached metadata. Default is Mountpoint's own default (typically low, e.g., 1 second).                                                      | Increase for improved performance on listings if eventual consistency is acceptable.                                                                               |
| `max-cache-size <MB>`| Maximum size (in MiB) of the local disk cache specified by `cache <path>`.                                                                                             | Helps manage disk usage on nodes.                                                                                                                                  |
| `force-path-style`   | Force S3 client to use path-style addressing.                                                                                                                        | Often necessary for non-AWS S3 endpoints, including Scality ARTESCA/RING.                                                                                        |
| `debug`              | Enable Mountpoint's debug logging. Logs appear in the systemd journal on the node where the pod is running and the volume is mounted.                                    | Useful for troubleshooting.                                                                                                                                        |
| `debug-crt`         | Enable verbose logging for the AWS Common Runtime (CRT) S3 client, which Mountpoint uses internally. Logs also go to systemd journal.                                       | Provides even more detailed S3 client logs.                                                                                                                        |
| `aws-max-attempts <N>`| Sets the `AWS_MAX_ATTEMPTS` environment variable for the Mountpoint process, configuring S3 request retries.                                                                | Useful for tuning resiliency in unstable network conditions.                                                                                                       |

For a comprehensive list and explanation of all available Mountpoint S3 client options, refer to the [official Mountpoint for Amazon S3 documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md).

## Configuration Precedence for Mount Options

Mount options are determined by a combination of factors. Understanding their precedence is key:

1. **`PersistentVolume.spec.mountOptions`**: These have the highest precedence for volume-specific behavior. Options defined here will be directly passed to the Mountpoint client for that specific volume.
2. **CSI Driver Defaults**: The Scality S3 CSI Driver may apply certain default options or interpret some PV/PVC parameters to derive mount options. For example:
    - If a volume is marked as `readOnly: true` in the PV or PVC, the driver implicitly adds a read-only behavior (conceptually similar to a `--read-only` flag for
    - Mountpoint, although Mountpoint's actual flag might be managed differently by the driver).
    - The driver adds a `--user-agent-prefix` for telemetry.
3. **Mountpoint Client Defaults**: If an option is not specified by the PV or the CSI driver, the Mountpoint S3 client's own internal defaults will apply.

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
