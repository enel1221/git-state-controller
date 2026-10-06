# Repository guide

This example follows SPEC-v0.3.md, which supersedes SPEC-v0.2.md and SPEC.md. Keep production code under `internal/`, use go-git
for Git operations, and keep Git publication independent of the ApplicationSet
inventory reconciliation. The status observer uses no Git client and reads exact
targets uncached. Do not add deferred platform features.

The API uses Kubebuilder v4.16.0's ordinary Go scaffold. Never hand-edit
`PROJECT`, `api/**/zz_generated.*`, `config/crd/bases/*`, or `config/rbac/role.yaml`.
Change API/RBAC markers and run `make generate manifests`. Preserve
`+kubebuilder:scaffold:*` comments. Scaffold new APIs through Kubebuilder.

Use `make lint-fix` after Go edits and run the relevant checks:

- `make test-unit`: race-enabled logic/controller/smart-HTTP Git and lifecycle tests.
- `make test-api`: real envtest admission, defaults, status and finalizers.
- `make test`: all required suites, including the managed k3d stack and approval demo browser journey.
- `make test-ui`: Node 24+, npm and Playwright Chromium must be installed; missing prerequisites are errors.

E2E tests require the dedicated `git-state-dev` cluster created by `make up` or
`make dev`. They deliberately disrupt Forgejo/Argo and temporarily add a new Go
source file to verify Skaffold watching. Do not run them against another cluster
or concurrently with source editing. Helpers/tests use `.dev/kubeconfig` and
`k3d-git-state-dev` explicitly. Per the user's preference, `up`/`dev` also merge
the cluster into `~/.kube/config` and select it as the current context. Cleanup
removes that cluster's global entries through k3d; preserve unrelated entries.

Keep secrets in ignored `.dev/` and namespaced Kubernetes Secrets. Do not include
credentials in logs, status, generator elements, repository URLs or reports.
The controller's Secret RBAC is scoped to `git-state-system`; its ApplicationSet
RBAC is scoped to `argocd/git-resources`. Match those scopes when changing flags.

`make up` and `make dev` bootstrap static infrastructure separately from
Skaffold's controller loop. Existing ApplicationSet generator elements belong
to the inventory reconciler and must survive bootstrap and redeployment. Teardown
must delete only the owned disposable cluster and local generated state.
