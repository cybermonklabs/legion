---
title: "Docs-only Legion PRs: handoff cadence, head-SHA drift, and the .legion cleanup gate"
category: legion
tags:
  - legion-workflow
  - handoff-ledger
  - head-sha-drift
  - docs-only-pr
  - merge-gate
date: 2026-09-27
status: active
module: legion-worker
related_issues:
  - "S394570180-1"
  - "sjawhar/legion-smoke#387"
---

# Docs-only Legion PRs: handoff cadence, head-SHA drift, and the .legion cleanup gate

Distilled from PR #387 (issue S394570180-1), a Stage-3 workflow proof whose product change
was a single fixture file `smoke/S394570180-1.md`: one naming line at creation, then one line
appended per review round across three `CHANGES_REQUESTED` rounds. The trivial content makes
the *process* the interesting artifact.

## Every phase moves the branch head; the product-fix SHA is not the branch head

Each phase commits its `.legion/<phase>.json` handoff as its own commit *on top of* the product
fix. So within a single review round the head advances at least twice: product fix → implement
handoff → (tester) test handoff → (reviewer) review handoff. Round 3 here went
`c5bc1483` (fix) → `0026a64e` (test handoff) → `5efb5b2b`/`4bbf6b68` (review handoffs).

Consequence, and the reusable rule:

- **The `E2E`/`CI` lines in the PR body must cite the head where CI actually ran, not the
  product-fix commit.** The implementer pushes the handoff commit *after* its E2E line is
  written, so re-cite CI + E2E at the final pushed head each round (watch CI at that head first).
  Citing the fix commit leaves the PR body stale the moment the handoff commit is pushed.
- **Head-SHA drift is expected but must be reconciled.** In #387 the proof lines cite
  `0026a64e`, `review.json` records `5efb5b2b`, and the approval-comment head is `4bbf6b68`.
  The product content is byte-identical across all three (only `.legion/*.json` differ), but a
  future reader cannot know that without the unchanged-diff fingerprint. When heads diverge,
  state explicitly that the divergence is `.legion/`-only and, where it matters, prove it with
  the fingerprint procedure (`jj diff --from … --to … --git --context 0 '~(.legion | docs/solutions)'`).

## The `.legion/` ledger rides the branch; isolate the product diff deliberately

`.legion/plan.json|implement.json|test.json|review.json` are committed to the branch and appear
in `pr diff`. They are process artifacts, not product. Every acceptance check must filter them
out — `legion gh -- pr diff <n> --name-only | grep -v '^\.legion/'` — or the "exactly one file
changed" criterion reads as five files changed. Bake the filter into the proof command itself so
the observation is unambiguous.

## Do not approve/merge a head that still carries `.legion/`

`plan.json` review step 3 was explicit: *"Approve only a head carrying no `.legion/`, named by
SHA."* The merge-gate ordering (`skill://legion-retro`) is: tester green → **implementer pushes
the `.legion/` deletion at the reviewer's direction and the reviewer approves that clean head**
→ retro (docs/solutions only) → merger. In #387 that deletion step did not run before retro:
the approved head `4bbf6b68` still lists all four `.legion/*.json` as added, and retro began with
the ledger intact. Retro itself must **not** create, modify, or delete `.legion/` (it writes only
`docs/solutions/`), so retro cannot repair this — the gap belongs to the implementer's
cleanup-push step and the reviewer's clean-head approval, which must happen before retro.

Reusable guardrail: before retro/merge, verify `jj -R "$WS" file list -r @- .legion` prints
nothing on stdout. If it does not, the `.legion/` deletion + clean-head re-approval is still owed
and the merger's `--summary` check (`~docs/solutions` must print nothing above the approved head)
will surface a `.legion/` change — send it back rather than publishing READY.

## Docs-only PRs legitimately skip the thermonuclear pair and ce-simplify-code

`ce-simplify-code` and the `thermonuclear-{deep-review,code-quality}` pair target runtime code.
A PR whose product diff is markdown/fixtures only has no code for either pass; the `Thermo` line
is omitted entirely (not "0 applied"). Record the skip with its reason in the handoff `simplify`
field and leave the `Thermo` line out of the PR body — do not fabricate a pass that did not run.
When no build/test/CI harness exists (as in legion-smoke), the reviewed PR diff is the sole
production-like surface, and that must be stated in the proof rather than substituting a unit suite.
