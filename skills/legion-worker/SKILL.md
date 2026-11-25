---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, or merger — booted from the daemon's LEGION_* environment.
---

# Legion Phase Worker

Your role prompt gives the workspace, the push, the handoff, `handoff_complete`, the waits and the pod release; this skill adds the rest.

## Every phase

1. Read the issue, then your task, then each earlier phase's handoff (`dispatch handoff-read --issue <issue> --phase <plan|implement|test|review>`).
   Your architect's decisions for you come in your task, and in a reply in the thread of the plan task they concern, which its message links.
   A record beats your memory.

2. Commit only your phase's paths: `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`.

3. Before you write the handoff, run `jj -R "$LEGION_WORKSPACE" status` and remove or restore every file you copied in that is not your phase's work:
   another role that commits next sets aside whatever the working copy holds.

4. Every role writes the one workspace, so a file can change under your check.
   Run checks from a copy of the head (`git archive`, or a clone under `$TMPDIR`) unless the check needs the workspace itself, and run `git diff --quiet <head> -- <files the check reads>` before you record a result.
   A script that fails in the workspace is a finding only when it fails again with `sha256sum <script>` unchanged just before and after.
   A run whose output you did not capture can be neither confirmed nor cleared.

5. A codebase-wide rename or deletion leaves historical records (transcripts, test snapshots, changelog entries) unchanged.

## Stop rules

- A refused completion is information, not a retry loop.
  Quote the answer verbatim to the architect.

  - `HANDOFF_STALE_GENERATION` or `HANDOFF_NO_RUN`: push nothing more and end the turn without completing.
    When the task then arrives with its work and handoff done, call `handoff_complete` again in its turn; never redo the work.

  - `HANDOFF_STALE_ENTRY`: end the turn without completing; the daemon sends the new entry's task by itself.
    Do what it asks at the head it names, with a handoff of its own.

  - `HANDOFF_HEAD_MERGED_FORWARD`: `skill://legion-worker/references/conflicts-and-rewrites.md#the-daemons-ask`.

  - `HANDOFF_NOT_CURRENT_PHASE`: report it; do not retry that completion.
    `HANDOFF_ALREADY_RECORDED`: the first answer stands.

- These waits stand rather than proceed, each ending the turn with a `WAITING` line naming what it waits on:

  - a planner on a decision the plan itself needs, because a plan completed around an open decision carries that gap into every later phase;

  - a reviewer on a half of the round that has no review, on a required check with no run at the head, or on the architect's rewrite of a check whose `run` is a whole test suite (`skill://legion-worker/references/merge-gate.md`);

  - retro on the architect's answer to a defect it reported;

  - a pushing role whose second push still did not carry the pull request onto its commit.

- Send any other decision someone else owns (product, scope, design, lifecycle, another phase's) to the owning architect with what you observed, what you tried and the decision needed, then go on.

- **Ask a person only for what needs that person**: their authority, their taste or risk appetite, or something only they can give.

- **Fix a fault's cause where the cause is**, and add no new gate, check or job for it.

- No surface reaches the changed path: report it to the architect, never complete (`skill://legion-worker/references/results.md`).

- A released pod is not a stop.
  Your session, phase and task are kept, and the next pod restores your unpushed tracked work; nothing else of the disk survives but a volume the deployment keeps.

- When another role's topic answers 404, read its handoff with `dispatch handoff-read`.

## Commits and pushes

- Write no trailer yourself.
  The pane's jj adds `Omp-Session:` and `Legion-Role:`, and `Dispatch: <KEY>` in a repository the deployment names;
  the git wrapper adds the first two to a `git commit`.
  A merge, revert, cherry-pick or rebase that git makes gets none.
  Cure an unpushed commit that lacks a line by describing it again in the pane's shell;
  where that adds no issue line and `LEGION_ISSUE_TRAILER` is set, add the trailer line `Dispatch: $LEGION_ISSUE_TRAILER` yourself.

- A repository you clone under `$LEGION_WORKSPACE` is stamped with this issue's key: clone one you will push elsewhere outside it.

- A planner whose issue moved back from implementing starts on the remote tip: `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`.

## Pull request and merge gate

- The implementer opens the pull request with the line `Dispatch: <KEY>` in its body.
  Later phases push to that branch and edit that pull request.

- No deferrals.
  A finding that changes behaviour, hides an error, or breaks a gate is fixed in this pull request.
  So is any clear fix in a file it touches: a later round costs a CI pass and a review, if any.
  `Fast-follow:` reads `none` or names cleanup in files the pull request does not touch.

- The merge gate follows the task's `steps` field: each named step runs in order, then retro and the merger.
  The default task has tester evidence, reviewer approval and architect acceptance.
  With no tester step, the implementer's evidence serves;
  with no reviewer step, no Legion approval is awaited; without `acceptance:owner`, the last step starts retro.
  Only the merger runs `gh pr merge`; nobody turns auto-merge on.

- End every pull-request comment you post with `<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->`.

- A Dispatch comment or message never carries the `<!-- legion: … -->` footer.
  Dispatch records the session that wrote it, and only a pull-request comment or review needs the footer.

- Never run `gh config set` or `gh alias set`, and leave `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` unset:
  a set variable outranks the gh directory the daemon refreshes.

- Before you report a resource unreachable, or read a GitHub `404` as no access, try the call through `skill://agent-secrets` and read the profiles in `AWS_CONFIG_FILE`.
  Request a secret only for your issue's work.

## Deployment instructions

They override this skill, except: no deferrals;
the base merged forward only as `skill://legion-worker/references/conflicts-and-rewrites.md#reintegrating-the-base` allows;
the implementer's own result for every `before_merge` check with no `ci_check` (`skill://legion-worker/references/results.md#what-a-result-is`);
and the production check after the close (`skill://legion-worker/references/merge-gate.md#the-production-check-after-the-close`).

## References

- The PR body, its `Thermo` line, the simplify pass: `skill://legion-worker/references/pr-body.md`.

- A check's result, the evidence each phase owes, a red or missing CI check, no surface for the changed path: `skill://legion-worker/references/results.md`.

- A check carrying `ci_check`: `skill://legion-worker/references/ci-checks.md`.

- Review, approval, merge, production check: `skill://legion-worker/references/merge-gate.md`.

- Review threads, a bot's finding: `skill://legion-worker/references/review-threads.md`.

- A conflict, the daemon's merge-forward message, a need for the base, a retarget, a wrong pushed commit:
  `skill://legion-worker/references/conflicts-and-rewrites.md`.

- A plan you write or review: `skill://legion-worker/references/plan-tests.md`.

- Builds, tests, tools or a database in the pod:
  `skill://legion-worker/references/tools.md`; before you unpack or install what the image lacks: `skill://legion-worker/references/withheld-capability.md`.

- A problem outside your issue, or a failed production check: `skill://legion-worker/references/findings.md`.

- Text that reaches a person outside the company: `skill://outside-message`.
