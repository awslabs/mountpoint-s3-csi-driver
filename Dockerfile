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

ARG MOUNTPOINT_VERSION=1.1.0

# Download and verify the mountpoint's RPM and DEB in this container
FROM --platform=$BUILDPLATFORM public.ecr.aws/amazonlinux/amazonlinux:2023 as mp_builder
ARG MOUNTPOINT_VERSION
ARG TARGETARCH
ARG TARGETPLATFORM
# We need the full version of GnuPG
RUN dnf install -y --allowerasing wget gnupg2

RUN MP_ARCH=`echo ${TARGETARCH} | sed s/amd64/x86_64/` && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/${MOUNTPOINT_VERSION}/$MP_ARCH/mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.rpm" && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/${MOUNTPOINT_VERSION}/$MP_ARCH/mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.rpm.asc" && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/${MOUNTPOINT_VERSION}/$MP_ARCH/mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.deb" && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/${MOUNTPOINT_VERSION}/$MP_ARCH/mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.deb.asc" && \
    wget -q https://s3.amazonaws.com/mountpoint-s3-release/public_keys/KEYS

# Import the key and validate it has the fingerprint we expect
RUN gpg --import KEYS && \
    (gpg --fingerprint mountpoint-s3@amazon.com | grep "673F E406 1506 BB46 9A0E  F857 BE39 7A52 B086 DA5A")

# Verify the downloaded binary
RUN MP_ARCH=`echo ${TARGETARCH} | sed s/amd64/x86_64/` && \
    gpg --verify mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.rpm.asc && \
    gpg --verify mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.deb.asc && \
    mv mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.rpm /mount-s3.rpm && \
    mv mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.deb /mount-s3.deb

# Build driver
FROM --platform=$BUILDPLATFORM golang:1.21.1-bullseye as builder
ARG TARGETARCH
WORKDIR /go/src/github.com/awslabs/mountpoint-s3-csi-driver
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
TARGETARCH=${TARGETARCH} make bin

FROM --platform=$TARGETPLATFORM public.ecr.aws/eks-distro-build-tooling/eks-distro-minimal-base-csi:latest-al2 AS linux-amazon
ARG MOUNTPOINT_VERSION
ENV MOUNTPOINT_VERSION=${MOUNTPOINT_VERSION}

# MP Installer
COPY --from=mp_builder /mount-s3.rpm /mount-s3.rpm
COPY --from=mp_builder /mount-s3.deb /mount-s3.deb
COPY ./cmd/install-mp.sh /install-mp.sh

# Install driver
COPY --from=builder /go/src/github.com/awslabs/mountpoint-s3-csi-driver/bin/aws-s3-csi-driver /bin/aws-s3-csi-driver

ENTRYPOINT ["/bin/aws-s3-csi-driver"]
