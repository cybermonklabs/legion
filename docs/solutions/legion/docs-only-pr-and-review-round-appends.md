---
title: "Docs-only Legion PRs: skip the code gates, append review rounds to the same file"
category: legion
tags:
  - docs-only
  - review-rounds
  - proof-discipline
  - smoke-fixture
date: 2026-09-27
status: active
module: legion-worker
related_issues:
  - "S392838755-1"
  - "sjawhar/legion-smoke#369"
---

# Docs-only Legion PRs: skip the code gates, append review rounds to the same file

Reusable process discipline from a docs-only Legion pull request (a Stage 3 proof fixture that
adds one Markdown file and then takes three review rounds). The diff itself carries no engineering
lesson; every durable takeaway below is about running the pipeline, and each was confirmed by an
independent fresh-eyes pass.

## When a PR touches no runtime code

- **Skip `ce-simplify-code` and the thermonuclear pair.** `legion-worker`'s docs-only rule: a diff
  with no runtime code has nothing for either pass to act on, so neither runs and the `Thermo` line
  is omitted from the PR body entirely. State this explicitly in the plan's `requiredSkills` so the
  reviewer does not expect a `Thermo` line.
- **The proof is reading the artifact on its real surface.** A Markdown/config repo has no unit
  harness for a one-line file; do not manufacture one. The `E2E (implementer)` proof is
  `cat <file>` (or `jj file show -r <sha> <file>`) on the actual repo checkout, with a negative
  control — a deliberately-absent line/file that `grep -qx` or a missing-path read confirms is not
  present. That negative control is what makes the read discriminating; a bare "the line is there"
  is not proof.
- **Both proofs still required.** Implementer and tester each record an independent `E2E` proof on
  the same real surface (the tester re-drives it a different way, e.g. `jj file show` vs `cat`, and
  sets `implementerProof.verdict`). Docs-only does not waive the two-proof gate.

## Handling review rounds on a single-file change

- **Append to the SAME file; never spawn a second file.** When the spec permits an added line per
  review round, each `CHANGES_REQUESTED` round appends exactly one line to the existing file in a
  new commit on the same PR. A second file fragments attribution and violates the plan. Put this as
  an explicit numbered implementation step in the plan so it survives across worker processes.
- **Review findings can arrive in the review body, not as inline threads.** These rounds requested
  changes through the review-summary body with zero inline `reviewThreads`. `legion threads resolve
  --pr <n> --repo <owner>/<repo>` then prints `no unresolved threads` and exits 0 — that is correct,
  not a miss. Disposition each round in the PR body's `Threads` section by naming the fixing commit
  per round (`round N line appended in commit <sha>`), even when there is no thread object to
  resolve.
- **Re-run CI and re-cite the head after every corrective push.** Each appended line is a new head;
  update the PR body `CI`, `E2E`, and `Threads` lines to that head SHA and re-run the implementer
  proof there. The reviewer and merger read these facts from GitHub, never from the handoff.

## Retro on a compressed proof flow

Retro writes no `.legion` file (the phase is not in the handoff schema) and reports with
`handoff_complete` alone; its only durable outputs are this `docs/solutions/` document and the
Dispatch retro message. Adding a `docs/solutions/`-only commit above the reviewer's approved head
does not void the approval, so the tree goes straight to the merger.
