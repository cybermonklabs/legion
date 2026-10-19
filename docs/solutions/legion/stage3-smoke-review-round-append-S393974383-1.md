---
title: "Legion review-round append: scope, CI-head, and byte-exact proof"
category: legion
tags:
  - legion-workflow
  - review-rounds
  - ci-verification
  - proof
date: 2026-10-19
status: active
module: legion-phase-workers
related_issues:
  - "S393974383-1"
  - "sjawhar/legion-smoke#628"
---

# Legion review-round append: scope, CI-head, and byte-exact proof

When a spec permits iterative single-line edits across review rounds, the implementer
appends one line per round to one file and reproves scope, CI, and content each round.
These rules make each round self-evidently correct and keep the merge gate unblocked.

## Rules

- **Pre-authorize iterative edits in the plan handoff.** If the spec allows one more line
  per review round, say so in `plan.json` (and `plan.md`) with the spec citation. A bot or
  reviewer thread asking "is this change needed?" then resolves in one exchange by quoting
  the plan, instead of re-litigating scope mid-round.
- **Keep the code commit scoped to one file every round, and prove it.** Re-run
  `gh pr diff <n> --name-only` and `jj diff -r <code-commit> --git` each round. When the
  hunk shows prior lines as pure context (`@@ -1,3 +1,4 @@`, no `-`/`+` pair on earlier
  lines), that context *is* the negative control that the append disturbed nothing — state
  it as such rather than merely asserting "nothing else changed".
- **Cite the code head for CI, not a handoff-only head.** A handoff-only commit pushed with
  `skip-checks: true` starts no CI run; it inherits the code head's verdict. Name both SHAs:
  the code SHA the `ci`/`fail-on-demand` runs actually ran on, and the handoff head that
  carries that verdict. Claiming "CI green at PR head" when the PR head is a handoff-only
  commit shows a reviewer a head with no runs.
- **Pin non-ASCII separators to their codepoint and prove them byte-exactly.** The title
  separator is an em-dash U+2014 with surrounding spaces, not an ASCII hyphen. Fix the exact
  bytes in the plan and verify with `printf '...\xe2\x80\x94...' | cmp - <file>` (no
  difference), never an eyeball `cat` that renders both dashes identically.
- **`fail-on-demand/gate` is a second real CI job.** It goes red only if a root `.fail-me`
  file exists; "added no `.fail-me`" is a legitimate reusable negative control for the gate.
- **A docs-only / Markdown diff legitimately omits the `ce-simplify-code` + thermonuclear
  pair** (no runtime code). State the omission with its reason; do not silently skip it.

## Evidence

Issue S393974383-1 (PR sjawhar/legion-smoke#628): `smoke/S393974383-1.md` grew 1→4 lines,
one appended line per review round 1-3, code commit `4ea69e83` the final code head. CI at
`4ea69e83`: `ci` run 39210601651 and `fail-on-demand` run 39210601731 both success; the PR
head `d9cdfc51` was a handoff-only `skip-checks` commit carrying that verdict. The tester
confirmed content with a byte-exact `printf | cmp` (FOUR_LINE_EXACT_OK), 0 failures; review
approved with 0 findings; the single bot thread ("is this file change needed?") resolved in
one exchange against the plan's pre-authorization.
