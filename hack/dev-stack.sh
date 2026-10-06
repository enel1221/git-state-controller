#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source hack/versions.env
# Prefer the pinned, project-local tools when installed.
export PATH="$PWD/bin:$PATH"
# The cluster name is intentionally fixed; overrides cannot select unrelated clusters.
cluster=git-state-dev
state="$PWD/.dev"
export KUBECONFIG="$state/kubeconfig"
export KUBECTL_KUBERC=false
context=k3d-git-state-dev
k() { kubectl --kubeconfig "$KUBECONFIG" --context "$context" "$@"; }
# k3d's global registration is separate from the helpers' explicit kubeconfig.
k3d_global() { KUBECONFIG="$HOME/.kube/config" k3d "$@"; }
require() { command -v "$1" >/dev/null || { echo "Missing prerequisite: $1. See README.md." >&2; exit 1; }; }
select_runtime() {
  runtime=${CONTAINER_RUNTIME:-}
  if [[ -f "$state/runtime" ]]; then
    saved_runtime=$(cat "$state/runtime")
    [[ -z "$runtime" || "$runtime" == "$saved_runtime" ]] || { echo "This stack uses $saved_runtime; clean it before switching runtimes." >&2; exit 1; }
    runtime=$saved_runtime
  fi
  if [[ -z "$runtime" ]]; then
    if command -v docker >/dev/null; then runtime=docker; else runtime=podman; fi
  fi
  export CONTAINER_RUNTIME="$runtime"
  case "$runtime" in
    docker) require docker; docker info >/dev/null ;;
    podman)
      require podman
      podman info >/dev/null
      if [[ -z "${DOCKER_HOST:-}" ]]; then
        socket=$(podman info --format '{{.Host.RemoteSocket.Path}}')
        [[ -S "$socket" ]] || { echo 'Start the Podman API socket: systemctl --user start podman.socket' >&2; exit 1; }
        export DOCKER_HOST="unix://$socket"
      fi
      if [[ "$DOCKER_HOST" == unix://* ]]; then export DOCKER_SOCK="${DOCKER_SOCK:-${DOCKER_HOST#unix://}}"; fi
      ;;
    *) echo 'CONTAINER_RUNTIME must be docker or podman' >&2; exit 1 ;;
  esac
}
check_stack() {
  [[ -f "$state/owned" && -f "$KUBECONFIG" ]] || { echo 'Managed stack is missing. Run make up or make dev first.' >&2; exit 1; }
  require kubectl
  k get nodes >/dev/null || { echo 'Managed stack is unavailable. Run make up or make dev first.' >&2; exit 1; }
}
setup() {
  for tool in go make kubectl k3d skaffold curl; do require "$tool"; done
  select_runtime
  [[ $(k3d version | head -1) == *"$K3D_VERSION"* ]] || { echo "Install k3d $K3D_VERSION" >&2; exit 1; }
  [[ $(skaffold version) == "$SKAFFOLD_VERSION" ]] || { echo "Install Skaffold $SKAFFOLD_VERSION" >&2; exit 1; }
  umask 077
  mkdir -p "$state"
  printf '%s\n' "$runtime" > "$state/runtime"
  if [[ "$runtime" == podman ]]; then
    export GIT_STATE_K3D_BINARY
    GIT_STATE_K3D_BINARY=$(command -v k3d)
    mkdir -p "$state/bin"
    cp hack/k3d-runtime.sh "$state/bin/k3d"
    chmod 700 "$state/bin/k3d"
    export PATH="$state/bin:$PATH"
  fi
  if k3d cluster list --no-headers | awk '{print $1}' | grep -qx "$cluster"; then
    [[ -f "$state/owned" ]] || { echo "Cluster $cluster exists without this checkout's ownership marker; refusing to adopt it." >&2; exit 1; }
  else
    # Persist ownership before creation, so clean can recover a partial failure.
    touch "$state/owned"
    forgejo_port=${FORGEJO_PORT:-3000}
    argo_port=${ARGOCD_PORT:-8080}
    [[ "$forgejo_port" =~ ^[0-9]+$ && "$argo_port" =~ ^[0-9]+$ ]] || { echo 'Ports must be integers' >&2; exit 1; }
    sed -e "s/127.0.0.1:3000:30080/127.0.0.1:${forgejo_port}:30080/" -e "s/127.0.0.1:8080:30443/127.0.0.1:${argo_port}:30443/" dev/k3d.yaml > "$state/k3d.yaml"
    printf '%s\n' "$forgejo_port" > "$state/forgejo-port"
    printf '%s\n' "$argo_port" > "$state/argocd-port"
    mkdir -p "$HOME/.kube"
    k3d_global cluster create --config "$state/k3d.yaml"
  fi
  k3d kubeconfig get "$cluster" > "$KUBECONFIG"
  mkdir -p "$HOME/.kube"
  k3d_global kubeconfig merge "$cluster" --kubeconfig-merge-default --kubeconfig-switch-context
  k wait --for=condition=Ready node --all --timeout=180s
  k apply -f dev/forgejo/forgejo.yaml
  k rollout status -n forgejo deployment/forgejo --timeout=180s
  k create namespace argocd --dry-run=client -o yaml | k apply -f -
  if [[ ! -f "$state/argocd-install.yaml" ]]; then
    curl --fail --silent --show-error --location "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml" -o "$state/argocd-install.yaml"
  fi
  if command -v sha256sum >/dev/null; then
    echo "$ARGOCD_INSTALL_SHA256  $state/argocd-install.yaml" | sha256sum --check --status
  else
    echo "$ARGOCD_INSTALL_SHA256  $state/argocd-install.yaml" | shasum -a 256 --check --status
  fi
  cp dev/argocd/local.yaml "$state/argocd-local.yaml"
  cat > "$state/kustomization.yaml" <<'ARGO'
namespace: argocd
resources:
- argocd-install.yaml
patches:
- path: argocd-local.yaml
ARGO
  # Apply the final configuration together, so repeated up does not toggle live settings.
  k apply --server-side --force-conflicts -k "$state"
  k wait --for=condition=Established crd/applicationsets.argoproj.io crd/applications.argoproj.io --timeout=120s
  k rollout status -n argocd deployment/argocd-server --timeout=300s
  k rollout status -n argocd deployment/argocd-repo-server --timeout=300s
  k rollout status -n argocd deployment/argocd-applicationset-controller --timeout=300s
  k rollout status -n argocd statefulset/argocd-application-controller --timeout=300s
  k apply -f dev/bootstrap/statusobject.yaml
  k wait --for=condition=Established crd/statusobjects.testing.gitops.example.io --timeout=120s
  k apply -f dev/bootstrap/project.yaml
  make generate manifests
  k apply -f config/crd/bases
  k wait --for=condition=Established crd/gitresources.gitops.example.io crd/clustergitconfigs.gitops.example.io --timeout=120s
  hack/bootstrap-forgejo.sh
  k apply -f config/samples/gitops_v1alpha1_clustergitconfig.yaml
}
endpoints() {
  echo "Forgejo: http://127.0.0.1:$(cat "$state/forgejo-port")"
  echo "Argo CD: http://127.0.0.1:$(cat "$state/argocd-port")"
  echo "Local credentials: $state/credentials.json (ignored; mode 600)"
  echo "Kubeconfig: $KUBECONFIG; context: $context"
}
case "${1:-}" in
  up)
    setup
    skaffold run --kubeconfig "$KUBECONFIG" --kube-context "$context" --status-check=true
    k rollout status -n git-state-system deployment/controller-manager --timeout=180s
    endpoints
    ;;
  dev)
    setup
    endpoints
    exec skaffold dev --kubeconfig "$KUBECONFIG" --kube-context "$context" --cleanup=false --status-check=true
    ;;
  check) check_stack ;;
  clean)
    # No project-owned background helpers are started. Skaffold dev stays in the foreground.
    if [[ -f "$state/owned" ]]; then
      require k3d
      select_runtime
      if k3d cluster list --no-headers | awk '{print $1}' | grep -qx "$cluster"; then
        k3d_global cluster delete "$cluster"
      fi
    fi
    rm -rf -- "$state" "$PWD/reports"
    ;;
  *) echo 'Usage: hack/dev-stack.sh {up|dev|check|clean}' >&2; exit 2 ;;
esac
