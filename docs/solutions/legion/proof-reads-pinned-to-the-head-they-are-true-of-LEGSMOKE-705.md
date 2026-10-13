---
title: "Pin every proof read to the head it is true of; CI evidence lives only at code heads"
category: legion
tags:
  - proof
  - sha-pinning
  - skip-checks
  - ci-evidence
  - negative-control
date: 2026-10-13
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-705"
  - "sjawhar/legion-smoke#561"
---

# Pin every proof read to the head it is true of; CI evidence lives only at code heads

- Name the exact commit in every proof read (`?ref=<sha>`, `--commit <sha>`, `commits/<sha>/check-runs`). A bookmark or `main` is a live check or a negative control, never evidence for a state a later push destroys.
- When an acceptance line says "first push", "before X" or "after Y", decide which SHA it means before the first push, write that SHA into the handoff under its own key, and check the line there in every later round.
- Expect zero workflow runs at a handoff-only head (`skip-checks: true` trailer): `check-runs total_count` is `0` there and that is not red. Read CI at the last code-bearing head below it, and say so in the `CI:` line, both events (`push` and `pull_request`) by run id.
- Every `wc -l` read comes with `wc -c`, `tail -c1 | od -An -c` and a digest or `cmp` against the planned bytes: `wc -l` counts newlines, so an unterminated two-line file reports `1`.
- Take the negative control from the same surface as the proof: the same call at a ref where the state must not hold (`?ref=main` → 404), a sibling path (→ 404), or `cmp` against a deliberately mutated variant (missing LF, CRLF, trailing space, swapped or extra line → exit 1).
- Re-pin after every code head, including one that only removed a fixture: the file may be byte-identical, and the proof says so by `cmp` of the two decodes, not by assumption.

## Evidence

- `smoke/LEGSMOKE-705.md` was proven one line at `firstFilePush` 2de1cd5d8c09be2c96c5b72e0a8976b6e95f9171, byte-identical at the fixture-removal head 825b17ed6da7c39f253788a3ec01fe34f8bf8f25 (`cmp` of the two decodes), and two lines at the review-append head 60c2c57c2d223b0cedd909d2d22af274d74f1d29 with `head -1` compared against 2de1cd5d. A bookmark read would have shown only the last state.
- The planner's, implementer's, tester's and reviewer's handoff heads (08ad3a58, 4f91a8d4, 467ea956, ff04c7fd, fecc4fb7, 15649bb6, 0a624824, c8a81c58) all had `total_count 0` and the 21-byte trailer tail; the code heads had four runs each. The reviewer approved c8a81c58 on the strength of CI at 60c2c57c.
- Only `gate` was a required check; `check` (workflow `ci`) was read explicitly at every code head because the spec required both workflows, and no role relied on the daemon's verdict for it.
- Predecessor (removed from `main` by #560, still in history at commit 152231add64a): `docs/solutions/legion/pr-body-claims-recomputed-per-head-LEGSMOKE-692.md`.
