# Publication and lifecycle

GitResource.spec.manifest is the complete desired Kubernetes object. `change`
contains only a commit message; it is not a patch payload. Rendering strips
server metadata/status and overwrites four reserved annotations:
`gitops.example.io/management-state`, `source-namespace`, `source-name`, and
`source-uid` (all prefixed `gitops.example.io/`). Those markers participate in the
canonical content hash. Authored spec fields are preserved.

A new path must be absent or owned by the wrapper UID. An existing unowned path
fails Published with PathAlreadyExists. Every retry checks current ownership;
normal updates cannot steal another UID's file. Mutating attempts refresh their
isolated clone before building the commit, then fetch into that same worktree to
verify the normal push. Branch races reread the wrapper and reapply only this path
from a fresh head. Three local attempts use exponential jitter from a 500 ms base;
continued contention reports PublishPending and schedules another attempt.
Verified prior commits/path-scoped UID trailers recover interrupted publication
without duplicate content commits;
ambiguous ownership requires explicit adoption or reports RecoveryRequired.

Publication intent is a canonical manifest change, not every event or generation.
Message/policy-only changes can advance lastPublishedGeneration with the same SHA
and no content commit. Every 30 seconds, GitDrift compares only this path at HEAD
with lastPublishedContentHash. External edits/deletions report ExternalModification
or FileMissing; access errors are Unknown. Detection never restores content or
advances the Application pin. A real desired manifest change can overwrite drift.

Pause exactly with the string `"true"`:

```sh
kubectl annotate gitresource example-config -n demo gitops.example.io/paused=true
kubectl annotate gitresource example-config -n demo gitops.example.io/paused-
```

The finalizer is installed even on a new paused wrapper. Pause blocks new Git
mutations, drift fetching, adoption consumption, inventory pin changes and
finalizer release, including a requested deletion. The existing inventory entry
is preserved exactly. Kubernetes observations continue and Ready reports
ReconcilePaused. Already completed in-flight external success may be recorded;
pause is not a distributed transaction cancelling a push already accepted.

Delete is the default deletion policy. v0.3 gates cleanup by namespace approval,
then freezes policy, request, effective attribution, configuration and observed
Application/published-target identities before starting. It removes only its Git
file and saves the verified cleanup revision. The aggregate excludes that Delete
entry while the wrapper is still terminating. Argo cascades Application/workload
deletion; only verified entry, Application and exact direct-resource absence allow
finalizer release. This intentionally replaces v0.2's early finalizer release. A rejected, never-owning wrapper never deletes the other owner's
file. Unavailable or ambiguous ownership holds cleanup rather than guessing.

For retention, set Orphan before deletion:

```sh
kubectl patch gitresource example-config -n demo --type=merge \
  -p '{"spec":{"deletionPolicy":"Orphan"}}'
kubectl delete gitresource example-config -n demo
```

The selected policy is durably stored in status.cleanup.policy before cleanup.
Later policy edits do not change the in-flight choice. Orphan restores the last
verified controller publication (never a pending spec or external branch drift),
marks it orphaned with the original UID, pushes and verifies it, and stores the
orphan SHA in cleanup.revision. The aggregate retains the same Application name
and path, pins that SHA and marks the element managementState=orphaned. Only an
uncached verified retained entry allows finalizer release. Missing/unwritable
ApplicationSet blocks Orphan; stopped Argo controllers alone do not.

After a verified cleanup revision is saved, retries complete only the API handoff;
later branch edits do not replace that checkpoint or its retained pin.

After CR removal the retained entry remains sticky across restarts and stale old
owner data. The controller stops Git polling/publication/status aggregation for
that entry. Argo continues managing it using ordinary apply: resource UID survival
is not guaranteed for every external sync strategy. Orphan does not detach the
Application. Manually removing its element can cascade deletion; deleting or
replacing the ApplicationSet is outside retention's guarantee. Back up its entries
with your other cluster state.

To adopt, recreate a wrapper in the original destination namespace for the exact repository/branch/path with a complete
supplied manifest and this annotation:

```yaml
metadata:
  annotations:
    gitops.example.io/adopt-existing: "true"
```

The worker resolves the unique existing entry before pushing, persists the same
Application reference, checks that the old wrapper UID is gone and the Application
is not terminating, assigns the new UID/managed marker and publishes the supplied
content. The aggregate transfers the retained entry to that UID. After verifying
this managed handoff, the worker consumes the annotation with a metadata patch.
Interruption before consumption is safe to retry; later ordinary updates do not
need the annotation. Multiple matching identities or a live old owner block
adoption. Explicit adoption is a one-time takeover, not permission for a former
owner to steal the path back.

On v0.1 upgrade, markerless files require a verified prior publication hash/SHA
and matching existing inventory entry to establish continuity. Existing
Application names are preserved. Upgrade alone does not create an incidental
content commit. The next real desired change includes ownership annotations.
Ambiguous markerless paths require adopt-existing; no arbitrary file is claimed.

See [status.md](status.md) for errors/readiness and [git-history.md](git-history.md)
for path history and why a Git-only revert does not advance a pinned Application.

## Prepared deletion and recovery in v0.3

Submit `spec.change.action: Delete` with optional author/message and Delete/Orphan
policy. The controller waits for approval, saves accepted context, then issues a
UID/resourceVersion-preconditioned Kubernetes DELETE. Removing the prepared block
can cancel it before Kubernetes accepts DELETE; after deletionTimestamp it cannot
be canceled. Raw DELETE uses the same gate, with bot/default attribution unless
explicit Delete metadata is present. A prior Apply approval/author is not reused.

A terminating wrapper remains visible through Git/Argo outages, unreadable targets
or blocked workload finalizers. Ready reports Deleting, WaitingForApplicationDeletion,
WaitingForResourceDeletion, Orphaning or IdentityConflict with the actual blocker.
Reads use frozen references after Application inventory disappears. Forbidden,
discovery failures and read errors do not prove absence. A same-name replacement
holds cleanup for operator recovery. The controller never deletes the workload,
strips its finalizers or times out its own guarantee.

Once accepted, cleanup resumes its stored context through restart or policy/spec
changes; pause still blocks new effects. A preexisting v0.2 cleanup checkpoint is
an already-started operation and is completed without retroactive approval, under
the new downstream checks. Old tests/automation must allow these longer waits.
Orphan still requires verified retention, not disappearance of retained objects.

Manual finalizer removal bypasses the guarantee and is an operator recovery action
only after verifying external cleanup. Namespace/cluster destruction is outside
this per-wrapper contract. `make clean` still removes only the disposable stack.
See [approvals.md](approvals.md) for policy, one-shot metadata and trusted evidence.
