---
title: "Proving a content-only change on the git/jj PR surface when the repo has no test harness"
category: legion
tags:
  - proof
  - content-only
  - jj
  - review-threads
  - legion-workflow
date: 2026-10-01
status: active
module: legion-worker
related_issues:
  - "S391594033-1"
  - "sjawhar/legion-smoke#482"
---

# Proving a content-only change on the git/jj PR surface when the repo has no test harness

When the only change is file content (a docs/marker/config file) and the repository has no unit or
integration suite, the production-like surface is the git/jj PR branch itself. Prove it there, with
negative controls — never lean on CI.

- **CI green is not evidence of file content.** A no-op CI job (`sleep 20 && exit 0`) proves nothing
  about the diff. Treat the named git/jj commands as the verification, and say so.
- **Verify exact bytes with `cat -A` + `wc -l`, never a terminal glyph.** Run
  `jj file show -r <head> <path> | cat -A`. In `cat -A`, `M-bM-^@M-^T` is the em dash U+2014 and a
  trailing `$` confirms the newline. Write non-ASCII bytes deliberately
  (`printf 'S391594033-1 \xe2\x80\x94 Primary durable workflow profile\n' > <path>`); a pasted glyph
  can silently become an en dash (–) or hyphen (-) that renders the same.
- **Pair every criterion with a negative control.** For an added file the control is its absence on
  the base: `jj file show -r main@origin <path>` → `Error: No such path`.
- **Scope "only one file changed" to repository-content paths.** Run
  `jj diff --from main@origin --to <head> --summary` and exclude workflow/tooling paths
  (`.legion/`, `.jj/`, `.git/`, `.codegraph/`). Under the Go daemon the `.legion/*.json` handoffs
  stay in the PR diff through merge and the operator strips them in a post-merge follow-up PR — do
  **not** delete `.legion/` pre-merge to satisfy the one-file check; filter it out of the assertion.
- **For an append-only change, prove the per-round hunk, not just the final content.**
  `jj diff --from <prev-head> --to <head> --git <path>` must show exactly one `+` line and zero `-`
  lines (header advancing `@@ -1,N +1,N+1 @@`). A final-content-only check would miss a mid-file
  rewrite; the zero-deletion hunk is the real invariant. Bound the shape (one append, earlier lines
  untouched), not a fixed total line count — review rounds add one line each.
- **Cite the head SHA per proof.** Content lands at the code commit and is unchanged under the
  handoff commit stacked above it; record "unchanged at tip" so the tester re-drives at the head
  that merges rather than a moving branch ref.
- **Disposition a bot-opened review thread; do not self-close it.** The implementer is the finding's
  subject: reply `Declined: <reason>` or `Fixed in <sha>: …` and cite the reply id. Only the Legion
  reviewer's `Accepted:` resolves a bot's thread, and `legion threads resolve` closes it on that
  acceptance. Never block the merge gate trying to self-close it.
- **Reply to a review thread through the REST replies endpoint, not GraphQL.** The `legion gh` shim
  refuses GraphQL mutations (the merge guard trips on any mutation), so
  `addPullRequestReviewThreadReply` fails. Use
  `legion gh -- api repos/<owner>/<repo>/pulls/<n>/comments/<comment-id>/replies -f body=…`.
- **With no test harness, state the absence; never fabricate a red test.** Record "no affected
  tests; no red test warranted — no defect found" and name the git/jj proof commands as the
  verification.

## Evidence

sjawhar/legion-smoke PR #482 (Dispatch issue S391594033-1) added one file, `smoke/S391594033-1.md`:
a pinned first line `S391594033-1 — Primary durable workflow profile` (em dash U+2014) plus one
`Stage 3 review round N (S391594033)` line per review round (three rounds → four
newline-terminated lines). The repo has no application code and no suite; CI is
`.github/workflows/ci.yml` (`sleep 20 && exit 0`) plus a `gate` job that fails only if a `.fail-me`
file exists.

- Exact-content proof: `jj file show -r a68b546f smoke/S391594033-1.md | cat -A` →
  `S391594033-1 M-bM-^@M-^T Primary durable workflow profile$` and three round lines; negative
  control `jj file show -r main@origin …` → `Error: No such path`.
- One-file proof: `jj diff --from main@origin --to a68b546f --summary` showed
  `A smoke/S391594033-1.md` plus `.legion/{plan,implement,test,review}.json`, filtered with
  `grep -v '.legion/'`.
- Per-round hunk proof: each round's `jj diff --from <prev> --to <head> --git` showed a single `+`
  line, zero `-` lines (round 3: `@@ -1,3 +1,4 @@`).
- Review thread `PRRT_kwDOUCdo886oHiTf` (opened by bot `sjawhar-agent`, "is this file change
  needed?"): implementer replied `Declined:` (reply `r4159965549`) via the REST replies endpoint
  after the GraphQL mutation was refused by the shim; the Legion reviewer posted
  `Accepted: not a defect`; `legion threads resolve` closed it on that acceptance.
- Both proofs recorded on the PR: `E2E (implementer)` at head a68b546f and `E2E (tester)` at head
  bd833075, each with surface, command, observation, head SHA, and negative control.
