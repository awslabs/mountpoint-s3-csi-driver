#!/usr/bin/env bash
# Installs a pinned version of the rosa CLI into /usr/local/bin after checking its sha256.
# To upgrade, change both values below. The sha256 is the `digest` of rosa-linux.tar.gz on
# https://github.com/openshift/rosa/releases (also listed in that release's rosa_<version>_SHA256SUMS).

set -euo pipefail

ROSA_VERSION="1.2.68"
ROSA_SHA256="4de2140c94cb1528ae6d62728f66ad86f4bbc64267c814f796997fc6d8b60a85"

archive="${RUNNER_TEMP:?RUNNER_TEMP must be set}/rosa.tar.gz"
curl -fsSL --retry 3 --retry-all-errors --retry-delay 2 -o "$archive" \
  "https://github.com/openshift/rosa/releases/download/v${ROSA_VERSION}/rosa-linux.tar.gz"
echo "${ROSA_SHA256}  ${archive}" | sha256sum --check
sudo tar -xzf "$archive" -C /usr/local/bin rosa
rm -f "$archive"
