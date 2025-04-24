# Custom Test Suites for S3 CSI Driver

This package provides test suites specific to the Scality S3 CSI driver. It extends the standard Kubernetes storage test framework with tests that validate Scality-specific functionality.

## Test Suites

### Mount Options Test Suite

The mount options test suite (`mountoptions.go`) verifies that the S3 CSI driver correctly handles volume mount options related to permissions, user/group IDs, and access controls when mounting S3 buckets in Kubernetes pods. It includes tests for:

- Access to volumes when mounted with non-root user/group IDs
- Proper enforcement of permissions when mount options are absent
- File and directory ownership when mounting with specific uid/gid

### Multi-Volume Test Suite

The multi-volume test suite (`multivolume.go`) validates scenarios involving multiple volumes and pods to ensure the S3 CSI driver properly handles concurrent access and volume isolation. It includes tests for:

- Multiple pods accessing the same volume simultaneously
- A single pod accessing multiple volumes concurrently
- Data persistence across pod recreations with the same volume

This suite verifies the core functionality needed for both stateless and stateful workloads in Kubernetes when using S3 CSI volumes.

### Cache Test Suite

The cache test suite (`cache.go`) provides smoke tests to validate the caching functionality of the Mountpoint S3 client when deployed through the CSI driver. It includes tests for:

- Basic read/write operations with local caching enabled
- Persistence of cached data even after removal from the underlying S3 bucket
- Cache behavior with different user contexts (root and non-root)
- Cache sharing between containers in the same pod

Note that comprehensive caching functionality tests are part of the upstream [Mountpoint S3 project](https://github.com/awslabs/mountpoint-s3), while these tests focus specifically on validating CSI driver integration with caching features.

### Performance Test Suite

The performance test suite (`performance.go`) measures the I/O throughput and performance characteristics of the S3 CSI driver using the FIO (Flexible I/O Tester) benchmarking tool. This suite:

- Spawns multiple pods (N=3) on the same node accessing a shared volume
- Runs a series of FIO benchmarks to test different I/O patterns:
  - Sequential reads: Testing continuous read throughput from S3 objects
  - Sequential writes: Evaluating write performance for streaming data to S3
  - Random reads: Measuring performance when accessing S3 data in a non-sequential pattern

#### Benchmark Configuration Details

The FIO benchmarks are configured with these parameters:

- **Common Settings**:
  - Block size: 256KB for all tests
  - Runtime: 30 seconds (time-based)
  - I/O engine: sync

- **Sequential Read Test**:
  - File size: 10GB
  - Operation: Sequential read

- **Sequential Write Test**:
  - File size: 100GB
  - Operation: Sequential write 
  - fsync_on_close=1 (ensures data is committed to storage)
  - create_on_open=1 (creates the file when opened)
  - unlink=1 (removes the file after testing)

- **Random Read Test**:
  - File size: 10GB
  - Operation: Random read

#### Test Methodology

- Each pod creates and operates on its own test file (e.g., `/mnt/volume1/seq_read_0`) to prevent contention
- Tests run concurrently across all pods to measure performance under multi-client load
- The minimum throughput (MiB/s) observed across all pods is recorded as the baseline metric
- Results are saved to a JSON file in the `test-results/` directory for further analysis

This test suite is particularly valuable for:
- Establishing performance baselines for the S3 CSI driver
- Validating that multiple pods can concurrently access the same S3 volume with acceptable throughput
- Detecting performance regressions in driver updates
- Comparing performance across different S3 storage configurations

**Note:** Performance tests are disabled by default and can be enabled by using the `--performance` flag when running the E2E tests.

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

For performance tests, use the `--performance` flag:

```
go test -v --performance ./tests/e2e/...
```

See the [main project documentation](../README.md) for details on setting up the test environment with proper credentials and S3 endpoint configuration.
