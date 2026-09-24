---
title: "Legion phase mechanics: handoff-commit SHA churn and single-file product-diff proofs"
category: legion
tags:
  - legion
  - jj
  - handoff
  - pull-request
  - e2e-proof
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S390560926-1"
  - "sjawhar/legion-smoke#134"
---

# Legion phase mechanics: handoff-commit SHA churn and single-file product-diff proofs

Durable learnings from the Stage 3 workflow proof (issue S390560926-1, PR
sjawhar/legion-smoke#134): a one-file change (`stage3-proof.txt`) taken through the full
implementer/tester/reviewer/retro loop, including three same-PR correction rounds. The change
was trivial; the reusable value is in the jj/handoff/PR mechanics the loop exercised.

## 1. Handoff-commit SHA churn: settle, push, then prove

A phase writes its handoff JSON into the working copy and separates it into its own
`<phase>: record handoff` commit. **Rewriting that handoff file changes the commit's git SHA.**
Any E2E "head SHA" cited before the final push therefore goes stale the moment the handoff is
rewritten, and can even become unreachable on the pushed branch.

Observed concretely: the implement handoff was written citing head `ff52b485`, then the handoff
was split and rewritten, moving the branch tip to `4353bb75` and then `ed9e15f8`. Each rewrite
invalidated the previously cited head.

Rule: **settle all handoff content first, then `jj split` the handoff commit, then push, then run
the E2E proof against the pushed head, and cite that final pushed SHA** in both the `.legion`
`proof` array and the PR body's `E2E` line. Do not cite a head you are about to rewrite. Downstream
roles (tester, reviewer) must run their checks only against a head the implementer has already
pushed — confirm the push is live on GitHub before running, never verify an intermediate
working-copy SHA.

## 2. AC2-style single-file product diff while `.legion/` is on the branch

`.legion/*.json` handoff files are legitimately committed on the PR branch until the implementer
deletes them at end of clean review. They appear in GitHub's changed-files list, so a bare
`--json files --jq '.files[].path'` shows two-plus paths and looks like a violation of a
"exactly one product file" criterion.

Rule: **verify the product diff with an explicit filter and record the exact filtered command**,
so the reviewer re-runs the same check on GitHub rather than trusting a paraphrase:

```sh
legion gh -- pr view <n> --repo <owner>/<repo> --json files \
  --jq '.files[] | select(.path | startswith(".legion/") | not) | .path'
```

State in the proof's observation that the raw list also carries `.legion/*.json` process
artifacts that are deleted at end of clean review and are not product changes. Pair it with a
negative control (a deliberately-broken input) — e.g. reading a nonexistent path returns
`Not Found (HTTP 404)` — so the check demonstrably distinguishes pass from fail.

## 3. Multi-round corrections on the same PR

A review that requests a correction (here: "append one more line") is a **new implementing
phase**, not an edit of the prior commit. Each round:

1. Implementer edits the product file only (never touches `.legion/` in the product commit),
   `jj describe`s it as a distinct `stage3-proof: append ...` commit.
2. Implementer writes the round's `implement.json`, splits it into an `implement: record handoff`
   commit, moves the bookmark, and pushes.
3. Tester writes its own `test.json`, split into a `test: record handoff` commit — authored by
   the review App (`legion-reviewer[bot]`), because only the implementer App has `contents`
   (push) permission. That commit is **not** pushed by the tester; it rides the implementer's
   next push.

Result: a tall stack alternating product commits and handoff commits. Authorship is mixed by
design — product/implement commits are `legion-implementer[bot]`; plan/test commits are
`legion-reviewer[bot]`. Before pushing, confirm each of *your own* commits carries your role's
App in both author and committer columns; other roles' commits legitimately carry theirs.

## 4. Approve only a head that carries no `.legion/`

Risk seen in this run: the reviewer's `APPROVE` landed on a head that still carried `.legion/`
files (no separate `.legion/`-deletion push had occurred). Per `legion-worker`, approval is only
valid for a head that carries no `.legion/` — the cleaned head that differs from the reviewed
head by the `.legion/` deletion alone.

Rule: the implementer pushes the `.legion/` deletion at the reviewer's direction; the reviewer
approves **that** cleaned head by SHA. Retro then commits only under `docs/solutions/` on top of
the approved head, which does not void the approval. The merger proves the tip equals the
approved head plus `docs/solutions/`-only commits with
`jj diff --from <approved-sha> --to <tip-sha> --summary`. If any product change slips in after
approval, it voids the approval and the head returns to review.
