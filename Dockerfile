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

ARG MOUNTPOINT_VERSION=1.21.0

# Download the mountpoint tarball and produce an installable directory
# Building on Amazon Linux 2 because it has an old libc version. libfuse from the os
# is being packaged up in the container and a newer version linking to a too new glibc
# can cause portability issues
FROM --platform=$TARGETPLATFORM public.ecr.aws/amazonlinux/amazonlinux:2 as mp_builder
ARG MOUNTPOINT_VERSION
ARG TARGETARCH
ARG TARGETPLATFORM
# We need the full version of GnuPG
RUN yum install -y gzip wget gnupg2 tar fuse-libs binutils patchelf

RUN MP_ARCH=`echo ${TARGETARCH} | sed s/amd64/x86_64/` && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/${MOUNTPOINT_VERSION}/$MP_ARCH/mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.tar.gz" && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/${MOUNTPOINT_VERSION}/$MP_ARCH/mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.tar.gz.asc" && \
    wget -q https://s3.amazonaws.com/mountpoint-s3-release/public_keys/KEYS

# Import the key and validate it has the fingerprint we expect
RUN gpg --import KEYS && \
    (gpg --fingerprint mountpoint-s3@amazon.com | grep "673F E406 1506 BB46 9A0E  F857 BE39 7A52 B086 DA5A")

# Verify the downloaded tarball, extract it, and fixup the binary
RUN MP_ARCH=`echo ${TARGETARCH} | sed s/amd64/x86_64/` && \
    gpg --verify mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.tar.gz.asc && \
    mkdir -p /mountpoint-s3 && \
    tar -xvzf mount-s3-${MOUNTPOINT_VERSION}-$MP_ARCH.tar.gz -C /mountpoint-s3 && \
    # set rpath for dynamic library loading
    patchelf --set-rpath '$ORIGIN' /mountpoint-s3/bin/mount-s3

# Build driver. Use BUILDPLATFORM not TARGETPLATFORM for cross compilation
FROM --platform=$BUILDPLATFORM public.ecr.aws/eks-distro-build-tooling/golang:1.25.5 as builder
ARG TARGETARCH

WORKDIR /go/src/github.com/awslabs/mountpoint-s3-csi-driver
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    TARGETARCH=${TARGETARCH} make generate_licenses bin

# `eks-distro-minimal-base-csi` includes `libfuse` and mount utils such as `umount`.
# We need to make sure to use same Amazon Linux version here and while producing Mountpoint to not have glibc compatibility issues.
FROM --platform=$TARGETPLATFORM public.ecr.aws/eks-distro-build-tooling/eks-distro-minimal-base-csi:latest AS linux-amazon
ARG MOUNTPOINT_VERSION
ENV MOUNTPOINT_VERSION=${MOUNTPOINT_VERSION}
ENV MOUNTPOINT_BIN_DIR=/mountpoint-s3/bin

# Copy Mountpoint binary
COPY --from=mp_builder /mountpoint-s3 /mountpoint-s3
COPY --from=mp_builder /lib64/libfuse.so.2 /mountpoint-s3/bin/

# Copy licenses of CSI Driver's dependencies
COPY --from=builder /go/src/github.com/awslabs/mountpoint-s3-csi-driver/LICENSES /LICENSES

# Copy CSI Driver binaries
COPY --from=builder /go/src/github.com/awslabs/mountpoint-s3-csi-driver/bin/aws-s3-csi-driver /bin/aws-s3-csi-driver
COPY --from=builder /go/src/github.com/awslabs/mountpoint-s3-csi-driver/bin/aws-s3-csi-controller /bin/aws-s3-csi-controller
COPY --from=builder /go/src/github.com/awslabs/mountpoint-s3-csi-driver/bin/aws-s3-csi-mounter /bin/aws-s3-csi-mounter

ENTRYPOINT ["/bin/aws-s3-csi-driver"]
