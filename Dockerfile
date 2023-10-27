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

# Download and verify the mountpoint's RPM in this container
FROM --platform=$BUILDPLATFORM public.ecr.aws/amazonlinux/amazonlinux:2023 as mp_builder

# We need the full version of GnuPG
RUN dnf install -y --allowerasing wget gnupg2

RUN MP_ARCH=`uname -p | sed s/aarch64/arm64/` && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/latest/$MP_ARCH/mount-s3.rpm" && \
    wget -q "https://s3.amazonaws.com/mountpoint-s3-release/latest/$MP_ARCH/mount-s3.rpm.asc" && \
    wget -q https://s3.amazonaws.com/mountpoint-s3-release/public_keys/KEYS

# Import the key and validate it has the fingerprint we expect
RUN gpg --import KEYS && \
    (gpg --fingerprint mountpoint-s3@amazon.com | grep "673F E406 1506 BB46 9A0E  F857 BE39 7A52 B086 DA5A")

# Verify the downloaded binary
RUN gpg --verify mount-s3.rpm.asc

# Build driver
FROM --platform=$BUILDPLATFORM golang:1.21.1-bullseye as builder
WORKDIR /go/src/github.com/awslabs/mountpoint-s3-csi-driver
ADD . .
RUN make bin

FROM --platform=$BUILDPLATFORM public.ecr.aws/amazonlinux/amazonlinux:2023 AS linux-amazon

RUN yum install util-linux -y

# Install MP
COPY --from=mp_builder /mount-s3.rpm /mount-s3.rpm

RUN dnf upgrade -y && \
    dnf install -y ./mount-s3.rpm && \
    dnf clean all && \
    rm mount-s3.rpm

RUN echo "user_allow_other" >> /etc/fuse.conf

# Install driver
COPY --from=builder /go/src/github.com/awslabs/mountpoint-s3-csi-driver/bin/aws-mountpoint-s3-csi-driver /bin/aws-mountpoint-s3-csi-driver

ENTRYPOINT ["/bin/aws-mountpoint-s3-csi-driver"]
