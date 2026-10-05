---
title: "Answer a CI red caused by a real failing input with a forward commit; keep every proof pinned to the SHA it ran at"
category: legion
tags:
  - ci-red
  - shared-branch
  - proof-heads
  - pr-body
  - review-threads
date: 2026-10-05
status: active
module: legion-worker (implementer, tester, reviewer, merger)
related_issues:
  - "LEGSMOKE-439"
  - "sjawhar/legion-smoke#496"
---

# Answer a CI red caused by a real failing input with a forward commit; keep every proof pinned to the SHA it ran at

- When CI is red because the tree really contains the input the job fails on, fix it with a new
  commit that removes the input. `legion gh -- run rerun … --failed` is for a job that failed on
  its own; a deterministic input never flakes. Never rewrite, drop, or re-author another actor's
  commit on the shared branch: later handoffs, reviews, and the PR body already name its SHA.
- Before each round, `jj -R "$LEGION_WORKSPACE" git fetch` and compare `@-` with
  `legion/<KEY>@origin`. A push by anyone else leaves the daemon-placed working copy below the
  remote tip; start your change with `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`, or
  `legion push` refuses the ancestry check.
- Keep the red head as the standing, read-only negative control for every later "CI green" claim:
  `actions/runs?head_sha=<red>` → failure rows, and the offending blob present at `<red>`
  (`contents/<path>?ref=<red>`) but HTTP 404 at every later head. Never create a failing input to
  make a control.
- Record the cost in the PR body's `Not proven / risk`: a non-squash merge carries the red
  commit's tree into the default branch's history even though the head tree is clean.
- Zero workflow runs at a head is not green. Handoff-only pushes carry `skip-checks: true`; name
  the head whose run ids you enumerated, and say explicitly that the handoff head has no runs of
  its own and which record carries the verdict to it.
- A proof entry is immutable at the SHA it was taken at. Append a new entry for a new head; never
  re-point an existing entry. The next role re-runs the previous role's command at its recorded
  SHA and independently drives the same surface at the current head.
- Re-read the PR head after `legion push` before recording it: `pr view --json headRefOid` can
  lag the push by a second and return the previous head.
- On a PR body several roles edit: GET `.body` immediately before each PATCH, rewrite only your
  own sections, and refresh any section your own action just falsified — a reviewer whose
  `Accepted:` closes a thread owns the `Threads` block's truth until the merger's
  `legion threads resolve` run, even though the implementer wrote it.

## Evidence

Issue LEGSMOKE-439, pull request sjawhar/legion-smoke#496. The proof driver (not a Legion role)
pushed `5e204246` adding a 25-byte `.fail-me` at the root; `.github/workflows/fail-on-demand.yml`
fails exactly when that file exists, so `fail-on-demand` went red (runs 37356610176 and
37356602419) while `ci` stayed green. The implementer's working copy sat on `846aac53`, below the
new remote tip; after `jj git fetch` and `jj new legion/LEGSMOKE-439@origin`, commit `124757f9`
removed the file and nothing else, and all four runs at that head were green. The foreign commit
stayed in the chain unrewritten, and every CI proof in `implement.json`, `test.json`, and
`review.json` thereafter used `5e204246` as its negative control.

The PR head moved eleven times between the first proof and the approval
(`c8e754ac → 846aac53 → 5e204246 → 124757f9 → 9f778446 → fdc40ab5 → ce3470eb → c10f5110 → 2d4289e9 → 66d00dce → 02230cd3`),
three of them code commits. `implement.json` ended with eight proof entries, each stamped with the
head it ran at, and the tester's `implementerProof.how` did both halves: re-ran the implementer's
command at its recorded SHA, then drove the surface at the current head. The reviewer recorded
`?head_sha=66d00dce → no runs … 'no checks reported'` for a handoff-only push and approved only
the head whose own four runs it had listed.

After `legion push` of `c8e754ac`, `pr view --json headRefOid` still returned the previous head
for about a second. After the reviewer posted `Accepted:` on the one thread and PATCHed the body,
the implementer-written `Threads` block still read "awaits the reviewer's `Accepted:`" with a
`legion threads resolve` paste stamped at an older head: the one place where the shared-body
discipline (seven whole-body PATCHes across three roles with no concurrency control) broke.
