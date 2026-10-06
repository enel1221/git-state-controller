#!/usr/bin/env bash
# No Secrets, environment dumps, or credential URLs are collected.
set -uo pipefail
cd "$(dirname "$0")/.."
report="$PWD/reports/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$report"
k() { kubectl --kubeconfig "$PWD/.dev/kubeconfig" --context k3d-git-state-dev "$@"; }
k get gitresources -A -o yaml > "$report/gitresources.yaml" 2>&1
k get applicationsets,applications -n argocd -o yaml > "$report/argo.yaml" 2>&1
k get events -A > "$report/events.txt" 2>&1
k get pods -A > "$report/pods.txt" 2>&1
for component in 'git-state-system controller-manager' 'argocd argocd-applicationset-controller' 'argocd argocd-repo-server' 'argocd argocd-server' 'forgejo forgejo'; do
  read -r ns deployment <<< "$component"
  k logs -n "$ns" "deployment/$deployment" --all-containers --tail=300 > "$report/$deployment.log" 2>&1 || true
done
k logs -n argocd statefulset/argocd-application-controller --tail=300 > "$report/argocd-application-controller.log" 2>&1 || true
# Inspect history inside Forgejo; no authenticated clone URL or Secret output.
k exec -n forgejo deployment/forgejo -- git --git-dir=/data/git/repositories/demo/resources.git log --oneline -60 > "$report/history.txt" 2>&1 || true
echo "Diagnostics: $report"
