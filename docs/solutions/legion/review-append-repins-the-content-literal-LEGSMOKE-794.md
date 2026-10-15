---
title: "When a review changes the deliverable's bytes, re-pin the literal and name the criterion that replaced the old one"
category: legion
tags:
  - proof
  - acceptance-criteria
  - review-round
  - implementer
  - tester
  - planner
date: 2026-10-15
status: active
module: proof entries and handoffs
related_issues:
  - "LEGSMOKE-794"
  - "sjawhar/legion-smoke#575"
---

# When a review changes the deliverable's bytes, re-pin the literal and name the criterion that replaced the old one

Plan the permitted correction's exact bytes before any review asks for it: when the spec allows "one more line", the plan fixes the line's text, its byte count, and the whole-file byte count after it, and names that end state as its own criterion beside the pre-correction one.

On the correction round, prove the new whole-file literal at the new code head and demote the old literal to a negative control: `cmp <served> <old-literal>` must print `cmp: EOF on <old-literal> after byte N` and exit 1, and the previous code head's served bytes against the new literal must print the mirror `EOF on - after byte N`. A proof that only re-runs the old comparison passes on the unchanged prefix and never sees the append.

Write into the handoff's `trickyParts` that the content criterion changed (which criterion id now holds, which is retired), so the tester reads `EOF after byte N` against the old literal as the recorded control and not as a failure.

Reply on the review thread with the byte facts the reviewer can re-check (`wc -c`, line count, which `cmp` was silent), not with "done": the reviewer's re-review quotes those numbers.

## Evidence

Pull request `sjawhar/legion-smoke#575`. Plan §3 fixed `L2` = `Legion proof review round 1 (LEGSMOKE)` + LF (39 bytes) and C6 = 2 lines, 95 bytes, before review round 1 asked for it in thread `PRRT_kwDOUCdo886s8UgI`. Commit `ba4777f0` appended the line; the implementer's round-3 proof read `contents/smoke/LEGSMOKE-794.md?ref=ba4777f0` and compared `head -c 56` to L1, `tail -c +57` to L2, and the whole file to L1+L2 (all exit 0), then ran the retired one-line literal as the control (`cmp: EOF on /tmp/l1 after byte 56`) and the previous code head `6faa4f8d` against the two-line literal (`cmp: EOF on - after byte 56`). `implement.json` round 3 `trickyParts` recorded "C6 replaces C1 as the content criterion from this head on"; `review.json` round 2 wrote "C6 replaces C1 as the content criterion" and approved.
