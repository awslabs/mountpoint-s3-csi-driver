# E2E Scripts for Scality CSI Driver

This directory contains scripts for end-to-end testing of the Scality CSI driver.

## Current Structure

The main entry point is `run.sh` which supports the following commands:
- `install`: Installs and verifies the CSI driver 
- `test`: Runs end-to-end tests
- `go-test`: Runs only Go-based tests directly (skips verification checks)
- `all`: Installs the driver and runs tests
- `uninstall`: Uninstalls the CSI driver
- `help`: Shows usage information

## Required Parameters

For tests that interact with S3, the following parameters are required:

- `--endpoint-url`: S3 endpoint URL (e.g., http://localhost:8000)
- `--access-key-id`: S3 access key for authentication
- `--secret-access-key`: S3 secret key for authentication, S3 endpoint should be operational

These parameters must be passed to both the `install` and `test` commands separately, or to the `all` command which will handle both steps.

## Environment Variables

- `KUBECONFIG`: Path to the Kubernetes configuration file (required if not using the default ~/.kube/config)

## Optional Parameters

- `--namespace`: Specify the namespace to use (default: kube-system)
- `--skip-go-tests`: Skip executing Go-based end-to-end tests (for test command)
- `--junit-report`: Generate JUnit XML report at specified path (for test command)

## Usage

Scripts in this directory can be called directly or from the Makefile targets.

### Direct script usage:

```bash
# Install the driver
./run.sh install --endpoint-url http://localhost:8000 --access-key-id accessKey1 --secret-access-key verySecretKey1

# Run tests
./run.sh test --endpoint-url http://localhost:8000 --access-key-id accessKey1 --secret-access-key verySecretKey1

# Run only Go tests
./run.sh go-test --endpoint-url http://localhost:8000 --access-key-id accessKey1 --secret-access-key verySecretKey1

# Install and test in one command
./run.sh all --endpoint-url http://localhost:8000 --access-key-id accessKey1 --secret-access-key verySecretKey1
```

### Using Makefile targets:

```bash
# Install the driver
make csi-install S3_ENDPOINT_URL=http://localhost:8000 ACCESS_KEY_ID=accessKey1 SECRET_ACCESS_KEY=verySecretKey1

# Run tests
make e2e S3_ENDPOINT_URL=http://localhost:8000 ACCESS_KEY_ID=accessKey1 SECRET_ACCESS_KEY=verySecretKey1

# Run only Go tests
make e2e-go S3_ENDPOINT_URL=http://localhost:8000 ACCESS_KEY_ID=accessKey1 SECRET_ACCESS_KEY=verySecretKey1

# Install and test in one command
KUBECONFIG=/Users/anurag4dsb/.kube/config make csi-all S3_ENDPOINT_URL=http://localhost:8000 ACCESS_KEY_ID=accessKey1 SECRET_ACCESS_KEY=verySecretKey1  CSI_IMAGE_TAG=<image-tag> CSI_IMAGE_REPOSITORY=ghcr.io/scality/mountpoint-s3-csi-driver
```
