---
title: "One-file docs change in a shared jj workspace across review rounds"
category: legion
tags:
  - jj-workspace
  - review-rounds
  - product-vs-process-diff
  - docs-only-pr
  - e2e-proof
date: 2026-09-27
status: active
module: legion-phase-pipeline
related_issues:
  - "S391724318-1"
  - "sjawhar/legion-smoke#405"
---

# One-file docs change in a shared jj workspace across review rounds

Durable learnings from S391724318-1 / PR #405: a docs-only Legion change that added exactly one
Markdown marker file (`smoke/S391724318-1.md`) and then appended one line per review round for
three rounds. No runtime code, no CI, no build. The value here is the verification discipline,
not the file.

## Product diff vs. process artifacts — the `'~.legion'` fileset is mandatory

Every Legion phase commits its own handoff JSON under `.legion/` on the same issue branch, so a
raw `jj diff --from main@origin` (or `legion gh -- pr diff`) is **never** one file — it carries
`.legion/{plan,implement,test,review}.json` alongside the product change. An acceptance criterion
of the form "exactly one added file under `smoke/`" is only verifiable against the *product* diff:

```bash
jj -R "$LEGION_WORKSPACE" diff --from main@origin --to legion/<KEY> --summary '~.legion'
# → exactly: A smoke/<KEY>.md
```

Use the single fileset `'~.legion'` (the same exemption the unchanged-diff fingerprint uses).
Without it, AC1/AC2 look violated. State in the E2E proof that the only other diff paths are the
exempt `.legion/` process artifacts, so the reviewer reading the raw PR diff is not surprised.

## Pin proofs to the product-content revision, not the branch head

The branch tip advances with each `implement: record handoff` commit, so the PR head SHA
(`24765dcc`) differs from the SHA that actually introduced the product content (`c3ec2b20`).
Record both in the E2E line — head SHA for GitHub, product-content SHA for the change itself —
and run the product-only diff against the bookmark, not `@`, which sits on an empty working copy
above the handoff commit.

## Verify the convention against more than one sibling — prefix, not whole-line

The plan verified "line 1 = issue key" against a single sibling (`smoke/S392703949-1.md`, bare
key). But `smoke/S394570180-1.md` has line 1 `S394570180-1 — Primary durable workflow profile` —
key **plus a suffix**. The real invariant is "line 1 **starts with** the issue key." A whole-line
exact assertion (`grep -qx '<KEY>'`) would reject a valid sibling-style file; assert the prefix
instead (`grep -q '^<KEY>'`) and check several siblings before fixing the format.

## Per-round append discipline: one focused commit + assert the absence of an extra line

Each review round was one product commit (`smoke: append review round N line`) followed by a
separate handoff commit, with an explicit "append below existing lines, change nothing else"
guard. The most likely silent defect in an append is an off-by-one extra/blank line, so each proof
asserted **both**:

- the new line's content (`sed -n '<N>p'` matches the round text), and
- the absence of any further line (`sed -n '<N+1>p' | grep -q .` → no match).

## The negative control is a real drill, not narration

Prove the product-only guard actually fires: deliberately dirty a second file
(`printf '\nextra\n' >> README.md`), confirm `jj diff --summary` now shows `M README.md` (a second
path that would be rejected), then `jj restore` it and confirm the working copy is clean before
pushing. A guard that only ever ran on a one-file diff has never been shown to reject anything.

## Rounds ran through Legion handoffs, not GitHub threads — still run the threads check

All three review rounds were delivered via the Legion review handoff; no GitHub review thread was
ever opened, and `legion threads resolve --pr <n> --repo <owner>/<repo>` reported
`no unresolved threads` (exit 0) each round. Do not wait on GitHub PR comments to drive rounds,
but still run the threads-resolve check before every corrective push and quote its output in the
PR body's `Threads` section.

## Record deliberate skips up front

For a docs-only PR, `ce-simplify-code` and the thermonuclear pair
(`thermonuclear-deep-review` / `thermonuclear-code-quality`) do not run (no runtime code) and CI
is `none`. State the skip and its justification in the plan and implement handoffs so the reviewer
does not flag a missing gate; do not invent a test suite for a marker file — the PR diff is the
regression-lockable surface.
