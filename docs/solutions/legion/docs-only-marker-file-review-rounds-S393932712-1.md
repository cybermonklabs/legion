---
title: "Docs-only marker-file proof: byte-exact pins, per-round appends, skip-checks trailer"
category: legion
tags:
  - legion
  - docs-only
  - marker-file
  - skip-checks
  - proof-discipline
date: 2026-10-19
status: active
module: legion-smoke
related_issues:
  - "S393932712-1"
  - "sjawhar/legion-smoke#669"
---

# Docs-only marker-file proof: byte-exact pins, per-round appends, skip-checks trailer

A Stage-3 workflow proof whose entire product change is one marker file (`smoke/<ISSUE>.md`)
still has to be proven and pushed correctly. The rules below are what a future docs-only marker
issue in `legion-smoke` must follow.

## Prove content byte-for-byte; CI proves nothing about it

Neither CI job inspects the file: `ci.yml`'s `check` is `sleep 20 && exit 0`, and
`fail-on-demand.yml`'s `gate` reds only when `.fail-me` exists. So "CI green" (AC2) is guaranteed
regardless of file content and proves nothing about correctness — the plan flagged this as "no
machine-checkable acceptance." The only real acceptance is a repository-tree surface proof
(`git show legion/<ISSUE>:<path>` or `jj file show`) that asserts each line byte-for-byte, with a
file-absent-at-base negative control (`git show main:<path>` → fatal / `jj file show -r main` →
`No such path`).

The pinned header's dash is an em dash **U+2014**, not an ASCII hyphen; an eyeball check cannot
tell them apart and CI cannot catch a wrong one. Assert the exact codepoint. Write it with an
explicit byte sequence (`printf 'S...\xe2\x80\x94 ...\n'`) and verify with `cat -A` (shows
`M-bM-^@M-^T`).

## Review rounds append exactly one line each; the per-round phrasing is operative

The reviewer's "the spec permits one more line for a review round" means **one line per round**,
unbounded — not the plan's singular "at most one appended line." Three CHANGES_REQUESTED rounds
each asked for the identical mechanical action (append `Legion proof review round N (S393932712)`,
change nothing else) and rounds 2–3 carried zero review threads. Treat the loop as synthetic
choreography: expect N rounds, keep each append to a strict `+1` diff below the existing content,
and touch nothing else (verify with `jj diff -r @- --git` before pushing).

## The handoff-only push trailer: last line, after two blank lines

A push touching only `.legion/` must end its head commit message with `skip-checks: true` as the
literal last line preceded by **two** blank lines, so GitHub starts no run and the daemon carries
the code-head verdict upward. One blank line is not enough. jj's session overlay inserts the
`Omp-Session:` trailer **before** `skip-checks: true` (not after), so the shape stays valid —
confirmed across all four rounds here. Set it with trailers disabled for that one describe:
`jj describe -r @- --config 'templates.commit_trailers=""' -m "<subject>"$'\n\n\nskip-checks: true'`.

A code push (anything outside `.legion/`, including the retro `docs/solutions/` + removal push)
carries **no** such trailer and none of GitHub's `[skip ci]`-family keywords.

## Proof heads: CI runs on the code head, not the handoff head

CI runs only on the code head (here `a84c4ab2284e`: `check` run 39258696006, `gate` run
39258696010). The handoff-only commits stacked above it carry `skip-checks: true`, so cite the
**code head** for the CI/AC2 proof and the branch tip for the file-surface proof; the tester
re-verifies that the code head's verdict is the one the handoff head inherits.

## Evidence

- `smoke/S393932712-1.md`: 4 lines, header (U+2014) + three review-round lines; sole product change (+4/−0).
- `.github/workflows/ci.yml`, `.github/workflows/fail-on-demand.yml`: neither inspects the file.
- `.legion/S393932712-1/implement.json` `reviewRounds`: heads `4ec6a11`, `c1bd849`, `a84c4ab`; rounds 2–3 empty threads.
- `.legion/S393932712-1/test.json`: byte-for-byte per-line equality, `wc -l` = 4; AC2 via check-runs on code head `a84c4ab`.
- PR #669 approved at head `fde72f8d`, 0 findings.
