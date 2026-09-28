---
title: "Legion phase head-SHA churn: re-resolve the head and re-run proof every round"
category: legion
tags:
  - legion-workflow
  - phase-worker
  - head-sha
  - e2e-proof
  - ci-verification
date: 2026-09-28
status: active
module: legion-phase-workers
related_issues:
  - "S391998586-1"
  - "sjawhar/legion-smoke#428"
---

# Legion phase head-SHA churn: re-resolve the head and re-run proof every round

A Legion issue branch advances through many commits for the *same* logical content. Every
phase records its state in `.legion/<phase>.json` as its own commit, and each review round
feeds a new corrective commit back through implement -> test -> review. The PR head SHA
therefore changes several times per round, and any worker that anchors CI or proof to an
upstream phase's recorded SHA validates a stale head.

Observed in PR #428 (S391998586-1), a docs-only one-file add (`smoke/S391998586-1.md`)
driven through three review rounds:

## What churns

- The implementer's *code* push and its *handoff* push are two commits. Each `jj split -m
  "implement: record handoff" .legion/implement.json` puts the handoff in its own commit and
  advances the head again after the code commit. In PR #428 the round-3 code commit was
  `965ada15` but the head the tester and reviewer saw was `6dda666a` (the handoff commit on
  top).
- Because of this, the tester's `reviewedHead` per round (`320a2be`, `4e275de`, `6dda666a`)
  never equals the implementer's code commit (`5a8f0cd`, `9450884`, `965ada1`). Same content,
  different SHA.

## Reusable rules

- **Never trust an upstream phase's recorded head SHA; re-resolve the live head.** Pin proof
  and CI to `ref=<branch>` or to the SHA you just pushed, resolved this turn
  (`jj -R "$LEGION_WORKSPACE" log -T commit_id -r 'legion/<KEY>'`), not to a number copied
  from a predecessor handoff.
- **Re-run the E2E proof with its negative control at your own head every round; don't inherit
  it.** In PR #428 both `implement.json` and `test.json` carry a fresh proof entry re-running
  the exact contents-API command plus the `...S391998586-1-nonexistent.md -> HTTP 404`
  negative control at that phase's head, and the tester also re-ran the implementer's command
  independently (`implementerProof.verdict: verified`). The `?ref=` in the contents API pins to
  a head, so an inherited proof silently describes an older head.
- **Every changes_requested round is a new commit that must re-clear CI and re-test.** Treat
  approval as a head-specific gate, not a one-time judgment: PR #428's review approved only
  once CI (`ci/check` + `fail-on-demand/gate`) was green, both E2E lines were present, and 0
  threads were unresolved *at the final head*.
- **After the handoff split moves the head, re-cite the PR body's CI and E2E head SHAs to the
  final head.** CI re-triggers on the handoff commit; watch those runs green and update the
  READY body rather than leaving it pointed at the code commit.

## Gotchas confirmed here

- A PR-level `CHANGES_REQUESTED` review with no inline threads leaves `legion threads resolve`
  nothing to act on; disposition still has to be recorded (PR-body `Threads` section + one
  per-round PR comment naming the fixing commit).
- Docs-only diffs skip `ce-simplify-code` and the thermonuclear pair and omit the `Thermo`
  line; detect docs-only early so those gates are not falsely expected.
- `jj split` of the single handoff file emits a warning ("the original revision will become
  empty") and leaves an extra empty working-copy commit above the handoff commit; push with
  `-r @-`, which points at the split-off handoff commit.
