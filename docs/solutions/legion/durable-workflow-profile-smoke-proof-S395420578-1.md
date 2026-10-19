---
title: "Durable workflow profile smoke proofs: byte-exact marker and per-round appends"
category: legion
tags:
  - smoke-proof
  - durable-workflow-profile
  - em-dash
  - review-rounds
  - handoff-push
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S395420578-1"
  - "sjawhar/legion-smoke#677"
---

# Durable workflow profile smoke proofs: byte-exact marker and per-round appends

A "Primary durable workflow profile" smoke proof delivers one content-only file,
`smoke/<ISSUE_KEY>.md`. The repository has no build, lint, or test that applies to Markdown, so
the proof is byte-exact content, verified by `grep`/`hexdump`, not a suite. Follow these rules so
the acceptance check and every review round pass without rework.

## The marker file

- First line is exactly `<ISSUE_KEY> — <Profile Name>`, e.g. `S395420578-1 — Primary durable workflow profile`.
  The separator is U+2014 EM DASH (bytes `e2 80 94`) with a single space on each side. A plain
  hyphen (`-`, byte `2d`) fails acceptance.
- End the file with exactly one trailing newline (`cat -A` shows one trailing `$`); match the
  byte convention of a precedent file such as `smoke/S391574992-1.md`.
- Create the file with `printf 'S395420578-1 \xe2\x80\x94 Primary durable workflow profile\n'` so
  the em dash bytes are exact; do not type the dash through an editor that may substitute it.

## Verify with an anchored grep plus a negative control

- Acceptance: `test -f smoke/<KEY>.md && grep -q '^<KEY> — <Profile Name>$' smoke/<KEY>.md`.
- Negative control every round: `grep -q '^<KEY> - <Profile Name>$' smoke/<KEY>.md` (plain hyphen)
  must exit 1. Reporting only the positive match does not prove the em dash is required; the
  hyphen control does.

## Per-review-round append lines

- Each actual review round appends exactly one line `Legion proof review round N (<PROJECT_KEY>)`,
  with the **bare project id** (`S395420578`), not the suffixed issue key (`S395420578-1`), and
  `N` counting from 1. Up to three rounds.
- Appends are reactive: add a line only when a round requests it; never pre-populate. Acceptance
  requires zero appends, so the initial file is the first line alone.
- The older `Stage 3 review round N (...)` wording is a rejected variant; use `Legion proof review round N`.

## Handoff-only push vs code push

- A commit that touches only `.legion/` is a handoff-only push: its head message ends with
  `skip-checks: true` as the last line, preceded by two blank lines (one blank line does not work —
  GitHub then runs every workflow). Write it with trailers off so the pane's `Omp-Session` overlay
  does not land after it.
- A commit that touches a tracked path (the marker, `docs/solutions/`, the retro removal of
  `.legion/<KEY>/`) is a code push: no `skip-checks` trailer and none of GitHub's `[skip ci]`
  family. The daemon classifies by changed paths, so a wrong trailer stalls the issue.
- The reviewer-approved **code** head and the PR **head** can differ: a handoff-only commit layered
  on the code head carries that code head's CI verdict via `skip-checks`, and no lane reruns on the
  handoff head. Record proofs against the head where the behaviour actually changed.

## Evidence

Issue S395420578-1, PR sjawhar/legion-smoke#677. Code head `beaaabea` adds the round-3 line;
`grep -q '^S395420578-1 — Primary durable workflow profile$'` → `ACCEPTANCE_PASS`, hyphen control
→ exit 1. `ci` run 39271521994 green at `beaaabea`. Handoff commits (`.legion/` only) carried the
trailer; the marker and this document are code pushes. Reviewer approved with 0 findings.
