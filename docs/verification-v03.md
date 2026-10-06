# v0.3 validation

Validated on 2026-10-06 against the owned rootless Podman/k3d stack. The existing
Go/Kubernetes/Git/Argo versions are unchanged; see the [v0.2 version table](verification.md).
The UI used Node 24.21.0, Vite 8.3.3, TypeScript 7.0.2, Playwright 1.63.0 and
its installed Chromium. Dependencies are pinned in the generated lockfile.

## Commands and results

| Check | Result |
| --- | --- |
| `make up` | CRDs/RBAC/controller deployed; static inventory preserved. |
| `make test` | Passed all required suites on the finished code. |
| Race-enabled unit/smart-HTTP Git tests | Passed, including concurrent authors, gate/retry and recovery cases. |
| Real envtest API checks | Passed admission/defaults, conflicts, status, and consumed-generation behavior. |
| Live Go acceptance | All 14 cases passed; 299.055 seconds. |
| UI helper tests | All six passed. |
| Strict TypeScript + Vite build | Passed. |
| Real Chromium walkthrough | Passed; 22.6 seconds for the journey, 33.8 including setup/cleanup. |
| `make lint-fix` | Zero issues. |
| `go vet -tags=e2e ./test/e2e` | Passed. |
| `make generate manifests` + checksum comparison | Generated files unchanged by regeneration. |
| `git diff --check` | Passed. |

The live suite exercises the actual controller and Argo, including Skaffold source
watching, bootstrap preservation, Forgejo/Argo outages, restart recovery, pause,
raw status, drift, and Orphan/re-adoption with stable Application/target UIDs.
The browser creates required and automatic enabled namespaces, submits and
approves real proposals, rejects a stale open review, edits with blank attribution,
and completes prepared deletion. It also checks restricted proxy/filesystem access.

## Contract evidence and refinements

- Required create/update/adoption/cleanup waits without a Git write; pending updates
  retain the prior pin. Fresh namespace policy and UID/generation/operation checks
  protect push retries. Invalid/unreadable policy remains Unknown during recovery.
- User author and configured bot committer are distinct. Concurrent authors remain
  scoped to their own commits. Raw DELETE ignores leftover Apply attribution;
  accepted cleanup freezes its policy, author, message, config and target identities.
- Successful and acknowledged no-op changes are consumed only after durable status.
  Real API tests verify the resulting generation increment with exactly one content
  commit and no second approval. Newer input and newly added annotations survive
  consumption races. Valid no-op metadata is consumed during Git-access failure;
  invalid attribution remains an error, with the old verified pin preserved.
- Prepared deletion can be canceled before DELETE. Approved Delete removes the
  inventory entry while its wrapper still exists, then waits for authoritative
  Application and target absence. A held test target finalizer survives a controller
  restart; releasing only that test finalizer allows completion. Forbidden reads and
  UID replacements hold cleanup. A never-owned Orphan safely completes without
  requiring a retained entry or changing another owner's file.
- Accepted-push recovery needs no new approval or duplicate commit. It preserves
  the verified pin and current branch drift. A regression first reproduced recovery
  hiding drift, then passed after comparing against branch HEAD. Generated generation
  trailers take precedence over similarly named lines in the user message. An
  unapproved revert cannot reuse a superseded publication.

An initial full run exposed an outdated uninstalled-XR deletion fixture: v0.3
correctly held it because NoMatch cannot prove absence. The unknown-field fixture
now uses a discoverable ConfigMap and verifies downstream deletion. Its previous
manual Application-finalizer shortcut was removed. The stranded test fixture was
recovered by temporarily restoring discovery/read access, then that temporary
CRD/RBAC and namespace were removed. The controller never strips target finalizers.

## Concurrent-publication measurements

20 wrappers share one branch with four publisher workers. The existing report
prefix is retained. One-second polling and API overhead limit timing precision;
these measurements are not performance guarantees or a controlled v0.2 comparison.
v0.3 adds approval reporting and verified downstream cleanup work.

| Seconds | p50 | p95 | max |
| --- | ---: | ---: | ---: |
| Create to publication | 10.608 | 18.211 | 19.150 |
| Update to publication | 6.210 | 19.008 | 19.410 |
| Delete request to Git cleanup | 7.264 | 16.754 | 19.084 |
| Git cleanup to wrapper removal | 6.542 | 12.610 | 13.331 |
| Delete request to wrapper removal | 16.196 | 18.796 | 21.802 |

Initial-publication throughput: **0.99/s**.
The run recorded **604 manager-wide status patches**, **27–33 status
changes per fixture** across creation/update/deletion, and **70 rejected-push
retries for 60 successful pushes**. Manager-wide counters can include other active
wrappers; per-case Git logs and status counts are scoped to the 20 fixtures.
The preceding successful run measured 1.23/s, 643 patches and 61 rejected retries;
this variation is why these runs do not establish a new optimization claim.

## Approval and deletion timing

One live required-approval flow measured:

| Measurement | Seconds |
| --- | ---: |
| Intentional test wait before create approval | 1.478 |
| Approval to publication | 3.011 |
| Raw delete request to Git cleanup | 3.022 |
| Raw delete request to final wrapper removal | 5.989 |

Deletion timing includes the test-held target finalizer and controller restart;
it does not represent an unblocked cleanup latency. The approval wait is reported
separately from publication latency. Orphan retention is checked independently.

Raw artifacts remain ignored in `reports/timing-v02-1791310332/` and `reports/approval-v03-1791310596/`, including
per-case timestamps, status counts, Git durations/retries and metrics. Browser
failure artifacts use `reports/ui/browser/`; the successful walkthrough cleans up
its namespaces and stops only its own Vite/proxy processes.

The normal controller image is restored after the temporary watch probe. The live
`demo/example-config-three` remains Ready at generation 1 and its original pin
`cae9e2bf43654bf5d03eb7c41bd0c69954338f9a`. No destructive `make clean` was run.

## Approval footer extension

The subsequent approver/footer change was validated on 2026-10-06 with
`make lint-fix` (zero issues), focused race-enabled Git tests, E2E-tagged vet,
and the required suites. Unit and envtest suites passed. All 14 live cases
passed in 273.979 seconds, including reading the actual Git commit to check
the request and `Approved-by` trailer. All seven UI helpers and the strict
TypeScript/Vite build passed.

`make test` reached Chromium but its server startup stopped because the user's
local UI was already listening on port 5173. The browser suite was rerun using a
temporary Playwright config with `reuseExistingServer: true` against that verified
project server: the journey passed in 17.7 seconds (28.6 with cleanup). The
existing UI/proxy processes were left running; the repository config still
requires its own server for ordinary runs.

Git tests cover named and anonymous approvals, automatic publication, stale and
invalid identities, approver replacement before push, concurrent annotation
consumption, and frozen deletion attribution. They check unchanged Git author
and committer roles and no duplicate commit during housekeeping. The browser
asserts blank approver inputs for each review and verifies the actual guarded
patch includes the reviewed request and supplied name/email together. The
normal controller image is restored after the live watch probe.
The current non-test resource `test2/testtwo` is Ready at generation 2 with pin
`c0f24c46849c9b8084414446f81637d1167bb5f2`; its inventory entry is present and
the test namespaces are removed.

## Final review before commit

The final review on 2026-10-06 passed `GIT_STATE_UI_REUSE_SERVER=1 make test`
end to end. The explicit environment option reuses this checkout's already-running
UI; ordinary test runs still start their own server. All 14 live cases passed in
237.251 seconds. Seven UI helper tests and the strict TypeScript/Vite build passed;
the Chromium journey passed in 19.7 seconds (30.6 including cleanup).

An additional `go test -race -count=1 ./internal/... ./cmd/... ./hack/bootstrap/...
./test/devstack/...` passed with test caching disabled. `make lint-fix` reported
zero issues, E2E-tagged vet and `git diff --check` passed, and checksums before and
after `make generate manifests` confirmed generated files were unchanged.
The normal controller image is restored after the probe; `test2/testtwo` retains
its UID, generation 2 and verified pin. Local notes/sample manifests are excluded
from the implementation commit.
