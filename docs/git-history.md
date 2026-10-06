# Git history and audit are external read operations

## Decision

`git-state-controller` publishes desired manifests and records a successful publication SHA. It does **not** provide a Git history API, maintain a commit ledger in Kubernetes, append audit files to the repository, or implement a rollback workflow.

An application needing history, audit views, diffs, or restore previews should query Git on demand, or use its Git hosting service's API in a separate read service. The controller's portable Git publication must not acquire a Forgejo/GitHub/GitLab dependency just to support a history UI.

The identifying inputs are the repository URL, branch, file path, and optional `GitResource.status.lastPublishedRevision`. A service using go-git can use `Repository.Log` with `LogOptions.FileName` or `PathFilter`; a hosting-provider client can offer an equivalent path-filtered query. Pagination, authentication, caching, merge-history presentation, and access controls belong in that read service, not in reconciliation. [1][2]

## Inspect one file in a shared repository

Run these commands in a clone with enough history. They are documentation examples, not a new runtime dependency on the Git executable:

```sh
file='resources/demo/example-config/resource.yaml'

git fetch origin

# Changes relevant to this file, even when other CRs share the branch.
git log --format='%H %aI %s' origin/main -- "$file"

# Latest commit that affected this path on the branch.
git log -1 --format=%H origin/main -- "$file"

# Previous file-changing commit in the displayed path history.
git log -1 --skip=1 --format=%H origin/main -- "$file"
```

A previous file-changing commit does not exist when the file has only one change. The latest change may be a deletion, in which case the file is absent at that commit. `origin/main` is an example; use the CR's actual branch. The initial example uses direct commits on a fixed path, not a merge/rename workflow. In repositories with merges, Git's path-history simplification affects what is shown; the external read service should explicitly choose its ancestry/merge presentation. [1]

To inspect history as of the controller's publication rather than today's branch head:

```sh
published='<GitResource.status.lastPublishedRevision>'
git log --format='%H %aI %s' "$published" -- "$file"
```

For a selected revision that contains the file:

```sh
revision='<selected-commit-sha>'
git show "$revision:$file"
```

To compare two selected snapshots:

```sh
older='<older-commit-sha>'
newer='<newer-commit-sha>'
git diff "$older" "$newer" -- "$file"
```

Git provides path-limited history and retrieval of a path at a revision. A shallow clone may need deepening before older changes are available. No CR status update is required to perform any of these reads. [1][3]

## Do not confuse the identifiers

| Identifier | Meaning |
|---|---|
| Branch-head commit SHA | Current repository snapshot. Another CR's file may have produced that commit. |
| Last file-changing commit SHA | A commit selected from history because it changed this path. |
| Previous file-changing commit SHA | The preceding relevant change under the selected history traversal, not necessarily the parent of branch HEAD. |
| Blob object ID | File content identity inside Git; it is not a complete repository revision for Argo to deploy. |
| Last published SHA | The revision our controller verified and handed to Argo. It need not remain branch HEAD. |

Using `HEAD~1` is not a reliable way to find this file's previous change in a shared branch: that commit may concern another file. [1][3]

## Restoring an old version

Retrieve the selected old manifest, review it, and put the desired business fields into `GitResource.spec.manifest`. Leave controller-reserved ownership annotations to the renderer. Ordinary reconciliation publishes a new forward commit and advances that resource's Application pin.

A Git-only revert is an external branch change. Under v0.2's informational-drift policy it does not update the CR and does not automatically change Argo's SHA pin. Do not force-reset the shared branch or treat a UI rollback to an old Argo revision as an update of the wrapper's desired state. This controller does not silently import Git back into the CR.

## Audit limits

Commit trailers provide useful attribution to wrapper namespace/name, UID and generation. They are not proof of an authenticated human approver, a tamper-proof audit log, or approval evidence. Git history is not a promise of one commit for every rapid intermediate CR edit. Deletion removes the file from the branch's current tree, not from all previous commits.

An external audit service may correlate Git records with Kubernetes audit records and future approval records. Those integrations are explicitly outside this small example. The raw Argo status mirror may include Argo's own existing deployment history; that is not a substitute for path-scoped Git history and is not independently accumulated by our controller.

## References

[1] Git log documentation: https://git-scm.com/docs/git-log  
[2] go-git LogOptions: https://pkg.go.dev/github.com/go-git/go-git/v5#LogOptions  
[3] Git revision/path syntax and object naming: https://git-scm.com/docs/gitrevisions
