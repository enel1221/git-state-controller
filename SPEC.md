# git-state-controller — SPEC v0.1

Date: 2026-10-05  
Deliverable: a standalone example Kubernetes controller and a reproducible local development/test stack.  
Status: implementation specification; this document does not represent a built or tested implementation.

## 1. Objective

Prove a small, understandable relationship between a namespaced Kubernetes custom resource and a file in Git:

```text
GitResource.spec.manifest
          |
          v
GitResource reconciler -- go-git --> repository / branch / file
          |
          v
GitResource.status: published generation + commit SHA
          |
          v
ApplicationSet reconciler --> shared sample ApplicationSet
                                      |
                                      v
                            one Application per GitResource
                                      |
                                      v
                           embedded resource applied by Argo CD
```

Users create, update, and delete one long-lived `GitResource`. Its embedded manifest is published directly to an existing Git branch. The controller records successful publication. A shared ApplicationSet generates a separate, SHA-pinned Application for each published GitResource.

This is an example repository, not an extensible platform, a reusable public library, a Crossplane provider, or a general GitOps replacement. Crossplane and External Secrets Operator are reference projects, not runtime dependencies.

## 2. Scope and deliberately omitted features

| Included in v0.1 | Deferred |
|---|---|
| Latest stable Kubebuilder scaffold; Go and controller-runtime | Custom scaffolding or a public SDK |
| Two CRDs: namespaced `GitResource`, cluster-scoped `ClusterGitConfig` | Namespaced GitConfig, configuration usage tracking |
| One nested object and one explicit Git file per GitResource | Multiple objects/files, render templates, change-request objects |
| Vendor-neutral Git publication using go-git | PR/MR APIs, approvals, provider-specific Git integrations |
| HTTPS username/password or token authentication | SSH, GitHub Apps, workload identity, commit signing |
| Concurrent CR reconciliation and bounded push retries | Distributed locks, batching, global ordering, sharding |
| Publication status and generation tracking | Argo sync/health and target-resource readiness on the CR |
| File deletion before releasing the finalizer | Orphan/retain policies, destination migration |
| One shared ApplicationSet with one Application per CR | Multiple ApplicationSets, multiple deployment clusters |
| k3d + Forgejo + Argo CD + Skaffold + a Makefile | A separate controller database or durable job service |

Repository URL, branch, and file path are immutable for a GitResource's lifetime. Two GitResources must be configured to use different files. v0.1 documents that rule but does not enforce cross-resource ownership or path uniqueness.

The first smoke example is a ConfigMap. Any otherwise valid single Kubernetes object, including an XR, can be embedded; its CRD and deployment permissions must exist for Argo CD to apply it. No schema discovery, Crossplane installation, or cloud credentials are required for Git publication.

## 3. Toolchain and version policy

Use Kubebuilder **v4.16.0** and go-git **v5.19.3**, the latest stable releases verified on the specification date. Use the normal Go scaffold and its compatible controller-runtime, Kubernetes libraries, controller-tools, and envtest versions. Do not independently mix the newest versions of every Kubernetes dependency. [1][2]

Pin the Go toolchain, k3d/k3s image, Skaffold, Forgejo image, Argo CD release, and CI actions during implementation. Check the resulting version inventory into the repository. Runtime setup must not resolve floating `latest` tags. Upstream changes after this specification are adopted deliberately, not silently by `make up`.

The developer prerequisites are Docker, Go, Make, kubectl, k3d, and Skaffold. Project-local code-generation, lint, and envtest tooling may be installed by the normal Makefile helpers. Initial image/tool downloads need network access; the tests do not require a personal GitHub account, token, or external Git service.

## 4. API

Use the example API group `gitops.example.io`, version `v1alpha1`. This is a sample group, not a requirement to use an Architect/JADE domain.

### 4.1 GitResource

```yaml
apiVersion: gitops.example.io/v1alpha1
kind: GitResource
metadata:
  name: example-config
  namespace: demo
spec:
  gitConfigRef:
    kind: ClusterGitConfig
    name: default
  repository:
    url: http://forgejo.forgejo.svc.cluster.local:3000/demo/resources.git
    branch: main
    path: resources/demo/example-config/resource.yaml
  change:
    message: Update example configuration
  deletionPolicy: Delete
  manifest:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: example-config
      namespace: demo
    data:
      greeting: hello
```

The HTTP URL above is only for the disposable local stack. External use defaults to HTTPS with certificate verification.

| Field | Contract |
|---|---|
| `gitConfigRef` | Optional. Defaults to `{kind: ClusterGitConfig, name: default}`. Only this configuration kind is supported. The reference may be changed. |
| `repository.url` | Required, immutable Git clone URL; no embedded credentials. |
| `repository.branch` | Required, immutable branch name, not a tag, SHA, or arbitrary refspec. |
| `repository.path` | Required, immutable repository-relative `.yaml` or `.yml` file path. |
| `change.message` | Optional human-readable commit message. Does not act as a submit token, request ID, or approval gate. |
| `deletionPolicy` | Defaults to `Delete`. `Delete` is the only accepted enum value initially. |
| `manifest` | Required nested object, not a YAML string, array, or multi-document document. |

Use a `runtime.RawExtension` or equivalent unknown-field-preserving object representation. Enable nested embedded-resource/unknown-field preservation in the generated CRD so unfamiliar resource fields survive API-server storage. Require `apiVersion`, `kind`, and `metadata.name`. Do not require the target kind to exist in the local cluster to publish it. [3]

Use CRD validation/CEL for required fields and destination immutability; no admission webhook or cert-manager dependency is needed for this example. Perform straightforward URL, branch, and file-path checks in the controller as well. Reject absolute paths, traversal, `.git` components, and symlink-based escape from the worktree. Reject filename glob metacharacters because the Argo example uses the basename as a literal include pattern.

There is no custom CR revision counter. Enable the status subresource and use `metadata.generation` to identify desired-state revisions. Never sort CRs by Kubernetes timestamps or treat `resourceVersion` as a business ordering number. [4]

### 4.2 ClusterGitConfig

```yaml
apiVersion: gitops.example.io/v1alpha1
kind: ClusterGitConfig
metadata:
  name: default
spec:
  credentials:
    source: Secret
    secretRef:
      namespace: git-state-system
      name: forgejo-writer
  commitAuthor:
    name: git-state-controller
    email: git-state-controller@example.invalid
```

The referenced Secret contains `username` and `password` keys; a Git access token can be supplied as the password. `source: Secret` is the only supported credentials source. Commit-author fields receive these sample defaults when omitted.

`ClusterGitConfig` is cluster-scoped; its Secret is namespaced. For v0.1, the Secret namespace must equal the controller's configured namespace. GitResource objects cannot override the Secret reference directly. ClusterGitConfig/Secret changes enqueue affected GitResources without requiring users to change the manifest.

This follows the shape of Crossplane's namespaced-resource → ClusterProviderConfig → namespaced-Secret relationship. It does not import Crossplane or copy its usage/protection controllers. A separate reconciler for ClusterGitConfig health is unnecessary initially; errors are reported on affected GitResources. [5]

### 4.3 Publication status

```yaml
status:
  observedGeneration: 3
  lastPublishedGeneration: 3
  lastPublishedRevision: "<full-commit-sha>"
  lastPublishedContentHash: "sha256:<canonical-content-hash>"
  conditions:
    - type: Published
      status: "True"
      observedGeneration: 3
      reason: Pushed
      message: Desired manifest is present in the remote repository.
      lastTransitionTime: "2026-10-05T22:00:00Z"
```

`observedGeneration` is the generation most recently evaluated. `lastPublishedGeneration` is the generation whose rendered content was verified in the remote repository. These may differ after an unsuccessful update. Retain the previous successful revision and content hash on failure.

`Published=True` with a matching condition generation means publication succeeded for that desired state. It does **not** mean Argo applied the SHA, that an ApplicationSet update succeeded, or that the resulting resource is healthy.

Useful reasons include `Pushed`, `Unchanged`, `InvalidSpec`, `ConfigNotFound`, `CredentialsInvalid`, `PublishFailed`, and `DeleteFailed`. Standard Kubernetes Events and structured logs supplement conditions. Avoid status updates when nothing materially changed, and do not reset condition transition timestamps on each reconcile.

A change-message-only update may advance the published generation without making a new commit if the file content is identical. The revision identifies content, not a promise of one Git commit per Kubernetes generation.

## 5. Manifest publication

### 5.1 What is written

Serialize only `spec.manifest` as deterministic YAML. Preserve arbitrary desired-state fields such as `data`, `rules`, and resource-specific `spec` content. Do not copy the wrapper CR, its credentials/configuration reference, its change metadata, or its status into the target file.

Drop the embedded object's top-level `status` and ordinary server bookkeeping in embedded metadata: `uid`, `resourceVersion`, `generation`, `managedFields`, timestamps, `deletionGracePeriodSeconds`, and `selfLink`. Preserve the remaining authored object. Do not implement kind-specific Service/Pod/PVC normalization, live-resource export, or automatic field inference in v0.1.

Publish one file per operation and stage only that path. Never stage the entire repository. Commit messages use `change.message` when provided, otherwise a deterministic action/name description. Add short commit trailers identifying the GitResource namespace/name, UID, and reconciled generation. Do not add an audit file, commit ledger, or per-change CR.

### 5.2 Reconcile algorithm

1. Read the GitResource and capture its UID, generation, and desired manifest.
2. Route a deleting object to the deletion path; do not publish new content for it.
3. Ensure the controller's Git-cleanup finalizer is persisted **before** creating Git state.
4. Resolve ClusterGitConfig and credentials, validate the destination, and render the manifest.
5. Clone/check out the current remote target branch into an isolated temporary worktree.
6. Compare the existing target file with the desired bytes. Equal content is a successful no-op.
7. Otherwise write the file, stage only that file, create a commit, and push normally.
8. On success, record the generation actually processed and the remotely verified revision.
9. A status-update conflict retries the Kubernetes status update, not the Git commit operation.
10. Clean up the worktree on all exit paths.

Use go-git for all production Git operations. Do not shell out to `git` or call GitHub/GitLab/Forgejo content APIs from the controller. A repository and branch must already exist and contain an initial commit. The local bootstrap creates them; runtime repository creation is out of scope.

Use bounded operation contexts and normal controller-runtime error backoff. For a push outcome that is uncertain because the connection failed, re-read remote state before deciding to create more work. If the desired file is already present, recover successfully without a duplicate content commit.

When content is unchanged, preserve the previously published SHA if it remains valid for that content. When publication status is absent or being recovered, the current verified branch-head SHA is an acceptable result. An unrelated CR's commit should not continually advance this CR's Application revision during ordinary no-op reconciles.

### 5.3 Concurrency and consistency

Default `MaxConcurrentReconciles` to **4**, configurable by a manager flag. Use one active manager with leader election enabled and one replica in the sample. Different CRs reconcile concurrently; there is no globally serialized Git worker or per-repository lock service.

Each Git attempt has its own working directory. Multiple CRs can target the same repository and branch. Distinct files avoid competing content ownership, but pushes still update a shared branch reference; normal Git push rules can reject a stale update. [6]

For a rejected push caused by branch advancement, discard/reset the attempted tree, obtain the latest branch head, reapply only this CR's file operation, and retry. Use at most three attempts per reconciliation with a short bounded jitter, then return for normal backoff. Do not merge, rebase, force-push, or replay another CR's files. Re-read the CR when restarting an attempt so an obsolete generation is not repeatedly retried.

Generations that arrive during an in-flight operation are handled by subsequent reconciliation. A successfully published older generation must never be mislabeled as the newest generation. Status patches must verify the UID so a stale operation cannot report success on a same-name replacement.

The contract is eventual convergence to the latest desired state, not FIFO across CRs, exactly-once delivery, or a commit for every rapid intermediate edit. Kubernetes status and a remote Git push are separate operations; recovery is based on observed content rather than a cross-system transaction.

v0.1 assumes a managed path is not also edited by another CR or a human. An accidental competing writer may be overwritten on reconciliation. Unrelated paths must always be preserved. No duplicate-owner admission rule or manual-drift conflict workflow is required initially.

## 6. ApplicationSet integration

### 6.1 One shared set, independent Applications

Skaffold bootstraps a sample `ApplicationSet` named `git-resources` in `argocd`. It uses a List generator. The Git controller manages only the agreed list of publication inputs; it does not directly manage generated Applications. Argo's List generator supports string-valued inputs passed into the Application template. [7]

Each element contains the GitResource identity, deterministic Application name, repository URL, parent directory, filename, full published SHA, and wrapper namespace. Use a safe name derived from namespace/name with a short UID-derived suffix to avoid naming collisions. This is identity tracking, not file-ownership enforcement.

The generated Application targets:

| Application field | Value |
|---|---|
| `spec.source.repoURL` | GitResource repository URL |
| `spec.source.targetRevision` | Its last successfully published full SHA, never a branch alias |
| `spec.source.path` | Parent directory of its file |
| `spec.source.directory.include` | Its literal filename |
| `spec.source.directory.recurse` | `false` |
| `spec.destination.server` | The in-cluster Kubernetes API |
| `spec.destination.namespace` | The wrapper CR's namespace as the default namespace |

A directory include filter isolates a CR's file even when several different files share a directory. This is supported by Argo's plain-directory source. The sample convention still places each file in its own directory for readability. [8]

Preserve any explicit namespace inside the authored manifest; do not inject a namespace into a cluster-scoped object. The sample ConfigMap explicitly matches its wrapper namespace. Argo project permissions, not Git publication, determine what can actually be applied.

### 6.2 Keep Git and handoff reconciliation separate

Implement a small ApplicationSet reconciler in the **same manager process**. Git publication workers only write Git and publication status. They do not each read/replace the shared List generator from stale snapshots.

All relevant GitResource publication/deletion events enqueue the one configured ApplicationSet key. Reconcile that aggregate key by listing GitResources, building a sorted list from their last successful publications, and updating only the managed generator elements. Recompute on a Kubernetes conflict. The shared object's workqueue key naturally coalesces its own writes; this does not serialize Git publication.

Retain an existing entry while a new generation is pending or fails to publish. Retain it while the CR is terminating but its Git finalizer has not finished. Remove it when the GitResource no longer exists. Never treat a list/read failure as an empty inventory.

Watch the ApplicationSet as well, and enqueue an inventory reconciliation on startup with a small periodic resync (for example, 30 seconds). Cleanup therefore survives deletion of the source CR and controller restarts. No durable cleanup CR, ConfigMap ledger, or job queue is needed.

When Argo or ApplicationSet updates fail, keep Git publication independent. Retry the handoff and report its error through Events/logs. v0.1 does not add Argo health or synchronization conditions to GitResource.

### 6.3 Field ownership during development

Bootstrap creates the sample ApplicationSet if absent. Subsequent `make up` or controller rebuilds must **not** reapply an empty `elements: []` over the controller-maintained list. Keep infrastructure/sample bootstrap separate from the controller's frequently redeployed Skaffold configuration. Any update to the static ApplicationSet template must preserve the live list.

Skaffold owns installation/static configuration. The ApplicationSet reconciler owns list inputs. The Argo ApplicationSet controller owns generated Applications. Argo CD owns application of the embedded workload. Test this separation during a controller redeploy.

### 6.4 Argo credentials

Argo CD needs its own repository credentials; ClusterGitConfig is not an Argo API. Local bootstrap uses the same generated local bot credentials to create the controller's Secret and an Argo repository credential Secret in `argocd`. Argo supports declarative repository Secrets. [9]

Do not put usernames, passwords, or tokens into generator elements, repository URLs, logs, or GitResource status. General credential replication/rotation into Argo is not a controller feature in v0.1; bootstrap configures the local fixture.

## 7. Deletion

The finalizer is `gitops.example.io/git-cleanup`. `deletionPolicy: Delete` means removing the configured file from the target branch's current tree, not erasing old Git history.

On deletion, resolve credentials, read the current remote branch, remove only the configured file, commit the deletion, and push with the same branch-advance retry behavior used for updates. If the file is already absent in the remotely verified tree, treat cleanup as complete without another commit. Never delete the repository, branch, parent directory contents, or unrelated files.

After remote absence is confirmed, release this controller's finalizer. Do **not** wait for an ApplicationSet update, Argo synchronization, Application deletion, or target-resource deletion. Other finalizers, when present, can still delay Kubernetes object removal.

The ApplicationSet inventory reconcile subsequently notices the CR is absent and removes its entry. Configure the sample ApplicationSet to permit generated Application deletion and not preserve resources; generated Applications use the Argo resource-deletion finalizer. Argo then performs cascading downstream cleanup asynchronously. [10]

The deletion SHA does not need to be deployed: this sample removes the entire per-CR Application instead. This also avoids depending on Argo rendering an empty/missing directory after the file deletion.

If Git is unavailable, the CR remains terminating and cleanup retries. If Argo is unavailable but Git cleanup succeeded, the CR can disappear while its Application remains until Argo recovers. A restart between those events must still result in eventual ApplicationSet entry removal. Manual removal of this controller's finalizer bypasses its Git cleanup guarantee.

## 8. Local developer workflow

### 8.1 Four primary commands

| Command | Required behavior |
|---|---|
| `make up` | Idempotently create/reuse the dedicated k3d cluster, bootstrap Forgejo/Argo/configuration, build and load the current controller image with Skaffold, install it, wait for readiness, and return. |
| `make dev` | Perform the same initial setup, then run Skaffold's watch/build/load/deploy/log loop for controller development. No prerequisite `make up` is required. |
| `make test` | Run all required unit, API/envtest, Git integration, and live-cluster end-to-end tests against the existing managed stack. A missing cluster produces a clear instruction to run `make up` or `make dev`, not skipped E2E tests. |
| `make clean` | Stop project-owned helper processes, delete the dedicated k3d cluster and its disposable data, and remove generated local state. Work even after partial bootstrap failure. |

Retain normal useful Kubebuilder targets such as `generate`, `manifests`, `fmt`, `vet`, and `lint`. Internal test targets may split the suites, but a new developer only needs the four commands above for the full workflow.

Stopping `make dev` with Ctrl+C stops watching without dismantling the stack. Use Skaffold's cleanup-disabled development behavior; `make clean` is the explicit full reset. Skaffold otherwise normally cleans up deployed resources on Ctrl+C. [11]

### 8.2 Stack layout and startup

Default cluster name: `git-state-dev`. Keep its generated kubeconfig under ignored `.dev/`; every Makefile/helper/test command explicitly uses that kubeconfig and context. Per the updated user preference, `up` and `dev` also merge the cluster into `~/.kube/config` and select its context, using k3d's normal behavior. Preserve unrelated kubeconfig entries; cleanup removes only this cluster's entries. Do not operate on a pre-existing unrelated cluster.

| Component | Local setup |
|---|---|
| k3d | One server node initially, pinned k3s image. Cluster lifecycle is handled by a small helper because a cluster must exist before Skaffold deployment. |
| Forgejo | In-cluster deployment in `forgejo`, SQLite, persistent data for the life of the disposable cluster, registration disabled. No external database is needed. |
| Argo CD | Pinned non-HA development installation including ApplicationSet support in `argocd`. |
| Controller | One Deployment in `git-state-system`, four CR workers, generated CRDs and RBAC. |
| Demo state | `demo` namespace, private `demo/resources` Git repository with seeded `main`, bot credentials, `ClusterGitConfig/default`, and the shared sample ApplicationSet. |

Wait for API/CRD establishment and dependency readiness instead of fixed sleeps. Initialize the Forgejo account and repository idempotently. Bootstrap may use Forgejo's administrative CLI/HTTP API to prepare the **test fixture**; the controller must not depend on those APIs.

Use an in-cluster service URL for controller and Argo repository access, not host `localhost`. Expose Forgejo and the Argo UI on loopback-only host ports through the local cluster configuration. Print the actual endpoints and the location of ignored local credentials when setup completes. Permit port overrides without editing checked-in YAML.

For the disposable stack only, enable an explicit local HTTP exception for the Git controller and use local Forgejo HTTP. Do not silently disable HTTPS verification or require developers to bootstrap a certificate authority. The local-only exception must be visible in the dev configuration, not the default production-style deployment.

### 8.3 Skaffold image loop

Use Skaffold local Docker builds with remote pushing disabled. Its local-cluster handling recognizes `k3d-*` contexts and supports image loading, so no GHCR account, container registry, or manual `docker push` is required. Configure the controller image pull policy for locally loaded images. [12]

A Go source change in `make dev` must build a new controller image, load it into k3d, redeploy the controller, and reconnect log output. Changes to API types must also regenerate/redeploy necessary generated code/CRDs. Ignore generated artifacts, test reports, and `.dev/` in watch inputs where necessary to prevent rebuild loops.

Infrastructure bootstrap must not reseed Git history, change the bot password, erase ApplicationSet entries, or restart Forgejo/Argo on every Go source edit. Forgejo state must survive controller rebuilds and repeated `make up` within the same cluster.

### 8.4 Cleanup guarantees

Delete the disposable cluster directly rather than waiting indefinitely for every Kubernetes finalizer during teardown. Remove only project-owned containers, volumes, port forwards, and generated files. Never use `docker system prune`, wipe unrelated kubeconfig entries, or delete unrelated clusters. A second `make clean` succeeds as a no-op.

## 9. Tests and acceptance criteria

Use fast tests for deterministic logic, envtest for real API admission/status/finalizer behavior, and Forgejo/k3d E2E tests for actual publication and ApplicationSet behavior. Do not substitute a mocked Git writer for all correctness testing.

| Case | Required assertion |
|---|---|
| Initial create | One GitResource produces the expected file on its specified branch; status reports the published generation and full SHA. |
| Update | A manifest edit changes only its file and eventually advances its published generation/revision. |
| Unknown embedded fields | Arbitrary nested fields survive the Kubernetes API and publication unchanged, apart from documented bookkeeping removal. |
| No-op/status/metadata edits | Reconciliation and wrapper status/metadata-only updates do not create duplicate Git content commits. |
| Message-only edit | Identical file content is accepted without forcing an empty commit. |
| Immutable destination | Attempts to change URL, branch, or path are rejected. |
| Default/explicit configuration | Omitted and explicit default references work; another valid configuration reference works. |
| Credential failure/recovery | Missing/incorrect credentials report failure without false publication; correction allows convergence. |
| Concurrent publication | At least 20 CRs publish different files to one branch with four workers; no files or unrelated repository content are lost. |
| Forced branch race | A controlled Git integration test makes two operations start at the same head and verifies normal retry preserves both changes without force-push. |
| Rapid same-CR updates | The latest desired content eventually wins; status never attributes older bytes to a newer generation. |
| Uncertain push/status failure | Recovery after push success but failed acknowledgment/status persistence does not create duplicate content commits. Use a narrow test seam, not a production fault-injection framework. |
| Delete | Only the CR's file is removed remotely, and its finalizer is then released. |
| Already absent | Deletion converges without creating an empty commit when the remote file is already gone. |
| Git outage during delete | The CR remains terminating until Git cleanup can complete. |
| Separate Applications | Two CRs generate distinct Applications pinned to their own successful SHAs and isolated files, including when their files share a directory. |
| Handoff isolation | ApplicationSet update failure does not prevent a GitResource from reporting successful Git publication or cause Git recommits. |
| Pending update | A failed new publication does not remove the prior Application or replace its SHA with an unverified revision. |
| Async deletion/restart | Once the CR is gone, its ApplicationSet entry is removed even after manager restart; Argo eventually deletes the generated Application and sample ConfigMap. |
| Argo unavailable | Git deletion still releases the CR; downstream cleanup completes after Argo recovers. |
| Skaffold redeploy | Controller rebuild/redeploy preserves Git history and generator entries and does not spuriously delete Applications. |
| Local lifecycle | `up` is repeatable, `dev` loads changed code, and `clean` works from healthy and partially created stacks. |

Tests may inspect the Argo Application and live ConfigMap directly to verify end-to-end behavior. This does **not** require the controller to implement Argo/target status tracking.

Use isolated test namespaces and paths. Poll observable conditions with bounded deadlines rather than fixed sleeps. Restore outages/modified fixtures in cleanup. On failure, collect controller/Argo/Forgejo logs, CR and ApplicationSet/Application YAML, events, and relevant Git history into an ignored reports directory without credentials.

`make test` must return nonzero when a required suite fails or cannot run. Document that E2E tests deliberately exercise failures in the disposable dev stack. CI runs the same workflow (`make up`, `make test`, diagnostic capture, `make clean` in an always-run cleanup step), with no personal credentials.

## 10. Repository structure

```text
git-state-controller/
  PROJECT
  go.mod
  go.sum
  Dockerfile
  Makefile
  skaffold.yaml
  README.md
  SPEC.md
  api/v1alpha1/
    gitresource_types.go
    clustergitconfig_types.go
  cmd/
    main.go
  internal/
    controller/
      gitresource_controller.go
      applicationset_controller.go
      *_test.go
    git/
      publish.go
      credentials.go
      *_test.go
    manifest/
      render.go
      *_test.go
  config/
    crd/
    rbac/
    manager/
    default/
    samples/
  dev/
    k3d.yaml
    forgejo/
    argocd/
    bootstrap/
  hack/
    dev-stack.sh
    bootstrap-forgejo.sh
    versions.env
  test/
    integration/
    e2e/
  .github/workflows/
    test.yaml
```

Keep ordinary concrete helpers under `internal/`. Small test interfaces are acceptable where necessary, but do not introduce a public `pkg/` API, generic plugin registries, custom workflow engines, unnecessary repositories/services layers, or reflection-based resource adapters.

Implementation may adjust individual filenames to preserve the latest Kubebuilder scaffold. Avoid hand-editing generated files when changes belong in markers or templates.

## 11. Operating boundaries

This first version targets a trusted development environment. Creating GitResources grants use of shared Git credentials against supplied repository URLs, and Argo can apply powerful manifests. This is not a secure untrusted-tenant boundary. Restrict who can create these CRs and configuration resources. Per-host authorization, tenant isolation, and policy evaluation are follow-on work, not implied guarantees.

The controller must not log credentials, write credentials into repository remotes on disk, or require cluster-admin. Limit Secret access to the controller namespace and ApplicationSet writes to the configured Argo namespace/object. Do not grant it permissions to apply arbitrary embedded target kinds; that responsibility belongs to Argo.

YAML is committed as ordinary plaintext. No Secret encryption or redaction feature is included. The examples use non-sensitive ConfigMaps; users must not treat Git history removal as a deletion policy for leaked secrets.

Namespace scoping of GitResource does not restrict the scope of its embedded manifest. Argo project/RBAC policy governs target access. Immediate delete-and-recreate of the same underlying workload while an old Application is still cascading is not a coordinated replacement workflow in v0.1; allow downstream cleanup to complete first.

## 12. Implementation order and completion

Build in small slices: scaffold/APIs and test harness; Git create/update/no-op behavior; concurrent writes and deletion recovery; ApplicationSet handoff/cleanup; then complete the four-command developer loop and E2E suite. Do not start deferred features before this path works.

v0.1 is complete when a clean checkout with documented prerequisites can run `make up`, pass `make test`, use `make dev` to observe a rebuilt controller in the same cluster, and use `make clean` to remove the whole disposable stack. The demonstrated flow must cover two independently pinned resources, updates, concurrent publication, restart recovery, and deletion—not only the initial happy-path commit.

The implementation's README must show an actual tested walkthrough and the recorded tool/image versions. Until implemented and executed, none of those runtime acceptance criteria should be described as passing.

## 13. References

These sources justify selected mechanics; the contracts above are design decisions for this example, not claims that an upstream project implements the same API.

[1] Kubebuilder v4.16.0: https://github.com/kubernetes-sigs/kubebuilder/releases/tag/v4.16.0  
[2] go-git v5.19.3: https://github.com/go-git/go-git/releases/tag/v5.19.3  
[3] Kubebuilder CRD processing: https://book.kubebuilder.io/reference/markers/crd-processing  
[4] Kubernetes CRD status and generation: https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/  
[5] Crossplane provider configuration: https://docs.crossplane.io/latest/packages/providers/  
[6] Git push semantics: https://git-scm.com/docs/git-push  
[7] Argo ApplicationSet List generator: https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Generators-List/  
[8] Argo directory include filtering: https://argo-cd.readthedocs.io/en/stable/user-guide/directory/  
[9] Argo declarative repository configuration: https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/  
[10] Argo ApplicationSet deletion: https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Application-Deletion/  
[11] Skaffold cleanup behavior: https://skaffold.dev/docs/cleanup/  
[12] Skaffold local-cluster support: https://skaffold.dev/docs/environment/local-cluster/

Additional source-reading references, not dependencies:

- GitOps Reverser: https://github.com/ConfigButler/gitops-reverser — resource-to-Git publication and serialization; primary functional reference.
- Flux image automation source/publication code: https://github.com/fluxcd/image-automation-controller/tree/main/internal/source — Git operations, no-op behavior, and tests.
- External Secrets ClusterSecretStore: https://external-secrets.io/latest/api/clustersecretstore/ — shared configuration and namespaced credential references.
- Forgejo container setup: https://forgejo.org/docs/latest/admin/installation/docker/ — local test-server deployment.
