#!/usr/bin/env bash
# Project-local pinned binaries for the Linux CI runner; does not install Docker.
set -euo pipefail
cd "$(dirname "$0")/.."
source hack/versions.env
mkdir -p bin
curl -fsSL "https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}/k3d-linux-amd64" -o bin/k3d
printf '%s\n' '06d8f25bc3a971c4eb29e0ff08429b180402db0f4dec838c9eac427e296800a0  bin/k3d' | sha256sum --check --status
curl -fsSL "https://storage.googleapis.com/skaffold/releases/${SKAFFOLD_VERSION}/skaffold-linux-amd64" -o bin/skaffold
printf '%s\n' '42b9e2e3246c19b78fcc53dd60cc7ded1da8704293886b909a2a02b9bef34d20  bin/skaffold' | sha256sum --check --status
curl -fsSL https://dl.k8s.io/release/v1.37.1/bin/linux/amd64/kubectl -o bin/kubectl
curl -fsSL https://dl.k8s.io/release/v1.37.1/bin/linux/amd64/kubectl.sha256 -o bin/kubectl.sha256
printf '%s  bin/kubectl\n' "$(cat bin/kubectl.sha256)" | sha256sum --check --status
chmod +x bin/k3d bin/skaffold bin/kubectl
