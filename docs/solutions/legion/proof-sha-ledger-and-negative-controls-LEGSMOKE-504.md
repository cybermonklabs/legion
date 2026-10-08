---
title: "Prove at a named SHA with a control built from the claim's own failure mode"
category: legion
tags:
  - proof
  - negative-control
  - skip-ci
  - pr-body
  - legion-implementer
  - legion-tester
date: 2026-10-08
status: active
module: legion-phase-workflow
related_issues:
  - "LEGSMOKE-504"
  - "sjawhar/legion-smoke#522"
---

# Prove at a named SHA with a control built from the claim's own failure mode

Record every proof entry at the SHA the observation was made on, and expect that SHA not to be the
pull request's head when your phase completes: the implementer pushes code and then its handoff,
so the branch head after completion is the handoff commit. Re-check the CI rows at the handoff
head too, since an implementer push runs CI in full.

Treat a handoff-only push by the planner, the tester, or a changes-requested reviewer as a head with
zero Actions runs: it carries the `skip-checks: true` trailer. A CI criterion is therefore a
positive count of named rows at one exact SHA, never "no failing runs" (zero rows would pass that).

Build each negative control out of what the claim would get wrong: query the pre-change SHA and show
the old bytes; drop exactly the byte the claim asserts (the trailing newline) and show `cmp` fail at
that byte; query the base SHA and show HTTP 404. A comparison against an unrelated file proves only
that `cmp` can exit 1.

After the implementer's last push, its PR-body lines (`Size`, `Threads`) go stale as later phases
push handoffs and post `Accepted:` replies. Retro is the implementer's slot to refresh them before the
final push; nothing else re-reads those lines until the merger quotes the body.

## Evidence

- Code heads and handoff heads on PR #522: `1c8f8f99` (marker) → handoff `22ef4c44`; `90984f5f`
  (cure) → `e092c5a3`; `4e1183a9` (append) → `f5232225`. Each handoff head had its own four green
  rows; the tester proved at the handoff head and re-ran the implementer's commands at the code head.
- Skip-CI heads with `total_count 0` runs: `dd01b07b` (plan), `9d87d963` and `9f6b59a5` (test),
  `8761c044` (changes-requested review). The approving reviewer pushed `8fb51994` without the
  trailer and approved that SHA after its rows settled.
- Strong controls used: `?ref=8761c044` still serving 56 bytes after the append; the 56-byte line
  `cmp`'d against the 95-byte file → `EOF on - after byte 56`; the line without its newline →
  `EOF after byte 55`; `?ref=34422c01` (base) → 404. Weak control carried in four entries:
  `cmp` against `smoke/LEGSMOKE-439.md` → `differ: char 10`.
- At retro the body's `Size` line still described `f5232225` (+673) while the approved head
  `8fb51994` carried the round-2 `test.json` and `review.json` (+725), and `Threads` still said the
  reviewer's `Accepted:` was outstanding after comment 4213993488 had landed.
