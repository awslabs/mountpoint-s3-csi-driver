# Quick Start Guide

This guide will walk you through a minimal installation of the Scality S3 CSI Driver and how to verify it.

## Prerequisites

- A Kubernetes cluster (v1.30.0+).
- Helm installed.
- `kubectl` configured to communicate with your cluster.
- An S3 endpoint URL. This is a **required** parameter for the Helm chart.

## Installation

1. **Add the Scality Helm repository:**

    ```bash
    helm repo add scality https://scality.github.io/mountpoint-s3-csi-driver/charts
    helm repo update
    ```

2. **Install the driver using Helm:**

    Replace `https://s3.your-scality-cluster.com` with your actual S3 endpoint URL.

    ```bash
    helm install mountpoint-s3-csi-driver scality/scality-mountpoint-s3-csi-driver \
      --set node.s3EndpointUrl=https://s3.your-scality-cluster.com \
      --namespace kube-system # Or your preferred namespace
    ```

    You should see output similar to this:

    ```bash
    NAME: mountpoint-s3-csi-driver
    LAST DEPLOYED: <timestamp>
    NAMESPACE: kube-system
    STATUS: deployed
    REVISION: 1
    TEST SUITE: None
    NOTES:
    The Scality S3 CSI driver has been installed.
    ...
    ```

## Create a Test Pod

1. **Create a PersistentVolume (PV) and PersistentVolumeClaim (PVC).**

    Save the following YAML as `test-pv-pvc.yaml`:

    ```yaml
    ---
    apiVersion: v1
    kind: PersistentVolume
    metadata:
      name: s3-pv-test
    spec:
      accessModes:
        - ReadWriteMany # Mountpoint for S3 typically supports ReadWriteMany
      capacity:
        storage: 1Gi # This is a nominal value for CSI drivers like Mountpoint for S3
      csi:
        driver: s3.csi.scality.com
        volumeHandle: my-s3-bucket-name # Replace with your S3 bucket name
        # Optional: Add volumeAttributes if needed for your S3 provider or Mountpoint
        # volumeAttributes:
        #   region: "us-east-1" # Example attribute
      storageClassName: "" # Important for static provisioning: leave empty or use a custom SC
    ---
    apiVersion: v1
    kind: PersistentVolumeClaim
    metadata:
      name: s3-pvc-test
    spec:
      accessModes:
        - ReadWriteMany
      resources:
        requests:
          storage: 1Gi
      volumeName: s3-pv-test # Must match the PV name
      storageClassName: "" # Important for static provisioning
    ```

    Replace `my-s3-bucket-name` with the name of an S3 bucket you have access to.
    Apply the YAML:

    ```bash
    kubectl apply -f test-pv-pvc.yaml
    ```

2. **Create a Pod that uses the PVC.**

    Save the following YAML as `test-pod.yaml`:

    ```yaml
    apiVersion: v1
    kind: Pod
    metadata:
      name: s3-test-pod
    spec:
      containers:
        - name: s3-test-container
          image: busybox # A small image for testing
          command: ["/bin/sh", "-c", "sleep 3600"] # Keep the pod running
          volumeMounts:
            - name: s3-volume
              mountPath: /data # The path where the S3 bucket will be mounted
      volumes:
        - name: s3-volume
          persistentVolumeClaim:
            claimName: s3-pvc-test # Must match the PVC name
    ```

    Apply the YAML:

    ```bash
    kubectl apply -f test-pod.yaml
    ```

## Verification

1. **Check the Pod status:**

    ```bash
    kubectl get pod s3-test-pod
    ```

    Wait for the pod to be in the `Running` state.

2. **Verify the mount inside the Pod:**

    ```bash
    kubectl exec -it s3-test-pod -- df -h /data
    ```

    You should see output indicating the S3 bucket is mounted at `/data`. The filesystem type might appear as `fuse.mountpoint-s3` or similar.

    Example output:

    ```bash
    Filesystem      Size  Used Avail Use% Mounted on
    my-s3-bucket-name  256T     0  256T   0% /data
    ```

    (The size reported will depend on your S3 provider and bucket configuration).

3. **Try writing and reading a file (if your bucket permissions allow):**

    ```bash
    kubectl exec -it s3-test-pod -- sh -c "echo 'Hello S3!' > /data/hello.txt && cat /data/hello.txt"
    ```

    If successful, you'll see `Hello S3!` printed.

## Cleanup

```bash
kubectl delete pod s3-test-pod
kubectl delete pvc s3-pvc-test
kubectl delete pv s3-pv-test
# Optional: Delete the Helm release
# helm uninstall mountpoint-s3-csi-driver --namespace kube-system
```

This quick start provides a basic overview. For more advanced configurations and features, please refer to the full documentation.
