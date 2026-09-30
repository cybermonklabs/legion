---
title: "Smoke-proof learnings, third run: `gh run list --commit` wants the full SHA, opening the pull request doubles the check runs, and the `.legion/` deletion lag now sits in the spec"
category: legion
tags:
  - legion-workflow
  - proof
  - smoke-repository
  - github-actions
  - gh-cli
  - go-daemon
date: 2026-09-30
status: active
module: legion-smoke
related_issues:
  - "LEGSMOKE-339"
  - "sjawhar/legion-smoke#460"
  - "LEGSMOKE-331"
  - "sjawhar/legion-smoke#452"
---

# Smoke-proof learnings, third run: `gh run list --commit` wants the full SHA, opening the pull request doubles the check runs, and the `.legion/` deletion lag now sits in the spec

Retro of LEGSMOKE-339 (Stage 4b proof tree 1: the whole workflow), delivered as pull request
sjawhar/legion-smoke#460, which adds `smoke/LEGSMOKE-339.md` holding one line. It is the third run
of the same one-file change under the Go daemon. The first run (LEGSMOKE-327, #449) recorded the
proof surface, the `wc -l` trailing-newline gate, and the head-per-push CI table; the second
(LEGSMOKE-331, #452) recorded that a learning committed here is wiped from `main` by the fixture
reset before the next planner reads it, where the handoff schema lives, and the gVisor
process-substitution refusal. Both documents are readable at their merge commits with
`jj -R "$LEGION_WORKSPACE" file show -r <merge-sha> docs/solutions/legion/<name>.md`; this one
does not repeat them. Both proofs on #460 were read and are complete: the implementer's line names
the authenticated Contents and compare APIs at code head `aaad456a` with two 404 negative controls,
and the tester's line names the same surfaces plus `pr view --json files` at pull-request head
`304b3384`, re-running the implementer's commands first (`.legion/test.json` `implementerProof`,
verdict `verified`).

## `gh run list --commit` with a short SHA returns an empty list and exit 0

Fifteen seconds after `legion push` of the implement handoff, `legion gh -- run list --repo
sjawhar/legion-smoke --commit 304b3384613a --json …` printed `[]` and exited 0. The same call with
the 40-character `304b3384613a2199545c0f55572cd9d4924e3899` printed both completed runs. Reproduced
at retro against the approved head: `--commit a5e20da2e5a8` → `[]`, exit 0; the full SHA → four
runs. GitHub's `head_sha` filter is an exact match, and `gh` does not expand a prefix or warn.

Rule: take the SHA for `--commit` from `jj log --no-graph -r @- -T 'commit_id'` (full) or
`pr view --json headRefOid`, never from `commit_id.short()` or a log line. An empty list from a
short SHA reads exactly like "CI has not started", and a worker waiting on it waits forever.

## Opening the pull request starts a second set of runs at the same head

Every push to `legion/LEGSMOKE-339` starts the two `push`-event runs. `legion gh -- pr create` then
started two more at the same head under the `pull_request` event, and every later push of the open
pull request starts both sets: the approved head `a5e20da2` carries `ci` 36724610970 and
`fail-on-demand` 36724610855 (push) plus `ci` 36724617819 and `fail-on-demand` 36724617882
(pull_request). Immediately after `pr create`, `legion gh -- pr checks 460` listed four rows with
the `pull_request` `ci / check` still pending and exited 8; `gh run watch <id> --exit-status`
on that run, then `pr checks` again, gave four passes and exit 0.

Rules:

- `gh pr checks` exit 8 means a check is pending, not failing (1). Watch the named run rather than
  re-pushing or re-reading the list.
- "CI green at the head" means both event sets. Cite them with their event, as
  `.legion/review.json` `forgeFacts.ci.runs[].event` does; the implementer's first CI line cited
  the two `push` runs only and was widened before `handoff_complete`.
- The tester's `test.json`-only push has no runs at all (`skip-checks: true` from `legion push`),
  so `run list --commit d2c0729c…` → `[]` is correct there and is not the short-SHA symptom above.
  The two look identical; the SHA length tells them apart.

## The `.legion/` deletion lag has moved from the plan into the spec

The second run's retro predicted the plan would repeat the pre-approval `.legion/` deletion push
the Go daemon does not have until Stage 7, because the learning that says so is wiped before the
next planner reads it. This run repeats it in three places, one of them new: `.legion/plan.json`
`integration[0]` ("Implementer .legion/ deletion push at the reviewer's direction") and
`reviewPlan.steps[4]` ("after the implementer's .legion/ deletion push … APPROVE that head"); the
approved spec's acceptance item 2 ("the `.legion/` handoff files, which are removed before the
final approval"); and the implementer's pull-request description, which copied the spec's phrase.
The reviewer's `.legion/review.json` `nextForArchitect` overrode it ("Under the Go daemon there is
no .legion/ deletion step before Stage 7") and APPROVE was submitted on a head still carrying
`.legion/`, as the deployment instructions require. Retro corrected the pull-request description to
name the operator's post-merge follow-up.

Rules for this project until Stage 7:

- The deployment instructions ("no role pushes the `.legion/` deletion") are the authority on the
  `.legion/` lifecycle; a spec, plan, or `skill://legion-retro` step that says otherwise is the lag,
  not a decision. The reviewer approves a head that still carries `.legion/`, retro commits above it,
  and the merger's READY names a head that still carries it.
- The AC2 allow-list a merger runs after retro must be the tester's form, which admits
  `docs/solutions/`; the plan's T3 form (`smoke/LEGSMOKE-339.md` or `.legion/` only) prints 1 at the
  retro head and would read as a failure.
- The architect writing the next spec on this project should state the Go-daemon lifecycle in
  acceptance item 2 rather than the deletion; the phrase has now been copied by two roles.

## The thermonuclear pair ran on a docs-only diff at the driver's instruction

`skill://legion-worker/references/pr-body.md` omits the Thermo line and both passes on a docs-only
pull request. The human proof driver asked the reviewer to run the pair anyway (Dispatch message
`ab59f016` on LEGSMOKE-339); `.legion/review.json` `thermo.ran: true` records the verdicts and
`thermo.why` records the instruction, while the pull-request body still has no Thermo line. That is
the right split: the body follows the template, and the handoff carries the deviation and who asked
for it, so a later reader is not left reconciling a Thermo line with the template's omission.

## Timeline

Admission 13:25:04Z; plan handoff 13:34:03Z; code commit 13:38:29Z; implement handoff 13:39:53Z;
pull request opened about 13:41Z; test handoff 13:45:30Z; review handoff 13:50:30Z; APPROVE
13:52:37Z. Five pushes and ten workflow runs (two at `aaad456a`, four at `304b3384`, none at
`d2c0729c`, four at `a5e20da2`) for one line, 27.5 minutes admission to APPROVE. The retro commit
is the sixth push and runs both event sets again.
