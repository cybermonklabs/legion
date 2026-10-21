---
title: "Prove a file-shape criterion against GitHub's served bytes, anchored to computed SHAs"
category: legion-proofs
tags:
  - proof
  - negative-control
  - github-api
  - review-round
  - ci-head
date: 2026-10-21
status: active
module: smoke
related_issues:
  - "LEGSMOKE-892"
  - "sjawhar/legion-smoke#712"
---

# Prove a file-shape criterion against GitHub's served bytes, anchored to computed SHAs

When an acceptance criterion fixes a file's shape ("one line ending in a newline", "two lines, the
first unchanged"), prove it from the bytes GitHub serves, not from the workspace, and not with
`wc -l` alone.

- Read the file at an explicit SHA with the contents API raw media type:
  `gh api -H 'Accept: application/vnd.github.raw+json' "repos/<owner>/<repo>/contents/<path>?ref=<sha>" > f`.
- Check `wc -c`, `tail -c1`, the CR count, the first three bytes (BOM), and a whole-file `cmp`
  against a literal composed with `printf`. `wc -l` accepts CRLF, a BOM, trailing spaces and a
  second unterminated line.
- Make the negative controls the almost-right inputs: the literal without its final LF, the other
  arm's literal, the line with one trailing space, the lines swapped. An unrelated wrong file
  proves nothing.
- Bytes a review asks you to add are extracted from the review comment, never retyped:
  `L=$(gh api "repos/<owner>/<repo>/pulls/<n>/comments?per_page=100" --jq '<filter>[0].body' | tr -d '\r' | awk '/^```/{f=!f; next} f')`,
  `LB=$(printf '%s' "$L" | wc -c)`, expected size `= <base bytes> + LB + 1`. Sibling files from
  earlier runs often carry the identical line, so a retyped guess passes by a broken method.
- A criterion phrased against a moving head ("the first push", "the pull request's head") is
  re-anchored to a computed SHA before it is judged: the first commit touching the file is
  `gh api "repos/<owner>/<repo>/commits?sha=<branch>&path=<path>" --jq '.[-1].sha'`; the CI head is
  the newest `pull_request` run's `head_sha`, and a handoff-only head above it (pushed with
  `skip-checks: true`) is covered by `gh api "repos/<owner>/<repo>/compare/<ci-head>...<head>" --jq '[.files[].filename] | all(startswith(".legion/"))'`
  → `true`. `mergeStateStatus: BLOCKED` at such a head is expected, not a failure.
- A detector over GitHub state ("did the review ask for a line?") carries two vacuity controls
  beside its result: a deliberately wrong parameter (expect `0`) and a known-positive pull request
  (expect `1`). Filter by review state and author login, never by review count: a `COMMENT`
  review decides nothing. Bound the arms (`K <= 1`) so an unplanned second request escalates
  instead of being appended.

## Evidence

- `sjawhar/legion-smoke#712` (LEGSMOKE-892): line 2 was extracted from review comment 4332166668's
  fenced block (38 bytes); `smoke/LEGSMOKE-882.md` and `smoke/LEGSMOKE-872.md` on `main` carry the
  same second line, so a retyped line would have passed undetected.
- The controls fired as designed at code head `e0fd742b`: one-line literal → `cmp: EOF on - after
  byte 56`; two-line literal without its final LF → `cmp: EOF on - after byte 94`; `L` with one
  trailing space → `differ: char 95, line 2`; detector with the implementer App as `RV` → `0`;
  detector against #710 → `1`.
- "The branch's first push" was judged at the first commit adding the file (`e131645c`), because
  the planner's handoff push preceded any code push; "both workflows pass on the pull request's
  head" was read at the CI head with the `.legion/`-only compare, because handoff heads start no
  workflow (`skip-checks: true`) and show 0 check-runs.
