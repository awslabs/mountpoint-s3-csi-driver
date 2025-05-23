# Scality Mountpoint S3 CSI Driver

(sample for MKdocs integration - to be updated)

The Scality Mountpoint S3 CSI Driver implements the [Container Storage Interface](https://github.com/container-storage-interface/spec/blob/master/spec.md)
for S3 object storage, allowing Kubernetes workloads to mount S3 buckets as persistent volumes.

## Overview

This driver leverages [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3), a high-performance open-source
file client for mounting an S3 bucket as a local file system, enabling the following capabilities:

- Mount S3 buckets as Kubernetes PersistentVolumes
- Support for dynamic
- Mount option configuration (read-only, prefix, etc.)
- S3 object API integration

## Installation

<!-- For detailed installation instructions, see the [deployment guide](./deployment.md). -->

```bash
# Quick installation using Helm
helm repo add scality https://scality.github.io/mountpoint-s3-csi-driver/charts
helm install mountpoint-s3-csi-driver scality/scality-mountpoint-s3-csi-driver
```

## Usage

Create a StorageClass, PersistentVolumeClaim, and Pod with the CSI driver:

```yaml
# Example StorageClass for dynamic provisioning
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-storage
provisioner: s3.csi.scality.com
parameters:
  # Add your parameters here
```

## Documentation

This directory contains documentation for the Scality Mountpoint S3 CSI Driver.

The documentation is organized to provide clear guidance for installation, configuration, and usage of the CSI driver
for integrating Scality S3 storage with Kubernetes clusters.

For quick-start instructions and examples, see the main [README.md](../README.md) in the repository root, which
includes basic installation steps and links to example configurations.

<!-- - [Configuration Options](./configuration.md) -->
<!-- - [Examples](./examples.md) -->
<!-- - [Troubleshooting](./troubleshooting.md)  -->
