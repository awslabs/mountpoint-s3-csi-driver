# Welcome to the Scality S3 CSI Driver Documentation

The Scality S3 Container Storage Interface (CSI) Driver allows Kubernetes applications to access Scality S3 objects through a file system interface.
This driver is a fork of the [Mountpoint for Amazon S3 CSI Driver](https://github.com/awslabs/mountpoint-s3-csi-driver) and has been adapted for use with Scality S3-compatible storage solutions.

Scality CSI driver presents an S3 bucket as a storage volume accessible by containers in Kubernetes clusters using [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3).
It implements the [CSI specification](https://github.com/container-storage-interface/spec/blob/master/spec.md) for container orchestrators to manage storage volumes.

## Key Features

- **Static Provisioning Only**: Integrate existing S3 buckets as persistent storage in Kubernetes. Dynamic provisioning is not supported.
- **Familiar File Access**: Access S3 objects as files and directories, simplifying application integration.
- **Customizable Mounts**: Fine-tune volume mounts with a variety of supported options for performance and behavior.
- **Scality Integration**: Optimized for Scality S3 storage solutions like [Scality RING](https://www.scality.com/ring/).

## Getting Started

The **[Quick Start Guide](quick-start.md)** provides step-by-step instructions for deploying the driver, securely creating secrets, and mounting S3 buckets in Kubernetes clusters.

## Documentation Overview

This documentation provides comprehensive information to install, configure, use, and troubleshoot the Scality S3 CSI Driver.

### Installation & Setup

- **[Installation](installation.md)** – Prerequisites and secure installation instructions, including manual secret creation
- **[Quick Start Guide](quick-start.md)** – Step-by-step deployment guide

### Configuration & Usage

- **[Configuration](configuration/index.md)** – Driver and volume configuration, including supported mount options and static provisioning patterns
- **[How-To Guides](how-to/static-provisioning.md)** – Practical implementation examples
- **[Minimal Helm Example](../examples/minimal-helm.yaml)** – Complete, copy-pasteable example for production

### Understanding the Driver

- **[Concepts](concepts/filesystem-semantics.md)** – Filesystem semantics, limitations, and S3-specific behaviors
- **[Reference](reference/access-modes.md)** – Detailed feature and option reference

### Support & Troubleshooting

- **[Troubleshooting](troubleshooting.md)** – Common issue resolution and diagnostic tips

## Security & Best Practices

- **Manual Secret Creation**: Always create S3 credential secrets manually before installing the chart. Do not store credentials in Helm values or use in-line secrets.
- **Access Modes**: Only `ReadWriteMany` and `ReadOnlyMany` are supported for S3 volumes.
- **Namespace Isolation**: Use dedicated namespaces and RBAC for improved security.

## Container Images

Container images for the Scality S3 CSI Driver are hosted on GHCR:

| Driver Version | Image URL                                                                 |
|----------------|---------------------------------------------------------------------------|
| 1.0.0          | `ghcr.io/scality/mountpoint-s3-csi-driver:1.0.0`                          |

*Note: Please check the [releases page](https://github.com/scality/mountpoint-s3-csi-driver/releases) for the latest available versions.*

## Support and Community

For issues or questions:

1. Search existing [GitHub Issues](https://github.com/scality/mountpoint-s3-csi-driver/issues)
2. Open a new [GitHub Issue](https://github.com/scality/mountpoint-s3-csi-driver/issues) if the problem is not already addressed

## License

This project is licensed under the Apache License 2.0. See the [LICENSE](https://github.com/scality/mountpoint-s3-csi-driver/blob/main/LICENSE) file for details.
It incorporates code from the original Mountpoint for Amazon S3 CSI Driver, also licensed under Apache 2.0.
