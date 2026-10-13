---
title: "Anchor a 'first push' file criterion to one named SHA and prove it by bytes"
category: legion
tags:
  - legion-proof
  - first-push
  - byte-check
  - negative-control
date: 2026-10-13
status: active
module: smoke
related_issues:
  - "LEGSMOKE-728"
  - "sjawhar/legion-smoke#565"
---

# Anchor a 'first push' file criterion to one named SHA and prove it by bytes

When a spec binds a criterion to "the branch's first push", pin it to the first commit on the
issue branch whose diff touches the file, record that full SHA as a top-level `firstPushSha` in
`.legion/<issue>/implement.json`, and never move it in a later round; move `codeSha` instead.
Every later phase reads that criterion at `firstPushSha` and nowhere else, so an appended line or
a fix-up commit never disturbs it.

Prove a "one line ending in a newline" claim by bytes on the surface the consumer reads (the
GitHub contents API at the pinned SHA), not by `wc -l` alone: `wc -l`, `head -1`, `grep -c <key>`,
`tail -c1 | od -An -tx1` (` 0a`) and `wc -c`. `wc -l` counts line feeds and passes `a\nb`.

Give each proof a control that fails for that exact check: the same read at `main` (404 while the
pull request is open), a prior multi-line file that decodes to a different `wc -l`, a `mktemp`
copy with the last byte stripped (`head -c -1`) whose `od` prints the last character instead of
` 0a`, `cmp` against a literal altered by one byte, and a check-runs read at an all-zero SHA
(HTTP 422) so an empty green list is never mistaken for a pass.

For a later "first line unchanged" criterion, decode the file at the new head and at
`firstPushSha` into two temp files and `cmp <(head -1 a) <(head -1 b)`; silence is the proof.

## Evidence

- The planner's handoff push (`ccddae7d`) was literally the branch's first push and carried no
  file; reading the criterion there would have been unsatisfiable. The plan's decision 1 pinned
  `firstPushSha` to `41b0cc93e254149be53c72a265d3001f67f7f0e5`, and `implement.json`,
  `test.json` and `review.json` all read acceptance 2 there across two review rounds and a
  CI-red round, while `codeSha` moved `41b0cc93` → `f68b72c7` → `c16e9401`.
- The round-2 append (`c16e9401`) was proven with `cmp` of `head -1` at both SHAs (silent) and
  the control `cmp` against the literal plus a trailing `.` (`differ: char 56, line 1`).
- Worker image: `od`, `cmp`, `base64`, `mktemp` present; `xxd` absent.
