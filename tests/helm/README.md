# Helm Chart Validation Tests

This directory contains scripts and tests to validate the Scality S3 CSI Driver Helm charts. These tests ensure that our Helm charts adhere to certain requirements and best practices.

## Validation Script

The main validation script is `validate_charts.sh`, which can check multiple aspects of the Helm charts:

- **S3 Endpoint URL Requirement**: Ensures that the S3 endpoint URL is properly marked as required in the Helm chart
- _(More validations can be added over time)_

### Usage

```bash
# Run all validations
./validate_charts.sh

# Run a specific validation
./validate_charts.sh validate_s3_endpoint_required

# Show help
./validate_charts.sh --help
```

## Adding New Validations

To add a new validation test:

1. Add a new function to `validate_charts.sh` following the pattern of existing validations
2. Add the new function to the list of validations in the `main()` function
3. Update the usage information in the `usage()` function

Example:

```bash
# New validation function
validate_my_new_check() {
  local chart_dir="$CHARTS_DIR/scality-mountpoint-s3-csi-driver"
  
  # Add validation logic here
  # Return 0 for success, non-zero for failure
  
  return 0
}

# Then in the main() function, add:
run_validation "My new check description" validate_my_new_check || ((errors++))
```

## Integration with CI

The validation script can be integrated with CI by adding a Makefile target:

```makefile
.PHONY: validate-helm
validate-helm:
	@echo "Validating Helm charts..."
	@tests/helm/validate_charts.sh
```

Then add `validate-helm` to the matrix of tests in `.github/workflows/code-validation.yaml`.

## Why These Tests Matter

- **Consistency**: Ensures all charts follow the same patterns and best practices
- **Correctness**: Prevents deployment issues by validating required fields
- **Quality**: Maintains a high standard for our Helm charts
- **Documentation**: Serves as living documentation for chart requirements 