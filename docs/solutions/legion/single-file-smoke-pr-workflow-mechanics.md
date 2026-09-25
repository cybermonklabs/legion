---
title: "Legion single-file smoke PR: workflow mechanics that bite"
category: legion
tags:
  - legion-workflow
  - jj
  - handoff
  - github-pr
  - credentials
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S390756943-1"
  - "sjawhar/legion-smoke#183"
---

# Legion single-file smoke PR: workflow mechanics that bite

Context: a Stage 3 proof issue whose product change is intentionally trivial — create/append
marker lines to one text file (`stage3-smoke.txt`) across an initial implement round plus three
review-correction rounds, all in the **same** PR (#183). No runtime code. The value is entirely
in exercising the plan -> implement -> test -> review -> retro workflow, so every learning below is
about mechanics, not the feature.

## Process artifacts vs. the product diff

`.legion/plan.json|implement.json|test.json` are committed onto the issue branch and **will**
appear in `gh pr view --json files` and `gh pr diff`. Never judge "one file changed" from the raw
file list. Measure the product diff with the same fileset Legion uses everywhere,
`~(.legion | docs/solutions)`:

```bash
legion gh -- pr view <n> --repo <owner>/<repo> --json files \
  --jq '[.files[] | select(.path | startswith(".legion/") | not)] | length'
```

Here that count stayed exactly `1` across all four rounds. The `.legion/` files are stripped in the
reviewer-directed deletion push before the human merge, so a "file present on main post-merge"
criterion only ever concerns the product file.

## Review rounds stack in the SAME PR and file

Each correction appended one line to the existing `stage3-smoke.txt` as a new commit on bookmark
`legion/S390756943-1` and rode the implementer's re-push — no second PR, no second product file.
As implementer on round N>0: edit the existing file, re-push the existing bookmark; do not branch,
do not open a new PR. The plan explicitly forbids a new PR for the "one more line" criterion.

## jj: the working copy IS a commit — keep scratch files out of the workspace

Writing a scratch file (a PR-body markdown) into `$LEGION_WORKSPACE` dirtied the
already-pushed product commit, changing its hash and desynchronizing the bookmark. Keep all
non-product scratch (`--body-file`, notes) under `/tmp`, outside `$LEGION_WORKSPACE`. If you must
stage unrelated content, `jj new` an empty child first so it lands in its own commit.

Also: any `jj describe`/`jj split` rewrite refreshes the committer timestamp, so a commit's hash
changes even when its tree is byte-identical. After such a rewrite, `jj git push` again to realign
the remote bookmark.

## Handoff commit + bookmark discipline

Per round, the reliable sequence is:

1. `jj new` (empty child) so the handoff lands in its own commit.
2. Write the handoff via the `legion` tool `handoff_write` (it writes `.legion/<phase>.json`).
3. `jj split -m "<phase>: record handoff" .legion/<phase>.json`.
4. `jj bookmark set legion/<KEY> -r <handoff-commit>` — the bookmark does **not** auto-follow `@`
   after `new`/`split`; set it explicitly to the handoff commit, which is the content tip.
5. `jj git push --bookmark legion/<KEY>`.

The tester and reviewer run as the review App, which **cannot push**; their handoff commits sit
above your bookmark and ride your next implementer push. So on a corrective push your identity
check must be **per commit** (`author.email() | committer.email()` for each `main@origin..@`
commit), not over the whole list: your commits are the implementer App, the tester/reviewer/plan
commits are legitimately the review App.

## HEAD SHAs go stale immediately

Product heads progressed 185ed0a -> 57bd406 -> 3e07e00 -> 5807605 as handoff/bookmark commits were
added. Any `headSha` written into a handoff JSON is stale almost at once. A downstream phase MUST
re-resolve the live PR head (`legion gh -- pr view <n> --json headRefOid`) before verifying rather
than trusting an upstream-recorded SHA.

## Credential grant expiry on long-running bash commands

The per-command `LEGION_GRANT` is valid ~60s from the bash call that minted it. A command that
first spends that budget elsewhere — e.g. `legion gh -- run watch <ci>` while a queued 20s-sleep CI
run finishes — and then pipes into another `legion gh` in the **same** command fails the second
call with `daemon returned 403: {"code":"GRANT_EXPIRED"}`. Split a long wait and the credentialed
read that follows it into **separate** bash calls so each gets a fresh grant.

## CI has two required workflows

Verify both `ci` (job `check`) and `fail-on-demand` (job `gate`) are `success` at the **current**
head each round; a green from a prior head does not carry. And the PR body must always contain the
`Dispatch: <KEY>` line — a plan gate independent of the diff; confirm it survives re-pushes.
