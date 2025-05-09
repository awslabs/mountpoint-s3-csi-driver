# Installation

## Prerequisites

<!-- TODO(S3CSI-17) Add minimum supported kubernetes version -->
* Kubernetes Version >=

## Installation

<!-- TODO(S3CSI-17): Update Installation guide -->

### Configure access to S3

### Deploy driver

You may deploy the Mountpoint for Amazon S3 CSI driver via Kustomize, Helm.

#### Kustomize

<!-- TODO(S3CSI-18): Support Kustomize deployment anbd update docs -->

> [!WARNING]
> Using the main branch to deploy the driver is not supported. The main branch may contain upcoming features incompatible with the currently released stable version of the driver.

#### Helm

<!-- TODO(S3CSI-17): Add helm installation steps -->

Review the [configuration values](https://github.com/scality/mountpoint-s3-csi-driver/blob/main/charts/scality-mountpoint-s3-csi-driver/values.yaml) for the Helm chart.

> [!IMPORTANT]
> The S3 endpoint URL (`node.s3EndpointUrl`) is a **required** parameter when installing the CSI driver via Helm.
> The driver will not function without this parameter and the Helm installation will fail if it's not provided.

Example installation with the required S3 endpoint URL:

```sh
helm install scality-mountpoint-s3-csi-driver ./charts/scality-mountpoint-s3-csi-driver \
  --namespace kube-system \
  --set node.s3EndpointUrl=https://s3.your-scality-cluster.com
```

#### Once the driver has been deployed, verify the pods are running

```sh
kubectl get pods -n kube-system -l app.kubernetes.io/name=scality-mountpoint-s3-csi-driver
```

### Volume Configuration Example

Follow the [README for examples](https://github.com/scality/mountpoint-s3-csi-driver/tree/main/examples/kubernetes/static_provisioning) on using the driver.

### Uninstalling the driver

#### Helm

```
helm uninstall scality-mountpoint-s3-csi-driver --namespace kube-system
```

#### Kustomize

```
kubectl delete -k "github.com/scality/mountpoint-s3-csi-driver/deploy/kubernetes/overlays/stable/?ref=<YOUR-CSI-DRIVER-VERION-NUMBER>"
```
