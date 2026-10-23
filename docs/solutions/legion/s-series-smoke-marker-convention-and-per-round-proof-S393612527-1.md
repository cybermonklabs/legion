---
title: "S-series smoke marker: copy the newest peer, and refresh the proof's negative control every review round"
category: legion
tags:
  - smoke-marker
  - s-series-convention
  - review-round-proof
  - em-dash-vs-colon
date: 2026-10-23
status: active
module: smoke
related_issues:
  - "S393612527-1"
  - "sjawhar/legion-smoke#750"
---

# S-series smoke marker: copy the newest peer, and refresh the proof's negative control every review round

A `smoke/<ISSUE-KEY>.md` marker is a fixed template, not free prose. Clone the newest S-series
peer and substitute only the key; then maintain the proof as each review round appends a line.

## Rule

- Copy the body shape from the **newest** current S-series peer (e.g. `smoke/S393512875-1.md`,
  `smoke/S393635167-1.md`, `smoke/S396017608-1.md`), not from the spec prose and not from an
  arbitrary old file. The template is exactly: line 1 `<full-key> — Primary durable workflow profile`;
  one appended line per review round `Legion proof review round <N> (<base-key>)`.
- Two byte-level points are the whole risk, both verifiable with `grep`:
  - **Title separator is a spaced em dash** `U+2014` (` — `), never a colon and never a hyphen.
    The colon belongs to the legacy `LEGSMOKE-*` family; at least one S-file slipped a colon in, so
    this is a recurring mistake. Negative control: `head -1 <file> | grep -c ':'` → `0`.
  - **The review-round parenthetical cites the base key with no `-N` suffix**:
    `Legion proof review round 2 (S393612527)`, never `(S393612527-1)`. The `-N`-suffixed key
    appears only on the title line.
- Phrasing drifted over time (older files say `Stage 3 review round N`). Copy the latest neighbor's
  wording, `Legion proof review round N`, verbatim.
- **Green CI is not proof here.** `ci.yml` sleeps and exits 0; `fail-on-demand.yml` fails only when a
  `.fail-me` sentinel exists. Neither reads the marker. The real proof is a direct file read plus the
  two `grep` convention checks on the pushed head; record it as the `E2E` line.
- **Refresh the negative control every review round.** Each round appends exactly one line and makes
  the file one line longer. A control of the form "round `N+1` line is absent"
  (`grep -c 'Legion proof review round <N+1>' <file>` → `0`) is correct only for the current round;
  next round that same line becomes present and the stale control reads `1`. Rewrite the implementer
  `E2E` line and its negative control to the new head and the next-absent round every push. The
  tester's `E2E` line goes stale the same way but is the tester's to refresh.

## Evidence

Issue S393612527-1 (PR sjawhar/legion-smoke#750) added `smoke/S393612527-1.md` and went through
three review rounds, each appending one `Legion proof review round N (S393612527)` line, ending at a
4-line file (title + 3 rounds) matching peers. The plan handoff's `gapAnalysis` had already flagged
both convention points (colon vs em dash; `-1`-suffix in the parenthetical) before implementation,
which is why the first push was correct; reading that gap analysis was decisive. Each round's
negative control had to be re-pointed: round 1 used `round 2` absent, round 2 used `round 3` absent,
round 3 used `round 4` absent — the prior round's control would have read `1` once its line landed.
Handoff-only pushes carried `skip-checks: true` as the last line after two blank lines; the
line-appending code pushes carried no trailer.
