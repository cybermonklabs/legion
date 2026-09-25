---
title: "Single-file proof PRs through a jj shared Legion workspace"
category: legion
tags:
  - jj
  - shared-workspace
  - handoff
  - ready-discipline
  - e2e-proof
  - docs-only
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S390753994-1"
  - "sjawhar/legion-smoke#222"
---

# Single-file proof PRs through a jj shared Legion workspace

Durable learnings from taking a single-file, append-only change (README.md gains a
durable-proof marker plus one line per review round) through the full Legion
implement -> test -> review -> retro flow in a shared `jj` workspace. The mechanics below
generalize to any docs-only or single-file proof PR.

## Commit structure: keep the code commit below the handoff commit, and pin proofs to the live head

Each phase records its handoff as its own commit on top of the code. That means the branch
**head SHA is the handoff commit, not the code commit**. The naive sequence — describe the
code commit, then `handoff_write` into the same working copy, then `jj split` the handoff
out — reorders commits and rewrites the code commit's SHA (jj updates the committer
timestamp on rewrite), so any SHA you captured a moment earlier goes stale.

Stable sequence that avoids SHA drift (used for review rounds 1-3 here):

1. Edit the file in the working copy `@`; `jj describe -m "<code change>"`. This is the code
   commit `C`; capture its full SHA with `jj log -r @ --no-graph -T commit_id`.
2. `jj new` — a fresh empty `@` on top of `C`. `C` now has no descendant that could rewrite
   it, so its SHA stays stable.
3. `handoff_write` (writes the handoff file into `@`), `jj describe -m "<phase>: record handoff"`.
4. `jj bookmark set legion/<KEY> -r @`; push.

Record `proof.headSha`/`commit` as the code commit `C` (stable, it is where the change
landed) and cite CI + E2E in the PR body against the **live bookmark head** returned right
before the read — not the code SHA. Verify `jj bookmark list` / `jj log -r 'main@origin..@'`
points where you intend before every push, because in a shared workspace a stray `jj new` or
`split` can silently make a handoff commit the new head under a reviewer's cited SHA.

Do NOT reorder with `jj rebase`/`split` after the fact to "fix" ordering — each rewrite
changes SHAs and forces a re-push and CI re-run. Build the stack in the right order the first
time.

## READY discipline for a docs-only / single-file PR

- **CI:** cite BOTH required workflows by run id and job at one head, e.g.
  `ci` run <id> job `check` success; `fail-on-demand` run <id> job `gate` success at `<head>`.
- **Thermo:** declare `ce-simplify-code` N/A for a docs-only diff and defer the thermonuclear
  pair to the reviewer's final pass. Do not run either on a no-code diff.
- **Threads:** paste the exact `legion threads resolve --pr <n> --repo <owner>/<repo>` output
  (`no unresolved threads`, exit 0) even when there are zero inline threads. Review rounds that
  arrive as top-level `CHANGES_REQUESTED` bodies create no inline threads to resolve.
- **E2E:** implementer and tester drive the SAME surface (the file on the branch) through
  DIFFERENT independent access paths, each pinning contents-API ref + `jj diff` + `pr checks`
  to one SHA.

## Negative controls and idempotent-append checks

- The negative control must exercise the acceptance predicate: assert a deliberately-wrong
  marker (`... (WRONG-999)`) is **absent**, proving the check rejects non-exact content. Do
  not use a decorative control (e.g. adding `.fail-me` to turn the gate red) when a
  content-level control is available; the plan explicitly reserved `.fail-me` as unused.
- For an append proof, prefer `grep -Fxc "<line>"` and assert the count is exactly 1 over a
  bare `grep -Fxq` presence check: the count also catches accidental duplicate appends.

## Merge-gate hazard: the `.legion/` deletion must precede the merge (and normally retro)

Acceptance criterion 2 required the merged PR to change only `README.md`, but the branch
carries `.legion/plan.json`, `implement.json`, and `test.json` as committed handoffs. Per the
merge-gate ordering, the implementer pushes the `.legion/` deletion at the reviewer's direction
and the reviewer approves THAT cleaned head **before** retro runs; retro then adds only
`docs/solutions/` on top. If retro or merge is reached while `.legion/` is still present at the
approved head, the merged PR violates the single-file criterion. Verify the file list
(`gh pr view <n> --json files`) after the deletion push and confirm the approved head is the
head the merger merges.
