# Custom Test Suites

Custom test suites for S3-specific functionality that extends the standard Kubernetes CSI test framework. These tests
validate unique S3 behaviors and edge cases specific to object storage filesystems.

## Overview

The standard Kubernetes CSI test framework provides general storage validation, but S3 object storage has unique
characteristics that require specialized testing:

- Object storage semantics (eventual consistency, listing behavior)
- Large file handling and streaming capabilities  
- Multi-part upload behavior and performance
- Bucket-level configurations and mount options
- Network resilience and retry mechanisms

## Test Suites

### `object_storage_semantics_test.go`

Tests S3-specific object storage behaviors:

#### File Operations

- Sequential writes and reads
- File replacement semantics (S3 objects are immutable)
- Directory listing consistency
- Large file streaming (>5GB multi-part uploads)

#### Performance Testing  

- Concurrent read/write operations
- Directory traversal speed with large numbers of objects
- Memory usage during large operations

#### Edge Cases

- File names with special characters
- Unicode and international character support
- Object key length limits and validation

### `bucket_configuration_test.go`

Validates bucket-level configurations:

#### Mount Options

- Region-specific bucket access
- Custom endpoint configurations
- Path-style vs virtual-hosted-style URLs  
- SSL/TLS certificate validation

#### Authentication

- Multiple credential providers
- STS token refresh and expiration
- Cross-account bucket access permissions

### `network_resilience_test.go`

Tests network-related scenarios:

#### Connection Handling

- Network interruption and recovery
- Timeout configuration validation
- Retry logic for failed operations
- DNS resolution failures and fallbacks

#### Performance Under Load

- High-throughput data transfer
- Concurrent connection limits  
- Bandwidth throttling and quality of service

## Running Custom Tests

### All Custom Tests

```bash
cd tests/e2e/customsuites
go test -v ./...
```

### Specific Test Suite

```bash
cd tests/e2e/customsuites  
go test -v -run "TestObjectStorageSemantics" ./...
```

### With Custom Configuration

```bash
cd tests/e2e/customsuites
go test -v ./... \
  --s3-endpoint-url=https://s3.example.com \
  --access-key-id=your_key \
  --secret-access-key=your_secret
```

## Configuration

Custom tests use the same configuration as the main E2E test suite but support additional S3-specific parameters:

### Standard Parameters

- `--s3-endpoint-url`: S3 endpoint URL (required)
- `--access-key-id`: S3 access key (required)  
- `--secret-access-key`: S3 secret key (required)

### Custom Parameters

- `--test-bucket-prefix`: Prefix for test bucket names (default: `csi-test`)
- `--cleanup-policy`: Test cleanup behavior (`always`, `on-success`, `never`)
- `--performance-threshold`: Performance test thresholds
- `--large-file-size`: Size for large file tests (default: `1GB`)

## Adding New Custom Tests

### Test Structure

Custom tests should follow this structure:

```go
func TestNewCustomSuite(t *testing.T) {
    // Setup test environment
    testEnv := setupTestEnvironment(t)
    defer testEnv.Cleanup()

    // Run test cases
    t.Run("specific_scenario", func(t *testing.T) {
        // Test implementation
    })
}
```

### Test Environment Setup

Use the provided test utilities:

```go
// Create test bucket
bucket := testEnv.CreateTestBucket(t, "test-bucket")

// Deploy CSI driver  
driver := testEnv.DeployCSIDriver(t)

// Create test pod with S3 volume
pod := testEnv.CreatePodWithS3Volume(t, bucket, driver)
```

### Best Practices

#### Resource Management

- Always clean up test resources (buckets, pods, PVs)
- Use unique test identifiers to avoid conflicts
- Implement proper timeout handling for async operations

#### Test Isolation  

- Each test should be independent and runnable in isolation
- Don't rely on external state or previous test results
- Use separate buckets/namespaces for concurrent test execution

#### Error Handling

- Test both success and failure scenarios  
- Validate error messages and codes
- Include retry logic for eventually consistent operations

## CI Integration

Custom tests are integrated into the CI pipeline:

```yaml
# Example CI configuration  
- name: Run Custom Test Suites
  run: |
    cd tests/e2e/customsuites
    go test -v ./... \
      --s3-endpoint-url=${{ secrets.S3_ENDPOINT }} \
      --access-key-id=${{ secrets.ACCESS_KEY_ID }} \
      --secret-access-key=${{ secrets.SECRET_ACCESS_KEY }} \
      --cleanup-policy=always
```

## Troubleshooting

### Common Issues

### Test Timeouts  

- Increase timeout values for network operations
- Check S3 endpoint connectivity and latency
- Monitor resource usage during large file tests

### Authentication Failures

- Verify S3 credentials have appropriate permissions
- Check bucket policies and access controls
- Validate endpoint URL format and accessibility

### Resource Cleanup

- Manually clean up test resources if tests fail
- Check for orphaned buckets with test prefixes
- Monitor namespace resource usage

### Debug Mode

Enable verbose logging:

```bash
go test -v ./... --debug-level=debug --log-output=./test.log
```
