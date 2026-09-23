---
title: "Spec-locked product under scripted, no-defect review rounds"
category: legion
tags:
  - review-rounds
  - spec-locked-acceptance
  - tester-proof-visibility
  - jj-commit-hygiene
date: 2026-09-23
status: active
module: legion-worker
related_issues:
  - "S392569266-1"
  - "sjawhar/legion-smoke#106"
---

# Spec-locked product under scripted, no-defect review rounds

Driven from Legion issue S392569266-1 / PR sjawhar/legion-smoke#106: a single-line
README change (`Legion smoke check: S392569266-1.`) taken through three
`CHANGES_REQUESTED` review rounds that named no defect, then approved. The product
line was byte-locked by the approved spec's Acceptance 1, so no corrective round could
touch it. Three reusable learnings came out of it; a fresh-eyes review corroborated all
three.

## 1. A no-defect review against a spec-locked line is dispositioned, never "fixed" by editing the product

Each review round carried boilerplate ("Stage 3 proof scripted changes requested, round
N.") and opened no inline thread. Acceptance 1 required README's final line to be
*exactly* `Legion smoke check: S392569266-1.`; any edit would have broken the approved
criterion. Editing the product to make a defectless review "look addressed" is a
regression, not a fix.

The correct corrective round when the product is spec-locked and the review names no
defect:

1. Confirm the product line is byte-exact to the approved spec (read it back from the
   real surface — here the GitHub contents API at the branch head, with a `main`
   negative control).
2. Record the disposition in `.legion/implement.json`: `reviewRound`, and a
   `reviewResponse` stating the line is spec-locked and no in-scope product correction
   is available or warranted.
3. Push a corrective commit that touches **only** `.legion/` to advance the head for
   re-review.
4. Post one PR comment (with the Legion footer) disposing the review as *not a defect*,
   and run `legion threads resolve` (here: no threads).

This keeps a defensible audit trail instead of oscillating the product against a
cosmetic review loop.

## 2. The tester runs as the review App and cannot surface its own proof on the PR

The tester's proof was complete in `.legion/test.json` — `implementerProof.verdict:
"verified"`, an independently re-run GitHub contents-API command, a tracked head SHA,
and a `main` negative control. But the PR body's `E2E (tester)` line stayed `pending`
through the approved head, because the tester runs as the review GitHub App
(`legion-reviewer[bot]`), which has neither push nor PR-body-edit permission
(`packages/daemon/src/daemon/AGENTS.md`, GitHub Apps).

Consequence: a proof that satisfies the merge gate lives only in the handoff file,
invisible on the PR surface where a reviewer or merger looks. Close the gap with a role
that *can* write the PR:

- The **implementer** (code-writing App, has push and `pr edit`) should transcribe the
  tester's `.legion/test.json` proof into the PR body's `E2E (tester)` line on its next
  corrective push, or
- the **merger** must read the tester proof from `.legion/test.json` for the READY
  packet rather than trusting the PR body text.

Do not treat a `pending` `E2E (tester)` line as a missing proof when the substance
exists in the handoff — but do not let it merge unsurfaced either.

## 3. Append handoff/retro commits; never rewrite a lower commit in the shared jj workspace

The branch stayed a clean linear stack: product commit `62f8095b` → per-round
implement/test handoff commits → reviewer-approved head `b3a4c1b9`. Each round appended
a new `.legion/*.json` disposition commit; none rewrote a prior round's commit. Two
concrete rules that kept it clean:

- **Corrective rounds append.** Adding a new commit on top (rather than amending a
  lower, already-pushed handoff commit) avoids auto-rebasing sibling roles' commits and
  reassigning their committer identity — a real hazard in the single shared operation log
  of a jj workspace clone.
- **The retro commit is a direct child of the *approved head*.** Build it with
  `jj new <approved-sha>`, not on top of un-pushed tester handoff commits that touch
  `.legion/`. That guarantees the merger's `jj diff --from <approved-sha> --to <tip>
  --summary` shows only `docs/solutions/`, so the retro documentation does not void the
  approval.

## Bonus hazard: `legion handoff write` has no dry-run, and `retro`/`production` are not handoff phases

`legion handoff write` writes the real `.legion/<phase>.json` immediately — there is no
probe mode. Testing "is this a valid phase?" by calling it corrupts the working copy;
recover with `jj restore --from @- .legion/<files>`. The valid handoff-write phases are
`plan`, `implement`, `test`, `review`, `merge`. `retro` and `production_check` are
**not** handoff-write phases (they exit non-zero): retro records its durable output in
`docs/solutions/` plus a Dispatch message and reports with `legion handoff complete`
alone.
