# git-state-controller — SPEC v0.3

**Date:** 2026-10-06

**Status:** implementation specification for review; not implemented or tested by producing this document.

**Baseline:** the existing v0.2 controller and development stack. This is an incremental change, not a rewrite.

**Delivery of this document:** local Markdown only. Repository edits, branches, commits, generated code, and the example application are not part of this specification-writing step.

## 1. Objective and decisions

Add a small upstream-application contract: optional per-change Git authorship, namespace-controlled approval before publication, visible and approval-aware deletion, and a local Vite demonstration app.

The controller must not care whether a person, a CLI, or another application submits a GitResource. It executes the same Kubernetes contract. Authentication, authorization of the human approver, and verification of author identity remain responsibilities of the upstream application and cluster administrator.

| Area | v0.3 decision |
|---|---|
| Change identity | No user-supplied `change.id`, request CR, UUID, or new revision counter. |
| Attribution | Optional `spec.change.author` beside the existing message; configured identity is the fallback. Same existing Git credentials. |
| One-shot metadata | Consume the completed apply's `spec.change` safely, so author/message do not become permanent defaults for future changes. |
| Approval mechanism | One trusted, request-bound annotation. No signing, admission policy, or approver-specific integration. |
| Approval policy | `gitops.example.io/approval-required` on the GitResource's Namespace. |
| Approval timing | Before Git publication or cleanup. Pending content stays in the CR; the existing Argo pin stays unchanged. |
| Approval lifetime | No expiry, revocation service, approval history, or periodic reauthorization. New requested mutations need matching evidence. |
| Normal deletion | `spec.change.action: Delete`; after approval, the controller issues Kubernetes DELETE and uses the normal finalizer path. |
| Raw Kubernetes DELETE | Also supported. Required approval holds external cleanup; absent explicit delete metadata, use the bot and default deletion message. |
| Delete completion | Under `deletionPolicy: Delete`, retain the wrapper until Git cleanup, ApplicationSet entry removal, Application deletion, and direct resource deletion are verified. |
| Orphan completion | Preserve Git, the existing Application, and its resource as in v0.2; do not wait for those retained objects to disappear. |
| Namespace discovery for UI | Only offer Namespaces annotated `gitops.example.io/enabled: "true"`. This is UI discovery, not a new controller authorization boundary. |
| Example app | Specify `examples/approval-ui/`: a small Vite app using the Kubernetes contract, not another workflow backend. |
| Links/history | No new links, backlinks, correlation fields, or history API. Keep v0.2's documentation boundary. |

Keep one embedded resource and one immutable repository/branch/path per CR, direct go-git pushes, existing authentication, concurrent publication, the existing shared ApplicationSet, raw status mirrors, and the four-command local workflow. Do not add PR/MR support, new Git authentication, extra CRDs, a database, an event ledger, a public SDK, dynamic target watches, or a workflow engine.

### 1.1 Two intentional changes to v0.2

The previously inspected v0.2 publication code used `change.message` but did not consume the `change` block. Do not assume author cleanup already exists. v0.3 explicitly introduces and tests that behavior.

v0.2 released the Delete finalizer once Git removal succeeded, then removed the ApplicationSet entry after the CR disappeared. **v0.3 deliberately changes this:** remove the entry after Git cleanup while the CR is still terminating, then wait for downstream deletion. Keeping the old inventory rule would deadlock this new lifecycle.

## 2. API changes

### 2.1 Optional change metadata

```yaml
spec:
  change:                    # Entire block remains optional.
    message: Increase capacity
    author:                  # Optional; omit both fields to use the configured bot.
      name: Alex Developer
      email: alex@example.com
    # action is omitted for normal publication.
```

| Field | Contract |
|---|---|
| `change.message` | Existing optional message and length limit. Empty uses the existing action-specific default. |
| `change.author` | Optional object. When supplied, require a usable nonempty name and email together. |
| `change.action` | Optional enum `Apply | Delete`. Omitted means Apply unless Kubernetes deletion is already requested. |

Do not insert an empty `change` block or an explicit Apply default into objects that omitted it. Use an optional representation that can actually be removed. Default the effective action in controller logic rather than continually recreating a consumed block.

Retain the current API group/version. Regenerate CRDs, deepcopy code and RBAC from their source markers. Do not hand-edit generated manifests.

### 2.2 Commit identity

Resolve identity independently for every operation:

| Commit property | Value |
|---|---|
| Author | Supplied `change.author`, otherwise the current ClusterGitConfig identity/default. |
| Committer | Existing ClusterGitConfig identity/default, explicitly supplied to go-git. |
| Transport credentials | Existing resolved Secret; never taken from author fields. |

go-git exposes author and committer separately; omitting the committer makes it default to the author. Set both explicitly so an override does not silently make the requesting person the automated committer. [R1]

Use basic validation and bounded strings, not an identity-verification subsystem. Reject line breaks and malformed identity/header input. Do not query Git hosting accounts or require the email to match a hosting-platform profile. Upstream callers own identity validation. Keep existing generated timestamps and UID/generation commit trailers; add no Change-ID trailer.

Capture content, message, and author from the same CR observation. A retry must never combine one generation's manifest with a different generation's author. Do not modify shared repository/global Git identity configuration to implement an override.

For Delete/Orphan, use explicit delete metadata only when `change.action` is Delete. A raw Kubernetes DELETE with a leftover Apply message/author must use the bot and the default cleanup message, not falsely attribute deletion to the previous editor. Distinguish Delete and Orphan in default messages.

### 2.3 Approval status

Add one small envelope and one condition; reuse the existing status-patch/summary helpers:

```yaml
status:
  approval:
    required: true
    generation: 7
    operation: Apply
    request: "v1:<GitResource-uid>:7:Apply"
  conditions:
    - type: Approved
      status: "False"
      reason: ApprovalPending
      observedGeneration: 7
      message: Namespace requires approval for this publication.
      lastTransitionTime: "2026-10-06T12:00:00Z"
```

`request`, `generation`, and `operation` describe a pending mutation. Omit them when there is no new mutation to authorize. `required` is the last observed namespace policy; omit it when the policy could not be resolved rather than representing an error as false. The existing conditions hold errors.

This envelope is neither a signed token nor an audit record. There is one current approval request, not an array. Do not store approver identities that the controller cannot verify, proof tokens in Git, or a timestamp heartbeat.

## 3. Namespace annotations

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: demo-approved
  annotations:
    gitops.example.io/enabled: "true"
    gitops.example.io/approval-required: "true"
```

### 3.1 Approval policy

| Annotation value | Meaning |
|---|---|
| `"true"` | Require matching approval for a new Apply, Adopt, Delete, or Orphan operation. |
| `"false"` or absent | Use the existing automatic flow. |
| Any other value | Invalid policy: block new side effects and report `InvalidApprovalPolicy`. |
| Namespace lookup fails | Policy is unknown: block new side effects and report the read failure. |

The controller reads this policy for every GitResource regardless of who created it. There is no per-CR switch to bypass a required namespace policy, and no approval setting in ClusterGitConfig in this version.

Watch Namespace annotation changes and enqueue GitResources in that namespace. Reuse a simple namespaced list; no global ordering mechanism or policy framework. Re-read policy and current intent before starting a not-yet-executed operation, including its rejected-push retries. Completion of an already accepted cleanup follows its durable context without seeking a new approval; pause is still checked before further mutations. Namespace status-only updates need not enqueue publishers.

Turning approval off allows pending work to proceed. Turning it on gates work that has not started; it does not roll back, unpublish, or demand approval for an already deployed unchanged resource. A started cleanup operation finishes its accepted operation; policy toggles are not a cancellation mechanism. Kubernetes and Git cannot be changed atomically, so a last-moment concurrent policy update cannot undo an already-started push.

### 3.2 Enabled namespace annotation

`gitops.example.io/enabled: "true"` makes a Namespace selectable in the example app. Only the literal value `"true"` qualifies; exclude terminating Namespaces from new-request selection.

This annotation is not another publisher gate. Removing it hides the Namespace from the demo but must not abandon existing CRs, delete Applications, disable approval enforcement, or strand finalizers. Direct API clients remain governed by Kubernetes permissions and the namespace approval policy.

The demo does not offer Namespace deletion. Deleting a Namespace or the cluster is outside this per-GitResource approval guarantee: Kubernetes can delete resources independently of this controller.

### 3.3 Trust boundary

A client authorized to edit a GitResource can also set the approval annotation unless other controls forbid that. A client authorized to modify the Namespace can disable its approval requirement. Ordinary RBAC does not make one particular annotation approver-only. [R2]

That is accepted for this trusted example. The annotation is coordination evidence, not cryptographic authorization or proof of separation of duties. Document this without adding login, admission rules, signing keys, or another approval resource.

## 4. Request-bound approval without a request ID or signing

### 4.1 One deterministic request value

Use a controller-derived value:

```text
v1:<GitResource UID>:<metadata.generation>:<effective operation>
```

Effective operations are only:

| Operation | Selection |
|---|---|
| `Apply` | New create/update publication. |
| `Adopt` | The existing `adopt-existing: "true"` annotation requests takeover. |
| `Delete` | Kubernetes deletion or `change.action: Delete`, with effective policy Delete. |
| `Orphan` | Kubernetes deletion or `change.action: Delete`, with effective policy Orphan. |

Deletion intent takes precedence over adoption and publication. The immutable CR UID prevents using evidence from a same-name replacement. The generation identifies the spec version, including author, message, manifest, config reference and deletion policy. Operation distinguishes a raw DELETE or annotation-driven adoption from an earlier Apply approval even when generation did not change.

With the CRD status subresource enabled, spec changes advance generation while metadata/status changes do not. This is why generation alone is insufficient for adoption/raw DELETE but generation plus operation is adequate for this scoped, trusted contract. [R3]

Do not introduce a separate UUID, counter, content-signing protocol, request-hash service, or client-side canonicalization routine. The client copies the advertised request string as an opaque value. The controller recomputes it from the current object; it must not trust a stale `status.approval.request` when deciding to execute.

Changing a spec revision invalidates a pending approval even if someone changes the content back later. Pause/resume does not invalidate the approved intent. Unrelated commits to other files and source-status changes do not invalidate it. This approval is not tied to the shared Git branch head.

### 4.2 Approval annotation

```yaml
metadata:
  annotations:
    gitops.example.io/approved-request: "v1:<GitResource-uid>:7:Apply"
```

The only initial verification is equality with the freshly computed request. `approved: "true"` is not supported. There is no expiry clock or revocation endpoint.

A caller should read the CR, present that exact manifest/action and request to its approver, then patch the annotation with UID/resourceVersion preconditions. An outdated approval should fail or remain nonmatching; never automatically approve the newer request after a conflict. Kubernetes supports conditional updates and reports conflicting versions rather than silently resolving their intent. [R4]

Removing an annotation before the controller accepts it can mean approval was never received. There is no promise of cancellation by deleting evidence after execution starts. Once an authorized side effect succeeds, recover its result and complete its required handoff; do not roll it back because the token was subsequently removed.

### 4.3 Gate placement

The gate protects new Git writes, adoption, deletion initiation via `change.action`, and cleanup. It does not block finalizer installation, status reporting, read-only validation, or observation of the existing deployment.

```text
Read current CR and Namespace
    → ensure our finalizer before external effects
    → honor pause
    → finish a verified completed operation's bookkeeping, if applicable
    → identify the next real mutation
    → publish its approval request / evaluate namespace policy
    → wait, or run the existing operation
```

If required evidence is missing or mismatched: do not commit/push, change ownership, remove Git content, initiate Kubernetes DELETE on behalf of a prepared request, or move/remove its ApplicationSet entry. Keep the last successful publication and deployment intact.

Re-evaluate current UID, generation, operation, approval and pause before a new push attempt. If a rejected push retry picks up a newer CR generation, it needs that newer generation's approval; never use the old evidence with new bytes.

The aggregate ApplicationSet reconciler uses only verified publication/cleanup records, not an unapproved current spec or branch head. Once Git success is durably recorded, its SHA handoff is completion of that authorized operation, not another approval request. Existing pause semantics can still hold a new handoff until resumed.

### 4.4 No-op behavior

Approval is for new external mutations, not every event or metadata cleanup. Retain v0.2's deterministic desired-content comparison and drift behavior.

If the current desired file is already the verified published content and no adoption/delete is requested, do not create a commit or seek another approval merely for a message-only edit, author-only edit, empty block, or controller consumption of the old block. Acknowledge and consume one-shot metadata safely. Preserve any external-drift warning and the previously published SHA.

Initial creation/adoption still uses existing ownership checks and approval when needed; identical bytes in an unowned file are not permission to take it over.

While one request is awaiting approval, newer edits simply replace the requested desired state and produce a new approval value. This remains a level-based controller, not a queue guaranteeing a commit for every intermediate edit.

## 5. Safely consuming `spec.change`

### 5.1 Successful Apply or acknowledged no-op

Treat message/author/Apply metadata as one-shot input. After successful remote publication verification, persist the existing publication status first. Then remove `spec.change` and the matching approval annotation with a conditional metadata/spec patch.

Consumption must verify all of the following against a fresh object:

1. The CR UID still matches.
2. Its generation is the generation whose change was completed; the object is not newly deleting or paused.
3. Its change block still matches the captured input.
4. The approval annotation is removed only if it is the exact value used for that operation.

Use optimistic concurrency and preserve unrelated annotations/spec fields. A newer user edit wins: skip consumption rather than erase or partially consume its author/message. Retrying a status/consumption conflict must not rerun a successful Git commit.

Adoption already has a completion handoff in v0.2. Consume adoption, matching approval, and change metadata only after that same ownership transfer is verified. Remove only the annotations belonging to the completed operation.

### 5.2 Generation changes caused by consumption

Removing `spec.change` is a spec edit and therefore increments generation. The following reconciliation must recognize that the desired published content has not changed, acknowledge the new generation with the same SHA, and require no new approval or commit.

Do not label that housekeeping generation as a new human-approved publication. Its approval condition can be `True/NoChanges`, meaning there is no new mutation waiting for approval. Raw deployment status continues to refer to the original publication.

This behavior needs one focused test: approved publication → consumption → generation increment → steady state, with exactly one content commit and no second approval prompt.

### 5.3 Failure, restart and concurrent writers

Do not consume a failed or pending change. If a push succeeds and status persistence fails, retain the change and approval evidence and use the existing Git ownership/commit recovery path after restart. If status succeeds and consumption fails, retry consumption without recommitting.

If a newer request arrives before consumption, leave its block untouched even when it reuses the same author/message values. Clients should submit the manifest and a fresh change block together; the example editor starts each new edit with blank author/message inputs. The controller cannot infer a different intended author from omitted changes to a still-pending block.

`change` is intentionally a consumable request input, unlike the durable manifest. An upstream declarative reconciler must not endlessly reassert a completed change block. Document this small API contract rather than introducing request objects to solve it.

Do not clear `change.action: Delete` during a prepared or active deletion. It must not revert to Apply while cleanup is pending. Once the wrapper is finally deleted, there is no block to consume.

## 6. Deletion: one cleanup path with two entry points

### 6.1 Normal app flow: `change.action: Delete`

```yaml
spec:
  deletionPolicy: Delete       # Or Orphan.
  change:
    action: Delete
    message: Retire the example
    author:
      name: Jamie Operator
      email: jamie@example.com
```

This is an instruction to the controller, not a requirement for the app to run a second workflow:

```text
Submit action Delete
    → request deletion approval when the Namespace requires it
    → controller issues Kubernetes DELETE once approved/not required
    → ordinary finalization performs Delete or Orphan
```

Before issuing DELETE, persist the accepted cleanup context and recheck current intent/approval. Use object UID/resourceVersion preconditions. The controller needs the Kubernetes `delete` verb on GitResources, not permission to delete arbitrary target objects.

A pending request can be canceled by removing/changing `change.action` before Kubernetes accepts DELETE. Do not promise a cancellation window after approval; the controller may act immediately. After deletionTimestamp is set, Kubernetes deletion is not reversible. [R5]

### 6.2 Raw Kubernetes DELETE

A normal DELETE from kubectl, another app, or garbage collection still enters the same approval-aware finalizer path.

If approval is required, keep the CR terminating with `Approved=False/ApprovalPending` and preserve Git and the ApplicationSet entry until the exact Delete/Orphan request is approved. An old Apply approval is not a delete approval. No spec edit is needed to expose the deletion request: the effective operation changes when deletionTimestamp appears.

Without an explicit `change.action: Delete`, use the configured bot and default Delete/Orphan commit message. Do not infer the deleting user's identity from Kubernetes metadata or borrow the previous Apply author. With no required approval, proceed automatically.

A CR deleted before our finalizer was ever installed can disappear, but the controller must not have created any external state for it. Do not add an admission webhook just to eliminate that standard creation/deletion race.

### 6.3 Small, durable cleanup context

Extend the existing cleanup checkpoint instead of adding another CR or workflow phase. Capture the accepted operation before the first irreversible effect. It must retain enough data to resume the same cleanup despite restart or later edits:

- Existing policy and eventual verified Git revision.
- The controller-derived accepted request value and effective commit author/message.
- The configuration reference used for cleanup; resolve its current Secret normally, never store credentials in status.
- The existing Application identity and exact last published target reference, including observed UIDs when available.

Use the existing `status.applicationRef` as the stable Application name/namespace, and keep it fixed throughout cleanup. A concrete checkpoint shape is:

```yaml
status:
  cleanup:
    policy: Delete
    request: "v1:<GitResource-uid>:8:Delete"
    gitConfigRef:
      kind: ClusterGitConfig
      name: default
    author:                         # Resolved effective identity, including bot fallback.
      name: Jamie Operator
      email: jamie@example.com
    message: Retire the example     # Captured effective commit message.
    applicationUID: "<observed-uid>" # Omit if no Application has been observed.
    resourceRef:                    # The published target, not an unpublished proposal.
      apiVersion: v1
      kind: ConfigMap
      namespace: demo-approved
      name: example-config
      uid: "<observed-target-uid>"  # Optional until observed.
    revision: "<verified-git-sha>"  # Absent until the Git step completes.
```

These are one operation's bounded recovery facts, not a history or phase machine. Do not duplicate raw source status or secrets in this checkpoint. Freeze the selected policy and attribution after deletion/cleanup starts. Do not continually recalculate them from an edited spec.

If a prepared checkpoint exists but DELETE has not been accepted and no external cleanup has occurred, a changed/canceled request may discard that unused checkpoint. Recheck current intent before DELETE; an unused checkpoint is not permission to bypass the gate.

For raw deletion, do not establish an authorized cleanup context merely because deletionTimestamp exists. Establish it only after approval/not-required evaluation. Once an approved cleanup is in progress, finish that operation; namespace toggles and later spec changes do not restart it as a different request. Pause still holds new mutations as in v0.2.

### 6.4 Delete completion now waits for downstream cleanup

Under `deletionPolicy: Delete`:

```text
Verify ownership and approval
    → remove only this file in Git and verify remote absence
    → persist Git cleanup completion
    → inventory reconciler removes this CR's ApplicationSet entry
       EVEN THOUGH the GitResource still exists and is terminating
    → Argo/ApplicationSet remove the generated Application and its resource
    → verify entry absent, Application absent, direct resource absent
    → remove our finalizer
```

Reuse the existing `cleanup.revision` as a durable indication of completed Git cleanup where possible. A verified already-absent path may use the verified branch revision without an empty commit. A never-owned/no-external-state object is a distinct safe no-op; it must not remove another owner's file, entry or resource.

The inventory reconciler must exclude a Delete cleanup whose Git step is complete while retaining a pending/unapproved deletion whose Git cleanup has not completed. It must not put a removed entry back because it sees the still-terminating CR in a later list. This exclusion must be recomputable from the stored checkpoint after restart.

Use the sample's existing Argo cascading resource-deletion behavior. Argo owns deletion of the Application's workload; this controller only requests entry removal and observes completion. [R6]

**Do not directly delete the workload, strip its finalizers, or add a timeout that releases our finalizer while cleanup is unverified.** Argo outages, a blocked workload finalizer, missing read permissions or API failures keep our CR visible with a useful status. Resume normally when the dependency recovers.

### 6.5 Verify absence without depending on a vanished Application

After entry removal, Application inventory may no longer be available. Continue GETs using the stored Application and published target references, not the current desired manifest and not a missing `status.resources` list.

Only a successful authoritative NotFound establishes absence. Forbidden, NoMatch/discovery failure, timeouts and read errors are not absence. If an expected resource reference cannot be recovered, report the limitation and hold rather than claiming successful deletion. A never-published CR with proven no owned external state may complete without a target read.

Handle UID replacement explicitly. Never delete a same-name replacement or claim it was the object originally cleaned up. If an unexpected replacement is still at the managed identity, hold with an identity-conflict diagnostic for operator recovery instead of expanding into takeover/deletion logic.

While an object is terminating, preserve its raw status and available error messages. Once it is authoritatively absent, clear the old raw snapshot, report absence, and retain its reference for diagnostics until wrapper removal. Expected absence during cleanup is not an observer failure that supersedes the deletion message.

The guarantee covers the Argo Application and the one directly managed Kubernetes object. It is not an independent inventory of all Pods, Crossplane managed resources, or external cloud assets. Their controllers/finalizers define their own cleanup completion.

### 6.6 Orphan remains retention, not deletion

Approval for Orphan is distinct from approval for Delete. After approval, keep v0.2's order: recover the last successfully published manifest, commit the orphan marker, verify it, retain the same ApplicationSet element/Application identity at that SHA, verify retention, and release our finalizer.

Do not deploy an unpublished new spec or unrelated branch drift during orphaning. Do not remove the retained entry or wait for retained resources to disappear. Approval/pause must not weaken the existing re-adoption and sticky-retention tests.

### 6.7 Operational behavior

A deleting resource may remain visible indefinitely while Git, Argo or its target cleanup is blocked. Report the actual blocker; keep the last successful publication and cleanup revision distinguishable.

Manual finalizer removal bypasses these guarantees. Document it as an operator recovery action requiring external cleanup verification, not a normal UI button. Namespace/cluster destruction can remove components independently and is outside this coordination contract. `make clean` must continue deleting only the disposable development stack without waiting indefinitely on this workflow.

## 7. Conditions and lifecycle display

Add only `Approved` to the existing `Published`, `Synced`, `Ready`, and `GitDrift` condition set. Keep raw Argo and target status envelopes unchanged. No new `phase` state machine or synthetic merge lifecycle.

| Situation | Approved | Ready headline |
|---|---|---|
| Namespace policy cannot be resolved | Unknown / `ApprovalPolicyUnknown` or `InvalidApprovalPolicy` | Unknown with the policy error; no new side effect. |
| Required request has no matching annotation | False / `ApprovalPending` or `ApprovalMismatch` | False / `ApprovalPending`, identifying the effective action. |
| Matching annotation for current mutation | True / `Approved` | Continue existing publication/sync/health evaluation. |
| Approval is disabled for pending work | True / `NotRequired` | Continue existing evaluation. This is not a human approval claim. |
| No new mutation is pending | True / `NoChanges` | Evaluate the already-published deployment normally. |
| Paused | Preserve/evaluate approval information read-only | False / `ReconcilePaused`, including pending deletion. |
| Approved Delete waiting on Git | True for the accepted cleanup | False / `Deleting`, with Git error/progress detail. |
| Git complete, waiting on Application deletion | True for the accepted cleanup | False / `WaitingForApplicationDeletion`. |
| Application gone, direct resource still present | True for the accepted cleanup | False / `WaitingForResourceDeletion`. |
| Orphan waiting on retained entry | True for the accepted cleanup | False / `Orphaning`, with the handoff detail. |

Use observedGeneration truthfully. An approval condition for an older generation is not authorization for a current pending request. For metadata-triggered Delete/Adopt, the approval envelope's operation distinguishes requests sharing a generation.

Pending-update status must distinguish the current request from the prior healthy deployment. Keep `lastPublished*` and nested Argo/resource data; do not erase the deployed snapshot because a new request awaits approval. The UI shows `ApprovalPending` for the proposal and the unchanged native Argo sync/health for what is running.

During accepted cleanup, its stored request is the operation being completed; do not redisplay a new approval prompt because progress/status changed. Keep approval and cleanup errors visible without rewriting the upstream status messages.

An approval gate should not set `GitDrift=True`. Drift stays informational and unrelated to whether a user is permitted to publish a new change.

## 8. Reconciler responsibilities and event safety

| Existing component | v0.3 addition |
|---|---|
| GitResource publisher/finalizer | Namespace policy lookup/watch, request computation/gate, optional author, safe change consumption, action Delete, accepted cleanup checkpoints and completion checks. |
| ApplicationSet inventory reconciler | Preserve previous pins during approval waits; exclude Git-cleaned Delete requests before CR disappearance; retain Orphan entries as before. |
| Status observer | Preserve existing raw observation; expose pending/cleanup context through the existing summary helper; observe deletion via frozen refs when inventory is gone. |
| ClusterGitConfig handling | No new approval configuration or signing keys. Existing identity/credential behavior remains. |

Keep generation/annotation/deletion predicates explicit. Approval, pause, adoption and namespace-policy changes must enqueue the publisher, while source status changes must not start Git operations. A recognized cleanup-checkpoint change may enqueue the inventory; arbitrary mirrored status changes must not rewrite its list.

Use the existing narrow status patch helper, merge conditions by type, and recompute Ready from fresh combined data. The publisher owns approval fields/condition; do not introduce a second approval writer. No new global lock, worker restriction, webhook server or background job queue.

If deletion needs a faster retry than the existing observation interval, use bounded `RequeueAfter` and events. Do not busy-wait within a reconcile. Status writers patch only semantically changed fields and do not generate clock-only updates.

## 9. Example app — implementation requirements only

### 9.1 Purpose and folder

The eventual implementation adds `examples/approval-ui/` with its own README, Vite configuration, small browser client, dependency lockfile, and tests. No files in that folder are created by writing this specification.

Use Vite with a minimal TypeScript browser application. A frontend framework is not required. Use ordinary controls and a native HTML table; no component platform, routing framework, global state library, schema-form generator, Monaco editor or workflow backend is necessary.

The demo is a trusted local client of Kubernetes. It does not implement controller behavior, authorize human identities, validate cryptographic approvals, access Git credentials or call Argo/Git APIs.

### 9.2 Screens and behavior

| View/action | Requirement |
|---|---|
| Namespace selector | List only non-terminating Namespaces with `enabled: "true"`. Do not show unrelated Namespaces as valid creation targets. |
| Create Namespace | Name plus approval-required checkbox. Create both annotations together. No namespace deletion button. |
| Edit namespace approval policy | Toggle the namespace annotation and display the actual stored result. Warn that disabling approval releases pending requests. |
| Create GitResource | Select Namespace; use editable demo repository/branch/path defaults; provide one ConfigMap example and a small JSON manifest editor. Support optional author/message and Delete/Orphan policy. |
| Edit GitResource | Change the manifest and optional one-shot attribution together. Do not offer immutable destination edits. Begin author/message inputs empty for a fresh change. |
| Resource table | Namespace/name, Approved, Published, native Argo sync/health, Ready reason, GitDrift, and actions. Show deletion/pause and errors without inventing another status vocabulary. |
| Details | Show the selected CR including current proposal, approval request, cleanup context and raw status snapshots. No separate Argo/workload reads. |
| Approve | Present the current action/manifest and copy that exact advertised request into the annotation using a conditional patch. Never approve future generations automatically. |
| Request deletion | Submit `change.action: Delete`, chosen deletion policy and optional author/message. The controller handles approval and Kubernetes DELETE. |
| Cancel pending deletion | Only before deletionTimestamp; remove the prepared Delete action through a guarded edit. Warn that approval may start deletion immediately. |
| Raw Kubernetes DELETE | Optional clearly marked advanced action with confirmation; it demonstrates the same finalizer gate but cannot cancel deletion. |
| Pause/resume and adopt | Use existing annotations. Adoption remains subject to required approval; do not implicitly approve it. |

Disable normal spec-edit actions once deletionTimestamp is present. Keep approval, pause/resume, refresh and details available where meaningful. Surface a stale-request conflict rather than retrying approval against changed intent.

The JSON editor is sufficient because the Kubernetes API accepts structured object content. YAML editing can be added later; no YAML round-trip/editor feature is required here.

### 9.3 Data access and safety

Poll Namespaces/GitResources approximately every three seconds while the page is visible. Serialize refreshes, preserve unsaved form input, stop polling on teardown, and use bounded request timeouts. A watch/reconnect implementation is not required for the demo.

Use a development-only, loopback-bound Kubernetes proxy, for example kubectl proxy using the project's explicit `.dev/kubeconfig` and `k3d-git-state-dev` context, reached through Vite's same-origin dev proxy. Never fall back to the user's current context. Vite supports dev proxying and explicit host/origin controls; keep them restrictive. [R7]

Keep credentials with kubectl, not in browser source, localStorage, committed files or HTTP logs. Bind both local services to loopback and refuse occupied ports rather than killing unrelated processes. Use a small path/method allowlist and same-origin request checks; these are local safeguards, not tenant authorization.

The permitted demo API surface is Namespace list/get/create/annotation patch, GitResource CRUD, and optionally CRD GET for API compatibility checks. Do not expose Secrets, arbitrary Kubernetes proxy paths, target mutations or status-subresource writes. Do not expose this development app publicly.

Use UID/resourceVersion-preconditioned writes. For an open editor, a fresh read may tolerate status-only version changes, but must reject a changed spec or replacement UID. Approval must remain bound to the request the user reviewed, not a newly fetched replacement request.

An old v0.2 controller/CRD must not be treated as approval-capable. Disable demo mutation until the expected v0.3 API fields are available and document the need for matching controller code. A schema check is only a compatibility check, not proof that the running controller enforces the contract.

### 9.4 Developer workflow

Preserve `make up`, `make dev`, `make test`, and `make clean`. The UI is optional to run, not part of every controller rebuild. Its documented workflow is:

```text
make up   (or make dev)
cd examples/approval-ui
npm ci
npm run dev
```

Pin the chosen frontend dependencies and check in the real generated lockfile during implementation. Provide `build` and `test` scripts. Stopping the demo stops only its own helper processes and leaves the cluster running. Controller watch rebuilds must not reset Namespaces, approvals, Git history, or ApplicationSet entries.

The main test command includes the new required helper/browser checks with documented Node/browser prerequisites. Do not silently skip them when a required dependency is unavailable. Extend the existing CI workflow rather than add a separate deployment system.

### 9.5 Walkthrough to document

Create an enabled namespace without approvals and publish a ConfigMap without an author. Create another enabled namespace requiring approval; submit an attributed create, observe zero Git publication, approve it, and observe normal deployment. Edit that manifest and show that the previous approval cannot authorize the new generation. Show consumed change metadata and the configured bot fallback on the next unattributed edit.

Then request deletion through the form, approve it when required, and observe the CR remain visible through downstream cleanup. Demonstrate Orphan retaining the same Application and resource, and optionally re-adopt it using the existing flow. Include one raw Kubernetes DELETE example and its default attribution. The browser only consumes Namespace/GitResource data for these steps.

## 10. Focused tests — boundaries, not a Cartesian product

Reuse the existing unit/envtest/Git integration/k3d harness. Test pure decision rules in tables and exercise a small number of actual end-to-end flows. Do not repeat every author/policy/operation/status combination through a browser.

| Test group | Required assertions |
|---|---|
| Optional author | Omitted author preserves bot behavior; supplied author and explicit bot committer are distinct; invalid partial identity rejected; concurrent different authors do not leak across operations. |
| Change consumption | Clear only a successfully completed/no-op change; failure leaves it; same-generation guarded cleanup; newer generation survives; cleanup generation causes no extra commit/approval. |
| Approval token | Deterministic UID/generation/operation; new spec, replacement UID, Adopt, Delete and Orphan do not accept stale evidence; pause/status/unrelated Git commits do not change the request. |
| Namespace policy | Missing/false auto flow; true waits; bad/read-failed policy blocks; toggles enqueue the right namespace; disabling policy releases pending work; enabling does not redeploy unchanged objects. |
| Pending publication | Create/update/adopt perform no Git write before approval; old successful SHA/Application remains during a pending update. |
| Stale review | Conditional annotation patch rejects concurrent edits; a stale annotation never approves newer bytes; branch retry rechecks the current request. |
| Prepared deletion | `change.action: Delete` waits before Kubernetes DELETE if required, then enters the finalizer path. Cancel before deletion works; Apply author is not reused for raw DELETE. |
| Raw deletion | deletionTimestamp creates a distinct Delete/Orphan approval request; missing approval preserves external state; no-approval default flow uses bot attribution. |
| Delete completion | Git cleaned → entry removed while CR still exists → Application/target absent → finalizer released. Stop Argo or hold a test target finalizer and verify waiting status plus later recovery. |
| Orphan | Approval gates marker commit/retention; same Application/resource survive. Preserve the existing orphan/re-adopt recovery assertions. |
| Recovery | Rejected push preserves both files; crash after approved push before status does not duplicate the commit; crash after Git delete before entry removal resumes; pending cleanup is not replanned from newer metadata. |
| Event isolation | Approval/namespace/intent events wake the gate; mirror updates and post-consumption no-ops cause no new content commits or unrelated inventory rewrites. |
| Demo helpers | Enabled-only namespace list, blank optional attribution, guarded edits/approvals, no approval token invention, old-API mutation disabled. |
| Browser walkthrough | One required-approval namespace create/update/delete journey and a small no-approval namespace smoke case against the dedicated cluster. |

Use fake clocks only where existing interval tests need them; approval introduces no time-based semantics. Use actual Git transport tests for author/committer and retry behavior, not only a mocked publisher.

Keep the existing basic timing report. Separate intentional approval waiting from approval-to-publication latency, retain concurrent-publication timings, and measure Git deletion versus final wrapper removal. Do not add per-request metric labels, performance guarantees, or a benchmark service.

## 11. Implementation boundaries and documentation

Implement these features inside existing concrete helpers and reconcilers. Expected changes are limited to current API types/markers, publisher/finalizer and inventory rules, status summary handling, a small approval helper, tests, documentation, and the self-contained example folder. Do not rename working code solely to fit this outline.

Add `docs/approvals.md` and update `docs/lifecycle.md`, `docs/status.md`, and the main README as needed. Document:

- Optional author fallback and why attribution is not authentication.
- One-shot `change` consumption and its concurrency/no-op rules.
- Namespace annotations, exact request matching, and the trusted-annotation limitation.
- Prepared versus raw DELETE, the new downstream-wait contract, and Orphan's different completion rule.
- Paused deletion and operator recovery without automatic finalizer stripping.
- The demo walkthrough and the fact that deleted CRs no longer provide a queryable audit record.

Keep `docs/git-history.md` as the delegated history/audit boundary. Add no hyperlinks subsystem, backlinks or request IDs as incidental work.

### 11.1 Upgrade notes

Existing namespaces without approval policy retain automatic publication. Existing GitResource manifests without author/action remain valid. Existing Application names/pins and orphan retention entries must not be recreated.

Warn that Delete now waits for Argo and target cleanup; old teardown assumptions and old assertions that a CR disappears immediately after Git removal must be updated. A preexisting v0.2 cleanup checkpoint proves an already-started operation: finish its policy safely under the new completion checks rather than retroactively demanding approval for a completed Git effect.

Turn on approval in selected Namespaces only after v0.3 CRDs and controller are installed. Do not claim the annotation is enforced by the old controller. Preserve the currently pinned Go/Kubernetes/Git/Argo toolchain; no unrelated upgrades or scaffold rewrites.

### 11.2 Suggested implementation order

1. Optional attribution and one-shot consumption, with generation/conflict tests.
2. Namespace policy and deterministic approval gate, including the Approved condition.
3. Prepared/raw deletion integration and inventory removal-before-finalization, with recovery tests.
4. Example app and upstream walkthrough, then the small browser smoke test.
5. Existing full test workflow, generated-file checks, documentation and timing report.

### 11.3 Completion criteria

The eventual implementation is complete when the existing four-command workflow remains usable, the new contract is documented, required tests pass, and the example demonstrably drives the real controller rather than simulating approval locally.

Unapproved work must cause no new Git writes; optional authors must be correct; completed change metadata must not leak into the next request; pending updates must retain the prior deployment; Delete must wait for its downstream cleanup; Orphan must retain it. Record the actual commands and test results during implementation. This document makes no claim that those changes or tests have been executed.

## Appendix A. Signing options considered and deferred

| Option | Meaning | Decision |
|---|---|---|
| Trusted request-bound annotation | Controller checks that supplied approval matches current intent; cluster/upstream authorization decides who may submit it. | Implement now. No keys. |
| App private key, controller public key | App signs approval evidence; controller independently verifies it. Merely knowing the public key does not permit approving. | Potential later extension; not part of v0.3. |
| Controller bootstraps a keypair | Mechanically possible, but the private signing key must be delivered only to approvers. Generating it does not solve permission management. | Defer; no automatic key generation. |
| Git commit signing | Attests who signed the resulting Git commit; does not by itself approve an unexecuted CR request. | Separate feature, deferred. |

Public keys verify signatures; private keys create them. Creation permission on GitResources does not inherently imply permission to read a separate signing Secret, but a broadly authorized administrator can bypass either workflow. The current trusted model deliberately avoids adding that key-distribution and authorization surface. [R2][R8]

## Reference notes

These sources establish upstream mechanics; the field names, annotations, request format, workflow, and test scope above are decisions for this example. The reference pages were checked when preparing the specification; they do not establish that the current repository has implemented v0.3.

- **R1 — go-git CommitOptions:** separate Author and Committer, defaulting, and empty-commit behavior. https://pkg.go.dev/github.com/go-git/go-git/v5#CommitOptions
- **R2 — Kubernetes RBAC:** resource/verb/subresource permissions and role bindings. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
- **R3 — Kubernetes CRD status subresource:** generation changes and status/metadata separation. https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#status-subresource
- **R4 — Kubernetes API concepts:** optimistic concurrency and resourceVersion-aware operations. https://kubernetes.io/docs/reference/using-api/api-concepts/
- **R5 — Kubernetes finalizers:** deletionTimestamp, finalizer processing, and irreversible accepted deletion. https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/
- **R6 — Argo ApplicationSet deletion:** generated Application ownership and Argo resource finalizers. https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Application-Deletion/
- **R7 — Vite development server:** proxy, host, CORS and allowed-host settings. https://vite.dev/config/server-options
- **R8 — Go Ed25519:** distinct signing and verification key operations. https://pkg.go.dev/crypto/ed25519
