#!/usr/bin/env bash
# Every row of .github/ci-tools.yaml printed beside the two call sites that read its digest: the
# line of the manifest that holds the literal, and what reads that row in an action or in the rig.
# A row nothing reads fails the job, since its digest is then pinned and read by no install.
#
# Who runs it: the step "Print every row beside the two call sites that read its digest" of
# .github/workflows/ci-job-tool-fallback.yaml's tool-fallback job;
# .github/ci-notes/ci-job-tool-fallback.md#why-every-row-is-printed-beside-the-two-call-sites-that-read-its-digest
# says why one literal is the evidence. It takes no argument. That job checks this repository
# out in its first
# step, above every step below it, so this script is in the checkout when the step runs.
#
# Run from the repository root: .github/scripts/tool-fallback-row-call-sites.sh
# CI runs it in ci-job-tool-fallback.yaml's tool-fallback job, at the step "Print every row beside
# the two call sites that read its digest".
set -euo pipefail

# This script downloads nothing itself: `ci_tools_rows`, `ci_tools_digest` and `ci_tools_field`
# read .github/ci-tools.yaml off this checkout, and the two greps read this tree. The grep of the
# rig's path is written outside the row loop, which is also one walk of those trees instead of one
# per row: written inside it, .github/scripts/check-download-retry.sh read it as a run of that
# script once per pass of a loop whose passes it cannot count, and could put no figure on the step
# at all.
# shellcheck source=.github/scripts/ci-tools.sh
. .github/scripts/ci-tools.sh
rows=$(ci_tools_rows)
unread=""
# Every line of an action or of the rig that reads a row, read once for the whole run rather than
# once per row: the walk of those two trees finds the same lines whichever row is being printed,
# and each row's own lines are selected out of this text below.
# The lines that read the row rather than every line that says its name: a ci_tools_* call that
# names it, and the `row=` an action assigns it to where the call site names the row through a
# variable (.github/actions/setup-bun does).
reader_lines=$(grep -rn -E 'ci_tools_|row=' \
  .github/actions .github/scripts/ci-tools-proof.sh || true)
for row in $rows; do
  digest=$(ci_tools_digest "$row")
  echo "$row: sha256 $digest, of $(ci_tools_field "$row" digest_of)"
  pinned=$(grep -n "digest: $digest" .github/ci-tools.yaml || true)
  readers=$(printf '%s\n' "$reader_lines" | grep -F -- "$row" || true)
  echo "  the manifest's own line:"
  printf '%s\n' "$pinned" | sed 's|^|    .github/ci-tools.yaml:|'
  echo "  what reads that row, in the action or the rig:"
  printf '%s\n' "$readers" | sed 's|^|    |'
  { [ -n "$pinned" ] && [ -n "$readers" ]; } || unread="$unread $row"
done
if [ -n "$unread" ]; then
  echo "::error::nothing reads$unread, so that row's digest is pinned and read by no install" >&2
  exit 1
fi
