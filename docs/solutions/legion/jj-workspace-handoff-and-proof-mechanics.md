---
title: "jj workspace, handoff, and proof mechanics for Legion phase workers"
category: legion
tags:
  - jj
  - legion-workflow
  - handoff
  - proof
  - review-rounds
date: 2026-09-25
status: active
module: legion-workflow
related_issues:
  - "S394355701-1"
  - "sjawhar/legion-smoke#205"
---

# jj workspace, handoff, and proof mechanics for Legion phase workers

Durable learnings from delivering PR sjawhar/legion-smoke#205 (issue S394355701-1), a
Stage 3 workflow proof whose product change was appending four lines to `smoke.txt`
(one workflow-proof line plus three review-round lines). The diff was trivial; the
reusable content is the Legion delivery mechanics, verified against the committed
`.legion/` handoffs and the branch/origin reflogs.

## 1. Cite the pushed bookmark tip, never a raw jj working-copy id

A jj working-copy commit id is rewritten on every content change to that commit, so a SHA
snapshotted before the final export/push is ephemeral. On this PR the round-3 implement
handoff recorded `headSha: dc49f4767187…` for its criterion-1 proof, but that id appears
nowhere in the branch or origin reflog — it was the working-copy id ~9s before the export
rewrote it to the actual pushed tip `bad67b70a886…`. The tester, re-running independently,
cited the real ref tip.

Rules that avoid the stranded-SHA trap:
- Prove through the ref, not a commit id: read `?ref=legion/<KEY>` (the GitHub contents
  API) for the content, and read the tip SHA back **from the ref after the final push**
  (`legion gh -- api repos/<owner>/<repo>/git/ref/heads/legion/<KEY> --jq .object.sha`).
- When you must name a commit SHA in a handoff/PR body, name the **product commit** (the
  code commit) or the pushed tip, and re-read it post-push — not the pre-push `@`.

## 2. Keep scratch files out of the tracked workspace

Writing a scratch file (a PR-body draft) inside the tracked tree — even under `.legion/` —
adds it to the working-copy commit and forces jj to rewrite that commit's id, which in turn
moved the product commit and required an extra reconciling push. The branch bookmark reflog
showed 16 local `export from jj` movements against only 6 real `update by push` events; the
surplus is exactly this working-copy churn. Write PR bodies and any transient artifact to
`/tmp`, pass them with `--body-file`, and reason about "what shipped" from the **origin push
reflog**, not the noisy local bookmark reflog.

## 3. Get code→handoff commit order with `jj new`, not `jj split`

`jj split -m "…" .legion/<phase>.json` puts the *selected* file into the **parent** commit
and leaves the remainder in `@`, which reorders the handoff **below** the code change. The
clean shape (code commit, then handoff commit on top) is: make and describe the code commit,
`jj new`, write the handoff into the fresh empty child, describe it
`implement: record handoff (round N)`, move the bookmark to it, push. After completing a
round, `jj new` again so `@` is empty — `handoff_complete` refuses to finish while the
handoff file shows as an uncommitted working-copy change (which it does when `@` *is* the
handoff commit).

## 4. Only the implementer pushes; other roles' handoffs ride that push

`legion-implementer[bot]` is the sole App with `contents` permission; every origin push on
this branch was authored by it. The planner's and tester's `record handoff` commits are
tracked files committed into the shared workspace and delivered upstream by the implementer's
next export/push. Consequence: a role that cannot push (planner, tester, reviewer) must have
its handoff **committed before** the implementer's final push of that round, or it will miss
the window and not appear at the approved head. Verify authorship per-commit before pushing
(`author.email()` / `committer.email()`); your own commits must be the implementer bot,
earlier phases' commits legitimately carry their own role's App.

## 5. Review-round appends are in-scope same-PR corrections

The approved spec (plan concern #3) pre-authorized "one more appended line per review round,
in scope for this issue, not a new ticket." Execution matched: rounds 1–3 each appended one
line to the same `smoke.txt` on the same branch/PR, each as a `Stage 3 review round N` code
commit plus an `implement: record handoff (round N)` commit, re-pushing the existing
bookmark. A review round that requests a bounded edit satisfying the same acceptance criteria
is a correction, not new scope — do not spawn a new issue/branch. Planners should emit this
in-scope clause up front so the implementer does not stall.

## 6. Proving a change with no test suite

This repo has no build/lint/test suite, so the production-like surface is the GitHub contents
API. The reusable proof triple for a suite-less content change:
1. Positive: read the file at `?ref=legion/<KEY>` and observe the new content.
2. Negative control: the **same** read at `?ref=main` (the pre-merge base) returns the
   unchanged tail.
3. Tester genuinely **re-runs** the identical command (not an echo of the transcript) and
   reaches the same bytes.
Acceptance criteria only observable on `main` after merge (here: "line present on main") are
correctly parked as the post-merge production check, not a testable-now gap.

## 7. `bun` may be absent in a devbox pane — use the `legion` tool's `handoff_write`

The `legion-worker` skill offers a `bun -e '…' | legion handoff write` pipe for editing an
existing handoff. In this pane `bun` was not on PATH (`mise ERROR No version is set for shim:
bun`). The portable path is the `legion` tool's `handoff_write` op with a `data` JSON object,
which needs no local `bun`/`jq`.

## Flagged process gap: approved head still carried `.legion/`

The reviewer approved `bad67b70a886`, which still contained `.legion/{plan,implement,test}.json`
as tracked files; no `.legion/` deletion commit preceded approval in this run (the standard
merge-gate step 2 — implementer pushes the `.legion/` deletion, reviewer approves that cleaned
head — was skipped/compressed). If merged as-is, coordination scratch (including the stale
`headSha` from learning 1) would land on `main`. Future runs: gate approval on a product-only
diff, and perform the `.legion/` deletion before the reviewer's final approval. (Retro itself
must not delete it here — the instruction was not to change the approved implementation — but
records the gap as the durable learning.)
