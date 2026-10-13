#!/usr/bin/env bash
# Prints the directory that holds kube-apiserver and etcd for the tests
# that start an API server, fetching them on first use:
#
#   KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test ./kube/...
set -euo pipefail
cd "$(dirname "$0")/.."
go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25 \
  use 1.37.x --bin-dir "${ENVTEST_BIN_DIR:-$PWD/.cache/envtest}" -p path
