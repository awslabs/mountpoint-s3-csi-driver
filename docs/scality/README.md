# Scality S3 CSI Driver - Quick Start Guide

This guide explains how to use the Scality S3 CSI Driver with Kubernetes, allowing your applications to use Scality S3 storage as persistent volumes.

## What You Need

- Kubernetes cluster (version 1.20+)
- `kubectl` configured to access your cluster
- Helm 3
- Access credentials for your Scality S3 service
- **S3 endpoint URL** (REQUIRED)

## Installation in 30 Seconds

The fastest way to install is using our Makefile:

```bash
make csi-install \
  S3_ENDPOINT_URL=https://s3.your-scality.com \
  ACCESS_KEY_ID=your_access_key \
  SECRET_ACCESS_KEY=your_secret_key
```

## Core Commands

| Command | Description |
|---------|-------------|
| `make csi-install` | Install the driver |
| `make e2e` | Run tests on installed driver |
| `make e2e-all` | Install driver and run tests |
| `make csi-uninstall` | Remove the driver (interactive) |
| `make csi-uninstall-clean` | Remove driver and namespace |
| `make csi-uninstall-force` | Force complete removal |

## Installation Options

### Required Parameters

When installing, you must provide these values:

- `S3_ENDPOINT_URL`: Your Scality S3 endpoint (REQUIRED, installation will fail without this)
- `ACCESS_KEY_ID`: S3 access key
- `SECRET_ACCESS_KEY`: S3 secret key

Example:

```bash
make csi-install \
  S3_ENDPOINT_URL=https://s3.example.com \
  ACCESS_KEY_ID=your_access_key \
  SECRET_ACCESS_KEY=your_secret_key
```

> [!IMPORTANT]
> The S3_ENDPOINT_URL is now a strict requirement - the installation will fail if it's not provided.

### Optional Parameters

- `CSI_IMAGE_TAG`: Specify a particular driver version
- `VALIDATE_S3`: Set to "true" to verify S3 credentials before installing
- `ADDITIONAL_ARGS`: Any extra arguments to pass to the script

Example with options:

```bash
make csi-install \
  CSI_IMAGE_TAG=v1.14.0 \
  S3_ENDPOINT_URL=https://s3.example.com \
  ACCESS_KEY_ID=your_access_key \
  SECRET_ACCESS_KEY=your_secret_key \
  VALIDATE_S3=true
```

### S3 Validation

When you set `VALIDATE_S3=true`, the script will perform validation checks before installation:

1. **Basic Endpoint Connectivity**: Verifies that the S3 endpoint URL is reachable.
   - Checks if the endpoint exists and responds
   - Verifies it looks like an S3 service (by checking for appropriate responses)

2. **Credential Validation** (if AWS CLI is installed):
   - Validates that your access key and secret key work correctly
   - Shows available buckets if successful

If AWS CLI is not installed, only the endpoint connectivity will be validated, but the credentials cannot be checked.
The installation will proceed with a warning that credential issues might occur later.

## Common Scality Configurations

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
  ACCESS_KEY_ID=ringkey \
  SECRET_ACCESS_KEY=ringsecret
```

### Scality Artesca

```bash
make csi-install \
  S3_ENDPOINT_URL=https://s3.artesca.example.com \
  ACCESS_KEY_ID=artescakey \
  SECRET_ACCESS_KEY=artescasecret
```

## Verifying Installation

Check that the driver is running correctly:

```bash
# View CSI driver pods
kubectl get pods -n mount-s3

# Verify CSI driver registration
kubectl get csidrivers
```

## Running Tests

### Testing an Installed Driver

If you've already installed the driver:

```bash
make e2e
```

### Installing and Testing in One Step

Install the driver and run all tests together:

```bash
make e2e-all \
  S3_ENDPOINT_URL=https://s3.example.com \
  ACCESS_KEY_ID=your_access_key \
  SECRET_ACCESS_KEY=your_secret_key
```

## Uninstalling

### Standard Uninstall (Interactive)

Will ask before deleting the namespace:

```bash
make csi-uninstall
```

### Clean Uninstall (Non-interactive)

Automatically deletes the namespace:

```bash
make csi-uninstall-clean
```

### Force Uninstall

For when normal uninstall methods aren't working:

```bash
make csi-uninstall-force
```

## Troubleshooting

### Can't Connect to S3 Endpoint

**What to check:**

- Is the S3 endpoint URL correct?
- Is there network connectivity to the endpoint?
- Are you using the right protocol (http:// or https://)?

### Authentication Problems

**What to check:**

- Double-check your access key and secret key
- Verify the credentials work with other S3 tools
- Check if your credentials have expired

### Pods Won't Start

**What to check:**

- View pod status: `kubectl get pods -n mount-s3`
- Check pod details: `kubectl describe pods -n mount-s3`
- Look at logs: `kubectl logs -n mount-s3 <pod-name> -c s3-driver`

### Namespace Stuck When Uninstalling

If the namespace is stuck in "Terminating" state:

```bash
make csi-uninstall-force
```

## Error Codes

When reporting issues, include the error code from your output:

| Code | Meaning | Common Cause |
|------|---------|--------------|
| 1    | General error | Various issues |
| 10   | Helm uninstall error | Resource conflicts |
| 11   | Namespace deletion error | Stuck resources |
| 12   | CSI driver deletion error | Resource conflicts |
