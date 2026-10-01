---
title: "Proof ordering and the approval head under the Go daemon: full-SHA run lookups, proving before the PR opens, and approving with .legion/ still on the branch"
category: legion-workflow
tags:
  - legion
  - go-daemon
  - implementer
  - reviewer
  - merger
  - legion-gh
  - run-list
  - skip-checks
  - pr-body
  - handoff
date: 2026-10-01
status: active
module: smoke
related_issues:
  - "LEGSMOKE-378"
  - "sjawhar/legion-smoke#479"
---

# Proof ordering and the approval head under the Go daemon: full-SHA run lookups, proving before the PR opens, and approving with .legion/ still on the branch

Learned on LEGSMOKE-378 (Stage 4b proof tree 1), pull request sjawhar/legion-smoke#479: one new
file `smoke/stage4b-legsmoke-378.md`, one line, 39 bytes, driven by the Go daemon. Everything
below was observed in that run; anything inferred is marked. The LEGSMOKE-372 retro
(`implementer-mechanics-on-a-legion-phase-branch.md`, on that run's merge commit) already covers
the 60-second `legion gh` grant, plan-predicted byte offsets, the two-push implement handoff,
`jj split`'s phantom "remaining" change, COMMENT-then-APPROVE on one head, and App-not-phase
authorship. This document adds only what that run did not hit.

## `gh run list --commit` with an abbreviated SHA returns zero rows, which is also what a skip-checks head returns

`legion gh -- run list --repo sjawhar/legion-smoke --commit 47725f0891cb --json …` printed
nothing. The same command with the full 40-character SHA
`47725f0891cb9addad28f6a0172483682fcc4e5a` printed four runs (36807890623, 36807890581,
36807894774, 36807894762, all `success`). Repeated at the approved head: `--commit bf90c20df7a4`
→ `0`; the full SHA → four runs. GitHub's workflow-runs filter does not expand abbreviations and
does not error on one.

The hazard is that an empty result is also the legitimate signature of a head whose push carried
GitHub's `skip-checks: true` trailer (on this branch, `dd71783e` and `c25ae1da`, both with zero
runs). A worker who passes a 12-character SHA for a code head will read "no runs" as "skip-checks
head" and go looking for a carried verdict that does not exist. Rules: always pass the full SHA
from `legion gh -- pr view <n> --json headRefOid` or `jj log -T 'commit_id'` (never
`commit_id.short()`); and when a run list is empty, check the commit message for the trailer
before concluding anything (`jj log -r <sha> -T 'description.contains("skip-checks: true")'`).
Plans that write `run list --commit <head-sha>` should say "full SHA" in the placeholder.

## Criterion proofs that need only the commit run between `legion push` and `pr create`

`skill://legion-worker/references/pr-body.md` requires the implementer to fill the
`E2E (implementer)` line when the pull request opens. The LEGSMOKE-372 retro's four-step order
proves after the PR exists, so the line at open was a placeholder. On this run the file-level
proof ran first:

1. `jj split -m "smoke: …" smoke/stage4b-legsmoke-378.md`; `legion push`
   (`c25ae1da → b94708754140`).
2. At `b94708754140`, before any PR existed: `legion gh -- api
   repos/<r>/contents/smoke/stage4b-legsmoke-378.md?ref=<sha> -q .content | base64 -d | od -c`
   (39 bytes, ends at offset 0000047), `legion gh -- api repos/<r>/commits/<sha> -q '.files[] |
   …'` (one row `added 1 0`), and both negative controls (`contents/…?ref=main -i` → 404;
   `contents/.fail-me?ref=<sha> -i` → 404). The contents and commits endpoints read a pushed
   commit; they do not need a pull request.
3. `legion gh -- pr create … --body-file` with the `E2E (implementer)` line already carrying step
   2's commands, observations, head, and negative controls, and the `CI` line marked pending.
4. `legion gh -- pr checks <n> --watch`; then `statusCheckRollup`, `run list --commit <full sha>`,
   and `pr diff --name-only`, the three facts that do need the PR; edit the body.

Only `pr diff --name-only` and the CI evidence depend on the PR. Splitting the proof this way is
what makes "fill the E2E line when the PR opens" satisfiable literally rather than as a promise.

## Under the Go daemon the reviewer approves a head that still carries `.legion/`; a plan written against the skill default is not executable as written

The deployment instructions for this daemon say that until Stage 7 no role pushes the `.legion/`
deletion: the reviewer approves a head carrying the handoffs, retro commits above it, the merger
publishes READY for it, and the operator removes `.legion/` from `main` afterwards. That overrides
`skill://legion-worker`'s merge gate and `skill://legion-retro` step 2.

The plan for this issue was written against the skill default. Its acceptance criterion 1
expected "exactly one path at the `.legion/`-deleted head", task 6 said "approval only on the
`.legion/`-deleted head", and task 7 had the implementer push the deletion. None of that happened.
What did: the reviewer approved `bf90c20d` (its own `review: record handoff` commit), where
`legion gh -- pr diff 479 --name-only` prints five paths, `.legion/{implement,plan,review,test}.json`
and the smoke file; the daemon record shows `reviewDecision: approved` at that head; and the
retro's `docs/solutions/` commit sits above it. The implement, test, and review handoffs, and a
fresh-eyes read of them during retro, all still said the one-path check "is evaluated at the
`.legion/`-deleted head when it exists": a skill-default assumption propagated through four
handoffs because the override lived only in the system prompt's deployment instructions, which
the plan never quoted.

For a planner under this daemon: write criterion 1 as "the one repository path plus
`.legion/*.json` handoffs" at the approved head and drop the deletion steps, citing the deployment
instruction in `repoContext` so later phases see it. For a merger: the tip to verify is the
approved head plus commits that change only `docs/solutions/`
(`jj diff --from <approved> --to <tip> --summary`), and `.legion/*.json` in the PR diff at READY
is expected, not a defect. For a reviewer: the "exactly one path" reading is `pr diff
--name-only` minus `.legion/`.

## `review.json.reviewedHead` names the head the reviewer inspected; the GitHub review object sits one commit later

`.legion/review.json` has `reviewedHead: dd71783e…` (the tester's handoff commit, the branch tip
when the reviewer read it) and `verdictNote` saying the review is submitted "on the head this
handoff push makes, after the push". Both are true and they name different commits: the handoff's
own SHA is unknowable while it is being written, so the file names the parent; the submission
then lands on the handoff commit. `legion gh -- pr view 479 --json reviews` shows both review
objects, `COMMENTED` and `APPROVED`, at `bf90c20d`, and `bf90c20d` differs from `dd71783e` by
`.legion/review.json` alone.

A merger or architect comparing `review.json.reviewedHead` to `reviews[].commit.oid` will see a
mismatch every time and should expect exactly this one-commit, handoff-only gap. The check that
matters is `jj diff --from <reviewedHead> --to <review commit> --summary` printing only
`A .legion/review.json`; anything else means the reviewer approved a head it did not inspect.
Because a `review.json` whose `verdict` is not `changes_requested` is not on `legion push`'s
skip-CI list, the approval head also has its own four green runs (36808820912, 36808820908,
36808823590, 36808823571), not a carried verdict.

## Delegating a one-file change: state the plan's verify commands as the worker's acceptance, then re-run them yourself

The implementer role has a `deep-worker` subagent write the code, even for a 39-byte file (55 s on
this run). What made the hand-off cheap to verify: the task gave the worker the plan's exact
verify commands with absolute paths as its acceptance (`jj diff --summary`, `wc -c`, `od -c`,
`cmp -l <new> <precedent>`, `jj status`), forbade every jj mutation and any commit, and asked for
verbatim output. The orchestrator then re-ran the same five commands and used its own output, not
the worker's report, for `.legion/implement.json` `localChecks` and for the split. Writing the
acceptance as runnable commands is what turns "never trust a worker's report" from a slogan into
a 10-second check; a prose acceptance ("file matches the precedent") leaves the orchestrator to
invent the check afterwards.

Observed and worth one sentence: a `pr checks --watch` followed by evidence commands in the same
bash call held on this run because the whole call finished in 24 s (`ci` sleeps 20 s); the
LEGSMOKE-372 retro's rule, watch in its own call, still stands wherever the watch can approach a
minute.
