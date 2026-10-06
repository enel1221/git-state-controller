# Approvals and one-shot attribution

v0.3 keeps the v1alpha1 API. Install both the v0.3 CRDs and matching controller
before enabling namespace approval policy. An old controller does not enforce it;
the demo's schema check only detects API compatibility.

Annotate selected namespaces:

```yaml
metadata:
  annotations:
    gitops.example.io/enabled: "true"
    gitops.example.io/approval-required: "true"
```

Only enabled, nonterminating namespaces are offered for new requests in the demo.
Enabled is discovery, not a controller gate. Removing it never abandons resources
or cleanup. Approval policy is evaluated by the controller for every GitResource:
true requires matching evidence, false/absent allows automatic flow, and any other
value or namespace read failure blocks new external mutations with a diagnostic.
Namespace policy updates enqueue resources in that namespace.

Submit one manifest and optional one-shot metadata together:

```yaml
spec:
  change:
    message: Update the greeting
    author:
      name: Alex Developer
      email: alex@example.com
  manifest:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: example
    data:
      greeting: hello
```

Author requires a usable name and email together. Omitted author uses the current
ClusterGitConfig identity/default. The bot remains the explicit Git committer and
uses the existing Secret for transport; attribution grants no access and verifies
no human identity. Raw deletion ignores leftover Apply attribution. Explicit
Delete metadata can attribute either Delete or Orphan. Default cleanup messages
distinguish those operations.

The controller advertises `status.approval.request`, for example
`v1:<uid>:7:Apply`. Operations are Apply, Adopt, Delete and Orphan. The value binds
UID, spec generation and effective operation; it is independent of shared branch
HEAD. Clients copy it as an opaque value after presenting the exact current
proposal. `approved: "true"` has no meaning.

An approver patches `gitops.example.io/approved-request` to that advertised value.
Use a fresh read and UID/resourceVersion preconditions. A stale review must fail
or remain nonmatching, never approve a newer proposal automatically. A spec edit,
replacement UID, adoption or deletion requires a different value. Pause/resume,
status updates and unrelated Git commits do not change it.

Optionally set `gitops.example.io/approved-by` in the same guarded patch. Its JSON
binds the supplied name and email to the reviewed request:

```yaml
metadata:
  annotations:
    gitops.example.io/approved-request: "v1:<uid>:7:Apply"
    gitops.example.io/approved-by: '{"request":"v1:<uid>:7:Apply","name":"Jamie Reviewer","email":"reviewer@example.com"}'
```

Both name and email are required when attribution is supplied. Names are limited
to 128 bytes, email addresses to 254 bytes, and neither may contain control or
header characters. Invalid attribution for the matching request blocks new writes
with `Approved=False/InvalidApprover`. Attribution for a different request is
ignored. Omitting attribution still permits anonymous approval; remove any old
`approved-by` value when submitting an anonymous approval. The demo provides
optional name/email inputs and patches both annotations together.

Actual publication commits append approval trailers to the existing resource
footer:

```text
GitResource: demo/example
GitResource-UID: <uid>
GitResource-Generation: 7
GitResource-Approval: Approved
GitResource-Approval-Request: v1:<uid>:7:Apply
Approved-by: Jamie Reviewer <reviewer@example.com>
```

Anonymous approvals omit `Approved-by`. Automatic operations use
`GitResource-Approval: NotRequired` and omit the request and approver trailers.
Approver attribution does not change the Git author or bot committer. Retries
recheck approval and attribution before pushing. Accepted deletion freezes the
footer with the cleanup message. No-op operations and identity-only edits do not
create or amend commits, so existing history stays as published.

While approval is pending, Git content and the last verified Application pin stay
unchanged. `Approved=False` and `Ready=False/ApprovalPending` describe the current
proposal; raw Argo status still describes the deployed revision. Invalid/unknown
policy reports Approved/Ready Unknown. GitDrift remains independent. Matching
approval or disabled policy allows the ordinary publication flow.

After a successful verified Apply, publication status is saved before a guarded
patch removes `spec.change` and the matching approval and approver annotations. The patch
checks UID, generation, captured change block, pause and deletion state. Adoption
also waits for its verified ownership handoff before consuming its annotation.
Newer input or changed approver annotations survive consumption; failed/pending operations retain them. Retries recover accepted
commits rather than duplicate them, even if an already-used annotation was removed.

Removing change increments generation. The following reconcile acknowledges the
same content/SHA with Approved=True/NoChanges and no second commit or prompt.
Message-only, author-only and empty blocks likewise cause no content commit or
approval request. External drift is still observed, never repaired by housekeeping.
An omitted block remains absent; there is no API Apply default or empty placeholder.
Declarative clients must not continually reassert completed one-shot metadata.
Start every new edit with blank author/message inputs unless intentionally supplied.

Approval annotations are trusted coordination evidence, not signatures or proof
of separation of duties. A principal who can patch the CR can normally approve
it; a principal who can edit the namespace can disable policy. The cluster
administrator and upstream application own human authentication/authorization.
Supplied approver names and emails are unverified attribution. There is no
identity verification, expiry, signing key, request CR or audit ledger.
Removing evidence does not undo a side effect already accepted by Git/Kubernetes.

For deletion, submit `spec.change.action: Delete` plus the desired Delete/Orphan
policy and optional metadata. Before Kubernetes DELETE, pending deletion can be
canceled by removing that block through a guarded edit. Approval may begin deletion
immediately; deletionTimestamp is irreversible. Raw Kubernetes DELETE enters the
same finalizer gate and uses bot/default attribution unless explicit Delete
metadata was supplied. See [lifecycle.md](lifecycle.md) for accepted cleanup,
downstream waits, pause and recovery. Deleted wrappers provide no retained audit
record; [git-history.md](git-history.md) remains the path-history boundary.

The local demo and real-controller walkthrough are documented in
[approval-ui README](../examples/approval-ui/README.md).
