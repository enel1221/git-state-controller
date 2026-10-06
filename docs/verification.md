# v0.2 validation

Validated on 2026-10-06 against the owned Podman/k3d stack.

| Component | Tested version |
| --- | --- |
| Go | 1.26.8 (linux/amd64) |
| Podman | 5.8.7 |
| k3d | 5.9.0 |
| k3s / kubectl | 1.37.1+k3s1 / 1.37.1 |
| Skaffold | 2.25.0 |
| Forgejo | 16.0.5 |
| Argo CD | 3.5.3 |
| controller-runtime / go-git | 0.25.0 / 5.19.3 |
| envtest Kubernetes | 1.37.0 |

`make up`, `make test`, `make lint-fix`, and lint with e2e build tags passed.
The full test command includes race-enabled unit/real smart-HTTP Git tests,
real API-server admission/opaque-status/conflict tests, and all 13 live cases.
Live cases include `make dev` source watching, bootstrap/redeployment,
Forgejo and Argo outages, restart recovery, pause, drift, opaque status,
target-only polling with Argo stopped, large-status recovery and publication,
and orphan/re-adoption preserving Application and ConfigMap UIDs.
The new same-path race test confirms one owner and safe loser deletion.
Durable cleanup-checkpoint tests finish the API handoff with Git unavailable,
without repairing later branch edits or changing the retained pin.

Cleanup ownership, global kubeconfig registration/removal and Podman socket
handling passed lifecycle fixture tests. The user's live cluster was preserved;
v0.2 validation did not run destructive `make clean` on their existing resources.
Its cleanup path is unchanged from Phase 1's tested destroy/recreate workflow.

## Measurements from the final run

20 distinct resources shared one repository/branch with four publication workers.
These results use one-second polling and include API overhead; they are not
hardware-independent performance promises. Raw per-case timestamps, status-write
counts, snapshot sizes, Git durations/retries and private metrics are in the
ignored `reports/timing-v02-1791265779/` directory.

| Measurement (seconds) | p50 | p95 | max |
| --- | ---: | ---: | ---: |
| Create to publication | 10.409 | 23.145 | 23.191 |
| Publication to observed sync | 0.999 | 1.001 | 1.002 |
| Update to publication | 9.008 | 20.984 | 23.810 |
| Updated publication to observed sync | 0.998 | 1.001 | 1.003 |
| Delete request to CR removal | 10.797 | 22.404 | 22.989 |
| CR removal to downstream deletion | 0.003 | 2.003 | 3.604 |

Initial-publication throughput: 0.83/s.
Manager-wide status patches during measurement: 573;
rejected-push retries: 162.
These counters can include other active wrappers; raw Git records and per-case
status-write counts are scoped to the 20 fixtures. No-op observation and
metadata-only source updates are separately checked for zero status writes.
