# git-state-controller

A small Kubernetes controller that publishes each `GitResource.spec.manifest` to
one Git file, records the published generation and commit SHA, and hands that SHA
to a shared Argo CD ApplicationSet. Each resource gets its own Application and
literal file filter. Git cleanup uses a finalizer; Argo cleanup follows
asynchronously after the CR disappears.

The implementation follows [SPEC.md](SPEC.md). It uses the Kubebuilder v4.16.0
Go scaffold, go-git v5.19.3, and controller-runtime v0.25.0. There is no webhook,
Crossplane dependency, external database, Git hosting account, or registry push.

## Validation recorded on 2026-10-05

The following checks were executed successfully during implementation:

- `make test-unit`: race-enabled manifest/controller tests, actual authenticated
  Git smart-HTTP publication/deletion/race/recovery tests, and stack lifecycle
  helper tests.
- `make test-api`: Kubernetes 1.37.0 envtest admission/defaulting, unknown embedded
  fields, immutable destinations, status generation, and finalizer behavior.
- `make lint`, `go vet -tags=e2e ./test/e2e`, and E2E compilation.
- Kustomize rendering and Skaffold v2.25.0 configuration diagnosis.
- `make up` and the complete `make test` on Linux amd64 with rootless Podman
  5.8.7. All ten live E2E scenarios passed, including twenty
  concurrent publications, credential/handoff recovery, repeated bootstrap,
  changed-code rebuild/import/deployment, and Git/Argo outages during deletion.
- `make clean` from the healthy stack, a second no-op cleanup, and cleanup after
  an intentionally failed bootstrap with partially written local state.
- Fresh `make up` after teardown and the sample walkthrough below: both samples
  published, both Applications were Synced/Healthy, the greeting update reached
  the ConfigMap, and deletion removed the files and downstream resources.
- A final E2E rerun (104.4 seconds) also checked that both loopback UI endpoints
  return HTTP 200.

## Prerequisites and pins

Install Docker with a working daemon or Podman with its API socket, plus Go, Make,
kubectl, k3d, Skaffold, Bash, curl, and Git. Git is used by the test fixture's
smart-HTTP backend, not by the
production controller. Initial tool/image downloads require network access.

| Component | Pin |
| --- | --- |
| Go toolchain / build image | 1.26.8 / `golang:1.26.8-bookworm` |
| Kubebuilder | v4.16.0 |
| go-git | v5.19.3 |
| controller-runtime / Kubernetes libraries | v0.25.0 / v0.37.0 |
| controller-tools / envtest | v0.22.0 / Kubernetes 1.37.0 |
| k3d / k3s image | v5.9.0 / `rancher/k3s:v1.37.1-k3s1` |
| Skaffold | v2.25.0 |
| Forgejo | `codeberg.org/forgejo/forgejo:16.0.5` |
| Argo CD | v3.5.3; installation manifest verified against a recorded SHA256 |
| Kustomize / golangci-lint | v5.8.1 / v2.13.1 |

The version inventory is in [hack/versions.env](hack/versions.env), Go module
files, the Dockerfile, and Makefile. CI actions use commit SHAs. On Linux amd64,
`hack/install-ci-tools.sh` installs pinned, checksum-checked k3d/Skaffold/kubectl
into `bin/`; use `export PATH="$PWD/bin:$PATH"` afterward. It does not install
a container runtime or change global configuration. The stack helper prefers these
project-local pinned tools automatically. Make installs generation, lint, and
envtest tools into `bin/` on demand.

## Podman

Podman is selected automatically when the Docker CLI is absent. To select it
explicitly, use `CONTAINER_RUNTIME=podman make up` (or `make dev`). For rootless
Linux, start the user API socket before setup:

```sh
systemctl --user start podman.socket
CONTAINER_RUNTIME=podman make up
```

The helper reads the socket path from `podman info` and exports `DOCKER_HOST` and
`DOCKER_SOCK` for k3d and Skaffold. Explicit endpoint variables are honored.
Skaffold's Podman profile uses the compatible API builder with BuildKit disabled.
For Podman, a project-local k3d wrapper selects direct image import through the
host API. This also covers Skaffold's automatic imports during source watching.
The chosen runtime is saved in `.dev/runtime` and reused for cleanup; switching
runtimes requires cleaning the existing stack first. This does not change global
container settings or manage the user's Podman service. See the
[k3d Podman setup](https://k3d.io/stable/usage/advanced/podman/) for remote sockets
and host configuration.

## Four-command workflow

Run `make` or `make help` to list available commands and their descriptions.

```sh
make up       # bootstrap dependencies, build/load/deploy, wait, return
make dev      # same bootstrap, then watch/build/load/deploy/log
make test     # unit + real API + live-stack E2E; every suite is required
make clean    # delete the owned disposable cluster and local state
```

`make dev` can be the first command. Ctrl+C stops watching and leaves the stack
running. Images build locally and load into k3d; no credentials or remote image
push are needed. Source/API edits regenerate code and CRDs during builds.
Infrastructure bootstrap runs separately from that loop.

The dedicated cluster is `git-state-dev`. Every helper uses `.dev/kubeconfig`
and the `k3d-git-state-dev` context explicitly. `up` and `dev` also merge the
cluster into `~/.kube/config` and select it as the current context, so ordinary
`kubectl` commands work immediately. Existing kubeconfig entries are preserved;
`clean` removes this cluster's entries through k3d. An existing same-name cluster without this
checkout's `.dev/owned` marker is rejected. Cleanup also handles a partial
bootstrap and is repeatable.

Forgejo and Argo UI endpoints bind to loopback at ports 3000 and 8080. Before
initial creation, override them with `FORGEJO_PORT=3001 ARGOCD_PORT=8081 make up`.
Repeated setup preserves the cluster's original ports, credentials, Git history,
and ApplicationSet entries. Actual endpoints and ignored `.dev/credentials.json`
(mode 600; Forgejo and initial Argo admin credentials) are printed after setup.
Controller and Argo use the in-cluster Forgejo service URL.

The local overlay explicitly passes `--allow-http=true`; the base deployment
requires verified HTTPS. The controller is one replica with leader election and
four Git workers. Temporary worktrees use a writable `/tmp` volume under a
read-only root filesystem.

## Local walkthrough

Define a helper that always selects the managed stack:

```sh
make up
k() { kubectl --kubeconfig "$PWD/.dev/kubeconfig" --context k3d-git-state-dev "$@"; }
k apply -k config/samples
k wait -n demo --for=condition=Published gitresource/example-config --timeout=180s
k wait -n demo --for=condition=Published gitresource/example-config-two --timeout=180s
k get gitresources -n demo
k get applicationsets,applications -n argocd
k get configmaps -n demo
```

The samples create two ConfigMaps in separate files and independently pinned
Applications. `Published=True` confirms Git publication for its condition's
observed generation. It does not indicate Argo sync or workload health; use the
Applications and ConfigMaps to inspect that part of the flow.

```sh
# Change the authored manifest; generation advances automatically.
k patch gitresource example-config -n demo --type=merge \
  -p '{"spec":{"manifest":{"data":{"greeting":"updated"}}}}'
k get gitresource example-config -n demo -o yaml
k get application -n argocd
k get configmap example-config -n demo -o yaml

# The full suite creates its own namespace/files and restores disrupted fixtures.
make test

# Run in a separate terminal, edit Go code, observe rebuild/deployment and logs.
make dev

# Git deletion completes before Argo's asynchronous cascade.
k delete gitresource example-config example-config-two -n demo
k get applications -n argocd
k get configmaps -n demo
make clean
```

Poll `status.lastPublishedGeneration == metadata.generation` when verifying an
update. An old True condition can remain visible briefly while a newer generation
is pending. Failed publication preserves the last successful SHA and Application.
A message-only edit can publish a new generation without creating a Git commit.

## Reconciliation and ownership

The destination URL, branch, and relative YAML path are immutable. The branch
must already have an initial commit. Credentials come from a cluster-scoped
`ClusterGitConfig`, whose Secret must be in the controller namespace. Omitted
references select `ClusterGitConfig/default`; config and Secret watches enqueue
affected resources.

Manifest rendering preserves unknown authored fields and removes top-level
status and documented metadata bookkeeping. Each Git attempt clones the target
branch into its own temporary directory, stages only the managed path, commits,
pushes normally, and verifies remote content. Branch advancement gets at most
three fresh attempts with bounded jitter; each retry rereads the CR. UID checks
protect same-name replacements, and a status conflict retries only status writes.
No-op reconciles retain a previously valid SHA despite unrelated commits.

The ApplicationSet inventory controller lists the current CRs, sorts successful
publications, and patches only the first List generator's elements. Pending or
failed revisions retain their previous entries. Inventory read failure never
becomes an empty list. Startup reconciliation, source/ApplicationSet watches,
and a 30-second resync recover removal after deletion and manager restarts.
Git workers do not write this shared list or generated Applications.

Bootstrap refreshes the static ApplicationSet template while preserving existing
generator elements. Skaffold's frequently deployed configuration contains only
the controller, CRDs, and scoped RBAC. Argo's controllers own generated
Applications and downstream resources. The sample AppProject permits ConfigMaps;
extend its permissions and install target CRDs when applying other embedded
kinds. Publication itself does not require those CRDs.

## Tests and diagnostics

`make test` requires the existing managed stack. Missing prerequisites, assets,
cluster, or failed suites return nonzero. For independent development use
`make test-unit`, `make test-api`, `make generate manifests`, `make fmt vet`, and
`make lint`. Production Git code never executes the Git CLI; the fast Git tests
use a real authenticated HTTP server backed by `git http-backend`.

The live suite exercises two isolated Applications (also with a shared directory),
manifest and message-only updates, rapid revisions, unknown fields, destination
immutability, alternate configuration and credential recovery, twenty concurrent
publications, handoff failure, repeated `up`, a real `dev` rebuild, Git outage
during deletion, and Argo outage/restart cleanup. A narrow test seam covers forced
branch races and uncertain push/status acknowledgment in fast tests.

E2E deliberately scales the disposable Forgejo/Argo deployments and changes
scoped ApplicationSet RBAC temporarily. It also creates and removes
`cmd/e2e_watch_probe.go` to verify that changed code is loaded. Run the suite
exclusively; avoid editing source or running another development loop concurrently.
Cleanup restores fixtures and removes test CRs/files. On E2E failure,
`hack/diagnostics.sh` collects logs, events, resource/Application YAML, and Git
history under ignored `reports/`, without collecting Secrets. `make diagnostics`
also runs it explicitly. CI uses the same `up`/`test` workflow, diagnostic capture,
and always-run `clean`.

## Boundaries

This is a trusted-environment example. GitResource creators use shared credentials
against supplied repository URLs; there is no untrusted-tenant isolation or host
policy. Restrict CR/config creation. Give different CRs different files; ownership
uniqueness is documented but not enforced. Human edits to a managed file may be
overwritten. Argo permissions govern embedded workload scope independently of
the wrapper CR's namespace.

YAML is ordinary plaintext in Git; sample manifests are non-sensitive ConfigMaps.
Deletion removes the current file, not historical commits. Removing the cleanup
finalizer manually bypasses Git cleanup. Wait for an old Application's cascading
cleanup before recreating the same downstream workload. Git publication,
Kubernetes status, and Argo application are separate eventual-convergence steps.
