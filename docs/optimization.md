# Publication and status optimization — 2026-10-06

The selected implementation combines related status writes, buffers configuration
access evidence, filters irrelevant Application events, and refreshes an isolated
Git worktree before mutation and after push. A 500 ms exponential retry base with
jitter gave the best measured balance of completion time and request counts among
seven tested strategies. Four publication workers remain active.

## Experiment design

The baseline is v0.2 commit `32e3b00645a3ff510659c63c9e49d6a34ed5c687`.
Each strategy ran the existing live twenty-concurrent-publications test three
times against the managed rootless Podman/k3d stack. Each burst creates twenty
wrappers, updates all twenty, then deletes them: **60 successful pushes per
burst** to distinct paths on one shared branch. All 21 comparison bursts passed,
covering 1,260 successful measured pushes. No trial reduced worker concurrency.

Counts below are arithmetic means per burst. Latency percentiles pool sixty
resources per phase across the three runs. The timing harness uses one-second
polling; API calls and asynchronous Argo processing add overhead.

Logical Git operations and wrapper status changes are scoped to the twenty test
resources. Manager HTTP counters and configuration status changes include other
active/background work during the measurement window. HTTP requests are those
made by the manager, excluding the test client's own requests. Watch counts
measure actual status changes; PATCH counters also include rejected requests and
metadata/inventory patches. These different scopes should not be conflated.

## Strategies tested

“API fixes” means combined publication/drift status, buffered configuration
evidence, semantic snapshot stability, focused watch predicates, and normal
branch contention reported as pending rather than an access failure. “Short” is
the original 30–129 ms local retry delay. Fetch verification replaces the second
full clone with an incremental fetch/reset in that attempt's isolated worktree.
Pre-refresh adds another incremental fetch immediately after cloning, before
checking ownership and constructing the commit.

| Strategy | Rejected pushes | Full clones | Clone + fetch reads | Manager GET | Manager PATCH | Create throughput/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Baseline v0.2 | 161.3 | 442.7 | 442.7 | 6,778.7 | 930.0 | 0.851 |
| API fixes; clone verification; short delay | 161.0 | 442.0 | 442.0 | 5,171.3 | 645.3 | 0.884 |
| API fixes; fetch verification; short delay | 162.0 | 222.0 | 444.0 | 5,123.7 | 621.0 | 0.985 |
| Fetch verification; 500 ms base | 104.0 | 164.0 | 328.0 | 4,762.0 | 614.3 | 0.939 |
| Fetch verification; 1,500 ms base | 59.0 | 119.0 | 238.0 | 4,549.0 | 606.7 | 0.729 |
| **Pre-refresh; 500 ms base (selected)** | **57.0** | **117.0** | **351.0** | **4,355.3** | **588.0** | **1.090** |
| Pre-refresh; 250 ms base | 64.0 | 124.0 | 372.0 | 4,424.7 | 599.7 | 1.003 |

| Strategy | Create p95 (s) | Update p95 (s) | Delete to CR removal p95 (s) |
| --- | ---: | ---: | ---: |
| Baseline v0.2 | 22.21 | 21.00 | 23.00 |
| API fixes; clone verification; short delay | 21.81 | 21.01 | 20.00 |
| API fixes; fetch verification; short delay | 18.33 | 19.00 | 19.17 |
| Fetch verification; 500 ms base | 20.36 | 17.95 | 17.79 |
| Fetch verification; 1,500 ms base | 23.21 | 21.98 | 22.80 |
| **Pre-refresh; 500 ms base (selected)** | **16.41** | **16.02** | **16.80** |
| Pre-refresh; 250 ms base | 17.41 | 17.00 | 17.95 |

The API-only trial isolates the status/event improvements: PATCH traffic falls
without reducing Git contention. Replacing verification clones with fetches
halves full clones but does not reduce the number of logical Git reads by itself.
A larger retry delay reduces collisions, but the 1,500 ms trial sacrifices
throughput and create latency. Pre-refresh costs a lightweight read per mutating
attempt yet avoids more stale-head commits: at 500 ms it improves all three p95
latencies and reduces clones, rejected pushes and API traffic compared with the
same delay without pre-refresh. The 250 ms variant performs worse on those
measures. Thus 500 ms plus pre-refresh is the best tested balance here; the
1,500 ms variant still wins on total Git read count alone.

## Before and after

| Measure, per twenty-resource burst | Baseline | Selected | Reduction |
| --- | ---: | ---: | ---: |
| Successful pushes | 60 | 60 | Same required work |
| Rejected pushes requiring retry | 161.3 | 57.0 | 64.7% |
| Full clones | 442.7 | 117.0 | 73.6% |
| Incremental fetches | 0 | 234.0 | Added in place of clones / to refresh |
| Total logical Git reads | 442.7 | 351.0 | 20.7% |
| Manager GET requests | 6,778.7 | 4,355.3 | 35.7% |
| Manager PATCH requests | 930.0 | 588.0 | 36.8% |
| PATCH requests rejected with HTTP 409 | 51.7 | 2.3 | 95.5% |
| Actual GitResource status changes | 541.3 | 459.0 | 15.2% |
| Actual ClusterGitConfig status changes | 234.7 | 24.0 | 89.8% |

Mean wrapper status changes per complete create/update/delete lifecycle fall from
27.1 to 23.0. The selected runs range from 18 to 29 changes per wrapper; real Argo
status transitions remain mirrored rather than suppressed. Mean initial-create
throughput rises 28.1%, from 0.851 to 1.090 publications/s.

| Phase | Baseline p50 / p95 / max (s) | Selected p50 / p95 / max (s) |
| --- | ---: | ---: |
| Create to publication | 11.21 / 22.21 / 25.01 | 8.21 / 16.41 / 18.01 |
| Update to publication | 10.21 / 21.00 / 25.22 | 6.21 / 16.02 / 18.00 |
| Delete to CR removal | 13.20 / 23.00 / 26.01 | 10.36 / 16.80 / 18.80 |

## Implementation

- Publication and its GitDrift evidence now share one optimistic status patch.
  The focused test proves one completed-publication patch instead of two.
  Durable application identity and cleanup checkpoints remain separate.
- Configuration access results are buffered until the end of each reconcile.
  A verified Push takes precedence over its verification Fetch; unchanged
  evidence causes no patch. An already-current CredentialsLoaded condition
  avoids the extra configuration-status read/validation path. Recording still
  validates the configuration generation and Secret UID/version against fresh
  reads, so stale credentials cannot certify a rotation.
- Expected non-fast-forward contention uses `PublishPending` with a delayed
  retry instead of `PublishFailed`, `DeleteFailed` or false Push access failures.
  Auth and transport failures retain diagnostics. A lost push acknowledgment
  still succeeds only when remote content verifies the intended result.
- Each mutating attempt clones once, refreshes its branch, checks ownership,
  builds the path-scoped commit and pushes normally. Verification fetches into
  that same temporary repository and resets to the remote tracking head. The
  force refspec updates only a local tracking reference; it never force-pushes.
  Retries still reread the wrapper and use a fresh isolated directory.
- Retry delays grow from a 500 ms base with jitter: 250–750 ms, then 500–1,500 ms
  between local attempts, and 1–3 seconds when requeueing after three attempts.
  Delays obey cancellation. Read-only drift checks skip the pre-refresh.
- ApplicationSet status-only events no longer rerun inventory reconciliation.
  Application events enqueue observation for identity/tracking, copied
  source/destination or any raw status change. An unchanged Application payload
  keeps its associated generation/resourceVersion/time instead of creating an
  envelope-only status patch. Target generation continues to affect readiness.
  Unknown upstream fields, timestamps and errors remain intact.
- Existing metrics now distinguish Clone/Fetch/Push and status writer
  (publisher/observer/config) with success/conflict/error outcomes. The timing
  harness records manager HTTP counts, configuration status changes and writer
  outcomes so future tuning can measure both attempts and actual changes.

No new dependency, API field, global repository cache, serialization lock,
worker reduction, mirror debounce or slower observation interval was introduced.
Ownership, scoped credentials, pause guards, exact uncached target reads,
independent inventory reconciliation and durable cleanup boundaries remain.

## Validation

- `make lint-fix`: passed, zero issues.
- `make test`: passed race-enabled unit/smart-HTTP/controller/lifecycle tests,
  real Kubernetes 1.37.0 envtest checks, and all thirteen live E2E scenarios.
  The live suites completed in 284 seconds. Tests cover publication, no-op and
  rapid generations, branch races, uncertain acknowledgments, ownership,
  credential rotation/denial/recovery, independent handoff, bootstrap preservation,
  source-watch rebuilding, Git/Argo outages, status polling/limits, drift, pause,
  orphan retention and re-adoption.
- Focused tests prove combined publication/drift writes, buffered/no-op
  configuration writes, branch contention versus actual transport failure,
  snapshot stability, and watch filtering that retains unknown status changes.
- A further burst inside the full suite recorded 60 successful pushes, 56 rejected
  pushes, 116 full clones, 232 incremental fetches, 4,427 manager GETs, 595 PATCHes
  (two conflicts), 463 wrapper status changes and 30 configuration status changes.
  Create throughput was 0.901/s. This separate correctness run confirms the
  call-count savings but also shows completion-time variation; it is not pooled
  into the three-run strategy comparison. Its report is
  `reports/timing-v02-1791292430/results.json`.
- `go vet -tags=e2e ./test/e2e` and `git diff --check`: passed.
- Normal controller image restored after the source-watch test; existing
  `demo/example-config-three` checked Ready at its original publication revision.
  No teardown was performed.

## Evidence and reproduction

Raw `results.json` and `metrics.prom` files live under ignored `reports/`.
`reports/optimization/summary.json` contains the aggregated values and full raw
file paths; `reports/optimization/summarize.py` reproduces the aggregation from
those files. All run directories below have the `timing-v02-` prefix:

| Strategy | Run identifiers |
| --- | --- |
| Baseline | 1791289512, 1791289587, 1791289667 |
| API fixes + clone | 1791290091, 1791290169, 1791290237 |
| Fetch + short delay | 1791290410, 1791290475, 1791290541 |
| Fetch + 500 ms | 1791290741, 1791290816, 1791290882 |
| Fetch + 1,500 ms | 1791291109, 1791291205, 1791291287 |
| Pre-refresh + 500 ms | 1791291626, 1791291691, 1791291755 |
| Pre-refresh + 250 ms | 1791292128, 1791292190, 1791292255 |

With an existing managed stack, run the selected strategy three times:

```sh
make up
go test -tags=e2e ./test/e2e \
  -run '^TestManagedStack$/^twenty-concurrent-publications$' \
  -count=3 -v -timeout=15m
```

Run `make test` for the required full correctness suite. Avoid concurrent source
editing or another development loop while live E2E runs.

These are sequential local trials, not randomized isolated benchmarks. Git
history grows across trials, the machine has other activity, and one-second
polling limits timing precision. Three repetitions support this workload's
choice; they do not prove a globally optimal delay or a scale guarantee for
other repositories, network delays or worker counts. Changing those conditions
warrants measuring again. The baseline labeled clone durations as Fetch; its read counts are therefore
classified as full clones. The updated instrumentation distinguishes them.
Logical operation counts are not counts of underlying HTTP requests within the
Git protocol.

Upstream pointers informed the design rather than deciding the winner:
[controller-runtime's per-item exponential retry defaults](https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.0/pkg/controller/controller.go),
[Flux's jitter and progressing-with-retry approach](https://github.com/fluxcd/source-controller/blob/main/docs/spec/v1/gitrepositories.md),
and [go-git's Fetch/Reset options](https://github.com/go-git/go-git/blob/v5.19.3/options.go).
No upstream implementation was copied and no dependency upgrade was needed.
