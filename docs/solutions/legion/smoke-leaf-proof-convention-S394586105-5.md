---
title: "legion-smoke proof-marker conventions: -5 leaves and the CI gate"
category: legion
tags:
  - legion-smoke
  - proof-marker
  - smoke-convention
  - ci-gate
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S394586105-5"
  - "sjawhar/legion-smoke#687"
---

# legion-smoke proof-marker conventions: -5 leaves and the CI gate

`sjawhar/legion-smoke` has no test suite, build, or linter. The entire proof surface is the git
tree plus two GitHub Actions check-runs. Follow these conventions when a Legion issue asks for a
smoke proof-marker file.

## The `-5` leaf format is exact

A `-5` leaf file is `smoke/<ISSUEKEY>.md` whose entire content is the bare issue key followed by a
single LF: `<ISSUEKEY>\n`. No frontmatter, no heading, no body, no second newline, no BOM. For
S394586105-5 that is `S394586105-5` (12 chars) + LF = exactly 13 bytes. Verify with
`wc -c` (13) and `wc -l` (1). Adding a title, a trailing blank line, or omitting the final newline
breaks the convention.

## `-5` vs `-1` is task shape, not size accident

`smoke/<KEY>-5.md` is the minimal dependent/admission **leaf** proof (bare key, ~13 bytes).
`smoke/<KEY>-1.md` files are substantive (~120–180 bytes, with body text). Pick the suffix the
issue dictates; never copy a `-1` body into a `-5` leaf.

## CI is green by construction; the only real lever is `.fail-me`

`.github/workflows/ci.yml` runs `sleep 20 && exit 0` — always green, proves nothing about content,
and reports after ~20 s (latency, not a hang; wait it out when polling check-runs).
`.github/workflows/fail-on-demand.yml` fails only when a file named `.fail-me` exists at the repo
root. A clean single-file leaf add is therefore green automatically; a leaf PR must introduce no
`.fail-me`. Both workflows trigger on `push` and `pull_request`, so each head produces two run sets
— cite the `pull_request`-event run at the code head for proof.

## Scope discipline: one file, nothing else

A leaf PR touches exactly `smoke/<KEY>.md` and nothing else. `smoke/` already holds 70+ markers;
do not tidy, rename, or touch sibling leaves — unrelated edits dilute the proof and trip reviewers
expecting a single-path diff. Prove the single-path diff with `jj diff -r <code-commit> --name-only`
and the negative control `jj file show -r <base>@origin smoke/<KEY>.md` (fails: absent on base).

## Evidence

PR #687 (issue S394586105-5) added only `smoke/S394586105-5.md` (13 bytes, `S394586105-5\n`),
byte-shape-identical to `smoke/S392473105-5.md` and `smoke/S396365777-5.md`. CI `check` run
39297911736 and `gate` run 39297911811 were both SUCCESS at code head `6e313687`; no `.fail-me` was
introduced; `git show main:smoke/S394586105-5.md` failed on base, confirming the add.
