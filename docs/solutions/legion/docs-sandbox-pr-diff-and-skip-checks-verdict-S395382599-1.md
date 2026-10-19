---
title: "Proving a docs-sandbox marker PR: handoff artifacts in the diff and the skip-checks CI verdict"
category: legion
tags:
  - legion-workflow
  - docs-only-pr
  - skip-checks
  - ci-verdict
  - smoke-proof
date: 2026-10-19
status: active
module: legion-smoke
related_issues:
  - "S395382599-1"
  - "sjawhar/legion-smoke#643"
---

# Proving a docs-sandbox marker PR: handoff artifacts in the diff and the skip-checks CI verdict

On a Legion docs/markdown sandbox PR (one marker file under `smoke/`), three things trip proof
and verification. Follow these rules.

## `.legion/<issue>/` handoffs appear in the diff; prove against the content path, not the stat total

Every phase commits its handoff JSON under `.legion/<issue>/`, so
`git diff --stat origin/main..<head>` lists `.legion/<issue>/{plan,implement,test,review}.json`
alongside the one real content change. An acceptance criterion like "no other file changes" is
about the repository content, not the workflow artifacts (retro removes `.legion/<issue>/` in its
final commit before merge). Prove AC2 by naming the single content path explicitly
(`smoke/<issue>.md`) and treating the `.legion/` entries as expected workflow artifacts — do not
read the stat's file count as a violation.

## Read the CI verdict on the code head, never the PR tip

A handoff-only push (commits touching only `.legion/`) ends its head message with `skip-checks:
true` as the last line, preceded by two blank lines, so GitHub starts no workflow on it. That tip
sits above the last code-bearing commit. Consequence: `gh pr checks <pr>` on the PR tip reports
`no checks reported on the branch`. That is expected, not a failure. Read the verdict on the code
head: `gh api repos/<owner>/<repo>/commits/<code-head-sha>/check-runs`, and record the code-head
SHA in the proof's `headSha`. The daemon carries the code head's verdict to the skip-checks tip.

The trailer shape is exact: last line, after two blank lines (one blank line is not enough —
GitHub then runs every workflow as if the line were absent). Write it with jj trailers off
(`--config 'templates.commit_trailers=""'`) so the pane's `Omp-Session` overlay is not appended
after it.

## Marker bytes are exact: em dash U+2014 and a trailing newline on every line

The naming line is `<issue> — <profile>` with an em dash **U+2014** (`e2 80 94`) and surrounding
spaces, followed by a trailing `\n`. Each review-round append is its own line ending in `\n`, with
the reviewer's requested text verbatim. Verify with `xxd` against a sibling marker (e.g.
`smoke/S396017608-1.md`) rather than trusting an editor; a wrong dash or a missing final newline is
a silent AC failure.

## Evidence

- PR sjawhar/legion-smoke#643, code head `51e7b037`: `git diff --stat origin/main..51e7b037`
  listed `smoke/S395382599-1.md` plus four `.legion/S395382599-1/*.json` handoffs; only the first
  is repository content.
- `gh pr checks 643` on the skip-checks handoff tip reported no checks; `check` (`ci`) and `gate`
  (`fail-on-demand`) were both `success` read on the code head `51e7b037`.
- `smoke/S395382599-1.md` at head: `53 33 ... 20 e2 80 94 20 ...` — em dash U+2014, every line
  terminated with `0a`, matching sibling `smoke/S396017608-1.md`.
