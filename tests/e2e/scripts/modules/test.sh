#!/bin/bash
# test.sh - Test functions for e2e scripts

set -euxo pipefail

# Source common functions
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# Default namespace value
DEFAULT_NAMESPACE="kube-system"

# Run Go tests
run_go_tests() {
  local project_root=$(get_project_root)
  local e2e_tests_dir="${project_root}/tests/e2e"
  local namespace="${1:-$DEFAULT_NAMESPACE}"
  local junit_report="${2:-}"
  local access_key_id="${ACCESS_KEY_ID:-}"
  local secret_access_key="${SECRET_ACCESS_KEY:-}"
  local s3_endpoint_url="${S3_ENDPOINT_URL:-}"
  
  log "Running Go-based end-to-end tests for Scality CSI driver in namespace: $namespace..."
  
  # Check if Go is installed
  if ! command -v go &> /dev/null; then
    error "Go is not installed. Please install Go to run the tests."
    return 1
  fi
  
  # Check if the tests directory exists
  if [ ! -d "$e2e_tests_dir" ]; then
    error "End-to-end tests directory not found: $e2e_tests_dir"
    return 1
  fi
  
  # Validate required parameters
  if [ -z "$access_key_id" ]; then
    error "Missing S3 access key. Please set ACCESS_KEY_ID environment variable or provide --access-key-id parameter."
    return 1
  fi

  if [ -z "$secret_access_key" ]; then
    error "Missing S3 secret key. Please set SECRET_ACCESS_KEY environment variable or provide --secret-access-key parameter."
    return 1
  fi

  if [ -z "$s3_endpoint_url" ]; then
    error "Missing S3 endpoint URL. Please set S3_ENDPOINT_URL environment variable or provide --endpoint-url parameter."
    return 1
  fi

  # Build the Go test command with required S3 credentials
  # Convert go test flags to ginkgo flags: -v ./... becomes -v ./..., -ginkgo.v becomes -v
  # Pass credential flags after --
  # Add 15m timeout
  local ginkgo_test_cmd="KUBECONFIG=${KUBECONFIG} ginkgo --procs=8 -timeout=15m -v ./... -- --access-key-id=${access_key_id} --secret-access-key=${secret_access_key} --s3-endpoint-url=${s3_endpoint_url}"

  # Add JUnit report if specified
  if [ -n "$junit_report" ]; then
    log "Using JUnit report file: $junit_report"

    # Handle absolute and relative paths
    local junit_absolute_path

    # If path is absolute, use it directly
    if [[ "$junit_report" = /* ]]; then
      junit_absolute_path="$junit_report"
    else
      # For relative paths, determine if we need to adjust the path based on the CWD
      # If path starts with ./ then make it relative to the e2e directory
      if [[ "$junit_report" = ./* ]]; then
        # For paths starting with ./, keep them relative to the test directory
        junit_absolute_path="$junit_report"
        log "Using relative path from e2e directory: $junit_absolute_path"
      else
        # For other paths (like just a filename), ensure they're created in the test directory
        junit_absolute_path="./$junit_report"
        log "Adjusted path to be relative to e2e directory: $junit_absolute_path"
      fi
    fi

    # Create the output directory if it doesn't exist
    local junit_dir=$(dirname "$junit_absolute_path")
    if [ ! -d "$junit_dir" ] && [ "$junit_dir" != "." ]; then
      log "Creating output directory for JUnit report: $junit_dir"
      mkdir -p "$junit_dir"
    fi

    # Use the correct format for Ginkgo JUnit report (-junit-report=...)
    # Keep -v flag for verbosity and add 15m timeout
    ginkgo_test_cmd="KUBECONFIG=${KUBECONFIG} ginkgo --procs=8 -timeout=15m -v -junit-report='$junit_absolute_path' ./... -- --access-key-id=${access_key_id} --secret-access-key=${secret_access_key} --s3-endpoint-url=${s3_endpoint_url}"
    log "Final JUnit report path: $junit_absolute_path"
  fi

  # Run the Go tests using Ginkgo
  log "Executing Ginkgo tests in $e2e_tests_dir"
  log "Test command: $ginkgo_test_cmd"

  if ! (cd "$e2e_tests_dir" && eval "$ginkgo_test_cmd"); then
    error "Ginkgo tests failed with exit code $?"
    # List any XML files that were created
    if [ -n "$junit_report" ]; then
      log "Checking for JUnit report files:"
      (cd "$e2e_tests_dir" && find . -name "*.xml" -ls || true)
    fi
    return 1
  fi

  # Verify the JUnit report was created
  if [ -n "$junit_report" ]; then
    log "Checking for JUnit report file:"
    (cd "$e2e_tests_dir" && find . -name "*.xml" -ls || true)
  fi

  log "Ginkgo tests completed successfully."
  return 0
}

# Wait for pods to reach the Running state
wait_for_pods() {
  local namespace="${1:-$DEFAULT_NAMESPACE}"
  local max_attempts=30
  local wait_seconds=10
  local attempt=1
  local all_namespaces=false

  if [ -n "${2:-}" ] && [ "$2" = "all-namespaces" ]; then
    all_namespaces=true
  fi

  log "Waiting for CSI driver pods to reach Running state..."
  
  while [ $attempt -le $max_attempts ]; do
    local pods_running=false
    local pod_output=""

    if [ "$all_namespaces" = true ]; then
      pod_output=$(exec_cmd kubectl get pods --all-namespaces | grep -E "s3|csi" || true)
    else
      pod_output=$(exec_cmd kubectl get pods -n "$namespace" | grep -E "s3|csi" || true)
    fi

    if [ -z "$pod_output" ]; then
      log "Attempt $attempt/$max_attempts: No CSI driver pods found yet. Waiting ${wait_seconds}s..."
    elif echo "$pod_output" | grep -q "Running"; then
      pods_running=true
      break
    else
      log "Attempt $attempt/$max_attempts: Pods are not running yet. Current status:"
      echo "$pod_output"
      log "Waiting ${wait_seconds}s for pods to start..."
    fi

    sleep $wait_seconds
    attempt=$((attempt + 1))
  done

  if [ "$pods_running" = true ]; then
    log "CSI driver pods are now in Running state:"
    echo "$pod_output"
    return 0
  else
    error "Timed out waiting for CSI driver pods to reach Running state after $((max_attempts * wait_seconds)) seconds."
    if [ "$all_namespaces" = true ]; then
      exec_cmd kubectl get pods --all-namespaces | grep -E "s3|csi"
    else
      exec_cmd kubectl get pods -n "$namespace" | grep -E "s3|csi"
    fi
    return 1
  fi
}

# Run basic verification tests
run_verification_tests() {
  local namespace="${1:-$DEFAULT_NAMESPACE}"

  log "Verifying Scality CSI driver installation in namespace: $namespace..."

  # Check if the CSI driver is registered
  if exec_cmd kubectl get csidrivers | grep -q "s3.csi.scality.com"; then
    log "CSI driver is registered properly."
  else
    error "CSI driver is not registered properly."
    return 1
  fi

  # Wait for the CSI driver pods to reach Running state
  if ! wait_for_pods "$namespace"; then
    # If pods not found in the specified namespace, try all namespaces
    log "CSI driver pods not found in namespace $namespace. Checking all namespaces..."
    if ! wait_for_pods "$namespace" "all-namespaces"; then
      error "Failed to find running CSI driver pods in any namespace."
      return 1
    fi
  fi

  log "Basic verification tests passed."
  return 0
}

# Main test function that will be called from run.sh
do_test() {
  log "Starting Scality CSI driver tests..."

  local skip_go_tests=false
  local skip_verification=false
  local namespace="$DEFAULT_NAMESPACE"
  local junit_report=""

  # Parse command-line parameters
  while [[ $# -gt 0 ]]; do
    key="$1"
    case "$key" in
      --namespace)
        namespace="$2"
        shift 2
        ;;
      --skip-go-tests)
        skip_go_tests=true
        shift
        ;;
      --skip-verification)
        skip_verification=true
        shift
        ;;
      --junit-report=*)
        # Extract the value after the equals sign
        junit_report="${key#*=}"
        shift
        ;;
      --junit-report)
        # Handle both formats: --junit-report=path and --junit-report path
        if [[ "$2" != --* && -n "$2" ]]; then
          junit_report="$2"
          shift 2
        else
          error "Missing value for parameter: $key"
          return 1
        fi
        ;;
      --access-key-id)
        export ACCESS_KEY_ID="$2"
        shift 2
        ;;
      --secret-access-key)
        export SECRET_ACCESS_KEY="$2"
        shift 2
        ;;
      --endpoint-url)
        export S3_ENDPOINT_URL="$2"
        shift 2
        ;;
      *)
        error "Unknown parameter: $key"
        return 1
        ;;
    esac
  done

  log "Using namespace: $namespace"

  # Run basic verification tests unless skipped
  if [ "$skip_verification" != "true" ]; then
    if ! run_verification_tests "$namespace"; then
      error "Verification tests failed. Cannot proceed with Go tests."
      return 1
    fi
  else
    log "Skipping verification tests as requested."
  fi

  # Run Go-based tests if not skipped
  if [ "$skip_go_tests" != "true" ]; then
    if ! run_go_tests "$namespace" "$junit_report"; then
      error "Go tests failed."
      return 1
    fi
  else
    log "Skipping Go-based end-to-end tests as requested."
  fi
  
  log "All tests completed successfully."
}
