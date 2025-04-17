#Copyright 2022 The Kubernetes Authors
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

PKG=github.com/awslabs/aws-s3-csi-driver
GIT_COMMIT?=$(shell git rev-parse HEAD)
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS?="-X ${PKG}/pkg/driver/version.driverVersion=${VERSION} -X ${PKG}/pkg/driver/version.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/driver/version.buildDate=${BUILD_DATE}"

GO111MODULE=on
GOPROXY=direct
GOPATH=$(shell go env GOPATH)
GOOS=$(shell go env GOOS)
GOBIN=$(GOPATH)/bin

REGISTRY?=""
IMAGE_NAME?=""
IMAGE?=$(REGISTRY)/${IMAGE_NAME}
TAG?=$(GIT_COMMIT)

DOCKERFILE?="Dockerfile"

OS?=linux
ARCH?=amd64
OSVERSION?=amazon

ALL_OS?=linux
ALL_ARCH_linux?=amd64 arm64
ALL_OSVERSION_linux?=amazon
ALL_OS_ARCH_OSVERSION_linux=$(foreach arch, $(ALL_ARCH_linux), $(foreach osversion, ${ALL_OSVERSION_linux}, linux-$(arch)-${osversion}))
ALL_OS_ARCH_OSVERSION=$(foreach os, $(ALL_OS), ${ALL_OS_ARCH_OSVERSION_${os}})

PLATFORM?=linux/amd64,linux/arm64

# region is expected to be the same where cluster is created
E2E_REGION?=us-east-1
E2E_COMMIT_ID?=local
E2E_KUBECONFIG?=""

# Kubernetes version to use in envtest for controller tests.
ENVTEST_K8S_VERSION ?= 1.30.x

# split words on hyphen, access by 1-index
word-hyphen = $(word $2,$(subst -, ,$1))

.EXPORT_ALL_VARIABLES:

# Builds all linux images (not windows because it can't be exported with OUTPUT_TYPE=docker)
.PHONY: all
all: all-image-docker

# Builds all images and pushes them
.PHONY: all-push
all-push: create-and-push-manifest-and-images

# Builds images only if not present with the tag
.PHONY: all-push-skip-if-present
all-push-skip-if-present:
	docker manifest inspect $(IMAGE):$(TAG) > /dev/null || $(MAKE) all-push

.PHONY: build_image
build_image:
	DOCKER_BUILDKIT=1 docker buildx build -f ${DOCKERFILE} -t=${IMAGE}:${TAG} --platform=${PLATFORM} .

.PHONY: push-manifest
push-manifest: create-manifest
	docker manifest push --purge $(IMAGE):$(TAG)

.PHONY: create-and-push-manifest-and-images
create-and-push-manifest-and-images: all-image-registry
# sed expression:
# LHS: match 0 or more not space characters
# RHS: replace with $(IMAGE):$(TAG)-& where & is what was matched on LHS
	docker buildx imagetools create -t $(IMAGE):$(TAG) $(shell echo $(ALL_OS_ARCH_OSVERSION) | sed -e "s~[^ ]*~$(IMAGE):$(TAG)\-&~g")

# Only linux for OUTPUT_TYPE=docker because windows image cannot be exported
# "Currently, multi-platform images cannot be exported with the docker export type. The most common usecase for multi-platform images is to directly push to a registry (see registry)."
# https://docs.docker.com/engine/reference/commandline/buildx_build/#output

.PHONY: all-image-docker
all-image-docker: $(addprefix sub-image-docker-,$(ALL_OS_ARCH_OSVERSION_linux))
.PHONY: all-image-registry
all-image-registry: $(addprefix sub-image-registry-,$(ALL_OS_ARCH_OSVERSION))

sub-image-%:
	$(MAKE) OUTPUT_TYPE=$(call word-hyphen,$*,1) OS=$(call word-hyphen,$*,2) ARCH=$(call word-hyphen,$*,3) OSVERSION=$(call word-hyphen,$*,4) image

.PHONY: image
image: .image-$(TAG)-$(OS)-$(ARCH)-$(OSVERSION)
.image-$(TAG)-$(OS)-$(ARCH)-$(OSVERSION):
	DOCKER_BUILDKIT=1 docker buildx build \
		-f ${DOCKERFILE} \
		--platform=$(OS)/$(ARCH) \
		--progress=plain \
		--target=$(OS)-$(OSVERSION) \
		--output=type=$(OUTPUT_TYPE) \
		-t=$(IMAGE):$(TAG)-$(OS)-$(ARCH)-$(OSVERSION) \
		--build-arg=GOPROXY=$(GOPROXY) \
		--build-arg=VERSION=$(VERSION) \
		--provenance=false \
		.
	touch $@

.PHONY: push_image
push_image:
	docker push ${IMAGE}:${TAG}

.PHONY: login_registry
login_registry:
	aws ecr get-login-password --region ${REGION} | docker login --username AWS --password-stdin ${REGISTRY}

.PHONY: bin
bin:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/aws-s3-csi-driver ./cmd/aws-s3-csi-driver/
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/aws-s3-csi-controller ./cmd/aws-s3-csi-controller/
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/aws-s3-csi-mounter ./cmd/aws-s3-csi-mounter/
	# TODO: `install-mp` component won't be necessary with the containerization.
	CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags ${LDFLAGS} -o bin/install-mp ./cmd/install-mp/

.PHONY: install-go-test-coverage
install-go-test-coverage:
	go install github.com/vladopajic/go-test-coverage/v2@latest

.PHONY: test
test:
	go test -v -race ./{cmd,pkg}/... -coverprofile=./cover.out -covermode=atomic -coverpkg=./{cmd,pkg}/...
	# skipping controller test cases because we don't implement controller for static provisioning,
	# this is a known limitation of sanity testing package: https://github.com/kubernetes-csi/csi-test/issues/214
	go test -v ./tests/sanity/... -ginkgo.skip="ControllerGetCapabilities" -ginkgo.skip="ValidateVolumeCapabilities"

.PHONY: cover
cover:
	${GOBIN}/go-test-coverage --config=./.testcoverage.yml
	go tool cover -html=cover.out -o=coverage.txt

.PHONY: fmt
fmt:
	go fmt ./...

# Run controller tests with envtest.
.PHONY: e2e-controller
e2e-controller: envtest
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TESTBIN) -p path)" go test ./tests/controller/... -ginkgo.v -ginkgo.junit-report=../../e2e-controller-tests-results.xml -test.v

.PHONY: e2e
e2e: e2e-controller
	pushd tests/e2e-kubernetes; \
	KUBECONFIG=${E2E_KUBECONFIG} ginkgo -p -vv -timeout 60m -- --bucket-region=${E2E_REGION} --commit-id=${E2E_COMMIT_ID}; \
	EXIT_CODE=$$?; \
	popd; \
	exit $$EXIT_CODE

.PHONY: check_style
check_style:
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
	./tests/e2e-scality/scripts/run.sh install $$INSTALL_ARGS

# Uninstall the Scality CSI driver (interactive mode)
# This will uninstall from the default namespace (kube-system) or the specified namespace
# Note: kube-system namespace will NOT be deleted even with --delete-ns
.PHONY: csi-uninstall
csi-uninstall:
	@UNINSTALL_ARGS=""; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		UNINSTALL_ARGS="$$UNINSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e-scality/scripts/run.sh uninstall $$UNINSTALL_ARGS

# Uninstall the Scality CSI driver and delete custom namespace
# This automatically deletes namespace without prompting ONLY for custom namespaces
# Note: kube-system namespace will NOT be deleted even with --delete-ns
.PHONY: csi-uninstall-clean
csi-uninstall-clean:
	@UNINSTALL_ARGS="--delete-ns"; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		UNINSTALL_ARGS="$$UNINSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e-scality/scripts/run.sh uninstall $$UNINSTALL_ARGS

# Force uninstall the Scality CSI driver
# Use this when standard uninstall methods aren't working
# Note: kube-system namespace will NOT be deleted even with --force
.PHONY: csi-uninstall-force
csi-uninstall-force:
	@UNINSTALL_ARGS="--force"; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		UNINSTALL_ARGS="$$UNINSTALL_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e-scality/scripts/run.sh uninstall $$UNINSTALL_ARGS

################################################################
# E2E test commands for Scality
################################################################

# Run tests on an already installed CSI driver
.PHONY: e2e-scality
e2e-scality:
	@TEST_ARGS=""; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		TEST_ARGS="$$TEST_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e-scality/scripts/run.sh test $$TEST_ARGS

# Run only the Go-based e2e tests (skips verification checks)
# 
# Usage: make e2e-scality-go
.PHONY: e2e-scality-go
e2e-scality-go:
	@TEST_ARGS=""; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		TEST_ARGS="$$TEST_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e-scality/scripts/run.sh go-test $$TEST_ARGS

# Run the verification tests only (skips Go tests)
# Makes sure the CSI driver is properly installed
.PHONY: e2e-scality-verify
e2e-scality-verify:
	@TEST_ARGS="--skip-go-tests"; \
	if [ ! -z "$(CSI_NAMESPACE)" ]; then \
		TEST_ARGS="$$TEST_ARGS --namespace $(CSI_NAMESPACE)"; \
	fi; \
	./tests/e2e-scality/scripts/run.sh test $$TEST_ARGS

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
# Example: make e2e-scality-all S3_ENDPOINT_URL=https://s3.example.com ACCESS_KEY_ID=key SECRET_ACCESS_KEY=secret
.PHONY: e2e-scality-all
e2e-scality-all:
	@if [ -z "$(S3_ENDPOINT_URL)" ]; then \
		echo "Error: S3_ENDPOINT_URL is required. Please provide it with 'make S3_ENDPOINT_URL=https://your-s3-endpoint.com e2e-scality-all'"; \
		exit 1; \
	fi; \
	if [ -z "$(ACCESS_KEY_ID)" ]; then \
		echo "Error: ACCESS_KEY_ID is required. Please provide it with 'make ACCESS_KEY_ID=your_access_key e2e-scality-all'"; \
		exit 1; \
	fi; \
	if [ -z "$(SECRET_ACCESS_KEY)" ]; then \
		echo "Error: SECRET_ACCESS_KEY is required. Please provide it with 'make SECRET_ACCESS_KEY=your_secret_key e2e-scality-all'"; \
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
	./tests/e2e-scality/scripts/run.sh all $$INSTALL_ARGS
