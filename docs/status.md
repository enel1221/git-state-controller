# Reading status

`Published` means the desired generation was verified in Git. `Synced` means the
tracked Application's source, destination, compared source/destination, sync SHA
and Synced result all describe that exact publication. Historical operations do
not establish synchronization. `Ready` combines current publication, correlated
sync, existence, conventional target Ready evidence and Argo health. Its
observedGeneration must equal the wrapper's generation. GitDrift is informational;
Ready can remain true while branch HEAD differs from Argo's pinned SHA.

`applicationRef` preserves the Application identity, including across adoption.
`publishedResourceRef` describes the manifest actually published, so an unpublished
rename does not redirect observation. The observer watches managed Applications
only in argocd, maps their source namespace/name/UID, and checks exact targets via
an uncached API reader every 15 seconds (plus jitter). It uses discovery and the
Application inventory, ignores hooks/unrelated children, respects cluster scope,
and never builds a descendant resource tree. ConfigMaps and the fixture
StatusObject have explicit GET permissions; add explicit GET RBAC for other kinds.
A permission failure is reported rather than prompting a new wildcard role.

`argoCD.status` is the full upstream Application status, including its own history,
conditions, operation errors and unknown future fields. `argoCD.source` and
`destination` copy only the selected spec fragments. `resource.status` copies only
the directly managed object's status. It never copies workload spec, ConfigMap or
Secret data, descendants, or full metadata. Statusless ConfigMaps omit the status
key; an existing empty status remains `{}`. Removed upstream fields are removed
from the mirror.

Observation reasons distinguish Observed, NotFound, NotTracked, Forbidden,
ReadFailed, UnsupportedTarget, SnapshotTooLarge and InvalidStatus. Unknown
existence omits `exists`; authoritative absence sets false and clears UID.
Failures clear raw data but retain references and the last successful time.
Same-name replacements get a fresh UID and snapshot. These are eventually
consistent observations, not a transaction across Git, Argo and the workload.

Each mirror including its envelope is limited to 128 KiB of compact JSON. A
prospective whole wrapper exceeding the 768 KiB soft guard omits raw mirrors;
existing user spec/metadata are never shrunk. Server size rejection gets one small
fallback. Oversize reports measured bytes; nothing is silently truncated. Copying
resumes when the source fits. Summary messages are bounded to 1024 bytes, while
mirrored messages remain complete. Publication and finalization can proceed with
small diagnostics even when observation fails. Recursive mirroring of this
controller's status or its own generated Application is unsupported.

A semantic no-op does not patch status. Source resourceVersion is associated with
the snapshot, not persisted as a heartbeat for metadata-only changes, and never
compared numerically. Application generation is likewise associated with the
mirrored payload; an envelope-only change preserves it. Target generation remains
readiness evidence and is observed when it changes. Condition transitions change
their time only when their truth value changes. Publication and observation use optimistic status patches,
merge independently owned fields, and recompute Ready/Synced after merging.
Application identity/tracking, selected source/destination and any raw status
change enqueue the observer. ApplicationSet status-only events do not enqueue
inventory reconciliation; config status events do not enqueue Git workers.
Publication and its GitDrift evidence share one patch, while durable identity and
cleanup checkpoints remain separate.

This example is trusted-only. Reading a GitResource grants access to its copied
upstream messages and outputs, even if the caller cannot read those source
objects. Unknown fields can contain sensitive information. There is no promised
redaction; clients must treat messages as untrusted text. The controller does not
log whole snapshots.

ClusterGitConfig Ready means credentials/configuration loaded, not universal
repository access. `credentialsRef` records the Secret UID and opaque version.
`lastAccess` is one actual Fetch or Push result for one URL and credential version,
with sanitized transport diagnostics. Fetch success does not prove push access;
there are no probe commits. Evidence using an old Secret cannot certify a rotated
Secret. Concurrent records are last reported evidence, not an ordered audit log.
A denied repository does not gate other wrappers. Argo credentials remain separate.
Access evidence is buffered once per reconcile, with verified Push evidence taking
precedence over verification Fetch. Identical evidence causes no patch. Expected
branch contention reports PublishPending rather than a false access failure;
authentication and transport failures retain diagnostics.

The private manager metrics endpoint listens on 8080. Metrics use low-cardinality
operation, writer and outcome labels: Git transport duration distinguishes
Clone/Fetch/Push; status-patch counts distinguish
publisher/observer/config and success/conflict/error. Rejected-push retries,
observation counts and controller-runtime metrics are also available. No
CR/repository/path/SHA/UID metric labels. Polling costs roughly N/15 exact target
GETs per second plus events. Full status/watch bandwidth scales with
object size and churn; the one ApplicationSet list grows with active and retained
entries. The example does not promise a hardware-independent scale threshold.
