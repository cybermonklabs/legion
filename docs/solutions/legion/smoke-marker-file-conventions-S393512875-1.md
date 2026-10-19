---
title: "Smoke marker files: match one chosen sibling, prove off-CI, expect multiple review rounds"
category: legion
tags:
  - smoke-proof
  - marker-file
  - off-ci-proof
  - review-rounds
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S393512875-1"
  - "sjawhar/legion-smoke#684"
---

# Smoke marker files: match one chosen sibling, prove off-CI, expect multiple review rounds

For a Stage 3 one-file marker change under `smoke/` in sjawhar/legion-smoke:

- **Pick one exemplar sibling and byte-match it; do not assume a single family-wide template.**
  The `smoke/S*-1.md` family is not uniform. Line 1 appears as an em-dash title
  (`<KEY> — Primary durable workflow profile`, U+2014 with one surrounding space), a bare key,
  prose, or a different em-dash title; review lines appear as `Legion proof review round N` or
  `Stage 3 review round N`. Choose the exemplar the spec points at, copy its bytes exactly
  (separator, spacing, single trailing LF), and state which sibling you matched — never cite "the
  convention" as if one form were canonical.

- **Prove content off-CI with shell exact-string checks.** This repo's CI cannot assert file
  contents: `ci.yml` is `sleep/exit 0` and `fail-on-demand.yml` keys only on a `.fail-me` file.
  The production-like surface is the committed file read back by shell at the merging head:
  per-line `test "$(sed -n Np file)" = '...'`, a `wc -l` line-shape check, and a negative control
  (the wrong separator, or the next round's text absent). A green CI run proves nothing here.

- **A one-line marker becomes a multi-line file across review rounds; do not hard-code the final
  line count in the plan.** The plan here scoped the file to "one line, at most one appended
  review line" with a `wc -l -eq 1` check; the actual pipeline ran three review rounds, each
  appending one line, ending at four lines (title + three rounds). Append-only edits must leave
  earlier lines byte-for-byte unchanged, and the line-shape check's expected count tracks the
  current round, not a fixed plan-time constant.

## Evidence

- PR sjawhar/legion-smoke#684 (S393512875-1): `smoke/S393512875-1.md` created with the em-dash
  title line (byte-matching sibling `smoke/S396017608-1.md`), then `Legion proof review round 1/2/3
  (S393512875)` appended over three rounds. Implementer and tester both proved each head with
  per-line `sed -n Np` equality + `wc -l` + a negative control; CI (`ci`, `fail-on-demand`) was
  read, never relied on for content.
- Sibling variants observed: `S394570180-1.md` (`Stage 3 review round N` wording),
  `S393198222-1.md` (bare-key line 1), `S392670114-1.md` (prose line 1),
  `S394058563-1.md` (different em-dash title) — the family is not a single template.
