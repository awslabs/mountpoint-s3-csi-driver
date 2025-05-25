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

## Explore the Documentation

This documentation provides comprehensive information to install, configure, use, and troubleshoot the Scality S3 CSI Driver.

- **Getting Started**
  - [Quick Start Guide](quick-start.md) - Get the driver up and running in minutes.
- **Configuration**
- **How-To Guides**
- **Understanding the Driver**
- **Reference**
- **Troubleshooting**
- **Guides for Specific Roles**
