# git-state-controller — SPEC v0.2

**Status:** implementation specification; not an implementation or a claim that these tests have run.  
**Date:** 2026-10-06.  
**Baseline:** the working v0.1 example and its four-command k3d / Forgejo / Argo CD / Skaffold workflow. This document overrides v0.1 where stated; do not rebuild or generalize the existing project.

## 1. Goal and limits

Make a `GitResource` the convenient read model for its Git publication, Argo Application, and single directly managed resource. Keep upstream status recognizable. Add safe orphaning, Crossplane-style pause, explicit adoption, informational Git drift, and basic credential diagnostics.

Keep namespaced `GitResource`, cluster-scoped `ClusterGitConfig`, one embedded object, one immutable repository/branch/path, one SHA-pinned Application per CR, direct go-git pushes, four configurable publication workers, and the existing Makefile commands.

Do not add approvals, PR/MR operations, branches per change, a public SDK, a database, workflow engine, plugin registry, multiple files, recursive resource-tree observation, new authentication methods, remote-cluster observation, security-policy engines, or a history API. Do not rename existing working code merely to match illustrative filenames below. Keep the installed toolchain pinned; no incidental dependency upgrades in this feature change.

## 2. Decisions

| Area | Decision |
|---|---|
| Argo observation | Watch managed `Application` CRs through the Kubernetes API; no Argo REST/gRPC authentication or notification receiver. |
| Status mirror | Copy the Application's `.status` and the one target object's `.status` as opaque JSON objects, including upstream conditions and messages. |
| Live target | Use Application inventory to match the expected directly managed object; GET that exact object using an uncached reader. Poll every 15 seconds by default and on relevant Application events. |
| Summary | Standard `Ready` condition with a reason; retain `Published`, add revision-correlated `Synced`, and informational `GitDrift`. No persisted phase machine. |
| Orphan | Commit a marker using last published content, retain the existing ApplicationSet entry and Application identity, then release the finalizer. |
| Pause | `gitops.example.io/paused: "true"` blocks new mutations, including finalization. Read-only Kubernetes status observation may continue. |
| Adoption | `gitops.example.io/adopt-existing: "true"` permits one explicit takeover; publish the supplied manifest, preserve a recoverable Application identity, then consume the annotation. |
| Git drift | Report branch-head differences; do not repair, block a subsequent desired change, or advance Argo's SHA automatically. |
| History | Document path-scoped Git history and external history APIs; do not store a controller history ledger. |

## 3. Status API: upstream data plus a small envelope

Keep existing publication fields. Add the following shape; example values are illustrative, not required defaults:

```yaml
status:
  observedGeneration: 7
  lastPublishedGeneration: 7
  lastPublishedRevision: "<full-commit-sha>"
  lastPublishedContentHash: "sha256:<canonical-file-hash>"
  applicationRef:
    namespace: argocd
    name: demo-example-config-<existing-suffix>
  publishedResourceRef:
    apiVersion: v1
    kind: ConfigMap
    namespace: demo
    name: example-config
  conditions:
    - type: Published
      status: "True"
      observedGeneration: 7
      reason: Pushed
      message: Manifest publication verified in Git.
      lastTransitionTime: "2026-10-06T12:00:00Z"
    - type: Synced
      status: "True"
      observedGeneration: 7
      reason: Synced
      message: Argo reports the expected source and revision synchronized.
      lastTransitionTime: "2026-10-06T12:00:05Z"
    - type: Ready
      status: "True"
      observedGeneration: 7
      reason: Healthy
      message: Published revision is synchronized and Argo reports Healthy.
      lastTransitionTime: "2026-10-06T12:00:05Z"
    - type: GitDrift
      status: "False"
      observedGeneration: 7
      reason: InSync
      message: Managed file matches the last published content.
      lastTransitionTime: "2026-10-06T12:00:00Z"
  argoCD:
    uid: "<Application-uid>"
    resourceVersion: "<opaque-source-version>"
    lastUpdatedAt: "2026-10-06T12:00:05Z"
    observation:
      reason: Observed
      message: ""
    source: # snapshot of Application.spec.source, not its whole spec
      repoURL: http://forgejo.forgejo.svc.cluster.local:3000/demo/resources.git
      targetRevision: "<full-commit-sha>"
      path: resources/demo/example-config
      directory:
        include: resource.yaml
        recurse: false
    destination: # snapshot of Application.spec.destination
      server: https://kubernetes.default.svc
      namespace: demo
    status: # complete Application.status JSON, not a translated schema
      sync:
        status: Synced
        revision: "<full-commit-sha>"
        comparedTo:
          source:
            repoURL: http://forgejo.forgejo.svc.cluster.local:3000/demo/resources.git
            targetRevision: "<full-commit-sha>"
            path: resources/demo/example-config
            directory:
              include: resource.yaml
              recurse: false
          destination:
            server: https://kubernetes.default.svc
            namespace: demo
      health:
        status: Healthy
      resources:
        - version: v1
          kind: ConfigMap
          namespace: demo
          name: example-config
          status: Synced
  resource:
    ref:
      apiVersion: v1
      kind: ConfigMap
      namespace: demo
      name: example-config
      uid: "<live-resource-uid>"
    # generation is omitted when the source does not report one.
    resourceVersion: "<opaque-source-version>"
    observedAgainstRevision: "<Argo-compared-revision>"
    exists: true
    lastUpdatedAt: "2026-10-06T12:00:05Z"
    observation:
      reason: Observed
      message: ""
    # No status key: this ConfigMap does not have a .status field.
```

### 3.1 Mirroring rules

`argoCD.status` is a deep copy of the upstream Application status, not just `sync` and `health`. Preserve `conditions`, `operationState`, all nested sync-result messages, timestamps, and unknown future fields. Its existing upstream `history` is copied when present; do not accumulate additional history. This mirrors the Kubernetes Application object, not Argo's logs, Redis resource tree, Events, or other API-only data. [S2][S3]

`resource.status` is the exact top-level status object of the single target. Copy no target spec, ConfigMap data, Secret data, full metadata, managed fields, descendants, or referenced connection Secrets. Preserve the distinction between an absent status field and an existing empty object. An XR with a populated status exposes its raw conditions and outputs without our inventing an XR status schema.

Use an opaque JSON representation such as `apiextensionsv1.JSON` with `type: object` and `x-kubernetes-preserve-unknown-fields: true` at both status subtrees. Do not use `x-kubernetes-embedded-resource` for status fragments: they are not complete Kubernetes objects. Read the source unstructured when necessary to avoid losing fields through a reduced Go struct. Keep the outer envelope and `metav1.Condition` types explicit. [S4]

Replace the whole mirrored subtree when the source changes. Do not merge old upstream keys into a new snapshot: removed errors/conditions/fields must disappear. Source conditions remain nested and retain their upstream schema; do not convert them to our top-level condition list.

An `observation` is read/copy bookkeeping, not another lifecycle. Reasons are `Observed`, `NotFound`, `NotTracked`, `Forbidden`, `ReadFailed`, `UnsupportedTarget`, `SnapshotTooLarge`, or `InvalidStatus`. Use explanatory messages. `exists` is optional: true after a successful GET, false only after authoritative NotFound, omitted when existence is unknown. An absent status is not `NotFound` or `InvalidStatus`.

On source read failure or a changed identity, clear the affected raw snapshot rather than present stale data as current. Retain its object reference and last successful snapshot time for diagnosis. Clear UID on confirmed absence; a later same-name replacement gets a new UID and a fresh snapshot. Never carry status from one UID to another.

`lastUpdatedAt` means a snapshot or observation result materially changed, not a heartbeat. Source `resourceVersion` is the version associated with that snapshot; do not persist a metadata-only RV change when copied data, selected source identity, generation, and observation outcome are otherwise unchanged. Never compare RV numerically or across objects. These snapshots are eventually consistent observations, not an atomic transaction across Git, Argo, and the workload.

### 3.2 Size and write limits

Full small statuses are acceptable for this one-to-one example. Unbounded or rapidly changing embedded collections are not its scaling model. Kubernetes API conventions explicitly caution about large/high-churn status, and etcd has configurable request-size limits; do not treat an etcd limit as a guaranteed Kubernetes object allowance. [S1][S5]

Use these conservative implementation constants, not new per-CR tuning fields:

- At most **128 KiB of compact JSON per mirror**, including that mirror's source envelope. Total raw mirrors are therefore bounded to about 256 KiB.
- Check the prospective whole GitResource encoding too; use **768 KiB as a soft guard**. Omit affected raw mirrors before their addition would exceed it. If the user's existing spec/metadata already exceeds the guard, do not try to shrink their object; omit mirroring and preserve small publication/finalization updates.
- For an oversized mirror, remove that raw payload and publish `SnapshotTooLarge` with its reference and measured size. Do not silently truncate individual messages or pretend a partial object is a full copy. Resume copying automatically when it fits again.

Observation failure or oversize must never prevent Git publication, adoption completion, or finalization. The primary-object status write should still succeed with a small diagnostic. Handle a server-side too-large error with the same small fallback, without a tight retry loop. Large statuses remain readable at their original object references.

Write status only when semantically changed. No per-poll timestamp-only patches. Preserve condition transition time when only reason/message changes without a truth-value transition. An upstream timestamp changing is an actual upstream change and may be mirrored; we only avoid generating our own heartbeat churn.

Do not recursively mirror this controller's own GitResource/ClusterGitConfig status types or the observer's own generated Application as its target. Report `UnsupportedTarget`; avoid a self-copy growth loop without building a generic cycle detector.

Mirrored messages/outputs can contain sensitive information. Reading GitResource grants access to the copied data even when the caller cannot read the source object. The example remains trusted-only; document this explicitly. Do not log whole snapshots or promise redaction. User-facing clients must treat messages as untrusted text. No new security-policy engine is part of this change.

## 4. Conditions and lifecycle vocabulary

Use `[]metav1.Condition` keyed by type. A condition's status is only `True`, `False`, or `Unknown`; a reason explains it. Kubernetes conventions support a common `Ready` summary and discourage new phase-enum state machines. Reasons are not instructions for the reconciler to execute. [S1]

| Type | Responsibility |
|---|---|
| `Published` | Existing publication success for the desired generation; independent of Argo health and later branch drift. |
| `Synced` | Argo convergence to the exact successful publication, with the checks below. |
| `Ready` | One convenient overall answer for ordinary consumers, with a reason and source details below it. |
| `GitDrift` | Informational difference between current branch file and last published file; not a failure gate for Ready or future publication. |

Do not add top-level `Healthy`, `Syncing`, `Merged`, `Deleting`, and `Paused` conditions as duplicates of information already available. `Ready.reason`, native Argo fields, the pause annotation, and `metadata.deletionTimestamp` cover those displays.

### 4.1 Synced=True contract

Require all of the following from the current stored observations:

1. `lastPublishedGeneration == metadata.generation` and current-generation `Published=True`.
2. The expected generated Application has been observed, is not deleting, and its tracking identity matches this GitResource or an explicitly completed adoption.
3. Its single `spec.source` has the published SHA, expected repository, path and filename include filter; its destination is the configured local cluster/namespace. Multi-source or remote-cluster configuration is unsupported in this version.
4. `status.sync.comparedTo` matches the same relevant source/destination fields, and `status.sync.revision == lastPublishedRevision`.
5. `status.sync.status == "Synced"`.

Missing information is Unknown, a known mismatched revision/source or OutOfSync is False. Compare the explicit fields we control with ordinary default normalization, not every unrelated Argo default. Never use only `operationState.phase == Succeeded`, only the presence of a SHA, or an old sync history entry as success. An unchanged desired file may already be synchronized without a new operation.

These checks report Argo's latest observed comparison; they cannot prove the Application controller is currently running. Its raw `reconciledAt` remains available. Do not add a timestamp-based liveness oracle to this example.

### 4.2 Ready summary

Compute Ready with a small pure helper after merging current publication and observation data. Suggested precedence:

| Evidence | Ready value / reason |
|---|---|
| Pause annotation is exactly `"true"` | False / `ReconcilePaused` (including a pending deletion). |
| Deletion requested, not paused | False / `Deleting`; cleanup errors appear in message. |
| Desired generation not published | False / existing publication failure reason, or `PublishPending`. |
| Argo/target cannot be read or copied | Unknown / observation reason. |
| Current publication not synchronized | False or Unknown / `OutOfSync`, `RevisionPending`, or the appropriate observation reason. Use `Syncing` only when Argo reports a Running operation for that same pending revision; never use a historical operation to override the correlation checks. |
| Exact live object absent/deleting | False / `ResourceMissing` or `ResourceDeleting`. |
| Target has a Ready condition with an explicit mismatched observedGeneration | Unknown / `StaleResourceStatus`. |
| Target has an explicit Ready=False/Unknown | Respect that value; use its reason/message, bounded in the summary but complete in raw status. |
| Argo health is Degraded/Missing/Progressing/Suspended | False / preserve Argo's health name. |
| Argo health absent/Unknown/unrecognized | Unknown / `HealthUnknown`. |
| Synced, live object exists, Argo Healthy, no contradictory/stale target Ready evidence | True / `Healthy`. |

Only inspect a conventional target `Ready` condition when present; do not invent interpretations for arbitrary status fields or deploy a runtime adapter registry. If the producer omits observedGeneration, preserve/report its readiness as observed, without claiming generation freshness it did not supply. Malformed/ambiguous Ready conditions produce Unknown rather than a panic or a guessed True. A ConfigMap does not need a fabricated Ready condition: existence plus correlated Argo Synced/Healthy satisfies the summary.

For the optional real-XR example, configure Argo's health check for that exact XR kind to interpret its Ready condition. A status-bearing test CRD suffices for required automated tests; installing all of Crossplane is not required. Readiness of every kind is not universally defined. [S6]

Approval and review terminology is documentation-only for now: future `Approved` would represent approval, but emit no fake approval success or pending gate. Direct pushes are `Published`/`Pushed`, not `MergeRequested`/`Merged`, because there is no merge workflow.

A consumer can use current-generation Ready for the headline and raw nested status for detail. GitDrift can be True at the same time as Published/Synced/Ready: Argo may correctly run our pinned revision while branch HEAD has external edits.

## 5. Argo watch and exact-resource observation

Keep the existing Git publication and aggregate ApplicationSet reconciler. Add one small GitResource status observer in the same manager. Its queue key is the GitResource, even when an Application event triggered it.

Label/annotate generated Applications with wrapper namespace/name/UID and the managed ApplicationSet identity. Watch only managed Applications in the configured Argo namespace. Map events back to the wrapper, verifying UID; an event for a deleted/replaced CR must not update a new same-name CR. Also enqueue the observer on GitResource generation, relevant annotation, publication result, or application reference changes and on startup.

Application status events must not enter the Git publisher or trigger ApplicationSet list rewrites. Filter the publisher's own status updates. The observer has no Git client. Configure a 15-second periodic exact-resource check, with modest jitter, so a target-only status update is observed even when Argo emits no event. Do not depend on individual resource health being in Application.status.resources; Argo versions can store that health in the resource tree instead. [S3]

### 5.1 Resolve exactly one direct object

Persist `publishedResourceRef` from the manifest actually published, not from an unpublished new spec. It records apiVersion/kind/name and authored namespace; resolve omitted namespace during observation using API discovery and the Application destination. This avoids a Git fetch on status events and avoids treating a pending rename as already deployed.

Match that identity against `Application.status.resources` by group/kind/name/effective namespace. Use its declared served version and ordinary REST mapping for the GET; do not assume array index zero. Ignore hooks, previous identities awaiting pruning, generated namespaces, and unrelated entries. Zero matches is `NotTracked`, multiple ambiguous matches is an observation error; neither proves the object is absent. Resource inventory is a tracking summary, not the live object's full status. [S2]

GET only the matched resource with an uncached client/APIReader. Do not accidentally create informer caches for arbitrary GVKs through the default cached client. Use discovery to distinguish namespaced and cluster-scoped resources; never invent a namespace for cluster-scoped kinds. Retry discovery after a NoMatch when a CRD may have appeared later.

Record the target's UID, generation, optional deletionTimestamp, existence and raw status, plus `observedAgainstRevision` identifying the Argo comparison associated with this observation. This is correlation, not proof that a live resource records a Git SHA. Do not recurse into an XR's composed resources, Deployment ReplicaSets/Pods, or references in status. No second resource tree is built.

Observe only the local cluster. Grant explicit `get` RBAC for ConfigMaps and the test status-bearing CRD; document the extra read rule required for a real XR. Do not grant wildcard target write access or cluster-admin. Forbidden is a visible observation error, not missing/healthy. Lack of target read permission does not prevent publication.

### 5.2 Status writers and event-loop safety

The publisher owns publication fields and GitDrift; the observer owns the two snapshots. Retain the existing aggregate ApplicationSet ownership boundary. Use one small status-patch helper that re-reads the current CR, verifies UID/generation, changes only the caller's fields, recomputes Synced/Ready against the merged result, and uses optimistic concurrency. Retry conflicts from a fresh read.

The shared reducer prevents a newly recorded SHA from retaining an old same-generation Synced=True. Merge condition entries by type; never replace the entire condition list from a stale snapshot. Do not downgrade a newer successful publication using an old reconciliation result. Argo or Kubernetes status errors never retry an already successful Git push.

Retain ordinary leader election and isolated Git worktrees. No global lock or reduction to one publication worker is introduced.

## 6. Pause/resume

Use the project's own annotation name with Crossplane's mutation/finalization semantics: [S7]

```sh
kubectl annotate gitresource example-config -n demo \
  gitops.example.io/paused=true --overwrite

kubectl annotate gitresource example-config -n demo \
  gitops.example.io/paused-
```

Only the literal `"true"` pauses. Ensure the Git finalizer is installed on a non-deleting CR before any external effects, including when first observing a paused CR. A paused CR with our finalizer remains terminating after kubectl delete until resumed. A create/delete racing before finalizer installation has no external effects and may disappear normally.

Pause blocks new publish/adopt/orphan/delete writes, finalizer release, adoption-annotation consumption, and advancement/creation of that CR's ApplicationSet entry. Preserve an existing entry exactly while paused. Continue read-only Application/target observation and report ReconcilePaused; skip periodic Git drift work while paused. In-flight external operations cannot be rolled back; accurately record an already successful push but do not initiate another operation. Recheck pause before each new side effect/retry.

Pause does not pause Argo, Crossplane, or the deployed workload. Argo can finish an already started sync and maintain its last pin. Removing the annotation resumes from current desired state, including pending deletion. Annotation and deletionTimestamp changes must pass watch predicates; metadata.generation alone is insufficient.

## 7. File ownership, adoption and publication intent

### 7.1 Reserved annotations in the published manifest

The renderer controls these fields; supplied values cannot impersonate another owner:

```yaml
metadata:
  annotations:
    gitops.example.io/management-state: managed
    gitops.example.io/source: demo/example-config
    gitops.example.io/source-uid: "<GitResource-uid>"
```

Add the marker to the manifest only; do not rewrite the embedded spec to insert it. This slightly extends v0.1's serialization contract. The marker participates in deterministic published bytes/hash and is ordinary metadata applied by Argo. It is not an Argo tracking annotation or an authentication boundary. Keep the existing UID/generation/action commit trailers for attribution and recovery.

### 7.2 Basic create/update check

For a new CR without verified ownership, require the path to be absent, already marked with its own UID from a recoverable previous push, or explicitly authorized for adoption. Otherwise report `Published=False/PathAlreadyExists`; leave the existing file and Application unchanged.

Perform this check against the fetched branch on every rejected-push retry, not just before the first attempt. This makes two first-time writes to the same absent file converge to one owner without a distributed reservation service. Existing owners may update their path. If a file explicitly names a different owner UID, do not let the old owner automatically seize it back. Report ownership loss until explicit recovery; ordinary content-only external edits remain overwriteable on the next desired change.

A rejected/new CR that never owned a path must not delete or orphan that path when it is removed. Deletion may release its finalizer after confirming non-ownership, without modifying another owner's file. A true unavailable/ambiguous Git read is not that confirmation.

### 7.3 One-time adoption

```sh
kubectl annotate gitresource example-config -n demo \
  gitops.example.io/adopt-existing=true --overwrite
```

Adoption means **overwrite with this CR's supplied desired manifest and assign its UID**, not import Git contents into the CR. For a no-change recovery, the user should first supply the existing desired manifest.

Resolve a matching ApplicationSet entry by the exact immutable repository/branch/path. Reuse its existing Application name and namespace, including its old UID-derived suffix. Persist that reference before the first ownership-changing push. No match means a normal new Application identity; ambiguity is a clear error, not a guessed match. Reject takeover while the referenced old wrapper UID is still live, or while the existing Application is already terminating. This is a small safety check, not a general ownership service.

Persist our finalizer; stamp the new UID and `managed` marker (removing `orphaned`), push and verify, record publication, and let the ApplicationSet reconciler transfer that same entry to the new owner. Only after both the publication and entry ownership are confirmed, remove `adopt-existing` with a conflict-safe metadata patch. A restart must finish the same takeover without a second Application or duplicate commit. The annotation is consumed, not a permanent permission to overwrite arbitrary future owners.

A previously recognized ownership marker or a verified published commit is recovery evidence when a push succeeded but status persistence failed. Retain a narrow recovery helper that reads the relevant path/UID commit attribution if necessary; do not build a general history service. If ownership/publication cannot be proven, report RecoveryRequired rather than invent success or delete unowned state.

### 7.4 Desired change versus drift check

Do not use every reconcile event or every generation increment as a command to overwrite branch HEAD. Compare deterministic rendered desired content with the last successful publication hash. A new desired content hash, first creation, or explicit adoption permits publication. Message-only/policy-only changes may update observed generation without a content commit, preserving any drift warning and the last SHA.

A no-op snapshot/status event is not publication intent. A manual branch edit must remain in place until a real desired-manifest change or explicit takeover, rather than being repaired by the next timer.

For a v0.1 upgrade, do not automatically claim arbitrary markerless paths or rename existing Applications. Use verified existing publication records and existing entries to establish continuity; require `adopt-existing` when continuity is ambiguous. Include an upgrade/recovery note and test one existing v0.1-style record.

## 8. Informational Git drift

Every 30 seconds by default, while not paused/deleting, compare the current managed path with `lastPublishedContentHash`. Reuse a fetch performed for an actual publication in that cycle; do not add a fetch to every Argo event.

Changed content or a missing file sets GitDrift=True with `ExternalModification` or `FileMissing`. Equal content sets False. Failed access produces Unknown with the error, not False. Compare only the managed file; another CR changing another path is not drift. This is a current observation, not guaranteed detection of every transient edit.

Detection never commits, restores content, changes the publication SHA, or rewrites the ApplicationSet revision. Published/Synced/Ready may remain True for the pinned revision. A later real manifest change is published normally on current HEAD and can overwrite the external file edit. Clear the warning after verification. Keep normal branch-race retries and preservation of unrelated files.

## 9. Deletion policies and durable handoff

Allow `spec.deletionPolicy: Delete | Orphan`, default Delete. Do not add additional policy values. Treat pause before either path. Select the policy when cleanup starts; once an orphan commit or deletion commit exists, complete that selected operation rather than oscillating because of a later policy edit. Persist `status.cleanup.policy` before the first cleanup effect and add `status.cleanup.revision` after verifying the corresponding Git commit. These are the minimal restart checkpoints, not an operation CR, phase machine or workflow log. Policy changes after that checkpoint are not applied to the in-flight cleanup.

### 9.1 Delete

Verify current ownership, delete only the file, push normally, and confirm remote absence. Then release our finalizer without waiting for ApplicationSet or Argo. The aggregate inventory removes the non-orphan entry after the CR disappears, and Argo performs its configured cascading deletion. A file already absent is a no-op success. An unowned existing file is never deleted by a CR rejected during creation/adoption.

### 9.2 Orphan

1. Recover/verify the last successful publication for this CR. Do not publish an uncommitted new spec merely because deletion was requested. A CR that truly never published or owned anything can finish without creating an orphan artifact.
2. Read this file at that verified publication, set `management-state: orphaned`, retain the original source identity, and commit those bytes on current branch HEAD. Preserve unrelated paths. If external edits exist, the retained file is deliberately restored to the last controller-published content plus the marker; this prevents orphaning from deploying unreviewed branch drift.
3. Verify the marker/content remotely, then store the small orphan publication checkpoint, including its SHA. Repeated retries reuse an already verified identical orphan commit.
4. The ApplicationSet reconciler retains the same element/Application identity, changes its pin to the orphan SHA, and adds a string-valued retention marker to the list element. Preserve repository, path, filename and destination.
5. Re-read the ApplicationSet uncached and verify that the correct element is retained/orphaned at that SHA. Only then release our finalizer.

Do not wait for Application synchronization, target health, or for the live object to display the new annotation. Those are asynchronous. If Argo's controllers are down but the ApplicationSet API update is verified, finalization can finish. If the ApplicationSet is missing or cannot be durably updated, Orphan must wait; otherwise its promise to keep Argo management would be false. [S8]

The exact target UID is not promised to survive every possible Argo sync strategy; use the sample's ordinary non-forcing apply behavior. Orphan keeps the desired resource managed and does not request deletion/replacement. Do not add force/replace options.

### 9.3 Retained entries and races

Store a retained entry in the existing List generator, not a separate registry. A representative entry adds `managementState: orphaned`, immutable destination identity, original source namespace/name/UID and the verified revision. Include identity/marker metadata on the generated Application for discovery, but do not give GitResource an ownerReference to the Application.

The aggregate reconcile merges live publications with existing explicitly orphaned entries. A missing CR does not remove an orphaned entry. The orphan marker is sticky: a stale cached list or a terminating old owner cannot reset it or revert its revision. Only a verified explicit adoption into a new UID makes it managed again. Read the latest ApplicationSet and use optimistic concurrency on updates; never treat a failed resource LIST as an empty inventory.

After finalization, our controller preserves the retained record but no longer publishes, polls Git, advances its SHA, or aggregates status for it. Argo/ApplicationSet continue managing the Application. Removing the retained entry manually may still trigger Argo cascading deletion: document this. Orphaning is not detaching the Application from its ApplicationSet. Deleting/replacing the ApplicationSet itself is outside this retention guarantee; back up the existing entries as part of normal cluster-state backup. [S8]

This preserves v0.1's two-reconciler handoff boundary: Git workers publish; the aggregate reconciler owns list entries. No parallel writer replaces the entire list from a stale snapshot.

## 10. ClusterGitConfig validation and evidence

Validate on use without adding a repository-probing subsystem: Secret existence, namespace restriction, expected keys, nonempty values and usable commit identity. Add a small config status with observedGeneration, the resolved Secret UID/resourceVersion, a standard Ready condition meaning **configuration loaded**, and one last-access observation from an actual fetch/push.

Use the following config status field names; omit `lastAccess` until an actual Git operation supplies evidence:

```yaml
status:
  observedGeneration: 2
  credentialsRef:
    namespace: git-state-system
    name: forgejo-writer
    uid: "<Secret-uid>"
    resourceVersion: "<Secret-version>"
  conditions:
    - type: Ready
      status: "True"
      observedGeneration: 2
      reason: CredentialsLoaded
      message: Secret loaded; repository-specific access is reported separately.
      lastTransitionTime: "2026-10-06T12:00:00Z"
  lastAccess:
    repositoryURL: http://forgejo.forgejo.svc.cluster.local:3000/demo/resources.git
    operation: Push
    success: true
    reason: Succeeded
    message: ""
    configGeneration: 2
    secretUID: "<Secret-uid>"
    secretResourceVersion: "<Secret-version>"
    lastUpdatedAt: "2026-10-06T12:00:00Z"
```

The access observation records repository URL, operation, outcome, a sanitized error message, config generation and Secret identity/version. Clearly state its scope: success on one repository does not establish access to every URL a CR might request. A successful read does not prove push permission; record successful real pushes, never create probe commits. Historical success with old credentials is not current verification after rotation.

Keep one last-access record, not a growing per-repository history. Patch only when the evidence/outcome/credential version changes, not on every identical successful poll. Re-read the current config/Secret identity before recording a completed request so a request using old credentials cannot certify a new Secret. Concurrent results are last reported evidence, not a globally ordered audit trail.

Do not block unrelated successful CRs because another repository denied access. A missing/invalid config fails affected CR publication as before. Secret/config edits enqueue affected publishers; config status-only edits do not. Argo credentials remain the explicitly configured separate Secret from v0.1; automatic cross-namespace credential distribution is still out of scope. Tests can rotate both local fixture credentials deliberately.

## 11. History documentation

Add `docs/git-history.md` (provided alongside this spec). Record that Git or an external Git-host/read API is responsible for history, diffs, audit views and restore previews. Our controller supplies repository, branch, path and last publication SHA; it does not expose a history endpoint, cache lists of commits in status, create audit files, or implement rollback workflows.

Explain path-scoped history, branch-head SHA versus file-changing commit SHA versus blob SHA, sufficient clone depth, and why a Git-only revert does not advance the SHA-pinned Application. Restoring old contents through GitResource.spec.manifest creates an ordinary new publication. Copying Argo's own bounded upstream history as part of its status snapshot does not turn it into a Git audit service. [S9][S10]

## 12. Tests and modest timing measurements

Keep existing unit/envtest/Git integration/k3d tests. Use table-driven tests for pure status decisions and comparisons; use real API-server tests for unknown JSON preservation, conditions, predicates and conflicts. No broad mock-only claim of Git correctness. Narrow injectable failure seams are acceptable; no runtime fault-injection framework.

| Case | Required result |
|---|---|
| Application snapshot round trip | Preserve unknown nested fields, upstream condition formats, operation errors and per-resource failure messages; remove keys when upstream removes them. |
| Old revision / old comparison / old success | Never set Synced/Ready=True for the new publication from an old SHA or a historical Succeeded operation. Include a same-generation SHA change. |
| Synced without a new operation | Valid matched comparison can become Synced even without a new successful operation record. |
| Statusless ConfigMap | Correct inventory match, exists=true, no manufactured status/Ready condition, wrapper can reach Ready from correlated Argo Synced/Healthy. |
| Status-bearing test CRD | Mirror Ready and arbitrary outputs; handle False/Unknown and explicit stale observedGeneration without new type-specific controller code. |
| Target-only change | Poll observes raw target status changes even when Application emits no update. |
| Wrong inventory / namespace / scope | Never GET the first arbitrary child or apply namespace to cluster-scoped objects; no match is NotTracked. |
| Missing / forbidden / replacement | Distinguish NotFound from Forbidden, clear stale snapshots, replace UID correctly, recover after permission/resource restoration. |
| Large snapshots | Omit with visible SnapshotTooLarge; no silent truncation; Git publication and finalization still work; recover after size decreases. |
| No-op observation / event isolation | Repeated identical observations do not update resourceVersion; source status updates cause zero Git calls and no unrelated ApplicationSet list rewrite. |
| Concurrent status writers | Publication and observer patches preserve each other's fields and do not regress generation/SHA/conditions. |
| Pause / resume / paused deletion | No new external effects while paused; status readable; finalizer waits until annotation removal; existing Application entry retained. |
| Existing-file collision | No annotation means refusal; simultaneous first writes to same path leave one owner; rejected CR deletion leaves that owner's file intact. |
| One-time adoption | Correct supplied content and UID, marker managed, same retained Application identity, consumed annotation, later ordinary updates work. |
| Adoption safety | Live old owner or terminating Application prevents unsafe handoff; old owner cannot silently overwrite new ownership. |
| Informational drift | Branch edit/deletion changes only warning; no automatic commit/pin change; later manifest change overwrites it; unrelated file commits are not drift. |
| Delete | Verified Git absence releases CR before Argo completion; controller restart still removes non-retained entry. |
| Orphan | Git marker and retained entry verified before CR disappears; same Application and ConfigMap survive; pending unpublished changes/external drift are not deployed accidentally. |
| Orphan/re-adopt | Same Application identity across a new CR UID, no transient entry removal or cascading workload deletion. |
| Orphan races / dependencies down | Sticky entry survives stale inventory/restarts; missing AppSet API holds orphan finalization; stopped Argo controller alone does not. |
| Credential diagnostics | Bad keys, denied read, denied push and recovery distinguish outcomes; old Secret version never certifies rotated credentials. |
| Upgrade continuity | Existing v0.1 publication/entry keeps its Application identity; ambiguous markerless ownership needs explicit adoption. |

### 12.1 Required recovery scenarios

**Rejected push:** force two different-file writers to start at the same branch head. The first pushes; the second receives a non-fast-forward rejection, fetches the new head, reapplies only its file, and retries. Both files survive, no force-push or unrelated overwrite. Keep the existing bounded-attempt/backoff design.

**Restart after external success:** stop/fail immediately after a successful push but before status persistence. Restart; recover ownership/publication without a duplicate content commit. Repeat at the orphan marker checkpoint and between retained-entry update and finalizer release. Test adoption interruption before annotation consumption. No exactly-once transaction is promised; observed state makes retries idempotent.

**Asynchronous deletion:** allow Git cleanup and CR removal while Argo is unavailable, then restore Argo and verify eventual ordinary Delete cleanup. Under Orphan, restart the manager and Argo and verify retention rather than deletion.

### 12.2 Timing, not a benchmark platform

Run at least 20 distinct GitResources against one repository/branch with four publication workers. Capture per-case create/update-to-publication time, actual Git operation durations/retries, publication-to-observed-sync time, and CR-removal versus downstream-deletion time. Report count, throughput, p50/p95/max and raw per-case results in ignored test artifacts. Include snapshot sizes and status-write counts to catch observer churn.

Use at most a few low-cardinality Prometheus metrics on the existing endpoint: Git operation duration by operation/outcome, rejected-push retries, and observation outcome/status-patch counts. Do not label metrics with CR names, repository URLs, paths, SHAs or UIDs. Existing controller-runtime workqueue/reconcile metrics remain useful.

Tests enforce correctness and bounded deadlines, not hardware-specific millisecond promises. Timings are measurements from that run; no untested scale claim. A 15-second poll implies roughly N/15 target GETs per second in steady state, before event-triggered reads. Status/watch bandwidth scales with full object size and update rate. The existing single ApplicationSet list also grows with active plus retained entries; keep that known limit rather than adding sharding now.

## 13. Implementation shape and delivery

Use ordinary concrete helpers under `internal/`; illustrative changes are:

```text
api/v1alpha1/                 # extend existing types and markers
internal/controller/
  gitresource_controller.go  # publication, ownership, pause, finalization
  applicationset_controller.go # existing inventory + retained entries/adoption
  status_controller.go       # Application watch and one-target poll
  status.go                  # pure summary and narrow conflict-safe patch helpers
internal/git/                # extend existing publish/delete/read/recovery helpers
internal/manifest/           # existing rendering + reserved ownership annotations
docs/
  status.md                  # snapshot/readiness/size/security contract
  lifecycle.md               # pause, Delete, Orphan, adoption examples
  git-history.md             # explicitly delegated history responsibility
test/                        # extend existing suites; no replacement harness
```

Keep each feature in the existing layer that owns it. No generic Observer/Driver/Workflow registries, public packages, unnecessary service/repository interfaces, dynamic-watch manager, or second Argo client stack. A small interface only at an external boundary where tests need it is acceptable. Use standard error wrapping, context deadlines, typed references and normal generated RBAC/CRD code. Regenerate rather than editing generated YAML by hand.

Implementation order: (1) raw mirror schema and size/patch helpers with tests; (2) Application watch/one-target read and summary; (3) reserved markers, adoption and informational drift; (4) pause and orphan retention; (5) config evidence, recovery E2E, docs and timing output. Preserve the four commands and existing cluster isolation throughout.

Completion means `make up`, `make test`, `make dev`, and `make clean` still work from a clean checkout; the new tests actually pass; generated files/lint are clean; and README examples demonstrate snapshot errors, ConfigMap/no-status, pause/resume, orphan/re-adopt and history lookup. Record the tested version inventory. Do not claim the implementation has passed before running it.

## 14. Reference notes

These sources establish upstream behavior. The limits, envelope fields and summary rules above are design choices for this example.

- **S1 — Kubernetes API conventions:** standard conditions, Ready summary, reasons versus phase enums, and large/high-churn status guidance. https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md
- **S2 — Argo Application types:** ApplicationStatus, resource inventory, sync comparison and operation/result messages. https://github.com/argoproj/argo-cd/blob/master/pkg/apis/application/v1alpha1/types.go
- **S3 — Argo 3.0 upgrade notes:** individual resource health can be stored outside the Application CR. https://argo-cd.readthedocs.io/en/stable/operator-manual/upgrading/2.14-3.0/
- **S4 — Kubernetes CRD schemas:** pruning, preserved unknown subtrees, embedded resources and status subresource. https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/
- **S5 — etcd limits:** request-size limits are configurable; large requests affect latency. https://etcd.io/docs/v3.6/dev-guide/limit/
- **S6 — Argo resource health:** resource-specific checks and aggregation. https://argo-cd.readthedocs.io/en/stable/operator-manual/health/
- **S7 — Crossplane managed resources:** pause annotation and paused deletion semantics. https://docs.crossplane.io/latest/managed-resources/managed-resources/
- **S8 — ApplicationSet deletion:** owner references and cascading resource deletion. https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Application-Deletion/
- **S9 — Git log:** path-scoped history and revision filtering. https://git-scm.com/docs/git-log
- **S10 — go-git LogOptions:** file/path filtering for on-demand Git access. https://pkg.go.dev/github.com/go-git/go-git/v5#LogOptions
