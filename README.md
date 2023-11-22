# Mountpoint for Amazon S3 CSI Driver

## Overview
The [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3) Container Storage Interface (CSI) Driver implements [CSI](https://github.com/container-storage-interface/spec/blob/master/spec.md) specification for container orchestrators (CO) to manage lifecycle of S3 filesystems. [S3](https://aws.amazon.com/s3/) iteslf is a cloud storage service and this CSI driver specifically uses Mountpoint to mount S3 as a filesystem.

## Features
* **Static Provisioning** - Associate an externally-created S3 bucket with a [PersistentVolume](https://kubernetes.io/docs/concepts/storage/persistent-volumes/) (PV) for consumption within Kubernetes.
* **Mount Options** - Mount options can be specified in the PersistentVolume (PV) resource to define how the volume should be mounted. For Mountpoint for S3 specific options, take a look at the [Mountpoint docs for configuration](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md) and [semantics](https://github.com/awslabs/mountpoint-s3/blob/main/doc/SEMANTICS.md).

## Container Images
| Driver Version | [ECR Public](https://gallery.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver) Image |
|----------------|---------------------------------------------------------------------------------------------------|
| v1.0.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.0.0                       |


## Releases
The Mountpoint for S3 CSI Driver follows [semantic versioning](https://semver.org/). The version will be bumped following the rules below:

* Significant breaking changes will be released as a `MAJOR` update.
* New features will be released as a `MINOR` update.
* Bug or vulnerability fixes will be released as a `PATCH` update.

Monthly releases will contain at minimum a `MINOR` version bump, even if the content would normally be treated as a `PATCH` version.

## Support

Support will be provided for the latest version and one prior version. Bugs or vulnerabilities found in the latest version will be backported to the previous release in a new minor version.

This policy is non-binding and subject to change.

## Compatibility

The Mountpoint for S3 CSI Driver is compatible with Kubernetes versions v1.23+ and implements the CSI Specification v1.8.0.

## Distros Support Matrix

The following table provides the support status for various distros with regards to CSI Driver version:

| Distro                                  | Experimental | Stable | Deprecated | Removed |
|-----------------------------------------|-------------:|-------:|-----------:|--------:|
| Amazon Linux 2    |         - |   1.0.0 |          - |       - |
| Amazon Linux 2023 |         - |   1.0.0 |          - |       - |
| Ubuntu 20.04      |         - |   1.0.0 |          - |       - |
| Ubuntu 22.04      |         - |   1.0.0 |          - |       - |

## Documentation

* [Driver Installation](docs/install.md)
* [Kubernetes Static Provisioning Example](/examples/kubernetes/static_provisioning)
* [Driver Uninstallation](docs/install.md#uninstalling-the-driver)
* [Development and Contributing](CONTRIBUTING.md)
