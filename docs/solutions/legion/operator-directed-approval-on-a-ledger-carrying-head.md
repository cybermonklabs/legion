---
title: "An operator-directed approval on a .legion/-carrying head: what lands on main and what each role still owes"
category: legion
tags:
  - legion-worker
  - legion-retro
  - reviewer
  - merger
  - handoff-ledger
  - approval
  - pr-body
  - threads-line
  - envoy-inbox
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-278"
  - "sjawhar/legion-smoke#326"
---

# An operator-directed approval on a `.legion/`-carrying head: what lands on main and what each role still owes

Distilled from PR #326 (issue LEGSMOKE-278). The reviewer's `APPROVE` (review 5326202356,
2026-09-26T14:25:34Z) names head `ca74581365c1a0228f6e3e3085d6fab55b05433a`, the tester's
`test: record handoff` commit, which still carries `.legion/plan.json`, `.legion/implement.json`
and `.legion/test.json`. The review body says why, verbatim: "The `.legion/` handoff ledger is
still on this head; under the normal gate this round would be a COMMENT and the approval would
name the `.legion/`-deleted head. This proof operation directed APPROVE at the current head
instead" (Dispatch message `2938709f-eec5-4fb0-930f-c0199302bc37` on LEGSMOKE-278). No
`.legion/` deletion commit exists, no `review.json` was written, and the daemon advanced the issue
to retro.

## What the merge will carry

Retro adds only `docs/solutions/` above `ca745813`, so the merger's two checks pass exactly as
`skill://legion-worker` describes: `jj diff --from ca745813 --to <tip> --summary` lists only
`docs/solutions/legion/*.md`, and the same diff with `'~docs/solutions'` prints nothing. The
human's merge then lands the three `.legion/*.json` handoff files on `main` together with the
product file and the learnings.

On this fixture repository that is the established pattern, not an accident: PR #319
(LEGSMOKE-270) and PR #321 (S392742634-1) both merged `.legion/implement.json`,
`.legion/plan.json`, `.legion/test.json` and a `docs/solutions/legion/*.md`, and the fixture
commits #320 and #322 ("proof fixture: remove the handoffs and learnings Stage 3 runs merged")
deleted them afterwards. In a real repository the same merge would be a defect: the ledger holds
session ids, run ids, command lines and per-phase observations that no product consumer wants,
and `.legion/` has no `.gitignore` entry to stop it.

## What retro must not do about it

A `.legion/` deletion commit placed above the approved head would be "anything else above the
approved head" (`skill://legion-retro`, merge-gate ordering): it voids the approval and sends the
head back to review. The deletion is the implementer's step in the *implementing* phase, at the
reviewer's direction; a retro task never finishes an earlier phase's leftover step. So retro commits
its learnings, states the ledger's presence in its Dispatch message and its report to the architect,
and leaves the decision — approve as is, or send the implementer back for the deletion and a fresh
`APPROVE` by SHA — to the architect and the operator.

## What each role owes in a real repository

- **Reviewer:** while `jj file list -r <head> .legion` prints anything, submit `COMMENT` (or
  `REQUEST_CHANGES` for a correctness finding) naming the head, and say "`.legion/` present;
  approve after the deletion push". If an operator directs `APPROVE` anyway, write the override
  and the directing message id into the review body exactly as review 5326202356 did — the
  merger and the human who merges read GitHub, not the daemon.
- **Implementer:** at the reviewer's direction, delete `.legion/` in one commit, run
  `legion threads resolve --pr <n> --repo <owner>/<repo>`, push with the one push procedure, and
  confirm `jj file list -r @- .legion` prints nothing. Recreating `.legion/` afterwards restarts
  the loop.
- **Merger:** quote the `--summary` output in READY and name, in plain words, that the merged tree
  will contain the ledger when the approved head carries it; the human deciding to merge should
  not have to open the Files tab to learn that.

## Refresh the `Threads` line in the same action that submits a review

Ten minutes after the `APPROVE` landed at 14:25Z, the PR body's `Threads:` line still read "No
review has been submitted yet" while the Reviews section of the same page showed that approval at
`ca745813`. The count (`0 resolved, 0 unresolved`) was right by accident; the sentence was wrong.
Whoever submits or first observes a review edits that line — review id, event, head, thread
count — with `legion gh -- pr edit <n> --body-file`, which changes no commit and voids nothing.
Retro corrected it here; the reviewer should have.

## Frames from other issues reach your inbox — check the sender before acting

During the implementing phase this session received two Envoy frames from an unrelated session
(`01a0a2bf-…`, "hiring pipeline") about another repository's test lanes, each `expects_reply:
none` and `re:` an id this session never sent. A phase worker acts only on frames whose
`dispatch.owner` is its own `LEGION_ISSUE`, whose `from` is a role on that issue, or that are the
daemon's own task prompts. Anything else is noise: read `from`, `re:`, and `dispatch.owner`
first, and never reply on another lane's thread from an issue worker.
