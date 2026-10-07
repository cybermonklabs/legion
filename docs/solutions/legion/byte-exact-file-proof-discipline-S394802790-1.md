---
title: "Byte-exact file-content proofs: verify bytes, re-run at the live head, keep every negative control"
category: legion
tags:
  - proof
  - verification
  - byte-exact
  - negative-control
  - docs-only-change
date: 2026-10-07
status: active
module: legion-worker
related_issues:
  - "S394802790-1"
  - "sjawhar/legion-smoke#515"
---

# Byte-exact file-content proofs

When an acceptance criterion pins exact file content — especially a non-ASCII character or an
append that must not disturb prior lines — prove it by comparing bytes, at the head that merges,
with negative controls that each break one named property.

- Assert content with hardcoded escape bytes piped through `cmp`, never an eyeballed literal:
  `printf 'S394802790-1 \xe2\x80\x94 …\n' | cmp - <file>`. A terminal, `grep`, or a visual diff
  renders em dash U+2014 (`e2 80 94`) and an ASCII hyphen identically; `cmp` on the raw bytes
  cannot be fooled by a homoglyph or an editor autocorrect. Decode the distinguishing bytes in the
  recorded observation so the proof is auditable.
- Pair the positive `cmp` with `wc -l` equal to the exact expected line count. `cmp` already
  implies byte identity, but the independent count catches a stray trailing blank line, a rewritten
  earlier line, or CRLF drift the instant an append lands — the real failure mode of a file edited
  across several review rounds.
- Re-run the proof at the live PR head; never trust a prior phase's recorded `headSha`. Layering
  per-phase handoff commits moves the head after the code commit, and approval happens at a head
  above both. A proof pinned to a stale code SHA proves a commit that is no longer what merges.
- Build a negative control that breaks the *specific* property under test, and keep every control
  the proof has ever needed. As this file grew, the negative control narrowed to "first N lines
  only → EOF" (append-completeness) and silently dropped the ASCII-hyphen-for-em-dash control that
  round 0 used. The positive `cmp` still covered byte-exactness, so the proof stayed sound, but a
  proof whose negative control no longer exercises the hard property (the non-ASCII byte) has
  stopped falsifying the thing the spec actually calls out. Carry both: one control that corrupts
  the non-ASCII byte, one that omits the required appended line.

## Evidence

Legion Stage 3 proof run S394802790-1, PR sjawhar/legion-smoke#515: a docs-only marker file
`smoke/S394802790-1.md` created, then appended one line per review round across three rounds, to a
final four-line file whose line 1 separator is em dash U+2014.

- Both the implementer's `E2E (implementer)` proof and the tester's `E2E (tester)` proof used
  `printf '…\xe2\x80\x94…' | cmp - smoke/S394802790-1.md && wc -l < …`, then drove GitHub CI
  (`check`/`gate`) independently. The production-like surface for this docs-only change was defined
  as: the file present byte-exact at the PR head plus the required CI checks green — nothing
  deployed.
- The implement handoff proved at code head `6c6cce2c`; the tester re-ran at the live head
  `5336ad8e` (the code was byte-identical there, only the implement handoff layered on top), and
  approval landed at `bc587a7270f8`. The tester re-running at the live head is what kept the proof
  honest.
- Gap recorded by this retro: from round 1 onward the negative control was "first N lines only →
  `cmp: EOF after byte <N>` (exit 1)", which proves the newest line is required but no longer
  exercises em-dash-vs-hyphen. Keep the byte-corruption control alongside it every round.
- Non-defects confirmed: the one bot review thread ("is this file change needed?") was disposed
  individually — declined by the implementer, accepted by the Legion reviewer, resolved by
  `legion threads resolve` — never in bulk; the 1-line-max spec scope versus the 3-round sibling
  precedent was escalated in the plan rather than silently resolved, and each review round
  re-granted the scope explicitly.
