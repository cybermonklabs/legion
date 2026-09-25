---
title: "Legion Stage 3 workflow-proof mechanics: per-round implement/test cycles and docs-only PRs"
category: legion
tags:
  - legion-workflow
  - phase-handoffs
  - review-rounds
  - docs-only-pr
  - ci-per-head
date: 2026-09-25
status: active
module: legion
related_issues:
  - "S390584896-1"
  - "sjawhar/legion-smoke#233"
---

# Legion Stage 3 workflow-proof mechanics

Reusable learnings from a docs-only Legion run that appended one round-0 marker plus three
review-round lines to `README.md` across separate implement/test cycles. Organized by reusable
topic, not by PR.

## Per-round implement/test cycles append to proof arrays; they never overwrite

Each review round re-runs implement then test, and both handoffs **append** a new element to
`proof[]` (and `reviewRounds[]`) rather than replacing the prior round. `implement.json`'s
`proof[]` ended with four entries — round 0 plus rounds 1-3 — each carrying its own `headSha`,
`observed`, and a *discriminating* `negativeControl` (e.g. `grep -n 'Stage 3 review round 3 (WRONG)'
README.md -> no match`). On every correction, append a round entry and add to `reviewRounds[]` with
a status/verdict, preserving earlier rounds as the audit trail; the top-level `summary`/`headSha`
reflect only the latest round. A negative control that greps for a deliberately wrong marker proves
the check discriminates the *exact* line rather than any substring.

## Implementer and tester cite different head SHAs for the same round

Committing a `.legion/<phase>.json` handoff advances the branch head, so within one round the
implementer's cited head (e.g. `58c5a830`, the corrective content commit) differs from the tester's
(e.g. `7b05cda4`, after the implement-handoff commit). Always re-read `tail -1 README.md` /
`pr diff --name-only` at *your own* current head and record that SHA; do not trust the previous
phase's SHA. Green CI at an earlier head does not certify the final head — re-check `pr checks` at
the current head and cite that head's run.

## `.legion/*.json` appears in the raw PR diff but is not a content change

Handoffs are committed to the branch, so an unfiltered `pr diff` lists
`.legion/plan.json|implement.json|test.json` alongside `README.md`. The workflow treats `README.md`
as the sole content file via the `~(.legion|docs/solutions)` fingerprint fileset and deletes the
handoffs before merge, so the merged PR touches only `README.md`. When verifying single-file
acceptance, run `pr diff --name-only` and subtract the `.legion/` entries; do not flag them as a
violation, and do not leave them in the final merge.

## Corrections may arrive as Dispatch directives, not GitHub review threads

The three round corrections were delivered as Dispatch issue directives, not GitHub PR review
threads. `legion threads resolve --pr <n>` therefore reports `no unresolved threads` — a no-op that
still must run before each corrective push to clear the gate and produce the `Threads` line. Do not
wait for or hunt GitHub threads on such runs; act on the Dispatch correction and still run resolve.

## Docs-only PRs skip the thermonuclear pair and ce-simplify-code by design

The plan set `requiredSkills.{implement,test,review}` all to `none` with explicit reasoning: a
markdown append has no build/test coupling, so `ce-simplify-code` and the thermonuclear
(deep-review/code-quality) pair do not run, and the PR body omits the `Thermo` line. Verification
collapses to shell inspection (`tail -1`, `jj diff --summary`, `pr diff --name-only`) plus CI
status. Reserve those heavyweight passes for runtime-code changes.

## Handoff accumulation: `bun`-pipe fallback when the pane shim is unset

The skill's convenience pattern for accumulating a handoff — piping
`bun -e '...' | legion handoff write` — failed in this worker pane with
`mise ERROR No version is set for shim: bun`. Fallback that always works: `read` the committed
`.legion/<phase>.json`, then pass the full reconstructed object as the `legion` tool's
`handoff_write` `data`. `handoff_write` validates against the phase schema (an implement handoff
without a well-formed `proof`, or a test handoff reporting no failure without its own `proof`, is
rejected naming the field), so a full-object write is safe.

## jj split/bookmark mechanics for the handoff commit

`jj split -m "<phase>: record handoff" .legion/<phase>.json` selects all changes in the working
copy, printing `Warning: All changes have been selected, so the original revision will become
empty` — benign: the split-off commit holds the handoff and an empty working copy is left above it.
Then `jj bookmark set legion/<KEY> -r @- --allow-backwards` puts the bookmark on that split-off
commit (`@-`), because `@` is the empty working copy with no description and `jj git push` refuses a
descriptionless commit. `jj git push --bookmark legion/<KEY>` authenticates through the provisioned
credential helper; never handle a token.
