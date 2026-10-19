---
title: "Proving a one-file smoke leaf marker without counting Legion handoffs"
category: legion
tags:
  - legion-workflow
  - smoke-proof
  - proof-surface
date: 2026-10-19
status: active
module: legion-smoke
related_issues:
  - "S396017608-5"
  - "sjawhar/legion-smoke#639"
---

# Proving a one-file smoke leaf marker without counting Legion handoffs

A `-5` leaf issue ships exactly one file, `smoke/<CHILD-KEY>.md`, whose sole content is
`<CHILD-KEY>\n` — the child key, trailing newline, 13 bytes for an `S…-5` key. Use the child key,
never the parent `-1` key; a leaf creates no parent marker.

Prove the "single path changed" criterion against the code commit, not the pull request diff.
Every Legion phase drops a `.legion/<ISSUE>/<phase>.json` handoff onto the branch, so
`gh pr diff <n> --name-only` lists the marker plus those handoff JSONs. The handoffs are removed
at retro and are not part of the shipped change, so measure the change with
`jj -R "$LEGION_WORKSPACE" diff -r <code-commit> --name-only`, which shows only the marker; cite the
PR diff only with a note that the extra paths are Legion artifacts removed before merge.

Pin the bytes with a `cmp`, not a visual check: `printf '<CHILD-KEY>\n' | cmp - <file>` exits 0,
and the negative control `printf '<PARENT-KEY>\n' | cmp - <file>` must exit non-zero (differs at
the key's last character). `wc -c` must read 13 for an `S…-5` key.

Read CI, do not re-run it: `ci` (sleeps then exits 0) and `fail-on-demand` (red only if a `.fail-me`
file exists) run on both `push` and `pull_request`. The handoff-only push that carries the
implement/test/review JSON is skipped with `skip-checks: true`, so CI's verdict lives on the code
commit SHA, not the branch tip; cite the code-commit SHA when quoting a green conclusion.

## Evidence

Issue S396017608-5, PR sjawhar/legion-smoke#639. Code commit `8cf073ac` added
`smoke/S396017608-5.md` = `S396017608-5\n` (13 bytes). `jj diff -r 8cf073ac --name-only` showed the
single path while `gh pr diff 639 --name-only` also listed `.legion/S396017608-5/plan.json` and
`implement.json`. `ci` run 39222998770 and `fail-on-demand` run 39222998762 were both success at
`8cf073ac`; the PR head `78045d28` was a handoff-only `skip-checks` push. Implementer and tester
each proved all four acceptance criteria (single path, exact bytes, CI green, `Dispatch:` line)
with negative controls; the reviewer approved with zero findings.
