# S3CSI-32: Make S3 Endpoint URL Mandatory

## Goals
- Make the AWS S3 endpoint URL (AWS_ENDPOINT_URL) a mandatory parameter
- Enforce validation at both Helm deployment time and driver runtime
- Prevent service startup if the endpoint URL is not configured
- Add appropriate tests to verify this behavior

## Functional Requirements
1. Helm chart must fail installation if S3 endpoint URL is not provided
2. Driver must fail to start if AWS_ENDPOINT_URL environment variable is not set
3. Error messages must be clear and actionable
4. Implementation must include sufficient test coverage

## Task Dashboard

| Phase | Task | Description | Status | Depends On |
|-------|------|-------------|--------|------------|
| 1     | 1    | Helm Chart Changes | ✅ Done |            |
| 1     | 1.1  | Update values.yaml comment to indicate S3 endpoint URL is required | ✅ Done |            |
| 1     | 1.2  | Modify node.yaml to use required function | ✅ Done |            |
| 2     | 2    | Go Code Changes | ✅ Done |            |
| 2     | 2.1  | Add validation in driver.go | ✅ Done | 1.2        |
| 3     | 3    | Testing | ⬜ To Do |            |
| 3     | 3.1  | Add unit tests for Go validation | ⬜ To Do | 2.1        |
| 3     | 3.2  | Add GitHub workflow for validation | ⬜ To Do | 2.1        |
| 4     | 4    | Documentation | ⬜ To Do |            |
| 4     | 4.1  | Update README to note required S3 endpoint URL | ⬜ To Do | 1.1, 2.1  |

---
## Plan Context (Jira: S3CSI-32)

# Complete Plan: Make S3 Endpoint URL Mandatory

## 1. Implementation Changes

### A. Helm Chart Changes
- Update `charts/scality-mountpoint-s3-csi-driver/values.yaml`:
  ```yaml
  # AWS S3 endpoint URL to use for all volume mounts (REQUIRED)
  s3EndpointUrl: ""
  ```

- Modify `charts/scality-mountpoint-s3-csi-driver/templates/node.yaml`:
  ```yaml
  - name: AWS_ENDPOINT_URL
    value: {{ required "S3 endpoint URL (.Values.node.s3EndpointUrl) must be provided for the CSI driver to function" .Values.node.s3EndpointUrl }}
  ```

### B. Go Code Validation
- Add validation in `pkg/driver/driver.go` during driver initialization:
  ```go
  func NewDriver(endpoint string, mpVersion string, nodeID string) (*Driver, error) {
      // Existing initialization code...

      // Validate that AWS_ENDPOINT_URL is set
      if os.Getenv(envprovider.EnvEndpointURL) == "" {
          return nil, fmt.Errorf("AWS_ENDPOINT_URL environment variable must be set for the CSI driver to function")
      }

      // Rest of the existing initialization code...
  }
  ```

## 2. Testing Strategy

### A. Unit Tests
- Add test in `pkg/driver/driver_test.go`:
  ```go
  func TestNewDriverValidatesEndpointURL(t *testing.T) {
      // Clear environment variables
      os.Unsetenv(envprovider.EnvEndpointURL)
      
      // Attempt to create driver without endpoint URL
      _, err := NewDriver("unix:///tmp/test.sock", "1.0.0", "test-node")
      
      // Verify it fails with the expected error
      if err == nil {
          t.Fatal("Expected driver creation to fail without AWS_ENDPOINT_URL")
      }
      if !strings.Contains(err.Error(), "AWS_ENDPOINT_URL environment variable must be set") {
          t.Fatalf("Unexpected error message: %v", err)
      }
      
      // Set the environment variable
      os.Setenv(envprovider.EnvEndpointURL, "https://test-endpoint.example.com")
      
      // Now driver creation should succeed
      drv, err := NewDriver("unix:///tmp/test.sock", "1.0.0", "test-node")
      if err != nil {
          t.Fatalf("Driver creation failed with endpoint URL set: %v", err)
      }
      if drv == nil {
          t.Fatal("Driver is nil despite successful creation")
      }
  }
  ```

### B. GitHub Workflow Test
- Create `.github/workflows/endpoint-url-validation.yml`:
  ```yaml
  name: S3 Endpoint URL Validation

  on:
    push:
      branches: [ main ]
    pull_request:
      branches: [ main ]

  jobs:
    validate-endpoint-url:
      runs-on: ubuntu-latest
      steps:
        - name: Check out repository
          uses: actions/checkout@v4

        - name: Setup environment
          uses: ./.github/actions/e2e-setup-common
          with:
            ref: ${{ github.ref }}

        # Test 1: Verify Helm chart fails without endpoint URL
        - name: Test Helm Chart Validation
          run: |
            echo "Testing Helm chart validation for S3 endpoint URL..."
            if helm install test-release ./charts/scality-mountpoint-s3-csi-driver --dry-run 2>&1 | grep -q "S3 endpoint URL .* must be provided"; then
              echo "✅ Helm validation working: Install failed without endpoint URL"
            else
              echo "❌ Test failed: Helm install did not fail when S3 endpoint URL was missing"
              exit 1
            fi

        # Test 2: Compile and test the driver without endpoint URL  
        - name: Build Driver
          run: make build

        - name: Test Driver Fails Without Endpoint URL
          run: |
            echo "Testing driver startup validation..."
            unset AWS_ENDPOINT_URL
            export CSI_NODE_NAME=test-node
            if ./bin/scality-csi-driver --endpoint unix:///tmp/test.sock 2>&1 | grep -q "AWS_ENDPOINT_URL environment variable must be set"; then
              echo "✅ Driver validation working: Startup failed without endpoint URL"
            else
              echo "❌ Test failed: Driver started when AWS_ENDPOINT_URL was missing"
              exit 1
            fi
  ``` 