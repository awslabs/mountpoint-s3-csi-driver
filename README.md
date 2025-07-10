# Mountpoint for Amazon S3 CSI Driver

> [!TIP]
> We’re planning to release version 2.0 of the Mountpoint for Amazon S3 CSI Driver in the upcoming months.
>
> We have released a v2 beta to enable wider community testing of the upcoming v2. Please note that this beta release is intended for experimental use and its adoption in production environments is not recommended.
> This Beta release is under active development and has not completed testing and review for performance, reliability, and security. Please refer to [Unstable Releases](https://github.com/awslabs/mountpoint-s3-csi-driver?tab=readme-ov-file#unstable-releases) for more information.
> Any testing is appreciated, however, we are especially interested in getting your feedback on how the beta CSI v2.0 driver performs with large scale workloads. The v2 beta should be tested on a fresh cluster.
>
> See [Mountpoint for Amazon S3 CSI Driver v2](https://github.com/awslabs/mountpoint-s3-csi-driver/issues/504) for more details.


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
| v1.15.0        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.15.0                      |

<details>
<summary>Previous Images</summary>

| Driver Version | [ECR Public](https://gallery.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver) Image |
|----------------|---------------------------------------------------------------------------------------------------|
| v1.14.1        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.14.1                      |
| v1.14.0        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.14.0                      |
| v1.13.0        | public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v1.13.0                      |
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

### Unstable Releases

Unstable versions (i.e., `beta`, `alpha`, or `rc`) are for testing purposes only and should not be used in production environments or with valuable data. The security and correctness of these releases are not guaranteed. Use unstable versions at your own risk.

## Support

Support will be provided for the latest version and one prior version. Bugs or vulnerabilities found in the latest version will be backported to the previous release in a new minor version.

This policy is non-binding and subject to change.

## Compatibility

The Mountpoint for S3 CSI Driver is compatible with Kubernetes versions v1.25+ and implements the CSI Specification v1.8.0. The driver supports **x86-64** and **arm64** architectures.

## Distros Support Matrix

The following table provides the support status for various distros with regards to CSI Driver version:

| Distro                                  | Experimental | Stable | Deprecated | Removed |
|-----------------------------------------|-------------:|-------:|-----------:|--------:|
| Amazon Linux 2       |         - |   1.0.0 |          - |       - |
| Amazon Linux 2023    |         - |   1.0.0 |          - |       - |
| Ubuntu 20.04         |         - |   1.0.0 |          - |       - |
| Ubuntu 22.04         |         - |   1.0.0 |          - |       - |
| Bottlerocket >1.19.2 |         - |   1.4.0 |          - |       - |

## Documentation

* [Driver Installation](docs/INSTALL.md)
* [Configuring volumes and credentials](docs/CONFIGURATION.md)
* [Troubleshooting](docs/TROUBLESHOOTING.md)
* [Kubernetes Static Provisioning Example](/examples/kubernetes/static_provisioning)
* [Driver Uninstallation](docs/INSTALL.md#uninstalling-the-driver)
* [Development and Contributing](CONTRIBUTING.md)

## Contributing

We welcome contributions to the Mountpoint for Amazon S3 CSI driver! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for more information on how to report bugs or submit pull requests.

## Security

If you discover a potential security issue in this project we ask that you notify AWS Security via our [vulnerability reporting page](http://aws.amazon.com/security/vulnerability-reporting/). Please do **not** create a public GitHub issue.

## Code of conduct

This project has adopted the [Amazon Open Source Code of Conduct](https://aws.github.io/code-of-conduct). See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for more details.

## License

This project is licensed under the Apache-2.0 License. It builds on a number of other awesome projects with open source licenses, and licenses for other projects can be found under the following locations in the CSI Driver's container image:

- Linux packages under `/usr/share/licenses/*`
- The CSI Driver's Go dependencies under `/LICENSES/*` (starting from v1.15.0)
- Mountpoint's Rust dependencies under `/mountpoint-s3/THIRD_PARTY_LICENSES`
