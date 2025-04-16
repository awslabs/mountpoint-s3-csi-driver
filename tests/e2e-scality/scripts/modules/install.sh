#!/bin/bash
# install.sh - Installation functions for e2e-scality scripts

# Source common functions
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# Default namespace value
DEFAULT_NAMESPACE="kube-system"

# Validate S3 configuration by testing connectivity and credentials
validate_s3_configuration() {
  local endpoint_url="$1"
  local access_key_id="$2"
  local secret_access_key="$3"
  
  log "Validating S3 configuration..."
  log "Checking endpoint connectivity: $endpoint_url"
  
  # Create a temporary file for capturing output
  local temp_output=$(mktemp)
  
  # Step 1: Basic endpoint connectivity check with curl
  local http_code=$(curl -s -o "$temp_output" -w "%{http_code}" "$endpoint_url" 2>/dev/null)
  
  # For S3 endpoints, a 403 is actually good - it means the endpoint exists and requires auth
  if [[ "$http_code" == "403" ]] || grep -q "AccessDenied\|InvalidAccessKeyId" "$temp_output"; then
    log "S3 endpoint is confirmed (received HTTP $http_code with access denied, which is expected)"
    log "Basic connectivity validation successful!"
    
    # Step 2: Check credentials with AWS CLI if it's installed
    if command -v aws &> /dev/null; then
      log "AWS CLI found, validating access key and secret key..."
      
      # Use environment variables method for AWS credentials
      if AWS_ACCESS_KEY_ID="$access_key_id" AWS_SECRET_ACCESS_KEY="$secret_access_key" exec_cmd aws --endpoint-url "$endpoint_url" s3 ls > "$temp_output" 2>&1; then
        log "SUCCESS: AWS access key and secret key validated successfully!"
        log "Available buckets:"
        cat "$temp_output"
      else
        error "Failed to validate AWS credentials. Error details:"
        cat "$temp_output"
        log "Please check your access key and secret key."
        rm -f "$temp_output"
        return 1
      fi
    else
      log "AWS CLI not installed - cannot validate access key and secret key."
      log "Only basic endpoint connectivity was confirmed."
      log "Proceeding with installation, but credential issues might occur later."
    fi
    
    # Clean up temporary file
    rm -f "$temp_output"
    return 0
  # For non-403 codes, check if it's otherwise a successful response
  elif [[ "$http_code" == 2* ]] || [[ "$http_code" == 3* ]]; then
    log "S3 endpoint is reachable (HTTP $http_code)"
    log "Response does not look like a typical S3 endpoint (no access denied response)"
    log "Response received:"
    cat "$temp_output"
    log "Will proceed with installation, but this might not be an S3 service."
    rm -f "$temp_output"
    return 0
  else
    error "Failed to connect to S3 endpoint (HTTP code: $http_code)"
    log "Please check if the endpoint URL is correct and the S3 service is running."
    cat "$temp_output"
    rm -f "$temp_output"
    return 1
  fi
}

# Install the Scality CSI driver using Helm
install_csi_driver() {
  # Process parameters
  local IMAGE_TAG=""
  local IMAGE_REPOSITORY=""
  local ENDPOINT_URL=""
  local ACCESS_KEY_ID=""
  local SECRET_ACCESS_KEY=""
  local VALIDATE_S3="false"
  local NAMESPACE="$DEFAULT_NAMESPACE"
  
  # Parse parameters
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --namespace)
        NAMESPACE="$2"
        shift 2
        ;;
      --image-tag)
        IMAGE_TAG="$2"
        shift 2
        ;;
      --image-repository)
        IMAGE_REPOSITORY="$2"
        shift 2
        ;;
      --endpoint-url)
        ENDPOINT_URL="$2"
        shift 2
        ;;
      --access-key-id)
        ACCESS_KEY_ID="$2"
        shift 2
        ;;
      --secret-access-key)
        SECRET_ACCESS_KEY="$2"
        shift 2
        ;;
      --validate-s3)
        VALIDATE_S3="true"
        shift
        ;;
      *)
        warn "Unknown parameter: $1"
        shift
        ;;
    esac
  done

  # Validate required parameters
  if [ -z "$ENDPOINT_URL" ]; then
    error "Missing required parameter: --endpoint-url"
    exit 1
  fi
  
  if [ -z "$ACCESS_KEY_ID" ]; then
    error "Missing required parameter: --access-key-id"
    exit 1
  fi
  
  if [ -z "$SECRET_ACCESS_KEY" ]; then
    error "Missing required parameter: --secret-access-key"
    exit 1
  fi

  # Validate S3 configuration if validation is enabled
  if [ "$VALIDATE_S3" = "true" ]; then
    if ! validate_s3_configuration "$ENDPOINT_URL" "$ACCESS_KEY_ID" "$SECRET_ACCESS_KEY"; then
      error "S3 configuration validation failed. Fix your S3 endpoint URL and credentials."
      exit 1
    fi
  fi

  log "Installing Scality CSI driver using Helm in namespace: $NAMESPACE..."
  
  # Get project root from common function
  PROJECT_ROOT=$(get_project_root)
  
  # Create S3 credentials secret if it doesn't exist
  log "Creating S3 credentials secret in namespace: $NAMESPACE..."
  exec_cmd kubectl create namespace $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -
  
  # Create or update the secret with provided values
  exec_cmd kubectl create secret generic aws-secret \
    --from-literal=key_id="$ACCESS_KEY_ID" \
    --from-literal=access_key="$SECRET_ACCESS_KEY" \
    -n $NAMESPACE \
    --dry-run=client -o yaml | kubectl apply -f -
    
  log "S3 credentials secret created/updated in namespace: $NAMESPACE."
  
  # Prepare helm command parameters
  local HELM_PARAMS=(
    "$PROJECT_ROOT/charts/scality-mountpoint-s3-csi-driver"
    --namespace $NAMESPACE
    --create-namespace
    --set "node.s3EndpointUrl=$ENDPOINT_URL"
    --wait
  )
  
  # Add image tag if specified
  if [ -n "$IMAGE_TAG" ]; then
    log "Using custom image tag: $IMAGE_TAG"
    HELM_PARAMS+=(--set "image.tag=$IMAGE_TAG")
  fi
  
  # Add image repository if specified
  if [ -n "$IMAGE_REPOSITORY" ]; then
    log "Using custom image repository: $IMAGE_REPOSITORY"
    HELM_PARAMS+=(--set "image.repository=$IMAGE_REPOSITORY")
  fi
  
  # Install/upgrade the Helm chart
  log "Running Helm upgrade with parameters: ${HELM_PARAMS[*]}"
  
  exec_cmd helm upgrade --install scality-s3-csi "${HELM_PARAMS[@]}"
  
  log "CSI driver installation complete in namespace: $NAMESPACE."
  
  # Export the namespace for other functions to use
  export CSI_NAMESPACE="$NAMESPACE"
}

# Verify the installation
verify_installation() {
  local namespace="${CSI_NAMESPACE:-$DEFAULT_NAMESPACE}"
  
  log "Verifying CSI driver installation in namespace: $namespace..."
  
  # Wait for the pods to be running
  log "Waiting for CSI driver pods to be in Running state in namespace: $namespace..."
  
  # Maximum wait time in seconds (5 minutes)
  MAX_WAIT_TIME=300
  WAIT_INTERVAL=10
  ELAPSED_TIME=0
  
  while [ $ELAPSED_TIME -lt $MAX_WAIT_TIME ]; do
    if exec_cmd kubectl get pods -n $namespace | grep -q "Running"; then
      log "CSI driver pods are now running in namespace: $namespace."
      
      exec_cmd kubectl get pods -n $namespace
      break
    else
      log "Pods not yet in Running state. Waiting ${WAIT_INTERVAL} seconds... (${ELAPSED_TIME}/${MAX_WAIT_TIME}s)"
      sleep $WAIT_INTERVAL
      ELAPSED_TIME=$((ELAPSED_TIME + WAIT_INTERVAL))
    fi
  done
  
  # Check if we timed out
  if [ $ELAPSED_TIME -ge $MAX_WAIT_TIME ]; then
    log "Timed out waiting for pods to be in Running state. Current pod status:"
    exec_cmd kubectl get pods -n $namespace
    error "CSI driver pods did not reach Running state within ${MAX_WAIT_TIME} seconds."
  fi
  
  # Check if CSI driver is registered
  log "Checking if CSI driver is registered..."
  
  if exec_cmd kubectl get csidrivers | grep -q "s3.csi.aws.com"; then
    log "CSI driver is registered successfully."
  else
    error "CSI driver is not registered properly."
  fi
}

# Main installation function that will be called from run.sh
do_install() {
  local namespace="$DEFAULT_NAMESPACE"
  
  log "Starting Scality CSI driver installation..."
  
  # Process namespace parameter first
  for ((i=1; i<=$#; i++)); do
    if [[ "${!i}" == "--namespace" && $((i+1)) -le $# ]]; then
      j=$((i+1))
      namespace="${!j}"
      break
    fi
  done
  
  log "Using namespace: $namespace"
  
  check_dependencies
  
  # Export namespace for other functions to use
  export CSI_NAMESPACE="$namespace"
  
  # Pass all arguments to install_csi_driver
  install_csi_driver "$@"
  
  verify_installation
  log "Scality CSI driver setup completed successfully in namespace: $namespace."
}
