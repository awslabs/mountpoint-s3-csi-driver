# E2E Test Scripts

These scripts provide automation for installing, testing, and managing the Scality S3 CSI Driver in Kubernetes environments.

## Scripts

- `install`: Installs and verifies the CSI driver with the given parameters
- `test`: Runs end-to-end tests on an already installed CSI driver
- `uninstall`: Removes the CSI driver from the cluster
- `all`: Combines install, test, and uninstall operations
- `go-test`: Runs only the Go-based end-to-end tests

## Quick Examples

Start a documentation server:

```bash
# From project root
mkdocs serve
```

This will start a development server at <http://localhost:8000> with live reloading for documentation changes.

For complete usage instructions and examples, see the main project documentation and the individual script help output
(`./run.sh <command> --help`).

## Script Organization

### Core Automation Scripts

- `run.sh`: Main entry point that delegates to specific operation scripts
- `install.sh`: Handles CSI driver installation with validation
- `test.sh`: Executes test suites with configurable parameters
- `cleanup.sh`: Provides cleanup and uninstall functionality

### Module System

The scripts use a modular design with shared functionality in the `modules/` directory:

- `validation.sh`: Input validation and prerequisite checking
- `k8s.sh`: Kubernetes cluster interaction utilities  
- `s3.sh`: S3 endpoint validation and bucket operations
- `logging.sh`: Consistent logging and error reporting

### Configuration Files

- `config/`: Default configuration templates
- `templates/`: YAML templates for Kubernetes resources

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

- `--endpoint-url`: S3 endpoint URL (e.g., <http://localhost:8000>)
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

### Direct script usage

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

### Using Makefile targets

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
