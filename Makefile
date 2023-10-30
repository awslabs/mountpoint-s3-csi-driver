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

VERSION=0.1.0

PKG=github.com/awslabs/aws-s3-csi-driver
GIT_COMMIT?=$(shell git rev-parse HEAD)
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS?="-X ${PKG}/pkg/driver.driverVersion=${VERSION} -X ${PKG}/pkg/driver.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/driver.buildDate=${BUILD_DATE}"

GO111MODULE=on
GOPROXY=direct
GOPATH=$(shell go env GOPATH)
GOOS=$(shell go env GOOS)
GOBIN=$(shell pwd)/bin

REGISTRY?=""
IMAGE_NAME?=""
IMAGE?=$(REGISTRY)/${IMAGE_NAME}
TAG?=$(GIT_COMMIT)

PLATFORM?=linux/amd64,linux/arm64

# region is expected to be the same where cluster is created
E2E_REGION?=us-east-1
E2E_COMMIT_ID?=local
E2E_KUBECONFIG?=""

.EXPORT_ALL_VARIABLES:

.PHONY: build_image
build_image:
	DOCKER_BUILDKIT=1 docker build -t=${IMAGE}:${TAG} --platform=${PLATFORM} .

.PHONY: push_image
push_image:
	docker push ${IMAGE}:${TAG}

.PHONY: login_registry
login_registry:
	aws ecr get-login-password --region ${REGION} | docker login --username AWS --password-stdin ${REGISTRY}

.PHONY: bin
bin:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux go build -ldflags ${LDFLAGS} -o bin/aws-s3-csi-driver ./cmd/

.PHONY: test
test:
	go test -v -race ./pkg/...
	# skipping controller test cases because we don't implement controller for static provisioning, this is a known limitation of sanity testing package: https://github.com/kubernetes-csi/csi-test/issues/214
	go test -v ./tests/sanity/... -ginkgo.skip="ControllerGetCapabilities" -ginkgo.skip="ValidateVolumeCapabilities"

.PHONY: e2e
e2e:
	pushd tests/e2e-kubernetes; \
	KUBECONFIG=${E2E_KUBECONFIG} go test -ginkgo.vv --bucket-region=${E2E_REGION} --commit-id=${E2E_COMMIT_ID}; \
	EXIT_CODE=$$?; \
	popd; \
	exit $$EXIT_CODE

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: check_style
check_style:
	test -z "$$(gofmt -d . | tee /dev/stderr)"

.PHONY: clean
clean:
	rm -rf bin/ && docker system prune
