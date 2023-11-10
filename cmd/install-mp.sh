#!/bin/sh

set -euox pipefail

CSI_DIR="/csi/"
cp -rf "/mountpoint-s3" "${CSI_DIR}"