# Scality S3 CSI Driver - Command Cheatsheet

This is a quick reference for common operations with the Scality S3 CSI Driver. Each command includes the required
parameters and brief descriptions of what it does.

For full documentation and examples, see the [main README](README.md).

**Context:** This document is a reference for experienced users. It provides quick command examples and configuration
snippets for installation, testing, and uninstallation.

**Note:** The `kube-system` namespace is the recommended and default namespace for deploying the Scality S3 CSI Driver.
Using other namespaces is supported only for testing and development purposes.

## Installation

### Basic Installation

```bash
make csi-install \
  S3_ENDPOINT_URL=http://localhost:8000 \
  ACCESS_KEY_ID=accessKey1 \
  SECRET_ACCESS_KEY=verySecretKey1 \
  VALIDATE_S3=true
```

This installs the CSI driver in the default namespace (kube-system).

### Installation with Custom Namespace

```bash
make csi-install \
  S3_ENDPOINT_URL=http://localhost:8000 \
  ACCESS_KEY_ID=accessKey1 \
  SECRET_ACCESS_KEY=verySecretKey1 \
  CSI_NAMESPACE=custom-namespace
```

### Installation with All Options

```bash
make csi-install \
  CSI_IMAGE_TAG=v1.14.0 \
  CSI_IMAGE_REPOSITORY=my-registry/mountpoint-s3-csi-driver \
  CSI_NAMESPACE=custom-namespace \
  S3_ENDPOINT_URL=https://s3.example.com \
  ACCESS_KEY_ID=your_key \
  SECRET_ACCESS_KEY=your_secret \
  VALIDATE_S3=true
```

## Testing

### Basic Testing of Already Installed Driver

```bash
make e2e
```

Tests the CSI driver in the default namespace (kube-system).

### Testing with Custom Namespace

```bash
make e2e CSI_NAMESPACE=custom-namespace
```

### Run Only Basic Verification Tests (Skip Go Tests)

```bash
make e2e-verify CSI_NAMESPACE=custom-namespace
```

This command only checks if:

- The CSI driver pods are running correctly in the specified namespace (or in any namespace as a fallback)
- The CSI driver is properly registered in the cluster
It skips running the Go-based tests.

### Run Only Go-Based End-to-End Tests

```bash
make e2e-go CSI_NAMESPACE=custom-namespace
```

### Advanced Testing with Go Test (for filtering tests)

```bash
# Go to the tests directory
cd tests/e2e

# Run tests with focus on specific test patterns (runs only matching tests)
go test -v -tags=e2e -ginkgo.focus="Basic Functionality" -args -namespace=custom-namespace

# Skip specific test patterns
go test -v -tags=e2e -ginkgo.skip="Volume Operations" -args -namespace=custom-namespace

# Combine multiple filters
go test -v -tags=e2e -ginkgo.focus="Basic" -ginkgo.skip="Volume" -args -namespace=custom-namespace
```

### Install and Test in One Step

```bash
make e2e-all \
  S3_ENDPOINT_URL=https://s3.example.com \
  ACCESS_KEY_ID=your_key \
  SECRET_ACCESS_KEY=your_secret
```

Installs in the default namespace (kube-system).

### Install with Custom Namespace and Test

```bash
make e2e-all \
  S3_ENDPOINT_URL=https://s3.example.com \
  ACCESS_KEY_ID=your_key \
  SECRET_ACCESS_KEY=your_secret \
  CSI_NAMESPACE=custom-namespace
```

## Uninstallation

### Uninstall from Default Namespace

```bash
make csi-uninstall
```

Uninstalls from the default namespace (kube-system). This will NOT delete the kube-system namespace.

### Uninstall from a Custom Namespace

```bash
make csi-uninstall CSI_NAMESPACE=custom-namespace
```

By default, this will prompt before deleting the custom namespace.

### Auto Uninstall with Custom Namespace Deletion

```bash
make csi-uninstall-clean CSI_NAMESPACE=custom-namespace
```

This automatically deletes the custom namespace without prompting.
Note that if you don't specify a custom namespace, the kube-system namespace will NOT be deleted.

### Force Uninstall

```bash
make csi-uninstall-force CSI_NAMESPACE=custom-namespace
```

Use this when standard uninstall methods aren't working.
Note that if you don't specify a custom namespace, the kube-system namespace will NOT be deleted.

## Common Configurations

### Local Development

```bash
make csi-install \
  S3_ENDPOINT_URL=http://localhost:8000 \
  ACCESS_KEY_ID=localkey \
  SECRET_ACCESS_KEY=localsecret
```

### Scality Ring

```bash
make csi-install \
  S3_ENDPOINT_URL=https://s3.ring.example.com \
  ACCESS_KEY_ID=ringaccesskey \
  SECRET_ACCESS_KEY=ringsecretaccesskey \
  CSI_NAMESPACE=scality-ring
```

### Scality Artesca

```bash
make csi-install \
  S3_ENDPOINT_URL=https://s3.artesca.example.com \
  ACCESS_KEY_ID=artescaaccesskey \
  SECRET_ACCESS_KEY=artescasecretaccesskey \
  CSI_NAMESPACE=scality-artesca
```
