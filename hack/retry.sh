#!/usr/bin/env bash

# Runs a command up to 10 times, waiting 5s longer after each failure (225s at most), and exits with
# its last exit code. CI uses it for steps that fetch Go modules, as origin hosts like gopkg.in flake.

set -u

for attempt in {1..10}; do
  "$@" && exit 0
  code=$?
  if [[ ${attempt} -eq 10 ]]; then
    echo "retry.sh: '$*' failed 10 times, giving up (exit ${code})" >&2
    exit ${code}
  fi
  echo "retry.sh: '$*' failed (exit ${code}), attempt ${attempt}/10; retrying in $((attempt * 5))s..." >&2
  sleep $((attempt * 5))
done
