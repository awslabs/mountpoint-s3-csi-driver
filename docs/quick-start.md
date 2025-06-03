# Quick Start Guide

This guide provides a fast way to deploy the Scality S3 CSI Driver using Helm and mount an S3 bucket into a pod.

## Prerequisites

- A Kubernetes cluster (v1.30 or newer).
- `kubectl` configured to communicate with your cluster.
- [Helm](https://helm.sh/docs/intro/install/) v3 installed.
- An existing S3 bucket on your Scality RING.
- S3 endpoint URL, access key, and secret key for your S3 bucket.

> **⚠️ Security Warning**: This guide demonstrates basic credential handling for testing purposes. Be aware of the following security considerations:
>
> - Environment variables expose credentials in shell history and process lists
> - Commands with credentials are visible to other users via `ps` commands
> - The `kube-system` namespace has elevated privileges
>
> **For Production Use**:
>
> - Create secrets from files instead of command line arguments
> - Use a dedicated namespace with appropriate RBAC policies
> - Consider using IAM roles or service accounts for credential management
> - Always clean up credentials after testing

## Installation

### Step 1: Add the Scality Helm repository

```bash
helm repo add scality https://scality.github.io/mountpoint-s3-csi-driver/charts/
```

```bash
helm repo update
```

### Step 2: Set your configuration variables

Replace these values with your actual S3 configuration:

```bash
# Required: Your S3-compatible endpoint URL
export S3_ENDPOINT_URL="http://s3.example.com:8000"

# Required: Your S3 credentials
export AWS_ACCESS_KEY_ID="YOUR_ACCESS_KEY_ID"
export AWS_SECRET_ACCESS_KEY="YOUR_SECRET_ACCESS_KEY"
# export AWS_SESSION_TOKEN="YOUR_SESSION_TOKEN"  # Optional, uncomment if needed

# Required: Your S3 bucket name for testing
export S3_BUCKET_NAME="my-test-bucket"

# Optional: Customize these if needed
export S3_REGION="us-east-1"
export SECRET_NAME="s3-secret"
export NAMESPACE="scality-s3-csi"
```

### Step 3: Create namespace (recommended)

Create a dedicated namespace for better security isolation:

```bash
kubectl create namespace ${NAMESPACE}
```

### Step 4: Create S3 credentials secret

Method 1: From environment variables (quick but less secure)

```bash
kubectl create secret generic ${SECRET_NAME} \
  --from-literal=key_id="${AWS_ACCESS_KEY_ID}" \
  --from-literal=secret_access_key="${AWS_SECRET_ACCESS_KEY}" \
  --namespace=${NAMESPACE}
```

If you need a session token, add it:

```bash
kubectl patch secret ${SECRET_NAME} -n ${NAMESPACE} --type='json' -p='[{"op": "add", "path": "/data/session_token", "value": "'$(echo -n "${AWS_SESSION_TOKEN}" | base64)'"}]'
```

Method 2: From files (more secure alternative)

Create credential files locally (these won't appear in shell history):

```bash
echo -n "${AWS_ACCESS_KEY_ID}" > /tmp/key_id
echo -n "${AWS_SECRET_ACCESS_KEY}" > /tmp/secret_access_key
```

Create secret from files:

```bash
kubectl create secret generic ${SECRET_NAME} \
  --from-file=key_id=/tmp/key_id \
  --from-file=secret_access_key=/tmp/secret_access_key \
  --namespace=${NAMESPACE}
```

Clean up temporary files:

```bash
rm /tmp/key_id /tmp/secret_access_key
```

### Step 5: Install the CSI driver

```bash
helm install mountpoint-s3-csi-driver scality/scality-mountpoint-s3-csi-driver \
  --set node.s3EndpointUrl="${S3_ENDPOINT_URL}" \
  --set node.s3Region="${S3_REGION}" \
  --set s3CredentialSecret.name="${SECRET_NAME}" \
  --namespace ${NAMESPACE}
```

### Step 6: Verify installation

Check if the CSI driver pods are running:

```bash
kubectl get pods -n ${NAMESPACE} -l app.kubernetes.io/name=scality-mountpoint-s3-csi-driver
```

Check CSI driver registration:

```bash
kubectl get csidriver s3.csi.scality.com
```

## Create and Test a Volume

### Step 1: Create PV and PVC

```bash
cat <<EOF | kubectl apply -f -
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-pv-test
spec:
capacity:
    storage: 1200Gi # This value is not enforced by S3 but required by Kubernetes
  accessModes:
    - ReadWriteMany
  storageClassName: "" # Required for static provisioning
  claimRef: # To ensure no other PVCs can claim this PV
    namespace: default # Namespace is required even though it's in "default" namespace.
    name: s3-pvc-test # Name of your PVC
  mountOptions:
    - allow-delete
    - allow-overwrite
  csi:
    driver: s3.csi.scality.com
    volumeHandle: s3-csi-driver-volume
    volumeAttributes:
      bucketName: s3-csi-driver

---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-pvc-test
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: "" # Required for static provisioning
  resources:
    requests:
      storage: 1Gi
  volumeName: s3-pv-test
EOF
```

### Step 2: Create test pod

```bash
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: s3-app
spec:
  containers:
    - name: app
      image: ubuntu
      command: ["/bin/sh"]
      args: ["-c", "echo 'Hello from the container!' >> /data/$(date -u).txt; tail -f /dev/null"]
      volumeMounts:
        - name: persistent-storage
          mountPath: /data
  volumes:
    - name: persistent-storage
      persistentVolumeClaim:
        claimName: s3-pvc-test
EOF
```

### Step 3: Wait for pod to be ready

```bash
kubectl wait --for=condition=Ready pod/s3-test-pod --timeout=60s
```

## Verification

### Check the mount

Verify the pod is running:

```bash
kubectl get pod s3-test-pod
```

Check if S3 bucket is mounted:

```bash
kubectl exec s3-test-pod -- df -h /data
```

List contents of the bucket:

```bash
kubectl exec s3-test-pod -- ls -la /data
```

### Test read/write operations

Write a test file:

```bash
kubectl exec s3-test-pod -- sh -c "echo 'Hello from Scality S3 CSI Driver!' > /data/test-file.txt"
```

Read the file back:

```bash
kubectl exec s3-test-pod -- cat /data/test-file.txt
```

Verify the file exists and check its details:

```bash
kubectl exec s3-test-pod -- ls -la /data/test-file.txt
```

This quick start provides a basic overview. For more advanced configurations and features, please refer to the full documentation.

- **[Configuration Options](configuration/index.md)** for detailed settings.
- **[How-To Guides](how-to/static-provisioning.md)** for common use cases.
- **[Minimal Helm Example](examples/minimal-helm.yaml)**: A self-contained example demonstrating a minimal deployment, PVC and Pod manifest.
  (Note: this example YAML uses a local Helm chart path. Adapt as needed for your environment).
