---
title: "Build one negative control per check, each able to fail exactly that check"
category: legion
tags:
  - proof
  - negative-control
  - implementer
  - tester
date: 2026-10-13
status: active
module: proof entries
related_issues:
  - "LEGSMOKE-760"
  - "sjawhar/legion-smoke#571"
---

# Build one negative control per check, each able to fail exactly that check

A proof's negative control shows the check can report a failure, not merely that the call returns something different for a different input. For each check in the command, construct an input that passes every other check and fails only that one, and say in the proof which check did the discriminating.

For a "CI is green" claim, the control is a known-red head or a known-failed run of the same workflow, kept from round one. An empty check-run list at a trailered handoff head, or HTTP 422 at an invalid SHA, separates "has runs" from "has none" and "valid SHA" from "invalid"; neither shows the pipeline would report a failed run.

For a byte-exact file claim, check bytes with `cmp` against a `printf` reference, not `wc -l` alone: `wc -l` counts line feeds and passes a file with no final newline, a CRLF file, and a file with its lines swapped.

## Evidence

Pull request `sjawhar/legion-smoke#571`. Plan decision 3 fixed one decode command (`wc -l`, `head -1`, `grep -c`, `tail -c1 | od`, `wc -c`, `cmp`) and decision 2 pinned the bytes (56 for one line, 95 for two, LF only), which is what made every later `cmp` possible. The tester's controls each isolated one check: a copy with the final newline stripped passed nothing but `grep -c` (`wc -l` 0, trailing byte ` 77`, `wc -c` 55, `cmp: EOF after byte 55`); a CRLF copy passed `wc -l` (1) and was caught only by `wc -c` (57) and `cmp` (`differ: char 56, line 1`); a copy with the two lines swapped passed `wc -l` (2) and `wc -c` (95) and failed only `cmp <(head -1 f) l1` (`differ: char 2, line 1`). The implementer's round-one control for the workflows claim was check-runs `[]` at the planner's handoff head `4570f895` and HTTP 422 at the all-zero SHA; the discriminating control, the red head `d712a117` with `gate` failure on runs 38478575797 and 38478580823, entered the proofs only in the CI-red round once that commit existed.
