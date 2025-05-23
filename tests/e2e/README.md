# E2E Tests

This directory contains end-to-end (e2e) tests for the Scality S3 CSI Driver. These tests validate the driver's
functionality in real Kubernetes environments by performing complete workflows including installation, volume
operations, and cleanup.

## Running Tests

Basic test execution:

```bash
make e2e
```

## Test Structure

The tests are organized into several components:

- S3 bucket creation and cleanup operations
- CSI driver installation and verification  
- Pod mounting and data operations
- Volume lifecycle management
- Cleanup and resource removal

## Test Configuration

Tests can be configured using environment variables or Make parameters:

- `CSI_NAMESPACE`: Kubernetes namespace for the driver (default: `kube-system`)
- `S3_ENDPOINT_URL`: S3 endpoint URL for testing
- `ACCESS_KEY_ID`: S3 access key for authentication
- `SECRET_ACCESS_KEY`: S3 secret key for authentication

Example with custom configuration:

```bash
make e2e \
  CSI_NAMESPACE=test-namespace \
  S3_ENDPOINT_URL=https://s3.example.com \
  ACCESS_KEY_ID=test_key \
  SECRET_ACCESS_KEY=test_secret
```

## Test Suites

### Basic E2E Tests

Standard test suite that validates:

- Driver installation
- Volume mounting
- File operations
- Cleanup procedures

### Go Test Suite

More comprehensive tests written in Go that include:

- Multiple volume scenarios
- Error handling validation
- Performance benchmarks
- Edge case testing

## Custom Test Suites

The `customsuites/` directory contains specialized test configurations for specific scenarios and environments.
See [Custom Test Suites](./customsuites/README.md) for detailed information.

## Troubleshooting

Common test failures and solutions:

### Authentication Errors

- Verify S3 credentials are correct
- Check endpoint URL accessibility
- Ensure bucket permissions are adequate

### Network Issues

- Validate cluster connectivity to S3 endpoint
- Check firewall and security group settings
- Verify DNS resolution

### Resource Conflicts

- Ensure namespace is clean before testing
- Check for existing CSI driver installations
- Verify sufficient cluster resources

## Test Artifacts

Tests generate artifacts in the following locations:

- Test logs: `./test-logs/`
- Generated manifests: `./generated/`
- Temporary files: `/tmp/e2e-tests/`

For development and debugging purposes, artifacts are preserved after test completion unless explicitly cleaned up.
