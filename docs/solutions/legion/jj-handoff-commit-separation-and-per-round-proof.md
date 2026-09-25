---
title: "Legion phase-worker jj mechanics: separating handoff commits, per-round proof, and corrective pushes"
category: legion
tags:
  - legion
  - jj
  - handoff
  - proof
  - review-rounds
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S390409560-1"
  - "sjawhar/legion-smoke#212"
---

# Legion phase-worker jj mechanics: handoff commits, per-round proof, corrective pushes

Delivering even a trivial single-file change (S390409560-1 / PR #212 appended five lines to
`README.md`) through the full Legion workflow — plan → implement → test → three review rounds →
retro — surfaces jj and workflow pitfalls that have nothing to do with the product diff. Record
them so the next worker does not rediscover them.

## The handoff file will silently fold into your code commit

In jj the working copy **is** a commit. When the implementer writes `.legion/implement.json` with
`handoff_write`, it lands in whatever commit `@` currently points at. On S390409560-1 that was the
already-pushed README commit, so `@` then held **both** the README edit and the handoff file.

Two concrete consequences observed:

1. `handoff_complete` **refuses** while the handoff file is an uncommitted change in the working
   copy: `.legion/implement.json has changes in the working copy that are not committed`. The fix
   is to make the handoff a *committed* (non-working-copy) revision: after describing it, run
   `jj -R "$LEGION_WORKSPACE" new` so `@` becomes a fresh empty child and the handoff commit
   becomes `@-`.
2. `implement.json`'s own single-file-scope proof depends on the **code** commit's
   `jj diff --summary` showing only `README.md`. If the handoff JSON is mixed into that commit,
   the scope proof breaks.

**Pattern:** make the product edit in its own commit, then put the handoff in a separate child
commit. If they got mixed, `jj -R "$LEGION_WORKSPACE" split <code-paths>` moves the selected code
paths into the *parent* commit and leaves the rest (the handoff) in `@`; then
`jj describe -m "implement: record handoff"` on `@`. Verify with `jj diff -r @ --summary`
(only `.legion/<phase>.json`) and `jj diff -r @- --summary` (only the code files) before pushing.

## Pin every proof to an explicit full SHA, never to a self-reference or a bare branch

Each review round appends a **new** commit, so the branch tip changes every round
(`0781e219 → 49b86d33 → a93de982 → 76681cf5 → a09cf775`, one push per round). Two rules follow:

- **Do not reference the handoff commit's own SHA inside that handoff.** Editing the handoff file
  changes `@`'s SHA — a chicken-and-egg. Pin the proof `headSha` to the **code** commit that sits
  *below* the handoff (implementer proved at `ca48a79b`, its round-3 code commit), which is stable
  while you rewrite the handoff.
- **Use `?ref=<full-sha>` in the Contents-API proof**, not a branch name, so a later round or
  rebase can't retroactively change what a green result meant. The tester independently drove the
  pushed PR head (`a09cf775`) *and* re-ran the implementer's `ca48a79b` command — two roles
  legitimately citing different SHAs.

## Only the implementer pushes; tester/reviewer handoff commits ride the next corrective push

The branch reflog shows all five tip movements authored by `legion-implementer[bot]`. Testers and
reviewers act as the review App, which has no `contents` (push) permission: their `split` of a
handoff commit is their last step, and it stays **local** until the implementer's next push. On
this issue each review round's chain was `… ← test: record handoff (round N-1) ← Append round N
line ← implement: record handoff (round N)`, and the implementer's corrective push advanced the
bookmark to carry the tester's intervening commit forward. Practical rules:

- A tester/reviewer must **not** claim its own handoff is "pushed" or "on the PR head" — assert
  only about commits at or below the last implementer push.
- The implementer, on each corrective round, sets the bookmark to `@` and pushes; the
  previously-unpushed tester commit rides along automatically.

## New-commit-per-round beats amend/force-push for additive changes

Review's "one more line" was pre-authorized in `plan.json` (T3: an additional appended line to the
same `README.md`, in scope for the same issue/PR). Stacking a fresh commit per round (rather than
amending the design-gate commit) preserved each round's proof SHA and avoided force-push races
against the reviewer's view of the PR head. Prefer stacking when the change is genuinely additive.

## Small operational notes

- `legion threads resolve --pr <n> --repo <owner>/<repo>` prints `no unresolved threads` when the
  review was delivered out-of-band (here via Dispatch, not as GitHub PR review threads). Run it
  anyway before each corrective push — it's the sanctioned pre-push step and its output belongs in
  the PR body's `Threads` section.
- Keep negative controls in every proof even for a mechanical append: pair `grep -c '<exact line>'
  → 1` with `grep -q '<broken variant>'` / `grep -q '<spurious next round>'` → exit 1, catching
  both a malformed line and an accidental extra one.
- CI here is a single `ci` workflow on `push` + `pull_request`, so each push produces two `ci`
  runs (one per event); cite both run ids at the head SHA in the PR body rather than assuming a
  single `Tests`/`PR Title` split that this repo does not have.
