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
    echo "  validate_s3_endpoint_required - Verify S3 endpoint URL is required in Helm charts"
    echo ""
    echo -e "${BLUE}Examples:${NC}"
    echo "  $0                           # Run all validations"
    echo "  $0 validate_s3_endpoint_required  # Run only the S3 endpoint URL validation"
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

# Validation test for S3 endpoint URL requirement
validate_s3_endpoint_required() {
  local chart_dir="$CHARTS_DIR/scality-mountpoint-s3-csi-driver"
  local node_yaml="$chart_dir/templates/node.yaml"
  
  # Check if node.yaml exists
  if [ ! -f "$node_yaml" ]; then
    echo -e "${RED}Error: $node_yaml does not exist.${NC}"
    return 1
  fi

  echo "Checking if s3EndpointUrl is marked as required..."
  if grep -q "required.*S3 endpoint URL.*must be provided" "$node_yaml"; then
    echo -e "${GREEN}✓ 'required' directive for S3 endpoint URL found.${NC}"
  else
    echo -e "${RED}✗ 'required' directive for S3 endpoint URL not found in $node_yaml.${NC}"
    return 1
  fi
  
  echo "Testing template rendering without endpoint URL (should fail)..."
  if helm template "$chart_dir" >/dev/null 2>&1; then
    echo -e "${RED}✗ Helm template succeeded but should fail without S3 endpoint URL.${NC}"
    return 1
  else
    local result=$(helm template "$chart_dir" 2>&1)
    if echo "$result" | grep -q "S3 endpoint URL.*must be provided"; then
      echo -e "${GREEN}✓ Helm template failed with the expected error about missing endpoint URL.${NC}"
    else
      echo -e "${RED}✗ Helm template failed but with an unexpected error:${NC}"
      echo "$result"
      return 1
    fi
  fi
  
  echo "Testing template rendering with endpoint URL (should succeed)..."
  if helm template "$chart_dir" --set node.s3EndpointUrl=https://example.com >/dev/null 2>&1; then
    echo -e "${GREEN}✓ Helm template succeeded with endpoint URL provided.${NC}"
  else
    local result=$(helm template "$chart_dir" --set node.s3EndpointUrl=https://example.com 2>&1)
    echo -e "${RED}✗ Helm template failed despite providing endpoint URL:${NC}"
    echo "$result"
    return 1
  fi
  
  return 0
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
  run_validation "S3 Endpoint URL is required" validate_s3_endpoint_required || ((errors++))
  
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