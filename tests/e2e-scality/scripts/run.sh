#!/bin/bash
# run.sh - Main entry point for e2e-scality scripts
set -e

# Get the directory where this script is located
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
MODULES_DIR="${SCRIPT_DIR}/modules"

# Source common functions
source "${MODULES_DIR}/common.sh"

# Default namespace value
DEFAULT_NAMESPACE="kube-system"

# Show help message
show_help() {
  echo "Usage: $0 [COMMAND] [OPTIONS]"
  echo
  echo "Commands:"
  echo "  install   Install and verify the Scality CSI driver (default)"
  echo "  test      Run end-to-end tests against the installed driver"
  echo "  go-test   Run only Go-based e2e tests (skips verification checks)"
  echo "  all       Install driver and run tests"
  echo "  uninstall Uninstall the Scality CSI driver"
  echo "  help      Show this help message"
  echo
  echo "Global options:"
  echo "  --namespace VALUE          Specify the namespace to use (default: kube-system)"
  echo
  echo "Options for install command:"
  echo "  --image-tag VALUE         Specify custom image tag for the CSI driver"
  echo "  --image-repository VALUE  Specify custom image repository for the CSI driver"
  echo "  --endpoint-url VALUE      Specify custom S3 endpoint URL (REQUIRED)"
  echo "  --access-key-id VALUE     Specify S3 access key ID for authentication (REQUIRED)"
  echo "  --secret-access-key VALUE Specify S3 secret access key for authentication (REQUIRED)"
  echo "  --validate-s3             Validate S3 endpoint and credentials before installation"
  echo
  echo "Options for test command:"
  echo "  --skip-go-tests           Skip executing Go-based end-to-end tests"
  echo
  echo "Options for uninstall command:"
  echo "  --delete-ns               Delete the CSI driver namespace without prompting (only for custom namespaces, not kube-system)"
  echo "  --force                   Force delete all resources including CSI driver registration"
  echo
  echo "Examples:"
  echo "  $0 install --endpoint-url https://s3.example.com --access-key-id AKIAXXXXXXXX --secret-access-key xxxxxxxx"
  echo "  $0 install --namespace custom-namespace --endpoint-url https://s3.example.com --access-key-id AKIAXXXXXXXX --secret-access-key xxxxxxxx"
  echo "  $0 install --image-tag v1.14.0 --endpoint-url https://s3.example.com --access-key-id AKIAXXXXXXXX --secret-access-key xxxxxxxx"
  echo "  $0 install --image-repository myrepo/csi-driver --endpoint-url https://s3.example.com --access-key-id AKIAXXXXXXXX --secret-access-key xxxxxxxx"
  echo "  $0 install --validate-s3 --endpoint-url https://s3.example.com --access-key-id AKIAXXXXXXXX --secret-access-key xxxxxxxx"
  echo "  $0 test                                 # Run all tests including Go-based e2e tests"
  echo "  $0 test --namespace custom-namespace    # Run tests in a custom namespace"
  echo "  $0 test --skip-go-tests                 # Run only basic verification tests"
  echo "  $0 go-test                              # Run Go tests directly (skips verification)"
  echo "  $0 all                                  # Install driver and run tests"
  echo "  $0 all --namespace custom-namespace     # Install driver and run tests in a custom namespace"
  echo "  $0 uninstall                            # Uninstall driver from kube-system namespace"
  echo "  $0 uninstall --namespace custom-namespace  # Uninstall driver from a custom namespace"
  echo "  $0 uninstall --namespace custom-namespace --delete-ns  # Uninstall driver and delete custom namespace"
  echo "  $0 uninstall --force                    # Force delete all resources"
  echo "  $0 help                                 # Show this help message"
  echo
  echo "For advanced filtering of Go tests, use 'go test' directly in the tests/e2e-scality/e2e-tests directory:"
  echo "  cd tests/e2e-scality/e2e-tests && go test -v -tags=e2e -ginkgo.focus=\"Basic Functionality\""
}

parse_install_parameters() {
  local params=""
  local has_endpoint_url=false
  local has_access_key_id=false
  local has_secret_access_key=false

  # Process options
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --namespace)
        params="$params --namespace $2"
        shift 2
        ;;
      --image-tag)
        IMAGE_TAG="$2"
        params="$params --image-tag $2"
        shift 2
        ;;
      --image-repository)
        IMAGE_REPOSITORY="$2"
        params="$params --image-repository $2"
        shift 2
        ;;
      --endpoint-url)
        ENDPOINT_URL="$2"
        params="$params --endpoint-url $2"
        has_endpoint_url=true
        shift 2
        ;;
      --access-key-id)
        ACCESS_KEY_ID="$2"
        params="$params --access-key-id $2"
        has_access_key_id=true
        shift 2
        ;;
      --secret-access-key)
        SECRET_ACCESS_KEY="$2"
        params="$params --secret-access-key $2"
        has_secret_access_key=true
        shift 2
        ;;
      --validate-s3)
        params="$params --validate-s3"
        shift
        ;;
      *)
        echo "Error: Unknown option: $1"
        show_help
        exit 1
        ;;
    esac
  done

  # Validate required parameters
  if [ "$has_endpoint_url" = false ]; then
    error "Missing required parameter: --endpoint-url"
    show_help
    exit 1
  fi
  
  if [ "$has_access_key_id" = false ]; then
    error "Missing required parameter: --access-key-id"
    show_help
    exit 1
  fi
  
  if [ "$has_secret_access_key" = false ]; then
    error "Missing required parameter: --secret-access-key"
    show_help
    exit 1
  fi

  # Return parameters
  echo "$params"
}

# Parse uninstall parameters
parse_uninstall_parameters() {
  local params=""
  
  # Process options
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --namespace)
        params="$params --namespace $2"
        shift 2
        ;;
      --delete-ns)
        params="$params --delete-ns"
        shift
        ;;
      --force)
        params="$params --force"
        shift
        ;;
      *)
        echo "Error: Unknown option: $1"
        show_help
        exit 1
        ;;
    esac
  done
  
  # Return parameters
  echo "$params"
}

# Parse test parameters
parse_test_parameters() {
  local params=""
  
  # Process options
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --namespace)
        params="$params --namespace $2"
        shift 2
        ;;
      --skip-go-tests)
        params="$params --skip-go-tests"
        shift
        ;;
      --junit-report)
        params="$params --junit-report $2"
        shift 2
        ;;
      *)
        echo "Error: Unknown option: $1"
        show_help
        exit 1
        ;;
    esac
  done
  
  # Return parameters
  echo "$params"
}

# Extract namespace from parameters if present, otherwise use default
get_namespace_param() {
  local namespace="$DEFAULT_NAMESPACE"
  local args=("$@")
  
  for ((i=0; i<${#args[@]}; i++)); do
    if [[ "${args[$i]}" == "--namespace" && $((i+1)) -lt ${#args[@]} ]]; then
      namespace="${args[$i+1]}"
      break
    fi
  done
  
  echo "--namespace $namespace"
}

# Main execution
main() {
  # Set default namespace
  local namespace_param=$(get_namespace_param "$@")
  
  # Process command line arguments
  COMMAND=${1:-install}
  shift || true # Remove the command from the arguments
  
  case $COMMAND in
    install)
      source "${MODULES_DIR}/install.sh"
      # Pass all command-line parameters to install module
      exec_cmd do_install $namespace_param "$@"
      ;;
    test)
      source "${MODULES_DIR}/test.sh"
      # Parse test parameters
      local test_parameters=$(parse_test_parameters "$@")
      # Pass processed parameters to test module
      exec_cmd do_test $test_parameters
      ;;
    go-test)
      # This command runs only the Go tests without verification
      source "${MODULES_DIR}/test.sh"
      # Parse test parameters
      local test_parameters=$(parse_test_parameters "$@")
      # Pass processed parameters to run_go_tests function directly
      test_parameters="$test_parameters --skip-verification"
      exec_cmd do_test $test_parameters
      ;;
    all)
      log "Starting Scality CSI driver installation and tests..."
      
      source "${MODULES_DIR}/install.sh"
      
      # Get namespace parameter
      local namespace_param=$(get_namespace_param "$@")
      
      # Extract installation related arguments, exclude the test-specific arguments
      local install_args=""
      local test_args=""
      
      # Separate install and test args
      for arg in "$@"; do
        # Check for JUnit report param with equals sign format
        if [[ "$arg" == --junit-report=* ]]; then
          test_args="$test_args $arg"
          continue
        fi
        
        case "$arg" in
          --junit-report)
            test_args="$test_args $arg"
            shift
            
            # If this argument requires a value, add it to test_args
            if [[ $# -gt 0 && "$1" != --* ]]; then
              test_args="$test_args $1"
              shift
            fi
            ;;
          --skip-go-tests | --skip-verification)
            test_args="$test_args $arg"
            ;;
          *)
            install_args="$install_args $arg"
            ;;
        esac
      done
      
      # Pass all command-line parameters to install module
      exec_cmd do_install $namespace_param $install_args
      
      source "${MODULES_DIR}/test.sh"
      
      # Run tests with same namespace and any test-specific arguments
      exec_cmd do_test $namespace_param $test_args
      
      log "Scality CSI driver setup and tests completed successfully."
      ;;
    uninstall)
      source "${MODULES_DIR}/uninstall.sh"
      # Parse uninstall parameters
      local uninstall_parameters=$(parse_uninstall_parameters "$@")
      # Pass processed parameters to uninstall module
      exec_cmd do_uninstall $uninstall_parameters
      ;;
    help)
      show_help
      ;;
    *)
      error "Unknown command: $COMMAND"
      show_help
      exit 1
      ;;
  esac
}

# Execute main function with all arguments
main "$@"
