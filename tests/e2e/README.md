# End-to-End Tests for Scality CSI Driver

This directory contains the end-to-end (E2E) test framework for the Scality S3 CSI driver. The tests verify the driver's functionality in a real Kubernetes cluster by creating S3 buckets, mounting them as volumes, and validating data persistence.

## Structure

- `e2e_test.go`: Main test file with Kubernetes CSI test framework integration
- `testdriver.go`: Implementation of Kubernetes test driver interfaces for S3 storage
- `pkg/s3client`: S3 client for creating managing buckets and volumes 
- `customsuites/`: Custom test suites specific to S3 CSI driver functionality
- `scripts/`: Helper scripts for test automation

## Tests

The E2E tests verify:
- S3 bucket creation and cleanup
- CSI driver integration with Kubernetes
- Data persistence with pre-provisioned PVs
- Volume mounting and unmounting
- Mount options and permission handling
- Multi-volume and multi-pod scenarios

Custom test suites provide specialized validation for S3-specific functionality. For details on these custom tests, see the [Custom Test Suites README](customsuites/README.md).

## Running Tests

### Prerequisites

- A running Kubernetes cluster
- Kubectl configured to access the cluster
- S3-compatible storage service (like Scality)
- Access credentials for S3 storage

### Environment Variables

- `KUBECONFIG`: Path to the Kubernetes configuration file (required if not using the default ~/.kube/config)

### Command-line Options

The test framework accepts the following command-line options:

- `--access-key-id`: S3 access key (required)
- `--secret-access-key`: S3 secret key (required)
- `--s3-endpoint-url`: S3 endpoint URL (required)

### Direct Go Test

```bash
# Navigate to the e2e directory
cd tests/e2e

# Run the tests with S3 credentials and KUBECONFIG
KUBECONFIG=/path/to/kubeconfig go test --access-key-id=accessKey1 --secret-access-key=verySecretKey1 --s3-endpoint-url=http://localhost:8000 -v -ginkgo.v ./...
```

### Using Makefile

```bash
# From the project root
KUBECONFIG=/path/to/kubeconfig make e2e S3_ENDPOINT_URL=http://localhost:8000 ACCESS_KEY_ID=accessKey1 SECRET_ACCESS_KEY=verySecretKey1
```

### Using Helper Scripts

```bash
# From the project root
KUBECONFIG=/path/to/kubeconfig ./tests/e2e/scripts/run.sh test --endpoint-url http://localhost:8000 --access-key-id accessKey1 --secret-access-key verySecretKey1
```

## Test Automation

The E2E tests can be integrated into CI/CD pipelines. The tests generate JUnit XML reports for integration with testing platforms.

To generate a JUnit report:

```bash
KUBECONFIG=/path/to/kubeconfig go test --access-key-id=accessKey1 --secret-access-key=verySecretKey1 --s3-endpoint-url=http://localhost:8000 -v -ginkgo.v ./... -ginkgo.junit-report=report.xml
```

## Adding Tests

Additional tests can be added to the framework by:

1. Extending the testdriver.go implementation
2. Adding new test suites to the CSITestSuites array in e2e_test.go
3. For custom S3-specific tests, adding new test suites in the customsuites/ directory (see the [Custom Test Suites README](customsuites/README.md) for guidelines)
