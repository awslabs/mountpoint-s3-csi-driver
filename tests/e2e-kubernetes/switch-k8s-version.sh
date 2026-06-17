#!/bin/sh
#
# Taken from https://github.com/kubernetes/kubernetes/issues/79384#issuecomment-521493597.
# If you get errors like "unknown revision v0.0.0" in any of Kubernetes packages,
# you need to use this script to sync versions of various Kubernetes packages.
# Example usage:
#   $ ./switch-k8s-version.sh 1.32.2
set -euo pipefail

VERSION=${1#"v"}
if [ -z "$VERSION" ]; then
    echo "Must specify version!"
    exit 1
fi
MODS=($(
    curl -sS https://raw.githubusercontent.com/kubernetes/kubernetes/v${VERSION}/go.mod |
    sed -n 's|.*k8s.io/\(.*\) => ./staging/src/k8s.io/.*|k8s.io/\1|p'
))
for MOD in "${MODS[@]}"; do
    V=$(
        go mod download -json "${MOD}@kubernetes-${VERSION}" |
        sed -n 's|.*"Version": "\(.*\)".*|\1|p'
    )
    go mod edit "-replace=${MOD}=${MOD}@${V}"
done
go get "k8s.io/kubernetes@v${VERSION}"
