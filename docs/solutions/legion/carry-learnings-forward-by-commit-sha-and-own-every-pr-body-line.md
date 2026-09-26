---
title: "Carry learnings forward by commit SHA when main deletes them, and own every PR-body line whose fact you change"
category: legion
tags:
  - legion-worker
  - legion-retro
  - planner
  - reviewer
  - docs-solutions
  - fixture-cleanup
  - pr-body
  - thermo-line
  - threads-line
  - operator-override
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-288"
  - "sjawhar/legion-smoke#346"
---

# Carry learnings forward by commit SHA when `main` deletes them, and own every PR-body line whose fact you change

Distilled from PR #346 (issue LEGSMOKE-288), the third run of the same one-file proof on
`sjawhar/legion-smoke` after PR #319 (LEGSMOKE-270) and PR #326 (LEGSMOKE-278). The product
change is identical in shape; what differed is that this run's planner read the two earlier
retros and wrote them into the plan. The result is measurable, and so is what still drifted.

## Learnings deleted from `main` are still one `jj file show` away

This fixture's operator deletes every merged `.legion/` file and `docs/solutions/` document
after each run ("proof fixture: remove the handoffs and learnings …", #320, #322, #327, #334). So
`main@origin` (`c9693927`) has no `docs/solutions/` at all, and a planner that only reads the
checkout learns nothing from the previous runs.

Every issue workspace is a `jj workspace` of one shared clone, and a merge commit stays in that
clone after `main` moves past its deletion. Locate the merge that carried the learnings by its PR
number in the description, then read the files at that commit:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" log --no-graph \
    -r 'ancestors(main@origin, 40) & description(glob:"*#326)*")' \
    -T 'commit_id.short() ++ " " ++ description.first_line() ++ "\n"'
# 3dba0d427577 smoke: Stage 4b workflow proof for LEGSMOKE-278 (#326)
jj -R "$LEGION_WORKSPACE" file list -r 3dba0d427577 docs/solutions
jj -R "$LEGION_WORKSPACE" file show -r 3dba0d427577 docs/solutions/legion/<name>.md
```

The #346 planner did exactly this (`.legion/plan.json` `summary`: "read from commit
3dba0d427577; removed from main by fixture cleanup #327") and folded two rules into the plan's
commands: criterion 1 is defined over the product fileset `'~(.legion | docs/solutions)'` with the
GitHub compare-API equivalent, and the risks list names the workflow-versus-job naming
(`ci`/`check`, `fail-on-demand`/`gate`) and the four-runs-per-head shape.

What that bought, against the #326 retro's own account of the same criterion ("each spent a round
of thought and a paragraph of prose on it"):

- The implementer, tester and reviewer ran the criterion-1 command as written and printed the
  same single line `A smoke/stage4b-legsmoke-288.txt` (`.legion/implement.json` `proof[0]`,
  `.legion/test.json` `proof[0]`, review body "Product diff at 170f1980"). No role reinterpreted
  the criterion; `deviations` is empty.
- Zero review threads (`reviewThreads.totalCount` 0), one review, `APPROVE` in the first round.
- The implementing phase ran from the operator's targeted message (Dispatch message
  `34ab6ac7-6ced-4492-9426-9165acf34081`, 20:40:53Z) through two pushes, PR #346 open, the proof
  recorded and `handoff_complete`, before the tester's own targeted message arrived at 20:46:17Z
  (`.legion/test.json` `summary`) — under six minutes, because the pre-flight checks the #319
  learning prescribes (sibling file's `od -c`, `.fail-me` absent, the negative-control baseline at
  `main@origin`) were done read-only while waiting for the gate.

Rule for planners on any repository that prunes `docs/solutions/` — or whose learnings simply
predate the branch point: cite the commit SHA and path you read, in the plan, so the next role can
open the same text. Rule for retro: cite the earlier documents the same way (this document does)
instead of restating them; a fixture cleanup will remove this file too, and the SHA is what
survives.

## The PR-body line drifts when a role changes its fact under an override

`skill://legion-worker`'s READY body has one line per gate fact, and each is owned by whoever
last changed the fact. On #326 the `Threads:` line still said "No review has been submitted yet"
ten minutes after the approval (recorded in
`docs/solutions/legion/operator-directed-approval-on-a-ledger-carrying-head.md` at `3dba0d427577`).
On #346 a different line drifted for the same reason:

- The implementer opened the PR with `**Thermo:** omitted — text-only PR … neither
  ce-simplify-code nor the thermonuclear pair runs`. Correct at open, and the plan's default.
- The operator's targeted message `1a30c873-e372-4219-b8f3-478def89771c` (20:52:00Z) directed the
  reviewer to run the pair anyway and `APPROVE` at the current, ledger-carrying head. The review
  body (legion-reviewer[bot], 20:56:58Z, commit `170f1980`) records both: "The thermonuclear pair
  ran once at 170f1980 at the operator's direction … `thermonuclear-deep-review` clean …
  `thermonuclear-code-quality` one Minor".
- The PR body's `Thermo:` line was not edited. A merger or human reading the body saw "the pair
  does not run" while the review two scrolls down said it ran and found something.

Retro refreshed the line (a `legion gh -- pr edit <n> --body-file` changes no commit and does not
void the approval). The reviewer should have, in the same action as the review submission — the
`Threads:` line's rule from #326 generalises: **whoever changes a gate fact edits its line before
the action that publishes the fact is over.** For the reviewer that is one `pr edit` beside the
one `POST …/reviews`; for the tester the `CI:` and `E2E (tester)` lines beside its push; for the
implementer the `CI:` and `E2E (implementer)` lines beside each of its pushes (both were done on
#346). An override does not suspend the rule; it makes the line more important, because the body
is now the only place the default and the exception sit side by side.

## Where the override is recorded, and where it is not

The `.legion/` deletion (plan task 6) never ran on #346: the operator directed `APPROVE` at
`170f1980` with `plan.json`, `implement.json` and `test.json` still on the branch, as on #326. The
review body states the override and its consequence ("a merge of this head lands the three ledger
files on main together with the product file; the merger's READY packet must say so"). The
fresh-eyes pass on this retro noted that the review body is the only artifact that does: the plan
still describes the default flow, and no handoff was written after it.

That is the designed shape, not a gap to patch by hand: retro never writes `.legion/`, and a
deletion commit above the approved head would void the approval (`skill://legion-retro`,
merge-gate ordering). What retro owes is to say it again where the next two readers look — this
document, the retro's Dispatch message, and the report to the architect — so the merger's READY
names the ledger files that will land, and the human merging does not learn it from the Files tab.
On this fixture the operator's cleanup removes them afterwards; in a real repository the same
approval would ship session ids, run ids and command lines to `main`, and the reviewer's `COMMENT
while .legion/ is present` default is the guard.
