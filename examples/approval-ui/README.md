# Local approval demo

A Vite/TypeScript client of the real Kubernetes contract. It reads only Namespaces,
GitResources and the GitResource CRD. It has no Git/Argo API, credentials in browser
storage, login, approval backend or persistent history. Status details come from
the wrapper's mirrors.

Install Node.js 24.19+ with npm and kubectl. Tested pins: Node 24.21.0,
Vite 8.3.3, TypeScript 7.0.2 and Playwright 1.63.0. Go/Kubernetes/Argo pins stay
unchanged. The generated package-lock.json pins the dependency tree.

```sh
make up                 # install v0.3 CRDs AND controller
make ui                 # install dependencies, build, and launch the local UI
```

`make ui` uses `bin/node/bin` when available, otherwise Node/npm from your PATH.
It requires the managed stack and runs in the foreground; Ctrl+C stops its helper
processes. For individual npm commands:

```sh
cd examples/approval-ui
npm ci
npx playwright install chromium     # required once for browser tests
npm run dev
```

If Node was installed locally in `bin/node/bin`, first export
`PATH="$PWD/bin/node/bin:$PATH"` from the repository root.

Open **http://127.0.0.1:5173**. The helper starts its own kubectl proxy at
127.0.0.1:8001 with this checkout's `.dev/kubeconfig` and explicit
`k3d-git-state-dev` context. It never falls back to the current context. Occupied
ports fail; no unrelated process is killed. Ctrl+C stops its Vite/proxy children
and leaves the cluster running. The UI is optional during controller development;
`make dev` does not recreate namespaces or reset approvals/history.

Vite forwards only an allowlist of Namespace list/get/create/annotation patches,
GitResource list/get/create/patch, and GET of the one compatibility CRD. Deletion
is a prepared action patch. There is no namespace deletion or raw DELETE button.
Raw DELETE can be demonstrated separately with kubectl. Secrets, arbitrary proxy
paths, target mutations and status writes are rejected. Host/origin/Fetch-Metadata
checks protect the local dev route; Vite filesystem access is limited to this
folder. kubectl rejects direct DELETE/PUT and restricts its paths as well.
These are local safeguards, not tenant authorization; never expose the app publicly.
[Official Vite server options](https://vite.dev/config/server-options) describe the
dev proxy and host controls.

The page polls about every three seconds while visible. Refreshes are serialized;
open forms retain unsaved input. Each request has an eight-second timeout. Every
existing-object edit uses UID/resourceVersion preconditions after verifying the
reviewed spec/identity. Approval copies exactly the reviewed advertised request;
conflicts are shown instead of silently approving newer intent. Only enabled,
nonterminating namespaces are creation targets. Removing enabled never strands
existing resources. The UI disables mutation for the old v0.2 schema. A schema
check is not proof that the installed controller enforces approval.

The review dialog accepts an optional approver name and email together. Inputs
start blank for each review. The UI patches request-bound `approved-by` JSON
alongside `approved-request`; anonymous approvals clear old attribution. New Git
commits include the approval request and an `Approved-by: Name <email>` trailer
when supplied. These names are unverified attribution and do not change the Git
author or committer. See [the approval contract](../../docs/approvals.md).

## Walkthrough

1. Create an enabled namespace with Require approval unchecked. Select it, create
   a ConfigMap using the editable repository/branch/path defaults, and leave
   attribution blank. Observe automatic publication and native Argo sync/health.
2. Create another namespace with Require approval checked. Submit an attributed
   ConfigMap. Inspect Details: there is a proposal/request but no published SHA.
   Approve after reviewing the current manifest. Observe deployment and consumed
   change metadata.
3. Edit its JSON manifest. Author/message start blank. Its prior approval cannot
   approve the new generation; the previous SHA and native deployment remain.
   Approve the new request. This unattributed edit uses the configured bot.
4. Request deletion with policy Delete. Approve if required. The controller issues
   Kubernetes DELETE and retains the visible wrapper until Git cleanup, inventory
   removal, Application deletion and direct-resource disappearance are verified.
   No second client-side deletion workflow is needed.
5. For another deployed wrapper, request deletion with Orphan. Approve it and
   observe the same Application and resource remain while the wrapper disappears.
   Recreate its namespace/path with the complete manifest, click Adopt, then
   approve the distinct Adopt request. The retained Application identity survives.
6. To show the raw entry point, run from the repository root:

   ```sh
   kubectl --kubeconfig .dev/kubeconfig --context k3d-git-state-dev \
     delete gitresource example -n YOUR_ENABLED_NAMESPACE
   ```

   A required namespace advertises a distinct Delete/Orphan request on the
   terminating CR. Approve it in the table. Without explicit Delete metadata,
   attribution is the bot and the action-specific default message.

Pending prepared deletion can be canceled before deletionTimestamp. Approval may
start it immediately; the warning is intentional. Normal spec edits are disabled
once deletion begins. Pause/resume, meaningful approval and details remain usable.
The policy toggle warns that disabling approval releases pending work and displays
the actual stored value. There is no finalizer-stripping or namespace-delete UI.
Deleted CRs do not provide an audit record; use the repository history docs.

## Required checks

```sh
npm test                # pure client/proxy boundary checks
npm run build           # strict TypeScript check + Vite production build
npm run test:browser     # Chromium journey against the dedicated live stack
```

`make test` includes these checks through `make test-ui`. Missing Node/npm or
Chromium fails rather than skipping. On Linux CI, install browser/system libraries
with `npx playwright install --with-deps chromium`; see
[Playwright browser installation](https://playwright.dev/docs/browsers).
By default, browser tests start their own UI/proxy and require free ports. If this
checkout's `make ui` is already running, use `GIT_STATE_UI_REUSE_SERVER=1 make test`
(or `make test-ui`) to explicitly reuse it. The test leaves that server running.
The browser test creates its own enabled required/automatic namespaces and cleans
up only those. It tests stale review, blank fresh attribution, create/update/delete,
and the proxy boundaries. It does not read Git credentials or target resources.
