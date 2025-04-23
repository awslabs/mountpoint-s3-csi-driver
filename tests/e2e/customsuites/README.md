# Custom Test Suites for S3 CSI Driver

This package provides test suites specific to the Scality S3 CSI driver. It extends the standard Kubernetes storage test framework with tests that validate Scality-specific functionality.

## Test Suites

### Mount Options Test Suite

The mount options test suite (`mountoptions.go`) verifies that the S3 CSI driver correctly handles volume mount options related to permissions, user/group IDs, and access controls when mounting S3 buckets in Kubernetes pods. It includes tests for:

- Access to volumes when mounted with non-root user/group IDs
- Proper enforcement of permissions when mount options are absent
- File and directory ownership when mounting with specific uid/gid

### Utilities

The `util.go` file contains utility functions that support all test suites:

- Helpers for file operations (read/write/verify)
- Pod configuration utilities
- Volume resource creation with custom mount options

## Adding New Test Suites

When adding new test suites to this package, follow these guidelines:

1. Create a new file named after the feature being tested (e.g., `multivolume.go`)
2. Implement the storage framework's `TestSuite` interface
3. Create an initializer function named `InitXXXTestSuite()`
4. Register the new test suite in `tests/e2e/e2e_test.go`
5. Add documentation for your test suite in this README

## Running Tests

Tests in this package are automatically executed as part of the [E2E test suite](../e2e_test.go) when running:

```
go test -v ./tests/e2e/...
```

See the [main project documentation](../README.md) for details on setting up the test environment with proper credentials and S3 endpoint configuration.
