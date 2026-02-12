# Unreleased

### Notable changes
* Support Mountpoint [version 1.22.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.22.0)
  * Breaking Change: Address an issue where opening a file for reading/writing immediately after the file had been closed would occasionally fail. Since this release, opening a new file handle after close will succeed and trigger the completion of a deferred upload if required. As a consequence, duplicate references to the closed file handle will become invalid and read or write operations on them will fail. See [this section in the semantics documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/SEMANTICS.md#close-and-re-open) for details. ([#1704](https://github.com/awslabs/mountpoint-s3/pull/1704))
  * Add metric to track cache hit rate in logs. ([#1716](https://github.com/awslabs/mountpoint-s3/pull/1716))
  * Remove redundant cache metrics in logs. ([#1716](https://github.com/awslabs/mountpoint-s3/pull/1716), [#1721](https://github.com/awslabs/mountpoint-s3/pull/1721))
  * Update cache metrics for consistency. ([#1721](https://github.com/awslabs/mountpoint-s3/pull/1721), [#1738](https://github.com/awslabs/mountpoint-s3/pull/1738))
  * Add cache metrics for OTLP export. ([#1724](https://github.com/awslabs/mountpoint-s3/pull/1724))

# v2.3.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v2.3.0/README.md)

### Notable changes
* Add `mountpointPod.createNamespace` flag to Helm chart to control namespace creation behavior. ([#580](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/580))
* Fix prefix parsing for S3 bucket prefixes in volume configurations. ([#658](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/658))
* Fix bug where workloads sharing a Mountpoint Pod could wait indefinitely if the first workload terminates before pod attachment is created. ([#652](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/652))
* Update minimum Kubernetes version for Helm chart to 1.29.0. ([#640](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/640))

# v2.2.2

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v2.2.2/README.md)

* When installing via Kustomize, or when installing via Helm on OpenShift clusters (including ROSA), rollback bug fix for where writes to an open file interacting with multiple processes would complete early and result in failed writes.
  ([#667](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/667))

# v2.2.1

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v2.2.1/README.md)

* Fix bug where writes to an open file interacting with multiple processes would complete early and result in failed writes.
  This change updates the node daemonset to run in host PID namespace.
  ([#632](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/632))

# v2.2.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v2.2.0/README.md)

### Notable changes
* Support Mountpoint [version 1.21.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.21.0)
  * Mountpoint for Amazon S3 adds support for exporting metrics using OTel Protocol. See [METRICS documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/METRICS.md). ([1685](https://github.com/awslabs/mountpoint-s3/pull/1685))
  * Change default logging level from WARN to INFO to improve visibility of Mountpoint operational messages. ([#1605](https://github.com/awslabs/mountpoint-s3/pull/1605), [#1668](https://github.com/awslabs/mountpoint-s3/pull/1668))
  * Change FUSE and S3 request metric names in logs. ([#1630](https://github.com/awslabs/mountpoint-s3/pull/1630), [#1653](https://github.com/awslabs/mountpoint-s3/pull/1653))
  * Change metric logging format to add metric units. ([#1677](https://github.com/awslabs/mountpoint-s3/pull/1677))
  * Fixed "file does not exist" errors during concurrent directory listing operations. ([#1648](https://github.com/awslabs/mountpoint-s3/pull/1648))

# v2.1.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v2.1.0/README.md)

### Notable changes
* Add removal of node `s3.csi.aws.com/agent-not-ready` taint after the CSI driver completes registration with kubelet for readiness detection. See the [configuration guide](./docs/CONFIGURATION.md#configure-node-startup-taint) for more details
* Fixed Mountpoint Pod creation failures with long volume handles by moving volumeID and PV name to annotations. ([#585](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/585))
* Added cleanup of stale Headroom Pods in StaleAttachmentCleaner when workload pods no longer exist or are past scheduling. ([#560](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/560))
* Improved selectableFields detection by querying CRD definition at runtime instead of relying on Kubernetes version checks. ([#557](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/557))
* Fixed Kubernetes compatibility warnings by conditionally including selectableFields based on cluster version and added server-side manifest validation. ([#554](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/554))
* Support Mountpoint [version 1.20.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.20.0)
  * Adopt a unified memory pool to reduce overall memory usage. ([mountpoint-s3#1511](https://github.com/awslabs/mountpoint-s3/pull/1511))
  * Log messages now include the thread ID that logged the message, like "ThreadId(01)", after the level. ([mountpoint-s3#1460](https://github.com/1460))awslabs/mountpoint-s3/pull/
  * Fix issue preventing incremental upload to handle very large write part sizes. ([mountpoint-s3#1538](https://github.com/awslabs/mountpoint-s3/pull/1538))
  * Fix race condition that could cause Mountpoint to panic on unlink. ([mountpoint-s3#1596](https://github.com/awslabs/mountpoint-s3/pull/1596))
  * Downgrade ioctl operation logging level from WARN to DEBUG to reduce log noise. ([mountpoint-s3#1598](https://github.com/awslabs/mountpoint-s3/pull/1598))

# v2.0.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v2.0.0/README.md)

### Breaking changes
See [Mountpoint for Amazon S3 CSI Driver v2](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/504) for new features, and [Upgrading Mountpoint for Amazon S3 CSI Driver from v1 to v2](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/UPGRADING_TO_V2.md) for more details on breaking changes and necessary steps for upgrading to v2.

# v1.15.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.15.0/README.md)

### Notable changes
* Support Mountpoint [version 1.19.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.19.0)
  * Mountpoint for Amazon S3 adds support for renaming files using the RenameObject API when mounting directory buckets in S3 Express One Zone. ([mountpoint-s3#1468](https://github.com/awslabs/mountpoint-s3/pull/1468))
* Re-create dbus connection to systemd when connection is lost. ([#512](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/512))

# v1.14.1

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.14.1/README.md)

### Notable changes
* Support Mountpoint [version 1.16.2](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.16.2)
  * Address an issue introduced in v1.16.0 (with CSI Driver v1.14.0) that could affect throughput and memory usage in workloads with many concurrent random read operations. ([mountpoint-s3#1355](https://github.com/awslabs/mountpoint-s3/pull/1355))

# v1.14.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.14.0/README.md)

### Notable changes
* Support Mountpoint [version 1.16.1](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.16.1)
  * __Breaking change:__ The logging target used by a significant amount of Mountpoint code has changed. Some logs originally written under the `mountpoint_s3` target are now written under `mountpoint_s3_fs`. You may need to update any code configuring or filtering logging. ([mountpoint-s3#1304](https://github.com/awslabs/mountpoint-s3/pull/1304))
  * Mountpoint for Amazon S3 adds support for S3 Access Points for directory buckets in AWS Dedicated Local Zones. ([aws-c-s3#502](https://github.com/awslabs/aws-c-s3/pull/502))
  * Address an issue in GetObject requests that could result in read operations to fail. ([mountpoint-s3#1334](https://github.com/awslabs/mountpoint-s3/pull/1334))
* Ensure `util.ReplaceFile` is safe to call concurrently ([#421](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/421)) (Fixes [#418](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/418))

# v1.13.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.13.0/README.md)

### Notable changes
* Add a more robust check for corrupted mounts ([#401](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/401))
* Support Mountpoint [version 1.15.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.15.0)
  * __Breaking change:__ CLI flags `--sse` and `--sse-kms-key-id` are now applied to object uploads to the xz cache. ([mountpoint-s3#1257](https://github.com/awslabs/mountpoint-s3/pull/1257))
  * Add a new command-line argument `--negative-metadata-ttl` to independently set the time-to-live (TTL) for cached negative entries. ([mountpoint-s3#1246](https://github.com/awslabs/mountpoint-s3/pull/1246))
  * Add support for appending to objects originally uploaded with a CRC64-NVME checksum. ([mountpoint-s3#1235](https://github.com/awslabs/mountpoint-s3/pull/1235))

# v1.12.0

[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/README.md)

### Notable changes
* Add `HostToContainer` mount propagation, replacing the previous method of reading mount points via `/host/proc/mounts`. ([#321](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/321))
* Support Mountpoint [version 1.14.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.14.0)
  * Fix an issue where an interrupt during readdir syscall leads to an error. ([mountpoint-s3#965](https://github.com/awslabs/mountpoint-s3/pull/965))
  * Fix an issue where the source bucket of a shared cache block was not correctly validated. ([mountpoint-s3#1208](https://github.com/awslabs/mountpoint-s3/pull/1208))

# v1.11.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.11.0/README.md)

### Notable changes
* Support Mountpoint [version 1.13.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.13.0)
  * Mountpoint now supports AWS Dedicated Local Zones. ([awslabs/aws-c-s3#465](https://github.com/awslabs/aws-c-s3/pull/465))
  * Mountpoint now offers a new command-line flag `--incremental-upload`, available when mounting directory buckets in S3 Express One Zone.
    When set, Mountpoint will perform all uploads incrementally and support appending to existing objects. ([mountpoint-s3#1165](https://github.com/awslabs/mountpoint-s3/pull/1165))
  * Mountpoint now offers a new command-line argument `--cache-xz <BUCKET>` which enables caching of object content to the specified bucket on S3 Express One Zone.
    To get started, see the [shared cache section of the configuration documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#shared-cache).
    ([mountpoint-s3#1145](https://github.com/awslabs/mountpoint-s3/pull/1145))
  * Mountpoint now implements statfs to report non-zero synthetic values.
    This may unblock applications which rely on verifying there is available space before creating new files. ([mountpoint-s3#1118](https://github.com/awslabs/mountpoint-s3/pull/1118))

# v1.10.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.10.0/README.md)

### Notable changes
* Support Mountpoint version 1.10.0, including adaptive prefetching for better memory utilization

# v1.9.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.9.0/README.md)

### Notable changes
* Add support for pod-level authentication in volumes ([#111](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/111))
  * See documentation for this feature in the [configuration documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md)
* Support Mountpoint [version 1.9.1](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.9.1)
  * Add AWS ISO partitions to STS credential provider, resolving IRSA authentication issues. ([awslabs/aws-c-auth#253](https://github.com/awslabs/aws-c-auth/pull/253))
  * Mountpoint now offers multi-nic configuration. See the Mountpoint documentation for details.
  * Customers may experience improvements in bandwidth usage when reading multiple files concurrently and reduced memory consumption.


# v1.8.1
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.8.1/README.md)

### Notable changes
* Pass long-term AWS credentials via file ([#252](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/252))

# v1.8.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.8.0/README.md)

### Notable changes
* Support Mountpoint [version 1.8.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.8.0),
  * Mountpoint now offers two new command-line arguments `--read-part-size <SIZE>` and `--write-part-size <SIZE>` which allow to specify different part sizes to be used when reading and writing respectively. ([mountpoint-s3#949](https://github.com/awslabs/mountpoint-s3/pull/949))
  * Fix issue where empty environment variables for STS web identity credentials could cause segmentation fault. ([mountpoint-s3#963](https://github.com/awslabs/mountpoint-s3/pull/963))
* Add retry to reading `/proc/mounts` ([#234](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/234))
* Add Kubernetes version to user-agent ([#224](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/224))

# v1.7.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.7.0/README.md)

### Notable changes
* Support Mountpoint [version 1.7.2](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.7.2), including the ability to configure the metadata cache independently of data caching, changes to default metadata TTLs when using `--cache` flag, the option to disable additional checksums for S3 implementations not supporting them, and other changes
* Support configuring `/proc/mounts` path ([#191](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/191))

# v1.6.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.6.0/README.md)

### Notable changes
* Support Mountpoint [version 1.6.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.6.0), including configurable retries and sse-kms support

# v1.5.1
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.5.0/README.md)

### Notable changes
* Support Mountpoint [version 1.5.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.5.0), including negative caching

# v1.4.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.4.0/README.md)

### Notable changes
* Support Bottlerocket OS ([#86](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/86))
* Support customizing tolerations ([#109](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/109))

# v1.3.1
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.3.1/README.md)

### Notable changes
* Support Mountpoint [version 1.4.1](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.4.1) which is a patchfix for a critical bug

# v1.3.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.3.0/README.md)

### Notable changes
* Support Mountpoint [version 1.4.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.4.0) which supports file overwrite ([#139](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/139))

# v1.2.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.2.0/README.md)

### Notable changes
* Support Mountpoint version 1.3.2 ([#121](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/121))
* Make helm charts more configurable ([#116](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/116))

# v1.1.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.1.0/README.md)

### Notable changes
* Support Mountpoint version 1.3.1 which supports S3 Express One Zone ([#90](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/90))

# v1.0.0
[Documentation](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/v1.0.0/README.md)

### Notable changes
* Initial release to support using [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) to mount S3 buckets as a persistent volume in your kubernetes cluster
    * Mountpoint Version: 1.1.1
* Add support for Static Provisioning ([#2](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/2), [#4](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/4))
* Add helm install  ([#8](https://github.com/awslabs/mountpoint-s3-csi-driver/pull/8))
