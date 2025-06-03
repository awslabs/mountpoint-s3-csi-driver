# Driver Configuration (Helm Chart Values)

The Scality S3 CSI Driver is configured primarily through the `values.yaml` file when deploying via Helm. This page details the available parameters.

## Driver Parameters

These parameters configure the overall behavior of the CSI driver components.

| Parameter                                            | Description                                                                                                                                        | Default                                                | Required                    |
|------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------|-----------------------------|
| `image.repository`                                   | The container image repository for the CSI driver.                                                                                                 | `ghcr.io/scality/mountpoint-s3-csi-driver`             | No                          |
| `image.pullPolicy`                                   | The image pull policy.                                                                                                                             | `IfNotPresent`                                         | No                          |
| `image.tag`                                          | The image tag for the CSI driver. Overrides the chart's `appVersion` if set.                                                                       | Image version, eg, 1.0.0                   | No                          |
| `node.kubeletPath`                                   | The path to the kubelet directory on the host node. Used by the node plugin to register itself and manage mount points.                               | `/var/lib/kubelet`                                     | No                          |
| `node.mountpointInstallPath`                         | Path on the host where the `mount-s3` binary will be installed by the initContainer. Should end with a `/`. *Relevant for systemd mounter only.* | `/opt/mountpoint-s3-csi/bin/`                          | No                          |
| `node.logLevel`                                      | Log level for the CSI driver node plugin. (0-5, 5 is most verbose).                                                                                | `4`                                                    | No                          |
| `node.seLinuxOptions.user`                           | SELinux user for the node plugin container context. *Relevant for systemd mounter only.*                                                             | `system_u`                                             | No                          |
| `node.seLinuxOptions.type`                           | SELinux type for the node plugin container context. *Relevant for systemd mounter only.*                                                             | `super_t`                                              | No                          |
| `node.seLinuxOptions.role`                           | SELinux role for the node plugin container context. *Relevant for systemd mounter only.*                                                             | `system_r`                                             | No                          |
| `node.seLinuxOptions.level`                          | SELinux level for the node plugin container context. *Relevant for systemd mounter only.*                                                            | `s0`                                                   | No                          |
| `node.serviceAccount.create`                         | Specifies whether a ServiceAccount should be created for the node plugin.                                                                          | `true`                                                 | No                          |
| `node.serviceAccount.name`                           | Name of the ServiceAccount to use for the node plugin. If not set and `create` is true, a name is generated.                                       | `s3-csi-driver-sa`                                     | No                          |
| `node.serviceAccount.annotations`                    | Annotations to add to the created node ServiceAccount.                                                                                             | `{}`                                                   | No                          |
| `node.serviceAccount.automountServiceAccountToken`   | Whether to automount the service account token for the node plugin.                                                                                | (not explicitly set, defaults to Kubernetes behavior)    | No                          |
| `node.nodeSelector`                                  | Node selector for scheduling the node plugin DaemonSet.                                                                                            | `{}`                                                   | No                          |
| `node.resources`                                     | Resource requests and limits for the node plugin container.                                                                                        | `requests: { cpu: 10m, memory: 40Mi }, limits: { memory: 256Mi }` | No                          |
| `node.tolerateAllTaints`                             | If true, the node plugin DaemonSet will tolerate all taints. Overrides `defaultTolerations`.                                                      | `false`                                                | No                          |
| `node.defaultTolerations`                            | If true, adds default tolerations (`CriticalAddonsOnly`, `NoExecute` for 300s) to the node plugin.                                                 | `true`                                                 | No                          |
| `node.tolerations`                                   | Custom tolerations for the node plugin DaemonSet.                                                                                                  | `[]`                                                   | No                          |
| `node.podInfoOnMountCompat.enable`                   | Enable `podInfoOnMount` for older Kubernetes versions (&lt;1.30) if your API server supports it but Kubelet version in Helm doesn't reflect it.    | `false`                                                | No                          |
| `node.s3EndpointUrl`                                 | The S3 endpoint URL to be used by the driver for all mount operations.                                                                             | `""`                                                   | **Yes**                     |
| `node.s3Region`                                      | The default AWS region to use for S3 requests. Can be overridden per-volume via PV `mountOptions`.                                               | `us-east-1`                                            | No                          |
| `s3CredentialSecret.name`                               | Name of the Kubernetes Secret containing AWS credentials (`access_key_id`, `secret_access_key`, optionally `session_token`). You must create this secret manually. | `s3-secret`                                           | No                          |
| `s3CredentialSecret.keyId`                              | Key within the secret for Access Key ID.                                                                                                       | `access_key_id`                                               | No                          |
| `s3CredentialSecret.secretAccessKey`                          | Key within the secret for Secret Access Key.                                                                                                   | `secret_access_key`                                           | No                          |
| `s3CredentialSecret.sessionToken`                       | Key within the secret for Session Token (optional).                                                                                            | `session_token`                                        | No                          |
| `sidecars.nodeDriverRegistrar.image.repository`      | Image repository for the `csi-node-driver-registrar` sidecar.                                                                                      | `k8s.gcr.io/sig-storage/csi-node-driver-registrar`     | No                          |
| `sidecars.nodeDriverRegistrar.image.tag`             | Image tag for the `csi-node-driver-registrar` sidecar.                                                                                             | `v2.13.0`                                              | No                          |
| `sidecars.nodeDriverRegistrar.image.pullPolicy`      | Image pull policy for the `csi-node-driver-registrar` sidecar.                                                                                     | `IfNotPresent`                                         | No                          |
| `sidecars.nodeDriverRegistrar.resources`             | Resource requests and limits for the `csi-node-driver-registrar` sidecar.                                                                          | `{}` (inherits from `node.resources` if not set)       | No                          |
| `sidecars.livenessProbe.image.repository`            | Image repository for the `livenessprobe` sidecar.                                                                                                  | `registry.k8s.io/sig-storage/livenessprobe`            | No                          |
| `sidecars.livenessProbe.image.tag`                   | Image tag for the `livenessprobe` sidecar.                                                                                                         | `v2.15.0`                                              | No                          |
| `sidecars.livenessProbe.image.pullPolicy`            | Image pull policy for the `livenessprobe` sidecar.                                                                                                 | `IfNotPresent`                                         | No                          |
| `sidecars.livenessProbe.resources`                   | Resource requests and limits for the `livenessprobe` sidecar.                                                                                      | `{}` (inherits from `node.resources` if not set)       | No                          |
| `initContainer.installMountpoint.resources`          | Resource requests and limits for the `install-mountpoint` initContainer. *Relevant for systemd mounter only.*                                      | `{}` (inherits from `node.resources` if not set)       | No                          |
| `nameOverride`                                       | Override the chart name.                                                                                                                           | `""`                                                   | No                          |
| `fullnameOverride`                                   | Override the full name of the release.                                                                                                             | `""`                                                   | No                          |
| `imagePullSecrets`                                   | Secrets for pulling images from private registries.                                                                                                | `[]`                                                   | No                          |
| `experimental.podMounter`                            | **EXPERIMENTAL:** Enables the Pod Mounter feature. Should be `false` for standard S3 configurations documented here.                               | `false`                                                | No                          |
| `controller.*`                                       | Configuration for the CSI controller (Deployment, ServiceAccount). *Only used if `experimental.podMounter` is true.*                               | N/A (not documented)                                   | No                          |
| `mountpointPod.*`                                    | Configuration for the Mountpoint pods spawned by the controller. *Only used if `experimental.podMounter` is true.*                                  | N/A (not documented)                                   | No                          |

### Security Notes on `s3CredentialSecret`

The Helm chart **does not create secrets automatically**. You must create a Kubernetes Secret containing your S3 credentials before installing the chart. The secret must contain the following keys:

- `access_key_id`: Your S3 Access Key ID.
- `secret_access_key`: Your S3 Secret Access Key.
- `session_token` (optional): Your S3 Session Token, if using temporary credentials.

Example for creating the secret manually:

```bash
kubectl create secret generic my-s3-credentials \
  --namespace kube-system \
  --from-literal=access_key_id='YOUR_ACCESS_KEY_ID' \
  --from-literal=secret_access_key='YOUR_SECRET_ACCESS_KEY'
```

Then in your `values.yaml`:

```yaml
s3CredentialSecret:
  name: "my-s3-credentials"
```

!!! tip "Security Best Practices"
    - Create secrets from files instead of command line arguments to avoid exposing credentials in shell history
    - Use dedicated namespaces with appropriate RBAC policies

### Global Configuration

The `node.s3EndpointUrl` and `node.s3Region` parameters set the default S3 endpoint and region for all volumes provisioned by this driver instance.
The `node.s3Region` can be overridden on a per-volume basis using the `region` mount option in the PersistentVolume definition.
However, the `node.s3EndpointUrl` **cannot** be overridden at the volume level for security reasons; it is a global setting for the driver instance.

The S3 credentials specified via `s3CredentialSecret` are also global by default but
can be overridden on a per-volume basis if the `authenticationSource: secret` and `nodePublishSecretRef` are used in the `PersistentVolume.spec.csi` definition.
