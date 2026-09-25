---
title: "Multi-round implement handoff commits: stacking, finalization, and SHA stability"
category: legion
tags:
  - jj
  - handoff-commit
  - handoff-complete
  - head-sha
  - multi-round-review
  - proof-discipline
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S392811554-1"
  - "sjawhar/legion-smoke#179"
---

# Multi-round implement handoff commits: stacking, finalization, and SHA stability

Complements `implementer-jj-bookmark-push-and-handoff-mechanics.md` (bookmark placement, proof
field schema, proof headSha = code commit, `.legion/` deletion diff). These four mechanics cost a
retry each on S392811554-1 and are **not** in that doc or in the legion-worker skill's literal
commands. The issue was trivial (append lines to `smoke.txt`) driven through the full
plan→implement→test→review×3→retro chain, so all the value is in the mechanics.

## 1. `jj split <handoff-file>` puts the SELECTED file in the PARENT — it reorders your stack

The skill shows `jj split -m "implement: record handoff" .legion/implement.json`. `jj split`
places the **selected** paths into the **first (parent)** commit and everything else into the
child. So splitting out `.legion/implement.json` from a working copy that also held the code
change produced `plan → implement-handoff → code-change`, the reverse of what a reader expects
(`plan → code-change → handoff`), and it rewrote the code-change commit's SHA because its parent
changed. On S392811554-1 the round-0 code commit was pushed as `386e4c56`, then the split
reordering orphaned that SHA (`d6fef634` became the head), stranding the SHA the PR body and
handoff already cited.

**Avoid `jj split` for the handoff. Stack with `jj new` instead** — it never touches the code
commit's SHA:

```bash
# @ is the described code-change commit
jj -R "$LEGION_WORKSPACE" new -m "implement: record handoff"   # empty commit on top
# ... legion handoff_write writes .legion/implement.json into this @ ...
jj -R "$LEGION_WORKSPACE" new                                   # finalize (see #2)
jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @-       # tip = handoff commit
```

If you already split and reordered, `jj rebase -r <handoff> -d <code-commit>` swaps them back
(jj auto-rebases the code commit onto the original base), then move the bookmark to the tip.

## 2. `handoff_complete` refuses while the handoff file is `@`'s working-copy change

After `legion handoff_write`, the file lands in the working copy. If `@` **is** the handoff commit
(e.g. you `jj edit`ed it, or you `jj new -m "…"` then wrote into that same `@`), `jj status` shows
`A .legion/implement.json` as an uncommitted working-copy change and `handoff_complete` fails:

```
.legion/implement.json has changes in the working copy that are not committed:
commit this phase's handoff (jj commit) before completing
```

Fix: run one more `jj -R "$LEGION_WORKSPACE" new` so the handoff commit is finalized as `@-` and
`@` is a fresh empty commit. Verify `jj status` prints "The working copy has no changes" before
`handoff_complete`.

## 3. Do not push before the handoff commit ordering is final; cite the code commit's SHA

Because the reorder in #1 rewrites SHAs, a SHA you pushed early is orphaned. Either (a) build and
order the whole `code → handoff` stack, set the bookmark, and push **once**; or (b) cite the
**code commit** SHA (a stable ancestor of the tip) in `proof[].headSha` and the PR body, never a
tip you are about to rewrite. On S392811554-1 the durable citation is the per-round code commit
(`13517d2f`, `54cee5bb`, `5fdf4d2f`, `4c06477d`); the handoff commit stacked on top never alters
`smoke.txt`, so the content is identical at the code commit and at the PR head.

## 4. Each review round stacks a fresh code+handoff pair; the tester's handoff sits between rounds

A push does not finish a correction round — `handoff_complete` does. Between your rounds the tester
runs and commits its own `test: record handoff` **on top of your last implement handoff** (authored
by the review App, `legion-reviewer[bot]` — legitimate). On the next round the daemon hands you a
fresh empty `@` above the tester's handoff. So each round:

1. Append the review-named line as a new **code** commit (`jj describe -m "smoke: append review round N line …"`).
2. `jj new -m "implement: record handoff (round N)"`, `legion handoff_write`, `jj new` (finalize).
3. `jj bookmark set legion/<KEY> -r @-`; run `legion threads resolve --pr <n> --repo <o>/<r>`
   before the push (even with zero inline threads it prints `no unresolved threads`, exit 0);
   `jj git push` (a forward move, no flag).

The round's negative control is the **parent** of the code commit — which is the tester's handoff
commit, carrying the pre-correction file (e.g. round 3's parent `27c420719e0d` has 15 lines, the
round-3 line absent). Pair that per-round control with a whole-branch delta
`jj diff --from main@origin --to <head> --git '~.legion'` so the **cumulative** four-line change is
proven, not just the latest line — a per-round control alone would miss an earlier line dropped
during restacking.

## 5. Reviewer's `CHANGES_REQUESTED` may carry zero inline threads

On this run each round's review was a bare `CHANGES_REQUESTED` with `pulls/<n>/comments` length 0;
the correction was named in the operator/review message, not an inline thread. Disposition it in
the PR body's `Threads` section by round anyway (`fixed in <code-sha> — <line>`), and still run
`legion threads resolve` before each corrective push so the `Threads` section quotes a real run.
