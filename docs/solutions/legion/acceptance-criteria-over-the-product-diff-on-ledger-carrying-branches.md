---
title: "Acceptance criteria over the product diff on ledger-carrying Legion branches"
category: legion
tags:
  - acceptance-criteria
  - legion-ledger
  - jj-fileset
  - github-compare-api
  - product-diff
date: 2026-09-28
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-310"
  - "sjawhar/legion-smoke#424"
---

# Acceptance criteria over the product diff on ledger-carrying Legion branches

## Problem

A spec written against a plain branch says things like "`gh pr diff <n> --name-only` prints
exactly one path" or "`git show <head>:<path>` prints the line". On a Legion branch that check
is false at almost every head a role verifies: the planner, implementer, tester, and reviewer each
commit a `.legion/<phase>.json` handoff onto the same branch, and retro adds `docs/solutions/`.
At the head the reviewer approved on LEGSMOKE-310 (`a537bf84`), `gh pr diff --name-only` printed
four paths — three `.legion/*.json` plus the one product file
(`.legion/review.json`, `specNotes[0]`).

Under the Go daemon (until its Stage 7) no role pushes the `.legion/` deletion before the merge:
the reviewer approves a head that still carries `.legion/`, retro commits above it, and the
operator removes `.legion/` from main afterwards. So there is no pre-merge head at which the
spec's literal form holds. A reviewer reading the spec literally would request changes on a
passing pull request.

## What worked

The planner redefined every file-count criterion over the **product diff** — the branch diff
excluding the Legion ledger and retro's learnings — and gave both a workspace form and a
GitHub-only form, because the reviewer verifies on GitHub and never from a handoff
(`.legion/plan.json`, `acceptanceCriteria[0].command`):

```bash
# Workspace form (jj). ONE fileset expression: two positional '~' arguments are unioned
# by jj and exclude nothing.
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && \
  jj -R "$LEGION_WORKSPACE" diff --from main@origin --to <head-sha> --summary \
    '~(.legion | docs/solutions)'
# expect exactly:  A smoke/stage4b-legsmoke-310.md

# GitHub form (compare API), same exclusion:
legion gh -- api "repos/<owner>/<repo>/compare/main...<head-sha>" \
  --jq '.files[] | select((.filename | startswith(".legion/") or startswith("docs/solutions/")) | not) | "\(.status) \(.filename)"'
# expect exactly:  added smoke/stage4b-legsmoke-310.md
```

This is the same fileset the unchanged-diff fingerprint in `skill://legion-worker` uses, so one
exclusion rule serves the acceptance check, the fingerprint, and the merger's
`--summary ... '~docs/solutions'` check.

Negative control that proves the filter is not vacuous: run the same compare filter with
`<head-sha>` replaced by the base's tip (`main...3e9c92600b2f` here). It must print nothing —
no product path changed before this branch.

The planner recorded the translation in `discoveredScope` so no later role reached for the
literal command; the reviewer recorded it in `specNotes` as "not a finding". Both records were
read by the next phase (`.legion/test.json`, `.legion/review.json`).

## The cross-head invariant: the product blob sha

Every ledger commit moves the PR head but must not touch the product. Rather than re-deriving
bytes at each head, record the product file's blob sha once and compare it at every later head:

```bash
legion gh -- api "repos/<owner>/<repo>/contents/<path>?ref=<head-sha>" --jq .sha
# LEGSMOKE-310: a410619ee86319e5e1251058fa12fe0fb7f03dca at c1142353, 76eed30d, a537bf84, 38f10718
```

The implementer wrote it as `productBlobSha` in `.legion/implement.json`; the tester compared
it at the product head and the current head and additionally `cmp`'d the decoded bytes
(`.legion/test.json`, `proof[1].observed`); the reviewer cited it at its own head. For a
multi-file change the same role is played by the fingerprint command in `skill://legion-worker`.

## Rules for the next issue

- **Spec authors:** phrase file-set acceptance as "the product diff — excluding `.legion/` and
  `docs/solutions/` — is exactly …". Name a GitHub-readable check (compare API, contents API)
  beside any workspace command, since workers read the shared clone through jj and reviewers read
  GitHub.
- **Planners:** when a spec's `Check:` sentence names `git`, `gh pr diff --name-only`, or a bare
  file count, translate it to the two forms above, record the translation in `discoveredScope`,
  and add the base-tip negative control.
- **Reviewers:** a literal-check mismatch caused only by `.legion/` or `docs/solutions/` is a
  `specNotes` entry, never a finding.
- **Everyone:** the fileset is one expression. `'~.legion' '~docs/solutions'` as two arguments
  selects every file.
