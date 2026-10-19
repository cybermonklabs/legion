---
title: "Single-file smoke proof conventions in smoke/"
category: legion
tags:
  - smoke-proof
  - conventions
  - markdown
  - review-rounds
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S394251237-1"
  - "sjawhar/legion-smoke#699"
---

# Single-file smoke proof conventions in smoke/

A Legion durable-workflow proof adds exactly one marker file, `smoke/<KEY>.md`, and nothing else.
Follow these rules; verify bytes, not glyphs.

- **Copy the newest same-family sibling verbatim; do not hand-type the title line.** `smoke/`
  holds two naming families: S-family `<KEY> — <Title>` (em dash U+2014) and LEGSMOKE-family
  `<KEY>: <desc>` (colon). Match the family of your key. Within S-family the review-line wording
  also drifted (`Stage 3 review round N` in older files vs `Legion proof review round N` in
  current ones), so diff against the most recent same-family sibling, not an arbitrary one.
- **The title separator is em dash U+2014 (bytes `e2 80 94`), never ASCII hyphen-minus `-`.** This
  is the single most likely silent defect because the glyphs look near-identical in editors.
  Produce it by copying an existing line and confirm with `hexdump -C` and the exact-string
  negative control (compare line 1 against the ASCII-hyphen variant; it must be rejected).
- **The appended review-round line drops the `-1` instance suffix.** The filename and title use
  the full key `<KEY>` (e.g. `S394251237-1`), but the round lines use the bare issue key:
  `Legion proof review round N (S394251237)`, not `(S394251237-1)`.
- **Line count is a function of review rounds, not a fixed invariant; split the shape check per
  phase.** Pre-review the file is one line (title + trailing LF); each CHANGES_REQUESTED round
  appends one more line. Scope the one-line check to the pre-review state and a separate N-line
  check to each post-append state. Never assert a single fixed line count across the whole
  workflow. Every round must keep the diff to that one file (`jj diff --stat`) and keep a single
  trailing LF.
- **An APPROVED PR can legitimately show merge state BLOCKED; do not re-push to "fix" it.**
  Code-head commits trigger CI; `.legion/<KEY>/` handoff-only pushes carry `skip-checks: true` as
  the last line after two blank lines, and the daemon reuses the code head's verdict. Confirm the
  required check is green at the last code head and that only `.legion/` changed since. Resolve
  each bot review thread individually (reply, then `resolveReviewThread` as the PR author), since
  an unresolved thread blocks regardless of approval.

## Evidence

- `smoke/S394251237-1.md` (PR #699): 4 lines, 173 bytes — `S394251237-1 — Primary durable workflow
  profile` (em dash `e2 80 94`) + `Legion proof review round {1,2,3} (S394251237)`, single
  trailing LF; byte-identical in shape to canonical siblings `smoke/S396017608-1.md`,
  `smoke/S395788430-1.md`.
- Implementer negative controls rejected the ASCII-hyphen title variant and each wrong
  review-round number; tester re-verified independently via `hexdump` at code head `d9af70bd`.
- Three review rounds (not the one the plan's AC5 enumerated) each appended one line, converging to
  the 4-line canonical S-family shape. Plan `gapAnalysis` had pre-resolved the "exactly one line"
  vs multi-line-norm contradiction by scoping AC2 pre-review and AC5 post-append.
