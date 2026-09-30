---
title: "Smoke-proof learnings, fourth run: one proof needs one SHA, the `.legion/` deletion lag left the spec but stayed in the plan, and an index of the learnings the fixture reset wipes"
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
  - "LEGSMOKE-349"
  - "sjawhar/legion-smoke#468"
  - "LEGSMOKE-339"
  - "sjawhar/legion-smoke#460"
---

# Smoke-proof learnings, fourth run: one proof needs one SHA, the `.legion/` deletion lag left the spec but stayed in the plan, and an index of the learnings the fixture reset wipes

Retro of LEGSMOKE-349 (Stage 4b proof tree 1: the whole workflow), delivered as pull request
sjawhar/legion-smoke#468, which adds `smoke/LEGSMOKE-349.md` holding one line. It is the fourth
run of the same one-file change under the Go daemon. Both proofs on #468 were read and are
complete: the implementer's line names the authenticated Contents API at head `69b595b8` with a
`?ref=main` → 404 negative control, and the tester's line names the same surface at the same head
with three 404 controls (`LEGSMOKE-350.md`, `.fail-me`, `?ref=main`), re-running the implementer's
command first (`.legion/test.json` `implementerProof`, verdict `verified`). The reviewer
reproduced both at its own head `4bcb0a20` and approved `65690c5a` with four green runs on it.
This document does not repeat the three earlier runs; it indexes them (last section).

## One proof, one SHA: the handoff's `proof.head` and the PR body's `E2E` head must agree

The implementer's proof was run twice, at the code commit `2be8af08` (before the implement
handoff existed) and again at the branch head `69b595b8` (after it). `.legion/implement.json`
`proof[0].head` names `2be8af08`; the PR body's `E2E (implementer)` line leads with `69b595b8` and
names `2be8af08` in parentheses. Both are true, and the tester re-ran the handoff's SHA while the
reviewer's `.legion/review.json` `e2e.implementer` quoted the PR body's — so the same proof is
recorded under two SHAs, and a reader reconciling the four handoffs has to work out that they are
one proof. A fresh-eyes reader at retro flagged it as a cross-phase SHA mislabel; it is not, but it
reads like one.

Rule: the implement handoff is committed above the code commit, so `proof.head` can only be the
code SHA. Lead the `E2E (implementer)` line with that same SHA. Name the branch head only in the
`CI:` line, where it belongs, and never as the E2E head. The plan's tolerance ("the pushed code SHA
or the PR head") is what let the two diverge; the next planner should name the code SHA alone.

## The `.legion/` deletion lag left the spec and now lives only in the plan

The third run's retro asked the architect to state the Go-daemon lifecycle in the spec's
acceptance item 2 instead of the `.legion/` deletion. This run's spec does that: item 2 reads
"a reviewer's approval of its final head, and a merge by a human" with no deletion phrase. The plan
did not follow: `.legion/plan.json` `summary` ("the implementer deletes .legion/ before the
reviewer's approval"), `integrationEvidence` ("tester green → implementer .legion/ deletion push →
reviewer APPROVE of that head"), `tasks[T3].steps[3]` ("after the implementer's .legion/ deletion
push, APPROVE that head by SHA") and `tasks[T5]` (".legion/ deletion push") all describe a push the
Go daemon does not have before Stage 7. The reviewer followed the deployment instructions instead
and approved `65690c5a`, a head that still carries all four handoffs; `.legion/review.json`
`decision` records why.

Rule: the deployment instructions in every worker's system prompt are the authority on this
lifecycle. A planner on this project should write the gate as the deployment instructions state it
(approve a head that still carries `.legion/`; the operator removes it after the merge) and cite
them in `integrationEvidence`, so the tester, reviewer and merger do not each override the plan
separately. The spec is fixed; the plan is the last carrier.

## The approved head is the reviewer's own handoff commit, and full CI runs on it

Under the Go daemon the reviewer's `review: record handoff` push carries `verdict: "approved"`, so
`legion push` runs CI in full on it (only a `changes_requested` review handoff skips CI). The
approval on #468 therefore names `65690c5a`, the review handoff itself, and that head carries four
green runs: `ci` 36745275182 (push) and 36745283385 (pull_request), `fail-on-demand` 36745275202
(push) and 36745283409 (pull_request). The merger's fingerprint is
`jj diff --from 65690c5a --to <tip> --summary`, which after retro must list only this document;
the plan's T6 `'~docs/solutions'` form already admits it.

Rule: "approved head" on this project means the review handoff commit, not a code commit and not a
`.legion/`-free head. Take it from `pr view --json reviews` (`.reviews[].commit.oid`), never from
the plan's description of which head would be approved.

## The plan's `cmp - <(printf …)` proof form was rewritten three times

The second run's retro recorded that process substitution is not seekable in the worker sandbox
(`cmp: /dev/fd/N: Illegal seek`). That document was wiped from `main` by the fixture reset before
this run's planner read anything, so the plan's `acceptance[0].implementerProof.command` and
`testerProof.command` both use `cmp - <(printf …)` again. The implementer, the tester and the
reviewer each hit the refusal and each rewrote the check to a temp file and plain `cmp`, and
`implement.json` and `test.json` each record the deviation; `review.json` reproduces the proof but
does not mention it. Same outcome, three independent rewrites.

Rule for the planner: write proof commands with a temp file (`printf … > /tmp/expected && … |
base64 -d > /tmp/got && cmp /tmp/got /tmp/expected`). Rule for every role: when a plan command fails
in the sandbox and you substitute an equivalent, record the substitution in your handoff so the
next role does not reconcile a plan command against a different transcript.

## The `E2E` line is written before the runs it must cite exist

`skill://legion-worker/references/pr-body.md` has the implementer write the `CI:` and `E2E` lines
when the pull request opens, but `pr create` itself starts the two `pull_request`-event runs, so a
body written before `pr create` can cite only the `push`-event runs. The sequence that worked on
#468: write the body with the push runs → `legion gh -- pr create --body-file` → `legion gh -- run
watch <pull_request ci run> --exit-status` and the same for `fail-on-demand` → `legion gh -- pr
edit --body-file` with all four run ids → `handoff_complete`. `pr checks` exited 8 while the
`pull_request` runs were pending and 0 after; 8 is pending, not failure.

## Index: where the earlier runs' learnings still are

The fixture reset (`proof fixture: remove the handoffs and learnings …`) removes every
`docs/solutions/` document from `main` after each run, so none of these is on the default branch.
Each is readable at the squash-merge commit that landed it:
`jj -R "$LEGION_WORKSPACE" file show -r <merge-sha> <path>`.

| Run | Issue / PR | Merge commit on `main` | Document |
| --- | --- | --- | --- |
| 1 | LEGSMOKE-327 / #449 | `84dd473f1445` | `docs/solutions/legion/one-file-smoke-change-proof-under-the-go-daemon.md` |
| 2 | LEGSMOKE-331 / #452 | `436363445232` | `docs/solutions/legion/smoke-proof-learnings-after-the-fixture-reset.md` |
| 3 | LEGSMOKE-339 / #460 | `5d70c6381f65` | `docs/solutions/legion/smoke-proof-run-ids-pull-request-checks-and-the-deletion-lag.md` |
| 4 | LEGSMOKE-349 / #468 | (this pull request's merge) | this document |

A planner on the next run should read all four before writing the plan; run 1 has the proof
surface and the `wc -l` trailing-newline gate, run 2 the sandbox process-substitution refusal, run 3
the full-SHA rule for `gh run list --commit` and the doubled check runs.

## Timeline

Admission 16:05:01Z; plan handoff 16:13:46Z; code commit 16:22:52Z; implement handoff 16:23:57Z;
pull request opened about 16:25Z; test handoff 16:29:11Z; review handoff 16:34:57Z; APPROVE
16:37:19Z. 32.3 minutes admission to APPROVE, of which each phase waited for the proof driver's
targeted Dispatch message before acting (the implement task arrived before 16:22:00Z's message;
the code commit followed it by 52 seconds). Five pushes and ten workflow runs for one line: none at
`80c48750` (plan, `skip-checks`), two at `2be8af08`, four at `69b595b8`, none at `4bcb0a20` (test,
`skip-checks`), four at `65690c5a`. The retro commit is the sixth push and runs both event sets
again.
