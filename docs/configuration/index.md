# Configuration Overview

The Scality S3 CSI Driver offers several levels of configuration to adapt to various environments and use cases. This section details how to configure the driver itself and the volumes it provisions.

## Configuration Layers

Configuration for the Scality S3 CSI Driver can be applied at different stages:

1. **Driver Installation (Helm Chart `values.yaml`)**:
    Global settings for the driver, such as default S3 endpoint, region, and node plugin behavior, are configured when installing the Helm chart. These values establish the baseline for all volumes
    managed by this driver instance.
    - See [Driver Configuration](driver-configuration.md) for details.

2. **PersistentVolume (PV) Definition**:
    When using static provisioning, specific mount options, bucket names, and other volume-level attributes are defined directly in the `PersistentVolume` manifest.
    These settings override global defaults for that specific volume.
    - See [Volume Configuration](volume-configuration.md) for details on attributes within the `csi` block of a PV.
    - See [Mount Options](mount-options.md) for details on `spec.mountOptions` in a PV.

## Key Configuration Areas

### Driver Settings

- **S3 Endpoint and Region**: Define the default S3 service endpoint and region for all volumes. The region can be overridden per volume.
- **Global S3 Credentials**: Configure the default AWS credentials used by the driver to access S3 buckets. These can also be overridden per volume.
- **Node Plugin Behavior**: Settings related to the CSI node daemon, such as Kubelet path and log levels.
- **Resource Limits/Requests**: Configure CPU and memory for the driver components.

[Learn more about Driver Configuration &raquo;](driver-configuration.md)

### Volume Settings

- **Bucket Name**: The S3 bucket to be mounted. This is a **required** attribute for each volume.
- **Authentication Source**: Specify whether to use driver-level credentials or volume-specific credentials (via a Kubernetes Secret).
- **Mount Options**: A list of options passed to the Mountpoint client during the mount operation. These control filesystem behavior, permissions, caching, etc.

[Learn more about Volume Configuration &raquo;](volume-configuration.md)
[Learn more about Mount Options &raquo;](mount-options.md)

### Security Considerations

- **Credentials Management**: Securely manage S3 credentials using Kubernetes Secrets. The driver supports both global credentials (via Helm values, ideally referencing a pre-existing secret) and
  per-volume credentials (via `nodePublishSecretRef` in the PV).
- **RBAC**: The Helm chart installs necessary RBAC roles and bindings. Review these to understand the permissions granted to the driver.
- **Network Policies**: If your cluster uses network policies, ensure that the CSI driver pods can communicate with the Kubernetes API server and that application pods can communicate with the CSI node
  pods if necessary (though typically not required for the default mounter). Node pods also need to reach your S3 endpoint.

For a comprehensive understanding of configuration precedence, refer to the [Configuration Precedence](../reference/config-precedence.md) page.
