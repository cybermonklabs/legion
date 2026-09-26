---
title: "Write acceptance criteria against the product diff, not the raw PR files list"
category: legion
tags:
  - legion-worker
  - planner
  - acceptance-criteria
  - handoff-ledger
  - pr-files
  - fileset
  - parallel-proof-trees
  - workflow-vs-job-names
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-278"
  - "sjawhar/legion-smoke#326"
---

# Write acceptance criteria against the product diff, not the raw PR files list

Distilled from PR #326 (issue LEGSMOKE-278, "Stage 4b proof tree 1: the whole workflow"): one new
file `smoke/stage4b-legsmoke-278.txt` with a single line. The product change was one commit
(`27d7cb53`); the plan's first acceptance criterion could not be verified the way the plan wrote
it, and three phases each had to reinterpret it.

## The Files changed tab always shows the ledger while `.legion/` is on the branch

`.legion/plan.json` acceptance criterion 1 read "adds exactly one file" and gave the command
`legion gh -- pr view <n> --json files --jq '.files[].path'` with "expect the single path". At
the moment the PR opened the branch already carried `.legion/plan.json` (the planner's own
handoff commit `cb2488ba`), so the command printed two paths; at approval (`ca745813`) it printed
four (`.legion/implement.json`, `.legion/plan.json`, `.legion/test.json`, the product file).

Every later role invented its own reconciliation:

- implementer (`.legion/implement.json`, `deviations[0]`, `proof[0]`):
  `jj -R "$LEGION_WORKSPACE" diff --from main@origin --to <head> --summary '~.legion'`
  → exactly `A smoke/stage4b-legsmoke-278.txt`;
- tester (`.legion/test.json`, `proof[0]`): the GitHub compare API `main...<head>` filtered to
  non-`.legion/` paths, plus the same jj diff;
- reviewer (review 5326202356 body): "adds exactly one product file … plus the three
  `.legion/*.json` handoff files (process ledger)".

They agreed, but each spent a round of thought and a paragraph of prose on it, and a reviewer
reading the criterion literally could have requested changes for a passing PR.

**Rule for the planner:** when a criterion counts files, define it over the product diff with the
same fileset the unchanged-diff fingerprint uses, and say "product file" in the sentence:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" diff \
  --from main@origin --to <head-sha> --summary '~(.legion | docs/solutions)'
# expect exactly: A smoke/<file>
```

or, on GitHub alone,
`legion gh -- api "repos/<owner>/<repo>/compare/main...<head-sha>" --jq '.files[] | select(.filename | startswith(".legion/") | not) | .filename'`.
The Files changed tab stays the right surface for *content* (the contents API at `ref=<head>`)
and for "no path outside `smoke/`", not for "exactly N files" while the ledger lives on the
branch. The fileset must be one expression: two positional `'~.legion' '~docs/solutions'`
arguments are unioned by jj and exclude nothing.

## A new keyed file is the collision-safe shape for parallel proof trees

The spec's Rejected section chose a new file under `smoke/` over appending to `README.md` or the
root `smoke.txt` (LEGSMOKE-270 / PR #319 appended to `smoke.txt`), because a sibling proof tree,
LEGSMOKE-279 on `legion/LEGSMOKE-279`, was pushing to the same shared clone at the same time. The
code-quality pass at `ca745813` called this out as the "collision-safe choice given the parallel
legion/LEGSMOKE-279 branch". It held: the PR stayed `MERGEABLE`/`CLEAN` through four heads with
no rebase. For any fixture repository that runs proofs concurrently, name the file after the issue
key (`smoke/<stage>-<key>.txt`) and never touch a file another tree appends to.

## Workflow names versus job names: the plan's risk list is where this belongs

`ci.yml` names its workflow `ci` and its job `check`; `fail-on-demand.yml` names its workflow
`fail-on-demand` and its job `gate`. The spec named the checks by workflow; `gh pr checks` lists
jobs. `.legion/plan.json` `risks[1]` pre-flagged exactly this and told every role to cite
`legion gh -- run list --json name,conclusion,headSha,databaseId` (workflow names, run ids) beside
`gh pr checks` (job names). Implementer, tester, and reviewer all did, and no false mismatch was
raised. Keep doing that: a criterion that names a check should name it as the workflow *and*
say which job name `gh pr checks` will show for it.

Both workflows are `on: [push, pull_request]`, so every head gets two runs per workflow at the same
SHA; the `push`-event duplicates sat in GitHub's runner queue for up to five minutes while the
`pull_request` ones finished in twenty seconds. `gh pr checks` exits 8 while any run is pending;
cite all four run ids in the `CI:` line once they settle. The ledger-commit head drift that forces
this per phase, and the 60-second `legion gh` grant that makes a loop of `gh run watch` calls fail
after the first, are documented from PR #319 in
`docs/solutions/legion/ledger-commits-move-the-pr-head-and-blind-relative-gates.md` and
`docs/solutions/legion/operator-gated-phases-credential-grants-and-honest-controls.md` (both
merged in #319 and removed from `main` by the fixture cleanup #320; read them from history).
