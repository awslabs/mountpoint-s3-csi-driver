# Installation Guide

This guide covers the prerequisites and steps to install the Scality S3 CSI Driver in your Kubernetes cluster.

## Prerequisites

Before installing the driver, ensure your environment meets the following requirements:

### Kubernetes Version

- Kubernetes **v1.30.0** or newer is required. The driver relies on features and API versions available in these Kubernetes releases.

### RBAC (Role-Based Access Control)

- The Helm chart will create the necessary `ServiceAccount`, `ClusterRole`, and `ClusterRoleBinding` for the driver to function.
- Ensure the user or tool performing the Helm installation has sufficient permissions to create these RBAC resources at the cluster scope.

### Node Requirements

- **Operating System**: Linux nodes are required for the CSI node plugin.
- **`hostPath` Access**: The driver's node component requires access to certain host paths (typically under `/var/lib/kubelet/`) to manage mount points. Ensure your cluster's security policies
  (like PodSecurityPolicies or OPA/Gatekeeper policies) allow the driver's DaemonSet pods this access.
- **Systemd**: The default mounter implementation requires systemd to be available on the nodes where the CSI driver node plugin runs.
  The driver uses systemd to manage the `mount-s3` processes that perform the actual S3 bucket mounts. Application pods do not directly interact with systemd.

### Node Selector

- By default, the CSI driver node pods (`s3-csi-node`) will be scheduled on all Linux nodes.
- If you need to restrict the driver to specific nodes, you can configure `node.nodeSelector` in your Helm `values.yaml`.
  Example:

  ```yaml
  node:
    nodeSelector:
      disktype: s3-eligible
  ```

### Network Connectivity

- Kubernetes worker nodes must have network connectivity to your Scality S3 endpoint (RING).
- This includes resolving the S3 endpoint DNS name and reaching the S3 service on the appropriate ports (typically 80 for HTTP or 443 for HTTPS, unless port is specified in the S3 endpoint URL).

### S3 Credentials

- You'll need an S3 Access Key ID and Secret Access Key with appropriate permissions for the buckets you intend to mount.
- These credentials will be stored as a Kubernetes Secret and accessed by the driver.

!!! warning "SELinux Not Supported"
    The Scality S3 CSI Driver does not currently support SELinux in `enforcing` mode. If your nodes use SELinux, you must set it to `permissive` or `disabled` mode for the driver to function properly.

## Installation with Helm

The recommended method for installing the Scality S3 CSI Driver is using Helm.

1. **Add Scality Helm Repository**:
   If you haven't already, add the Scality Helm repository:

   ```bash
   helm repo add scality https://scality.github.io/mountpoint-s3-csi-driver
   helm repo update
   ```

2. **Create S3 Credentials Secret**:
   Before installing the driver, create a Kubernetes Secret containing your S3 credentials:

   ```bash
   # Set your S3 configuration
   export S3_ENDPOINT_URL="https://your-s3-endpoint.example.com"
   export AWS_ACCESS_KEY_ID="YOUR_ACCESS_KEY_ID"
   export AWS_SECRET_ACCESS_KEY="YOUR_SECRET_ACCESS_KEY"
   # export AWS_SESSION_TOKEN="YOUR_SESSION_TOKEN"  # Optional, uncomment if needed
   export SECRET_NAME="s3-credentials"
   ```

   Create the secret:

   ```bash
   kubectl create secret generic ${SECRET_NAME} \
     --from-literal=access_key_id="${AWS_ACCESS_KEY_ID}" \
     --from-literal=secret_access_key="${AWS_SECRET_ACCESS_KEY}" \
     --namespace kube-system
   ```

   If you need a session token, add it:

   ```bash
   kubectl patch secret ${SECRET_NAME} -n kube-system --type='json' -p='[{"op": "add", "path": "/data/session_token", "value": "'$(echo -n "${AWS_SESSION_TOKEN}" | base64)'"}]'
   ```

   !!! tip "More Secure Alternative"
       For better security, create credential files locally (these won't appear in shell history):
       ```bash
       echo -n "${AWS_ACCESS_KEY_ID}" > /tmp/access_key_id
       echo -n "${AWS_SECRET_ACCESS_KEY}" > /tmp/secret_access_key
       kubectl create secret generic ${SECRET_NAME} \
         --from-file=access_key_id=/tmp/access_key_id \
         --from-file=secret_access_key=/tmp/secret_access_key \
         --namespace kube-system
       rm /tmp/access_key_id /tmp/secret_access_key  # Clean up
       ```

3. **Create a Custom Values File**:
   Create a `values.yaml` file that references the secret you created:

   ```yaml
   # my-scality-s3-values.yaml
   node:
     # REQUIRED: Specify your Scality S3 endpoint URL
     s3EndpointUrl: "https://your-s3-endpoint.example.com"

     # Optional: Specify the default AWS region for S3 requests
     # This can be overridden per-volume via PV mountOptions.
     s3Region: "us-east-1"

     # Optional: Customize the path where kubelet stores its data.
     # Default is /var/lib/kubelet. Change if your cluster uses a different path.
     # kubeletPath: /var/lib/kubelet

   s3CredentialSecret:
     # Reference the secret you created above
     name: "s3-credentials"  # Must match the secret name you created
   ```

   - Replace `https://your-s3-endpoint.example.com` with your actual S3 endpoint.
   - Ensure the `s3CredentialSecret.name` matches the secret name you created.
   - Review other options in the default [chart values](https://github.com/scality/mountpoint-s3-csi-driver/blob/main/charts/scality-mountpoint-s3-csi-driver/values.yaml) and customize as needed.

   !!! important "S3 Endpoint URL is Required"
       The `node.s3EndpointUrl`  and `s3CredentialSecret.name` parameter is **mandatory**. The Helm installation will fail if it's not provided.

4. **Install the Helm Chart**:
   Deploy the driver using Helm, by default into the `kube-system` namespace. If you want to install the driver in a different namespace, the credentials secret should be created in that namespace.

   ```bash
   helm install scality-s3-csi scality/mountpoint-s3-csi-driver \
     -f my-scality-s3-values.yaml \
     --namespace kube-system
   ```

5. **Verify the Installation**:
   Check that the driver pods are running correctly:

   ```bash
   kubectl get pods -n kube-system -l app.kubernetes.io/name=scality-mountpoint-s3-csi-driver
   ```

   You should see one `s3-csi-node-*` pod per eligible worker node in your cluster, and they should all be in the `Running` state.

   Verify the CSIDriver object is created:

   ```bash
   kubectl get csidriver s3.csi.scality.com
   ```

## Uninstallation

To uninstall the driver:

```bash
helm uninstall scality-s3-csi --namespace kube-system
```

If you created the S3 credentials secret manually (or if `s3CredentialSecret.create` was true and Helm didn't clean it up), you may need to delete it separately:

```bash
kubectl delete secret s3-secret --namespace kube-system
```
