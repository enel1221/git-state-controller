#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export KUBECONFIG="$PWD/.dev/kubeconfig"
export KUBECTL_KUBERC=false
go run ./hack/bootstrap
