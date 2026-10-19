---
title: "Legion smoke-proof marker file conventions"
category: legion
tags:
  - smoke-proof
  - marker-file
  - review-rounds
  - skip-checks
date: 2026-10-19
status: active
module: legion-smoke
related_issues:
  - "S394586105-1"
  - "sjawhar/legion-smoke#686"
---

# Legion smoke-proof marker file conventions

A Stage 3 smoke proof in `sjawhar/legion-smoke` is one marker file `smoke/<KEY>.md` taken through
the full plan → implement → test → review → retro pipeline. Follow these conventions so the
trivial change does not stall the pipeline.

- The first line of a `<KEY>-1.md` marker is exactly `<KEY> — Primary durable workflow profile`.
  The separator is a spaced em dash U+2014 (bytes `20 e2 80 94 20`), not a hyphen or colon. Verify
  with `xxd`, never by eye, and match a sibling `-1` marker before committing.
- Each review round appends exactly one line `Legion proof review round <N> (<KEY-without-suffix>)`
  below the lines already present, and changes nothing else. `<N>` is the sequential round number
  and the key is the bare issue key with no `-1` suffix. Take the wording from the reviewer's
  request on this PR, not from a sibling file: sibling markers drift (e.g. `S394570180-1.md` uses
  `Stage 3 review round N`), so copying one produces the wrong line.
- Never add a `.fail-me` file. The required `gate` check (`fail-on-demand.yml`) fails only when
  `.fail-me` exists; `ci.yml`'s `check` job always passes. A marker PR keeps CI green by adding
  nothing but the one marker line.

## Verify at the code head, not the handoff head

Each phase commits its `.legion/<KEY>/<phase>.json` handoff. A handoff-only push carries a
`skip-checks: true` trailer (last line, after two blank lines), so GitHub starts no workflow on
that head and the daemon carries the code head's verdict forward. Consequently every proof and CI
citation must name the **code** head (where the workflows actually ran), not the later handoff
head. Cite the run ids from `gh run list --branch legion/<KEY>` at the code head SHA.

## Evidence

Issue S394586105-1 / PR #686: one 4-line marker (`smoke/S394586105-1.md`) — first line plus three
per-round appends across three CHANGES_REQUESTED review rounds, then APPROVED. Implementer and
tester proofs both named the code head `f8ff604c` (ci run 39300280199, fail-on-demand run
39300280246 green); the handoff heads carried `skip-checks: true`. One bot review thread ("is this
file change needed?") was answered, accepted by the reviewer, and resolved by the PR author.
