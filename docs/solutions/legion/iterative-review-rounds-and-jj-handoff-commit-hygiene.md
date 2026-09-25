---
title: "Iterative review rounds and jj handoff-commit hygiene"
category: legion
tags:
  - jj
  - legion-worker
  - handoff
  - review-rounds
  - proof-discipline
date: 2026-09-25
status: active
module: legion-implementer
related_issues:
  - "S393609063-1"
  - "sjawhar/legion-smoke#302"
---

# Iterative review rounds and jj handoff-commit hygiene

Reusable lessons from a Legion implement phase that went through three review-round
corrections on a jj-backed shared workspace (PR #302). The change content was trivial
(appending exact lines to `smoke.md`); the durable value is the jj/Legion mechanics that
recur on every implement phase regardless of the change.

## 1. Never let one working-copy commit carry both code and its `.legion/` handoff

**What happened.** In round 0 the handoff was written with `handoff_write` while `@` was the
same working-copy commit that carried the `smoke.md` change. `jj split .legion/implement.json`
then separated them but placed the handoff commit *below* the code commit, forcing a
`jj rebase -r <handoff> -d <code>` reorder to restore `code -> handoff` order. The reorder
rewrote SHAs and left the already-pushed impl SHA (`98780dd6`) superseded.

**Why it matters.** A superseded pushed SHA orphans a green CI run and any review anchor
pinned to it; per-commit references in the PR body then point at a commit that is no longer on
the branch.

**Rule.** Author the handoff in its *own* commit from the start. Concretely, per round:
1. Make the code correction its own commit and `jj describe` it.
2. `jj new <correction>` to open a fresh empty commit.
3. `handoff_write` into that empty commit, `jj describe` it as `implement: record handoff (round N)`.
4. `jj bookmark set <branch> -r @`, then push.

This keeps the reviewer's/tester's diff free of handoff bookkeeping and never needs a
post-push reorder. If a split is ever unavoidable, do it *before* the first push; after any
reorder that rewrites a pushed SHA, re-verify CI/threads against the new head and update the
handoff's recorded SHA.

## 2. `handoff_complete` requires an *empty* working-copy commit, not merely "all committed"

**What happened.** `legion handoff_complete` failed with
`.legion/implement.json has changes in the working copy that are not committed` even though the
handoff was in its own commit — because in jj the current `@` *is* the working copy, so when
`@` was the handoff commit, its content read as pending changes.

**Rule.** Before calling `handoff_complete` on a jj workspace, run `jj new` so `@` is a clean
empty commit sitting above the committed handoff. The gate wants no pending working-copy
changes; an empty `@` provides that deterministically. Set the branch bookmark on the handoff
commit (`-r @-` after the `jj new`, or on the handoff commit before opening the empty `@`),
not on the empty `@`.

## 3. Review approval does not imply the head is merge-clean (`.legion/` still present)

**What happened.** The reviewer `APPROVED` head `b1ccc95d`, which still carried
`.legion/{plan,implement,test}.json`. The normal merge-gate step 2 (implementer pushes the
`.legion/` deletion, reviewer approves *that* clean head) did not run before approval in this
run; a targeted human Dispatch instruction moved the pipeline to retro directly.

**Rule.** Treat `.legion/` removal as a distinct pre-merge step that the merger must still
verify *after* approval — never assume an approved head is stripped of `.legion/`. When a human
Dispatch instruction short-circuits the review loop, explicitly confirm the remaining
merge-clean steps still execute. Retro itself writes no `.legion/` file and commits only
`docs/solutions/`, so it never re-dirties the tree.

## 4. Append/marker proofs need exact-line matching plus two controls

**What happened.** `smoke.md` already contained prefixed variants (e.g.
`Stage 3 smoke proof: Legion workflow verified …`) and a previous issue's rounds. A substring
grep would false-pass. The proof used `grep -Fxq '<exact line>'` (whole-line, fixed-string)
paired with two controls: (a) a garbage negative control (`'<line> NOPE'` / `NOT PRESENT`
→ exit 1) proving the check discriminates, and (b) a *before-append* run of the exact grep
returning exit 1, proving *this* commit added *this* line. The tester then re-ran the
implementer's exact commands independently rather than trusting recorded output.

**Rule.** For any append/marker acceptance criterion:
- Match with `grep -Fxq` (exact whole line), never a substring.
- Keep two controls: a non-existent-line negative and a pre-change absence check.
- Have the next phase re-run the prior phase's exact command, not trust its transcript.

## 5. Keep each review-round correction a minimal single-purpose commit

Each round appended exactly one line and touched no other file, so the tester could confirm
the correction diff was a single added line. One-line-per-round keeps the before/after
negative control meaningful and the reviewed diff minimal. Pre-authorize the expected
follow-up in the plan (here plan task T2/AC2 pre-authorized one review-round line to the same
file); anything outside that file is out of scope and becomes a separate issue.
