# Helm Chart Tests

Tests for validating the Scality Mountpoint S3 CSI Driver Helm chart. These tests ensure the chart generates correct
Kubernetes manifests and follows Helm best practices.

## Running Tests

```bash
make validate-helm
```

This runs the validation script which performs:

- Helm template rendering validation
- YAML syntax checking
- Required field validation
- Security policy compliance

## Test Structure

### Chart Validation (`validate_charts.sh`)

Core validation script that:

- Validates Helm chart syntax and structure
- Checks for required values and configurations
- Verifies generated manifest correctness
- Tests default and custom value scenarios

### Template Tests

Tests that validate generated Kubernetes manifests:

- Service account and RBAC configurations
- Pod security contexts and resource limits  
- Volume and mount configurations
- Network policies and service definitions

## Test Categories

### Syntax Validation

- Helm chart YAML syntax
- Template function usage
- Value reference validation

### Security Validation  

- Pod security contexts
- Service account permissions
- Network policy configurations
- Resource limit enforcement

### Functional Validation

- Required field presence
- Default value application
- Custom value override behavior
- Conditional template rendering

## Configuration

Tests use the chart's default values but can be customized:

```bash
# Test with custom values
helm template test-release ./charts/scality-mountpoint-s3-csi-driver \
  --values custom-values.yaml \
  --debug
```

## Adding New Tests

1. Add validation logic to `validate_charts.sh`
2. Include test cases for new chart features
3. Update this documentation with new test descriptions
4. Ensure tests run in CI environment

The script returns a formatted list of charts in the repository and their validation status.
For detailed troubleshooting and development guidelines, check the inline script documentation.
