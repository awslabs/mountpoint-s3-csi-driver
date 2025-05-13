#Copyright 2022 The Kubernetes Authors
#Copyright 2025 Scality, Inc.
#
#Licensed under the Apache License, Version 2.0 (the "License");
#you may not use this file except in compliance with the License.
#You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
#Unless required by applicable law or agreed to in writing, software
#distributed under the License is distributed on an "AS IS" BASIS,
#WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#See the License for the specific language governing permissions and
#limitations under the License.
SHELL = /bin/bash

# MP CSI Driver version
VERSION=1.13.0

PKG=github.com/scality/mountpoint-s3-csi-driver
GIT_COMMIT?=$(shell git rev-parse HEAD)
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS?="-X ${PKG}/pkg/driver/version.driverVersion=${VERSION} -X ${PKG}/pkg/driver/version.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/driver/version.buildDate=${BUILD_DATE}"

GO111MODULE=on
GOPROXY=direct
GOPATH=$(shell go env GOPATH)
GOOS=$(shell go env GOOS)
GOBIN=$(GOPATH)/bin

# TODO(S3CSI-20): Implement simplified container image building.
# Docker image building functionality has been temporarily removed and will be addressed in S3CSI-20.

# Test configuration variables
E2E_REGION?=us-east-1
E2E_COMMIT_ID?=local
E2E_KUBECONFIG?=""

# Kubernetes version to use in envtest for controller tests.
ENVTEST_K8S_VERSION ?= 1.30.x

.EXPORT_ALL_VARIABLES:

.PHONY: bin
bin:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/scality-s3-csi-driver ./cmd/scality-csi-driver/
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/scality-csi-controller ./cmd/scality-csi-controller/
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/scality-s3-csi-mounter ./cmd/scality-csi-mounter/
	# TODO: `install-mp` component won't be necessary with the containerization.
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/install-mp ./cmd/install-mp/

.PHONY: unit-test
unit-test:
	go test -v -parallel 8 ./{cmd,pkg}/... -coverprofile=./coverage.out -covermode=atomic -coverpkg=./{cmd,pkg}/...

.PHONY: csi-compliance-test
csi-compliance-test:
	go test -v ./tests/sanity/... -ginkgo.skip="ControllerGetCapabilities" -ginkgo.skip="ValidateVolumeCapabilities"

.PHONY: test
test:
	go test -v -race ./{cmd,pkg}/... -coverprofile=./cover.out -covermode=atomic -coverpkg=./{cmd,pkg}/...
	# skipping controller test cases because we don't implement controller for static provisioning,
	# this is a known limitation of sanity testing package: https://github.com/kubernetes-csi/csi-test/issues/214
	go test -v ./tests/sanity/... -ginkgo.skip="ControllerGetCapabilities" -ginkgo.skip="ValidateVolumeCapabilities"

.PHONY: cover
cover:
	go tool cover -html=coverage.out -o=coverage.html

.PHONY: fmt
fmt:
	go fmt ./...

# Validate Helm charts for correctness and requirements
.PHONY: validate-helm
validate-helm:
	@echo "Validating Helm charts..."
	@tests/helm/validate_charts.sh

# Run controller tests with envtest.
.PHONY: controller-integration-test
controller-integration-test: envtest
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TESTBIN) -p path)" go test ./tests/controller/... -ginkgo.v -ginkgo.junit-report=../../controller-integration-tests-results.xml -test.v

.PHONY: lint
lint:
	test -z "$$(gofmt -d . | tee /dev/stderr)"

.PHONY: clean
clean:
	rm -rf bin/ && docker system prune

## Binaries used in tests.

TESTBIN ?= $(shell pwd)/tests/bin
$(TESTBIN):
	mkdir -p $(TESTBIN)

ENVTEST ?= $(TESTBIN)/setup-envtest
ENVTEST_VERSION ?= release-0.19

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): $(TESTBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

# Copied from https://github.com/kubernetes-sigs/kubebuilder/blob/c32f9714456f7e5e7cc6c790bb87c7e5956e710b/pkg/plugins/golang/v4/scaffolds/internal/templates/makefile.go#L275-L289.
# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(TESTBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef


################################################################
# Scality CSI driver configuration
################################################################

# Image tag for the CSI driver (optional)
CSI_IMAGE_TAG ?=

# Custom image repository for the CSI driver (optional)
CSI_IMAGE_REPOSITORY ?=

# Namespace to deploy the CSI driver in (optional, defaults to kube-system)
CSI_NAMESPACE ?=

# S3 endpoint URL (REQUIRED)
# Example: https://s3.your-scality.com
S3_ENDPOINT_URL ?=

# AWS/S3 access key for authentication (REQUIRED)
ACCESS_KEY_ID ?=

# AWS/S3 secret key for authentication (REQUIRED)
SECRET_ACCESS_KEY ?=

# Set to 'true' to validate S3 credentials before installation (optional)
# Checks endpoint connectivity and validates credentials (if AWS CLI is available)
VALIDATE_S3 ?= false

# Additional arguments to pass to the script (optional)
ADDITIONAL_ARGS ?=

################################################################
# Scality CSI driver commands
################################################################

# Install the Scality CSI driver
# 
# Required parameters:
#   S3_ENDPOINT_URL - Your Scality S3 endpoint 
#   ACCESS_KEY_ID - Your S3 access key
#   SECRET_ACCESS_KEY - Your S3 secret key
#
# Optional parameters:
#   CSI_IMAGE_TAG - Specific version of the driver
#   CSI_IMAGE_REPOSITORY - Custom image repository for the driver
#   CSI_NAMESPACE - Namespace to deploy the CSI driver in (defaults to kube-system)
#   VALIDATE_S3 - Set to "true" to verify S3 credentials
#
# Example: make csi-install S3_ENDPOINT_URL=https://s3.example.com ACCESS_KEY_ID=key SECRET_ACCESS_KEY=secret
.PHONY: csi-install
csi-install:
	@if [ -z "$(S3_ENDPOINT_URL)" ]; then \
		echo "Error: S3_ENDPOINT_URL is required. Please provide it with 'make S3_ENDPOINT_URL=https://your-s3-endpoint.com csi-install'"; \
		exit 1; \
	fi; \
	if [ -z "$(ACCESS_KEY_ID)" ]; then \
		echo "Error: ACCESS_KEY_ID is required. Please provide it with 'make ACCESS_KEY_ID=your_access_key csi-install'"; \
		exit 1; \
	fi; \
	if [ -z "$(SECRET_ACCESS_KEY)" ]; then \
		echo "Error: SECRET_ACCESS_KEY is required. Please provide it with 'make SECRET_ACCESS_KEY=your_secret_key csi-install'"; \
		exit 1; \
	fi; \
	INSTALL_ARGS=""; \
	if [ ! -z "$(CSI_IMAGE_TAG)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --image-tag $(CSI_IMAGE_TAG)"; \
	fi; \
	if [ ! -z "$(CSI_IMAGE_REPOSITORY)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --image-repository $(CSI_IMAGE_REPOSITORY)"; \
	fi; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	INSTALL_ARGS="$$INSTALL_ARGS --endpoint-url $(S3_ENDPOINT_URL)"; \
	INSTALL_ARGS="$$INSTALL_ARGS --access-key-id $(ACCESS_KEY_ID)"; \
	INSTALL_ARGS="$$INSTALL_ARGS --secret-access-key $(SECRET_ACCESS_KEY)"; \
	if [ "$(VALIDATE_S3)" = "true" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --validate-s3"; \
	fi; \
	if [ ! -z "$(ADDITIONAL_ARGS)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS $(ADDITIONAL_ARGS)"; \
	fi; \
	./tests/e2e/scripts/run.sh install $$INSTALL_ARGS

# Uninstall the Scality CSI driver (interactive mode)
# This will uninstall from the default namespace (kube-system) or the specified namespace
# Note: kube-system namespace will NOT be deleted even with --delete-ns
.PHONY: csi-uninstall
csi-uninstall:
	@UNINSTALL_ARGS=""; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		UNINSTALL_ARGS="$$UNINSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e/scripts/run.sh uninstall $$UNINSTALL_ARGS

# Uninstall the Scality CSI driver and delete custom namespace
# This automatically deletes namespace without prompting ONLY for custom namespaces
# Note: kube-system namespace will NOT be deleted even with --delete-ns
.PHONY: csi-uninstall-clean
csi-uninstall-clean:
	@UNINSTALL_ARGS="--delete-ns"; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		UNINSTALL_ARGS="$$UNINSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e/scripts/run.sh uninstall $$UNINSTALL_ARGS

# Force uninstall the Scality CSI driver
# Use this when standard uninstall methods aren't working
# Note: kube-system namespace will NOT be deleted even with --force
.PHONY: csi-uninstall-force
csi-uninstall-force:
	@UNINSTALL_ARGS="--force"; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		UNINSTALL_ARGS="$$UNINSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e/scripts/run.sh uninstall $$UNINSTALL_ARGS

################################################################
# E2E test commands for Scality
################################################################

# Run tests on an already installed CSI driver
.PHONY: e2e
e2e:
	@TEST_ARGS=""; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		TEST_ARGS="$$TEST_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	if [ ! -z "$(S3_ENDPOINT_URL)" ]; then \
		TEST_ARGS="$$TEST_ARGS --endpoint-url $(S3_ENDPOINT_URL)"; \
	fi; \
	if [ ! -z "$(ACCESS_KEY_ID)" ]; then \
		TEST_ARGS="$$TEST_ARGS --access-key-id $(ACCESS_KEY_ID)"; \
	fi; \
	if [ ! -z "$(SECRET_ACCESS_KEY)" ]; then \
		TEST_ARGS="$$TEST_ARGS --secret-access-key $(SECRET_ACCESS_KEY)"; \
	fi; \
	./tests/e2e/scripts/run.sh test $$TEST_ARGS

# Run only the Go-based e2e tests (skips verification checks)
# 
# Usage: make e2e-go
.PHONY: e2e-go
e2e-go:
	@TEST_ARGS=""; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		TEST_ARGS="$$TEST_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	if [ ! -z "$(S3_ENDPOINT_URL)" ]; then \
		TEST_ARGS="$$TEST_ARGS --endpoint-url $(S3_ENDPOINT_URL)"; \
	fi; \
	if [ ! -z "$(ACCESS_KEY_ID)" ]; then \
		TEST_ARGS="$$TEST_ARGS --access-key-id $(ACCESS_KEY_ID)"; \
	fi; \
	if [ ! -z "$(SECRET_ACCESS_KEY)" ]; then \
		TEST_ARGS="$$TEST_ARGS --secret-access-key $(SECRET_ACCESS_KEY)"; \
	fi; \
	./tests/e2e/scripts/run.sh go-test $$TEST_ARGS

# Run the verification tests only (skips Go tests)
# Makes sure the CSI driver is properly installed
.PHONY: e2e-verify
e2e-verify:
	@TEST_ARGS="--skip-go-tests"; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		TEST_ARGS="$$TEST_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e/scripts/run.sh test $$TEST_ARGS

# Install CSI driver and run all tests in one command
# 
# Required parameters:
#   S3_ENDPOINT_URL - Your Scality S3 endpoint 
#   ACCESS_KEY_ID - Your S3 access key
#   SECRET_ACCESS_KEY - Your S3 secret key
#
# Optional parameters:
#   CSI_IMAGE_TAG - Specific version of the driver
#   CSI_IMAGE_REPOSITORY - Custom image repository for the driver
#   CSI_NAMESPACE - Namespace to deploy the CSI driver in (defaults to kube-system)
#   VALIDATE_S3 - Set to "true" to verify S3 credentials
#
# Example: make e2e-all S3_ENDPOINT_URL=https://s3.example.com ACCESS_KEY_ID=key SECRET_ACCESS_KEY=secret
.PHONY: e2e-all
e2e-all:
	@if [ -z "$(S3_ENDPOINT_URL)" ]; then \
		echo "Error: S3_ENDPOINT_URL is required. Please provide it with 'make S3_ENDPOINT_URL=https://your-s3-endpoint.com e2e-all'"; \
		exit 1; \
	fi; \
	if [ -z "$(ACCESS_KEY_ID)" ]; then \
		echo "Error: ACCESS_KEY_ID is required. Please provide it with 'make ACCESS_KEY_ID=your_access_key e2e-all'"; \
		exit 1; \
	fi; \
	if [ -z "$(SECRET_ACCESS_KEY)" ]; then \
		echo "Error: SECRET_ACCESS_KEY is required. Please provide it with 'make SECRET_ACCESS_KEY=your_secret_key e2e-all'"; \
		exit 1; \
	fi; \
	INSTALL_ARGS=""; \
	if [ ! -z "$(CSI_IMAGE_TAG)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --image-tag $(CSI_IMAGE_TAG)"; \
	fi; \
	if [ ! -z "$(CSI_IMAGE_REPOSITORY)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --image-repository $(CSI_IMAGE_REPOSITORY)"; \
	fi; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	INSTALL_ARGS="$$INSTALL_ARGS --endpoint-url $(S3_ENDPOINT_URL)"; \
	INSTALL_ARGS="$$INSTALL_ARGS --access-key-id $(ACCESS_KEY_ID)"; \
	INSTALL_ARGS="$$INSTALL_ARGS --secret-access-key $(SECRET_ACCESS_KEY)"; \
	if [ "$(VALIDATE_S3)" = "true" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS --validate-s3"; \
	fi; \
	if [ ! -z "$(ADDITIONAL_ARGS)" ]; then \
		INSTALL_ARGS="$$INSTALL_ARGS $(ADDITIONAL_ARGS)"; \
	fi; \
	./tests/e2e/scripts/run.sh all $$INSTALL_ARGS
