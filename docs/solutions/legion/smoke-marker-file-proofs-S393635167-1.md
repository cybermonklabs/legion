---
title: "Smoke marker-file proofs: content is not machine-checked, verify it yourself"
category: legion
tags:
  - smoke-proof
  - marker-file
  - review-rounds
  - ci-is-content-blind
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S393635167-1"
  - "sjawhar/legion-smoke#652"
---

# Smoke marker-file proofs: content is not machine-checked, verify it yourself

A `smoke/<KEY>.md` marker issue adds one file; its correctness is entirely content,
and this repository has no machine validator for content. Prove the bytes yourself and
do not read green CI as content evidence.

- **CI here is content-blind.** `ci`/`check` is `sleep 20 && exit 0` (no checkout); the
  required `fail-on-demand`/`gate` only fails when a root `.fail-me` exists. Both stay green
  for any marker content, including a wrong separator or a misspelled title. Green CI proves
  the branch built and the pipeline ran — never the file. The real acceptance is an explicit
  file-content check (`cat`/`grep -Fxq` + `xxd` on the separator byte) plus reviewer visual
  confirmation; record that as the proof, not the CI conclusion.

- **Line 1 separator is an em dash U+2014, not an ASCII hyphen.** When the issue supplies a
  title, line 1 is `<KEY> — <issue title verbatim>` with bytes `20 e2 80 94 20`
  (space + U+2014 + space). An ASCII hyphen (`0x2d`) looks identical in most fonts and passes
  every CI job. Use the ASCII-hyphen variant as the proof's negative control:
  `grep -Fxq '<KEY> - <title>' file` must return non-zero. Peers vary: some use the bare key
  on line 1 with no title/em dash, so copy the convention the issue's own spec states rather
  than assuming.

- **Review-round corrections append exactly one line, and never touch line 1.** Each
  CHANGES_REQUESTED round says append one line and change nothing else; implement it as a true
  append (`>> file`), preserve earlier lines byte-for-byte, and re-prove line 1 is unchanged
  each round. The parenthetical uses the base key **without** the `-1` suffix
  (`Legion proof review round N (S393635167)`), matching the `smoke/S394802790-1.md` peer. The
  round-line wording itself is per-issue (peers also use `Stage 3 review round N`): take it
  verbatim from the review's instruction, not from another issue's file.

- **Handoff pushes must carry the `skip-checks: true` trailer; code pushes must not.** A push
  that touches only `.legion/<KEY>/` ends its head message with `skip-checks: true` as the last
  line after two blank lines; a push that changes `smoke/` (or, in retro, `docs/solutions/` and
  the `.legion/` removal) carries no such trailer so CI runs. The daemon classifies by changed
  paths, and a mismatched trailer stalls the issue without ever failing a check.

## Evidence

S393635167-1 / PR #652: `smoke/S393635167-1.md` opened with line 1 (em dash U+2014) and grew by
one `Legion proof review round N (S393635167)` line across three review rounds. `ci` and
`fail-on-demand` concluded success at every head (e.g. runs 39239821001 / 39239820942 at
`111e3375`) while proving nothing about the content; acceptance came from `cat`/`grep -Fxq`/`xxd`
file-content checks with the ASCII-hyphen negative control, plus reviewer visual confirmation.
One `sjawhar-agent` bot review thread ("is this file change needed?") was answered
not-a-defect (required by spec and the review rounds) and resolved by the implementer.
