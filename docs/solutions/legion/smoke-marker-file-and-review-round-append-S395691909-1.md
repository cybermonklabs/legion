---
title: "Legion smoke marker files: em-dash convention and one-line-per-review-round appends"
category: legion
tags:
  - legion-smoke
  - review-round-append
  - byte-convention
  - handoff-only-push
  - diff-scope
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S395691909-1"
  - "sjawhar/legion-smoke#634"
---

# Legion smoke marker files: em-dash convention and one-line-per-review-round appends

A `smoke/<issue>.md` marker and each review-round append are byte-exact changes. Prove them by bytes and a single-hunk diff, not by eye.

## Rules

- Line 1 is exactly `S<issue> — Primary durable workflow profile`. The dash is U+2014 EM DASH (UTF-8 `e2 80 94`) with one ASCII space (`0x20`) on each side. Not `-` (`2d`), not `:` (`3a`), not prose. Write it with `printf 'S<issue> \xe2\x80\x94 Primary durable workflow profile\n'`.
- Copy the reviewer's requested line verbatim; do not copy a neighbouring file. Wording drifted across peers (older files say `Stage 3 review round N`, current say `Legion proof review round N`), so a neighbour is not a template.
- Append with `>>`, one line terminated by a single LF, no blank line, never a rewrite: `printf 'Legion proof review round <N> (S395691909)\n' >> smoke/<issue>.md`. A whole-file rewrite risks normalising the line-1 em dash to an ASCII hyphen that passes a visual read.
- Prove every change three ways, scoped to the target path: `hexdump -C`/`cat -A` (every line ends `0a`, line 1 offset 13 is `e2 80 94`, the appended line ends `… 29 0a`); `wc -l` equals the expected count; `git diff <prevHead> <head> -- smoke/<issue>.md` shows exactly one `+` line and no other hunk. The per-round negative control is the prior head's lower line count.
- Diff scope (AC "exactly one new file") is about the repository change, not branch bookkeeping. `gh pr diff --name-only` also lists `.legion/<issue>/*.json` handoffs; retro removes them before merge. Scope every diff/negative-control assertion to `-- smoke/<issue>.md` and record a deviation note that `.legion/*` are Legion handoff mechanics, rather than reporting extra files as scope violations.
- Two heads per round: a CI-verified code head and a later handoff-only head pushed with a `skip-checks: true` trailer that deliberately starts no CI. Read CI at the code head (`gh run list`, read-only; never rerun) and confirm `git diff <code-head> <handoff-head>` touches only `.legion/`. A check-less handoff head is expected, not a missing gate.

## Evidence

Issue S395691909-1, PR sjawhar/legion-smoke#634. One marker file `smoke/S395691909-1.md`, then three CHANGES_REQUESTED review rounds each appending one line, ending APPROVED; final file 4 LF-terminated lines (+4/-0).

- Code head `4749640`: `ci`/`check` (run 39217810826) and `fail-on-demand`/`gate` (run 39217810809) both success; handoff head `d46ea224` pushed handoff-only with `skip-checks: true`, daemon carried the code-head verdict.
- `git diff acfe71dd 4749640 -- smoke/S395691909-1.md` → exactly one `+` line (round-3 append); lines 1-3 unchanged; line 1 bytes `… 2d 31 20 e2 80 94 20 …`.
- Peers showing the superseded conventions the rules above pin against: `smoke/SMOKE-3.md` (colon style), `smoke/S392670114-1.md` (prose + trailing period), `smoke/S393198222-1.md` (bare id, no descriptor), `smoke/S393569515-1.md` (`Stage 3 review round N` wording). `smoke/S394802790-1.md` is the matching current-convention peer.
