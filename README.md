# Mountpoint for Amazon S3 CSI Driver

## Overview
The Mountpoint for Amazon S3 Container Storage Interface (CSI) Driver allows your Kubernetes applications to access Amazon S3 objects through a file system interface. Built on [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3), the Mountpoint CSI driver presents an Amazon S3 bucket as a storage volume accessible by containers in your Kubernetes cluster. The Mountpoint CSI driver implements the [CSI](https://github.com/container-storage-interface/spec/blob/master/spec.md) specification for container orchestrators (CO) to manage storage volumes.

For Amazon EKS clusters, the Mountpoint for Amazon S3 CSI driver is also available as an [EKS add-on](https://docs.aws.amazon.com/eks/latest/userguide/eks-add-ons.html) to provide automatic installation and management.

## Features
* **Static Provisioning** - Associate an existing S3 bucket with a [PersistentVolume](https://kubernetes.io/docs/concepts/storage/persistent-volumes/) (PV) for consumption within Kubernetes.
* **Mount Options** - Mount options can be specified in the PersistentVolume (PV) resource to define how the volume should be mounted. For Mountpoint-specific options, take a look at the [Mountpoint docs for configuration](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md).

Mountpoint for Amazon S3 does not implement all the features of a POSIX file system, and there are some differences that may affect compatibility with your application. See [Mountpoint file system behavior](https://github.com/awslabs/mountpoint-s3/blob/main/doc/SEMANTICS.md) for a detailed description of Mountpoint's behavior and POSIX support and how they could affect your application.

## Container Images
| Driver Version | [ECR Public](https://gallery.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver) Image |
|----------------|---------------------------------------------------------------------------------------------------|
| v1.13.0        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.13.0                      |

<details>
<summary>Previous Images</summary>

| Driver Version | [ECR Public](https://gallery.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver) Image |
|----------------|---------------------------------------------------------------------------------------------------|
| v1.12.0        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.12.0                      |
| v1.11.0        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.11.0                      |
| v1.10.0        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.10.0                      |
| v1.9.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.9.0                       |
| v1.8.1         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.8.1                       |
| v1.8.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.8.0                       |
| v1.7.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.7.0                       |
| v1.6.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.6.0                       |
| v1.5.1         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.5.1                       |
| v1.4.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.4.0                       |
| v1.3.1         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.3.1                       |
| v1.3.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.3.0                       |
| v1.2.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.2.0                       |
| v1.1.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.1.0                       |
| v1.0.0         | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.0.0                       |
</details>

## Releases
The Mountpoint for S3 CSI Driver follows [semantic versioning](https://semver.org/). The version will be bumped following the rules below:

* Significant breaking changes will be released as a `MAJOR` update.
* New features will be released as a `MINOR` update.
* Bug or vulnerability fixes will be released as a `PATCH` update.

Monthly releases will contain at minimum a `MINOR` version bump, even if the content would normally be treated as a `PATCH` version.

## Compatibility

The Mountpoint for S3 CSI Driver is compatible with Kubernetes versions v1.23+ and implements the CSI Specification v1.8.0. The driver supports **x86-64** and **arm64** architectures.

## Documentation

<!-- TODO(S3CSI-17): Update documentation links in README.md -->

<!-- TODO(S3CSI-17): Link to quick start or other docs -->
