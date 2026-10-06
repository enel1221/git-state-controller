#!/usr/bin/env bash
set -euo pipefail
# Skaffold invokes k3d without an import mode. Podman needs the host API's
# streaming import rather than the tools container's Docker socket path.
if [[ "${CONTAINER_RUNTIME:-}" == podman && "${1:-} ${2:-}" == 'image import' ]]; then
  explicit_mode=false
  for arg in "${@:3}"; do
    case "$arg" in --mode|--mode=*|-m|-m=*) explicit_mode=true ;; esac
  done
  if [[ "$explicit_mode" == false ]]; then
    exec "${GIT_STATE_K3D_BINARY:?}" image import --mode=direct "${@:3}"
  fi
fi
exec "${GIT_STATE_K3D_BINARY:?}" "$@"
