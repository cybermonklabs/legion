---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, merger, or a deployment's role — booted from the daemon's LEGION_* environment.
---

# Legion Phase Worker

Your role prompt gives the workspace, push, handoff, `handoff_complete`, waits and pod release; this skill adds the rest.

## Every phase

1. Read the issue, then your task, then the report each `Step <j> (<step>) reported: <word> version <v>.` line of it names (`dispatch handoff-read --issue <issue> --phase <word> --version <v>`), or, for a deployment's role, which writes none, its runs (below).
   Where your task has no such line, `dispatch steps --issue <issue>` lists the step runs; without `--version` the command reads a phase's newest report.
   Your architect's decisions for you come in your task, and in a reply in the thread of the plan task they concern, which its message links.
   A record beats your memory.

2. Commit only your phase's paths: `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`.

3. Before you write the handoff or report your step, run `jj -R "$LEGION_WORKSPACE" status` and remove or restore every file you copied in that is not your phase's work:
   another role that commits next sets aside whatever the working copy holds.

4. Before you run a check, read `skill://legion-worker/references/results.md`, "Where a check runs".

5. A codebase-wide rename or deletion leaves history (transcripts, test snapshots, changelog entries) unchanged.

## Stop rules

- A refused completion is information, not a retry loop.
  Quote the answer verbatim to the architect, and do what its code asks in `skill://legion-worker/references/stop-rules.md`.

- Wait in these cases, and end the turn with a `WAITING` line that names what you wait on:

  - a planner on a decision the plan itself needs, because a plan completed around an open decision carries that gap into every later phase;

  - a reviewer on a half of the round that has no review, on a required check with no run at the head, or on the architect's rewrite of a check whose `run` is a whole test suite;

  - retro on the architect's answer to a defect it reported;

  - a pushing role whose second push still left the pull request off its commit.

- Send any other decision someone else owns (product, scope, design, lifecycle, another phase's) to the owning architect with what you observed, what you tried and the decision needed, then go on.

- Another agent that needs an action (a stop, retry, suspension, resume, compaction or fresh session) is the controller's: ask it with a `dispatch comment` on that agent's issue mentioning its role, never with `envoy_publish` (`skill://legion-worker/references/findings.md`).

- **Ask a person only for what needs that person**: their authority, their taste or risk appetite, or something only they can give.

- **Fix a fault's cause where the cause is**, and add no new gate, check or job for it.

- No surface reaches the changed path: report it to the architect, never complete (`skill://legion-worker/references/results.md`).

- A released pod is not a stop.
  Your session, phase and task are kept, and the next pod restores your unpushed tracked work; nothing else of the disk survives but a volume the deployment keeps.

- Another role's topic answers 404: read that step's report or runs (item 1).

## Commits and pushes

- Write no trailer yourself.
  The pane's jj adds `Omp-Session:` and `Legion-Role:`, and `Dispatch: <KEY>` in a repository the deployment names;
  the git wrapper adds the first two to a `git commit`.
  A merge, revert, cherry-pick or rebase that git makes gets none.
  Cure an unpushed commit that lacks a line by describing it again in the pane's shell;
  where that adds no issue line and `LEGION_ISSUE_TRAILER` is set, add the line `Dispatch: $LEGION_ISSUE_TRAILER` yourself.

- A repository you clone under `$LEGION_WORKSPACE` is stamped with this issue's key: clone one you will push elsewhere outside it.

- A planner whose issue moved back from implementing starts on the remote tip: `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`.

## Pull request and merge gate

- The implementer opens the pull request with the line `Dispatch: <KEY>` in its body.
  A later phase pushes only to that branch and edits only that pull request.

- No deferrals.
  A finding that changes behaviour, hides an error, or breaks a gate is fixed in this pull request.
  So is any clear fix in a file it touches: a later round costs a CI pass and a review, if any.
  `Fast-follow:` reads `none` or names cleanup in files the pull request does not touch.

- The merge gate follows the task's `Steps:` line: each named step runs in order, then retro and the merger.
  The default task has tester evidence, reviewer approval and architect acceptance.
  With no tester step, the implementer's evidence serves;
  with no reviewer step, no Legion approval is awaited; without `acceptance:owner`, the last step starts retro, or holds a parent until the issues beneath it leave.
  Only the merger runs `gh pr merge`; nobody turns auto-merge on.

- Never run `gh config set` or `gh alias set`, and leave `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` unset:
  a set variable outranks the gh directory the daemon refreshes.

- Before you report a resource unreachable, or read a GitHub `404` as no access, try the call through `skill://agent-secrets` and read the profiles in `AWS_CONFIG_FILE`.
  Request a secret only for your issue's work.

- A comment on a pull request in a repository the implement App is installed on and your App is not takes the broker's implement App token, narrowed to that repository (`skill://legion-architect/references/operations.md#the-token-the-secrets-broker-lends`).
  It is the one write the token is lent to you for.
  A branch that your App cannot create is the implementer's work: send it to the architect as a cross-phase decision.
  Any other write your `gh` cannot make is a finding.
  A denial is final: ask no other session to make the write.

## Deployment instructions

They override this skill, except: no deferrals;
the base merged forward only as `skill://legion-worker/references/conflicts-and-rewrites.md#reintegrating-the-base` allows;
the implementer's own result for every `before_merge` check with no `ci_check` (`skill://legion-worker/references/results.md#what-a-result-is`);
and the production check after the close.

## References

- The PR body, its `Thermo` line, the simplify pass, a comment's footer: `skill://legion-worker/references/pr-body.md`.

- A check's result, the evidence each phase owes, a red or missing CI check, no surface for the changed path: `skill://legion-worker/references/results.md`.

- A check carrying `ci_check`: `skill://legion-worker/references/ci-checks.md`.

- Review, approval, merge, production check: `skill://legion-worker/references/merge-gate.md`.

- Review threads, a bot's finding: `skill://legion-worker/references/review-threads.md`.

- A conflict, the daemon's merge-forward message, a need for the base, a retarget, a wrong pushed commit:
  `skill://legion-worker/references/conflicts-and-rewrites.md`.

- A plan you write or review: `skill://legion-worker/references/plan-tests.md`.

- Builds, tests, tools or a database in the pod:
  `skill://legion-worker/references/tools.md`; before you unpack or install what the image lacks: `skill://legion-worker/references/withheld-capability.md`.

- A problem outside your issue, a to-do only a person may do, another agent that needs an action, or a failed production check: `skill://legion-worker/references/findings.md`.

- Text that reaches a person outside the company: `skill://outside-message`.
