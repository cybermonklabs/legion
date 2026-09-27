---
title: "Running the Legion durable workflow on a docs-only smoke change"
category: legion
tags:
  - legion-workflow
  - docs-only
  - ci-verification
  - review-rounds
  - jj-push
date: 2026-09-27
status: active
module: legion-smoke-workflow
related_issues:
  - "S392523424-1"
  - "sjawhar/legion-smoke#378"
---

# Running the Legion durable workflow on a docs-only smoke change

Reusable lessons from PR #378 (issue S392523424-1): a docs-only Stage 3 durable-workflow
proof that adds one Markdown marker file and threads it through implement, three review
rounds, test, review approval, and retro.

## Review rounds are strict single-line appends

Each `CHANGES_REQUESTED` round scoped exactly one appended line ("the spec permits one more
line ... change nothing else"). The fix commits (`4253340f`, `3786c8fd`, `93775603`) each
added only that line, leaving earlier lines byte-identical. Touching a prior line is
out-of-scope churn and breaks the round-N verbatim string check the tester re-runs. Land each
correction as a new commit on the existing branch — never rebase — and reply on the review with
`Fixed in <sha>: <one line>`.

## Docs-only diffs skip ce-simplify-code and the thermonuclear pair — say so explicitly

A Markdown-only diff has no runtime code, so `ce-simplify-code` and the two thermonuclear
review agents do not run. Record the skip reason inline (the `Thermo` line / approval body:
"docs-only diff, no runtime code") so the omission is auditable rather than looking like a
missed gate. Set `docsOnly: true` in every phase handoff.

## Prove trivial CI by pinning run id + job + head SHA, and prove the negative gate

This repo's CI is two trivial workflows: `ci`/`check` (`sleep 20 && exit 0`) and
`fail-on-demand`/`gate` (fails only if a `.fail-me` file exists). Green is meaningful only when
the CI line pins each workflow's run id and job to the exact head SHA and notes
`(no .fail-me present)` — the gate's green proves the absence of the sabotage file, so state it.

## `gh run watch` outlives the 60s credential grant — poll instead

`legion gh -- run watch` on a queued run repeatedly 403'd with `GRANT_EXPIRED`: the watch
loop outlives the pane's 60-second credential grant even though `ci` only sleeps 20s. Working
pattern: push, then a single `sleep 28 && legion gh -- run list --commit <sha> --json
...,conclusion` in one bash call — one grant redemption, completes inside the 60s window, and
returns the settled conclusions. Never chain two long watches in one command.

## The handoff commit retriggers CI on a new head; cite the product head for E2E

After the code commit, the `implement: record handoff` split adds a `.legion/` commit and
pushing it moves the branch head, retriggering CI. The product-file diff is unchanged, so cite
the product-change head in the `E2E` proof and confirm bare CI green at the new head. The
`fork_point`/`sha256sum` fingerprint (fileset `'~(.legion | docs/solutions)'`) is what lets a
later role confirm the product diff is unchanged across these process-only commits.

## Keep the shared bookmark safe with the ancestry `behind` check

Every issue workspace is a `jj workspace` of one shared clone, so another role's push moves
`legion/<KEY>@origin` here immediately. The push procedure's `behind` check (refuse unless `@-`
descends from the remote bookmark) before `bookmark set --allow-backwards` + `jj git push`
prevents a sideways move that would silently drop another role's commit.

## `wc -l` proofs need a trailing newline; pair with a content check

`wc -l < file >= N` counts newlines, so a final append lacking a trailing newline undercounts
and fails. Always pair the count with a `sed -n 'Np'` string-equality check (as PR #378 does)
so the proof is not hostage to trailing-newline handling, and write the file with a trailing
newline.

## Assert the empty-threads state explicitly

The reviewer used top-level `CHANGES_REQUESTED` reviews with no inline threads. Even so, record
`Threads: 0 resolved, 0 unresolved` and cite `legion threads resolve` returning
`no unresolved threads`, so the merger can trust nothing is dangling.
