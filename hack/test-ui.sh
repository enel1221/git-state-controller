#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../examples/approval-ui"
command -v node >/dev/null || { echo 'Node.js >=24.19 and npm are required; see examples/approval-ui/README.md.' >&2; exit 1; }
command -v npm >/dev/null || { echo 'npm is required for the approval demo tests.' >&2; exit 1; }
node --input-type=module -e 'if (Number(process.versions.node.split(".")[0]) < 24) throw new Error("Node.js 24+ is required")'
npm ci
npm test
npm run build
# Missing Playwright/browser assets fail the required suite; no silent skipping.
npm run test:browser
