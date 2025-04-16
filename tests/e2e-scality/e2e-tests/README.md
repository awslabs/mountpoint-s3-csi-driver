# End-to-End Tests for Scality S3 CSI Driver

This directory contains end-to-end tests for the Scality S3 CSI Driver. The tests are written in Go and use the Kubernetes E2E framework with Ginkgo and Gomega for BDD-style testing.

## Directory Structure

- `kubernetes/` - Contains tests using the standard Kubernetes storage E2E framework
- `scality/` - Contains Scality-specific tests for additional functionality

## Running Tests Directly

You can run the tests directly using the Go test command:

### Basic Usage

```bash
# Run all tests
cd tests/e2e-scality/e2e-tests
go test -v -tags=e2e
```

### Filtering Tests

Ginkgo provides built-in filtering capabilities:

```bash
# Focus on specific tests (run only tests with "Basic Functionality" in their description)
go test -v -tags=e2e -ginkgo.focus="Basic Functionality"

# Skip specific tests
go test -v -tags=e2e -ginkgo.skip="Volume Operations"

# Combine focus and skip
go test -v -tags=e2e -ginkgo.focus="Functionality" -ginkgo.skip="Volume"
```

### Custom Options

The test framework supports additional command-line options:

```bash
# Override the namespace where the CSI driver is installed (default: mount-s3)
go test -v -tags=e2e -namespace="my-custom-namespace"
```

### Running with Script

Alternatively, you can use the provided shell script:

```bash
# Run all tests
./tests/e2e-scality/scripts/run.sh test

# Focus on specific tests
./tests/e2e-scality/scripts/run.sh test --focus "Basic Functionality"

# Skip specific tests
./tests/e2e-scality/scripts/run.sh test --skip "Volume Operations"
```

## Test Categories

The tests cover:

1. **Basic Functionality**: Verifies that the CSI driver is properly installed and registered
   - CSI driver pods are running
   - CSI driver is registered in the cluster

2. **Volume Operations**: Tests for storage provisioning (placeholders for now)
   - Storage class creation
   - PVC creation and binding
   - Volume mounting

3. **File Operations**: Tests for data operations (placeholders for now)
   - Reading and writing files
   - Concurrent access

4. **Error Handling**: Tests for error conditions (placeholders for now)
   - Invalid credentials
   - Resource limits

## Development

When adding new tests, follow these guidelines:

1. Use the Go test framework and Ginkgo for BDD-style tests
2. Place Kubernetes storage tests in the `kubernetes/` directory
3. Place Scality-specific tests in the `scality/` directory
4. Ensure tests clean up after themselves
5. Document test requirements and any special setup needs

### Writing New Tests

New tests should follow the Ginkgo BDD style:

```go
Describe("Feature being tested", func() {
    It("should behave in a certain way", func() {
        By("First doing this")
        // Test code
        
        By("Then checking that")
        // More test code
        
        Expect(result).To(Equal(expectedValue))
    })
})
```

### Test Patterns

- Use `BeforeEach` for setup code
- Use `AfterEach` for cleanup code
- Use `By` to document test steps
- Use `Skip` to mark tests that are not yet implemented
- Use `Eventually` for operations that may take time to complete
