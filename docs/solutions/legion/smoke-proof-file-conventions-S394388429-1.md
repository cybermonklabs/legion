---
title: "Authoring Legion smoke proof files under smoke/"
category: legion
tags:
  - smoke-proof
  - legion-workflow
  - diff-scope
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S394388429-1"
  - "sjawhar/legion-smoke#654"
---

# Authoring Legion smoke proof files under smoke/

`smoke/` holds one throwaway Markdown proof artifact per issue, with no code, build, or test
coupling. A proof file follows a fixed convention with two silent-diff gotchas; get them exactly
right because nothing lints them.

- Separator in the header line is an em-dash **U+2014**, not a hyphen or en-dash. Write the first
  line as `<ISSUE> — <title>` and verify the separator bytes are `e2 80 94` (e.g.
  `printf 'S394388429-1 \xe2\x80\x94 Primary durable workflow profile'` and an exact-string
  compare), with an ASCII-hyphen variant as the negative control.
- Each review round appends exactly one line `Legion proof review round N (<PREFIX>)`, where
  `<PREFIX>` is the issue key **with the trailing `-1` suffix stripped** (issue `S394388429-1` →
  prefix `S394388429`). Append below the existing lines; change nothing else.
- Keep the diff to the single new `smoke/` file only. These artifacts have no coupling, so any
  other touched path is out of scope; confirm with `jj diff --stat` before committing.

## Evidence

Issue S394388429-1 / PR sjawhar/legion-smoke#654 added `smoke/S394388429-1.md` across four
rounds (initial file plus three review-round appends). The header matched reference file
`smoke/S394802790-1.md` byte-for-byte on the em-dash; each round line used the `-1`-stripped
prefix; every round's diff touched only that one file. CI (`ci` check + `fail-on-demand` gate)
stays green as long as no `.fail-me` file is added, which the single-file discipline guarantees.
