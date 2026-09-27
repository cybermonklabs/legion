---
title: "Legion implementer: jj commit ordering for proofs, and docs-only PR proof discipline"
category: legion
tags:
  - legion
  - jj
  - implementer
  - handoff
  - proof
  - docs-only
date: 2026-09-27
status: active
module: legion-worker
related_issues:
  - "S393381512-1"
  - "sjawhar/legion-smoke#359"
---

# Legion implementer: jj commit ordering for proofs, and docs-only PR proof discipline

Distilled from S393381512-1 / PR #359 (sjawhar/legion-smoke): an implement phase plus three
`CHANGES_REQUESTED` review rounds, each appending one line to `README.md`. The change is
trivial; the durable value is in the phase-worker mechanics below.

## 1. Handoff-as-parent is a proof-SHA cycle — put the code commit first, finalize it last

The implement handoff (`.legion/implement.json`) requires a `proof[]` where each entry names a
`headSha`: the commit the proof was taken at. The trap: if you write the handoff into the *same*
working-copy commit as the code change and then `jj split` the handoff out, `jj split` puts the
**selected file (the handoff) into the parent** and leaves the code change as the child. The
handoff commit becomes the *parent* of the code commit. You now cannot cite a stable code SHA:
editing the handoff (to fix the very SHA you are recording) rewrites its child — the code commit —
so the SHA goes stale the moment you write it.

Observed on this issue: the first implement pass described the code commit as `de6f5164`, then
`handoff_write` added `implement.json` to that same working copy and `jj split` reordered it to
`handoff -> code`. The proof cited `de6f5164`, which no longer existed after the split.

**Correct pattern (what shipped):**

1. Make the code change in `@`, `jj describe` it. This is the code commit.
2. `jj new` — an empty working copy *above* the code commit.
3. `handoff_write` — writes `.legion/<phase>.json` into that empty working copy.
4. `jj split -m "<phase>: record handoff" .legion/<phase>.json` — since the working copy holds
   only the handoff file, split yields `code -> handoff(@-) -> empty(@)`. The code commit is the
   parent; its SHA is now stable because nothing above it feeds back into it.
5. Read the code commit's SHA and run the proof against it *after* the ordering is final; write
   the proof with that SHA.

If you already landed in the reversed order, recover forward (never `jj abandon`): consolidate
with `jj squash --from <handoff> --into <code> -u` (`-u` keeps the code commit's description),
`jj restore --from <plan-handoff-sha> .legion/<phase>.json` to drop the handoff file back out,
then rebuild via steps 2–5. The `-u` flag matters: bare `jj squash`/`jj split`/`jj describe`
open an editor and fail in a headless worker pane (`jj refused to open an editor without a TTY`).

## 2. Prove against a reference that survives rewrites; re-derive after any reorder or rebase

A jj working-copy id is not durable. Prefer a reference a reader actually reaches and that does
not move when commits are rewritten: the PR `headRefOid`, or the GitHub Contents API at
`repos/<owner>/<repo>/contents/<path>?ref=<sha>`. On this issue the tester proved the round-3
line by reading the Contents API at the PR head and using a negative control at the parent
commit (`e5491efa` returns the round-2 line) — race-free and immune to local-id churn. After any
reorder (section 1) or conflict-forced rebase, re-run the proof and update the `headSha`; do not
trust a number captured before the rewrite.

## 3. Docs-only PRs skip ce-simplify-code and the thermonuclear pair — but make it an explicit plan decision

Per `legion-worker`, a docs-only diff runs neither `ce-simplify-code` nor the thermonuclear
review pair, and the reviewer's PR body carries no `Thermo` line. This is only safe when it is a
*stated* decision, not a silent omission: the planner set
`requiredSkills.{implement,test,review} = "none"` with a one-line justification ("README is
markdown/text, no runtime code"), which is what let the reviewer approve a thin pipeline without
it reading as a gap. If your diff touches any runtime code, these passes are mandatory.

## 4. A top-level CHANGES_REQUESTED with no inline threads is answered by the corrective commit

Each review round here was a bare `CHANGES_REQUESTED` naming an edit, with zero inline review
threads. The correct "reply" is: make the requested edit as a new commit, push it, note it in
the handoff `deviations`, and post one PR comment (`Fixed in <sha>: <one line>` + Legion footer).
`legion threads resolve --pr <n> --repo <owner>/<repo>` still runs before the answering push and
reports `no unresolved threads` — there is nothing to resolve because the reviewer opened no
thread. (When the spec pre-authorizes the change — here "one more line per review round" — no new
spec approval is needed; cite the spec Requirements in the deviation.)

## 5. Read CI from the status rollup at the exact head, not from `gh run watch` exit codes

The repo CI is deliberately slow-but-green (`check` = `sleep 20 && exit 0`) with a red lever
(`fail-on-demand`/`gate` fails iff a `.fail-me` file exists) — not flaky. Two operational facts
seen this issue: (a) `legion gh -- run watch <id> --exit-status` returned a spurious exit 1 once
while `gh run view <id> --json conclusion,jobs` reported `success` — trust the run's
`conclusion`, verify with `run view`, and file the watch discrepancy; (b) the Actions runner can
sit `queued` for minutes, so a `run watch` loop may exceed a foreground timeout — poll
`run view` for the final `conclusion` rather than blocking. Cite the concrete run ids and the
`headRefOid` in the PR body's `CI` line.

## Process observations (for the architect / merger, not fixed by retro)

- `.legion/*.json` handoffs are committed to the branch and were still present at the approved
  head `f6e0a35d`; the review-cleanup `.legion` deletion did not run before retro in this flow.
  Retro adds only `docs/solutions/`, so the merger's
  `jj diff --from <approved> --to <tip> --summary` excluding `.legion`/`docs/solutions` is still
  empty. In the canonical flow the implementer pushes the `.legion` deletion at the reviewer's
  direction and the reviewer re-approves that clean head before retro.
- The tip `49da310d` (the reviewer's own `review.json` handoff commit) sits one commit above the
  formally approved head `f6e0a35d`; that is a `.legion`-only commit, so the approval stands for
  the merger's product-diff check.
