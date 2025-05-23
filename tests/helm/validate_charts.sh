#!/bin/bash
# validate_charts.sh - A script to validate Helm chart requirements and configurations

set -eo pipefail

# Define color codes for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Script root directory (the directory this script is in)
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# Project root directory
PROJECT_ROOT="$( cd "$SCRIPT_DIR/../.." && pwd )"
# Charts directory
CHARTS_DIR="$PROJECT_ROOT/charts"

# Print script usage information
usage() {
    echo -e "${BLUE}Usage:${NC} $0 [validation_name]"
    echo ""
    echo "When run without arguments, all validations will be executed."
    echo "To run a specific validation, provide its function name as an argument."
    echo ""
    echo -e "${BLUE}Available validations:${NC}"
    echo "  validate_custom_endpoint - Verify ability to set custom S3 endpoint URL"
    echo "  validate_s3_region     - Verify ability to set S3 region"
    echo ""
    echo -e "${BLUE}Examples:${NC}"
    echo "  $0                           # Run all validations"
    echo "  $0 validate_custom_endpoint  # Run only the custom S3 endpoint validation"
    echo ""
}

# Function to run a validation test and report results
run_validation() {
  local test_name="$1"
  local test_func="$2"

  echo -e "\n${YELLOW}Running validation: ${test_name}${NC}"

  if $test_func; then
    echo -e "${GREEN}✅ PASSED: ${test_name}${NC}"
    return 0
  else
    echo -e "${RED}❌ FAILED: ${test_name}${NC}"
    return 1
  fi
}

# Check if helm is installed
check_helm_installed() {
  if ! command -v helm &> /dev/null; then
    echo -e "${RED}Error: helm is not installed. Please install helm before running this script.${NC}"
    exit 1
  fi
}

# Validation test for custom S3 endpoint URL
validate_custom_endpoint() {
  local chart_dir="$CHARTS_DIR/scality-mountpoint-s3-csi-driver"
  local custom_endpoint="https://custom-s3.example.com:8443"

  echo "Testing ability to set custom S3 endpoint URL..."

  # Run helm template with custom endpoint
  echo "Rendering template with custom endpoint: $custom_endpoint"
  local result=$(helm template "$chart_dir" --set node.s3EndpointUrl="$custom_endpoint" --show-only templates/node.yaml 2>&1)

  # Check if rendering succeeded
  if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Helm template failed with custom endpoint URL:${NC}"
    echo "$result"
    return 1
  fi

  # Check if our custom endpoint appears in the rendered template
  if echo "$result" | grep -q "value: $custom_endpoint"; then
    echo -e "${GREEN}✓ Custom endpoint URL successfully applied in rendered template${NC}"
    return 0
  else
    echo -e "${RED}✗ Custom endpoint URL not found in rendered template${NC}"
    return 1
  fi
}

# Validation test for S3 region configuration
validate_s3_region() {
  local chart_dir="$CHARTS_DIR/scality-mountpoint-s3-csi-driver"
  local custom_region="us-west-2"

  echo "Testing ability to set S3 region..."

  # First check default value
  echo "Checking default region is set to us-east-1"
  local result=$(helm template "$chart_dir" --show-only templates/node.yaml 2>&1)

  if ! echo "$result" | grep -Eq "^[[:space:]]*value: us-east-1"; then
    echo -e "${RED}✗ Default S3 region not properly set to us-east-1${NC}"
    return 1
  else
    echo -e "${GREEN}✓ Default S3 region correctly set to us-east-1${NC}"
  fi

  # Then check custom value
  echo "Rendering template with custom region: $custom_region"
  result=$(helm template "$chart_dir" --set node.s3Region="$custom_region" --show-only templates/node.yaml 2>&1)

  if echo "$result" | grep -Eq "^[[:space:]]*value: $custom_region"; then
    echo -e "${GREEN}✓ Custom S3 region successfully applied in rendered template${NC}"
    return 0
  else
    echo -e "${RED}✗ Custom S3 region not found in rendered template${NC}"
    return 1
  fi
}

# Add other validation functions here
# Example:
# validate_resource_limits() {
#   local chart_dir="$CHARTS_DIR/scality-mountpoint-s3-csi-driver"
#
#   # Check if resource limits are set properly
#   ...
# }

# Main function - runs all validations or a specific one
main() {
  # Display banner
  echo -e "${BLUE}===============================================${NC}"
  echo -e "${BLUE}   Scality S3 CSI Driver Helm Validation Tool   ${NC}"
  echo -e "${BLUE}===============================================${NC}"

  # Check if helm is installed
  check_helm_installed

  # Check if a specific validation was requested
  if [ $# -eq 1 ]; then
    # Check if the function exists
    if declare -f "$1" > /dev/null; then
      # Run the specified validation
      run_validation "$1" "$1"
      exit $?
    else
      echo -e "${RED}Error: Validation function '$1' not found.${NC}"
      usage
      exit 1
    fi
  fi

  # If no specific validation was requested, run all validations
  local errors=0

  # Run all validations
  run_validation "Custom S3 endpoint URL can be specified" validate_custom_endpoint || ((errors++))
  run_validation "S3 region configuration" validate_s3_region || ((errors++))

  # Add more validations here
  # run_validation "Resource limits are set" validate_resource_limits || ((errors++))

  # Report final results
  if [ $errors -eq 0 ]; then
    echo -e "\n${GREEN}All validations passed!${NC}"
    return 0
  else
    echo -e "\n${RED}${errors} validation(s) failed!${NC}"
    return 1
  fi
}

# If the script is being sourced, don't run main
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  # Process command line arguments
  if [ "$1" == "-h" ] || [ "$1" == "--help" ]; then
    usage
    exit 0
  fi

  # Run main with all arguments
  main "$@"
fi
