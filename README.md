# Mountpoint for Scality's fork of Amazon S3 CSI Driver

## Overview
The Mountpoint for Scality S3 Container Storage Interface (CSI) Driver allows your Kubernetes applications to access Scality S3 objects through a file system interface. Built on [Mountpoint for Amazon S3](https://github.com/awslabs/mountpoint-s3), the Mountpoint CSI driver presents a Scality S3 bucket as a storage volume accessible by containers in your Kubernetes cluster. The Mountpoint CSI driver implements the [CSI](https://github.com/container-storage-interface/spec/blob/master/spec.md) specification for container orchestrators (CO) to manage storage volumes.

## Features
* **Static Provisioning** - Associate an existing S3 bucket with a [PersistentVolume](https://kubernetes.io/docs/concepts/storage/persistent-volumes/) (PV) for consumption within Kubernetes.
* **Mount Options** - Mount options can be specified in the PersistentVolume (PV) resource to define how the volume should be mounted. For Mountpoint-specific options, take a look at the [Mountpoint docs for configuration](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md).

Mountpoint for Amazon S3 does not implement all the features of a POSIX file system, and there are some differences that may affect compatibility with your application. See [Mountpoint file system behavior](https://github.com/awslabs/mountpoint-s3/blob/main/doc/SEMANTICS.md) for a detailed description of Mountpoint's behavior and POSIX support and how they could affect your application.

## Container Images
| Driver Version | [GHCR Public](https://github.com/scality/mountpoint-s3-csi-driver/pkgs/container/mountpoint-s3-csi-driver) Image |
|----------------|-----------------------------------------------------------------------------------------------------------------|
| v0.1.0         | ghcr.io/scality/mountpoint-s3-csi-driver                                                                        |

## Releases
The Mountpoint for S3 CSI Driver follows [semantic versioning](https://semver.org/). The version will be bumped following the rules below:

* Significant breaking changes will be released as a `MAJOR` update.
* New features will be released as a `MINOR` update.
* Bug or vulnerability fixes will be released as a `PATCH` update.

## Compatibility

The Mountpoint for S3 CSI Driver is compatible with Kubernetes versions v1.23+ and implements the CSI Specification v1.8.0. The driver supports **x86-64** and **arm64** architectures.

## Documentation

<!-- TODO(S3CSI-17): Update documentation links in README.md -->

<!-- TODO(S3CSI-17): Link to quick start or other docs -->
