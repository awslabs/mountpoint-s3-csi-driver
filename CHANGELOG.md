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
* Support Mountpoint [version 1.5.0](https://github.com/awslabs/mountpoint-s3/releases/tag/mountpoint-s3-1.5.0), including negative cahcing

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
* Add support for Static Provisioning ([#2](https://github.com/awslabs/aws-s3-csi-driver/pull/2), [#4](https://github.com/awslabs/aws-s3-csi-driver/pull/4))
* Add helm install  ([#8](https://github.com/awslabs/aws-s3-csi-driver/pull/8))
