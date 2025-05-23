# Mountpoint for Scality's fork of Amazon S3 CSI Driver

## Overview

The Mountpoint for Scality S3 Container Storage Interface (CSI) Driver allows your Kubernetes applications to access
Scality S3 objects through a file system interface. Built on [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3),
the Mountpoint CSI driver presents a Scality S3 bucket as a storage volume accessible by containers in your Kubernetes cluster.
The Mountpoint CSI driver implements the [CSI](https://github.com/container-storage-interface/spec/blob/master/spec.md)
specification for container orchestrators (CO) to manage storage volumes.

## Features

- **Static Provisioning** - Associate an existing S3 bucket with a
  [PersistentVolume](https://kubernetes.io/docs/concepts/storage/persistent-volumes/) (PV) for consumption within Kubernetes.
- **Mount Options** - Mount options can be specified in the PersistentVolume (PV) resource to define how the volume should be mounted.
  For Mountpoint-specific options, take a look at the [Mountpoint docs for configuration](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md).

Mountpoint for Amazon S3 does not implement all the features of a POSIX file system, and there are some differences that may
affect compatibility with your application. See [Mountpoint file system behavior](https://github.com/awslabs/mountpoint-s3/blob/main/doc/SEMANTICS.md)
for a detailed description of Mountpoint's behavior and POSIX support and how they could affect your application.

## Container Images

| Driver Version | [GHCR Public](https://github.com/scality/mountpoint-s3-csi-driver/pkgs/container/mountpoint-s3-csi-driver) Image |
|----------------|-----------------------------------------------------------------------------------------------------------------|
| v0.1.0         | ghcr.io/scality/mountpoint-s3-csi-driver                                                                        |

## Requirements

### S3 Endpoint URL (Required)

The S3 endpoint URL (`node.s3EndpointUrl`) is a **required** parameter when installing the CSI driver via Helm.
This URL specifies the endpoint for your S3 service. The driver will not function without this parameter and the Helm
installation will fail if it's not provided.

Example:

```bash
helm install mountpoint-s3 ./charts/scality-mountpoint-s3-csi-driver \
  --set node.s3EndpointUrl=https://s3.your-scality-cluster.com
```

## Installation

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

## Compatibility

The Mountpoint for S3 CSI Driver is compatible with Kubernetes versions v1.23+ and implements the CSI Specification v1.8.0.
The driver supports **x86-64** and **arm64** architectures.

## Documentation

For detailed configuration and advanced usage, see:

- [Configuration Options](CONFIGURATION.md)
- [Logging Configuration](LOGGING.md)
- [Installation Guide](install.md)
- [Scality-specific Documentation](scality/README.md)
