---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, acceptance, or merger — booted from the daemon's LEGION_* environment.
---

# Legion Phase Worker

Your role prompt gives the workspace, the push, the handoff, the waits and the pod release.
This skill adds the rest.
Where it disagrees with the code or a command's `--help`, follow those and tell your architect which line is wrong.

## Every phase

1. Read the issue, then its `open-items.md` where one exists (`dispatch doc-read --issue <issue> --artifact open-items-md`):
   the architect records decisions there that it does not message.
   Then read each handoff a `(<role>) reported:` line of your task names, as it says.
   Its version is the one your task names: `dispatch handoff-read --issue <issue> --phase <word> [--version <v>]`.
   A record beats your memory.

2. Before the turn's first `jj new`, list `jj -R "$LEGION_WORKSPACE" diff -r @ --name-only` and `jj -R "$LEGION_WORKSPACE" diff --from "legion/<issue>@origin" --to @ --name-only`.
   A path you did not write in either list: read `skill://legion-worker/references/foreign-path.md`.

3. Commit only your phase's paths: `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`.

4. Before you write the handoff, run `jj -R "$LEGION_WORKSPACE" status` and remove or restore every file you copied in that is not your phase's work.
   The working copy is the next commit.

5. Every role writes the one workspace, so a file can change under your check.
   Run checks from a copy of the head (`git archive`, or a clone under `$TMPDIR`) unless the check needs the workspace itself, and run `git diff --quiet <head> -- <files the check reads>` before you record a result.
   Commit nothing in the copy.
   A script that fails in the workspace is a finding only when it fails again with `sha256sum <script>` unchanged before and after.
   A run whose output you did not capture proves nothing either way.

6. A codebase-wide rename or deletion leaves historical records (transcripts, test snapshots, changelog entries) unchanged.

## Stop rules

- A refused completion is information, not a retry loop: complete again once its cause is fixed, or as its code says.
  Quote the answer verbatim to the architect.

  - `HANDOFF_STALE_GENERATION` or `HANDOFF_NO_RUN`: push nothing more and end the turn without completing.
    When the task arrives with its work and handoff done, complete again in its turn; never redo the work.

  - `HANDOFF_STALE_ENTRY`: end the turn without completing; the daemon sends the new entry's task itself.
    Do what it asks at the head it names, with a handoff of its own.

  - `HANDOFF_HEAD_MERGED_FORWARD`: a merge forward the daemon asked for landed above your completion's head, or it names none; nothing was recorded.
    Read the merge (`skill://legion-worker/references/conflicts-and-rewrites.md`), then complete again at the new head, with a handoff naming it.

  - `HANDOFF_NOT_CURRENT_PHASE`: report it; do not retry that completion.
    `TASK_STOPPED`: your handoff counts as used; never send it again.
    `HANDOFF_ALREADY_RECORDED`: the first answer stands.

- These waits end the turn with a `WAITING` line naming what they wait on:

  - a planner on a decision the plan itself needs, because a plan completed around an open decision carries that gap into every later phase;

  - a reviewer on a half of the round that has no review, on a required check with no run at the head, or on the architect's rewrite of a check whose `run` is a whole test suite (`skill://legion-worker/references/merge-gate.md`);

  - retro on the architect's answer to a defect it reported;

  - a pushing role whose second push still left the pull request off its commit.

- Send any other decision someone else owns (product, scope, design, lifecycle, another phase's) to the owning architect with what you saw, tried and need decided, then go on.

- No surface reaches the changed path: report it to the architect, never complete (`skill://legion-worker/references/results.md`).

- A released pod is not a stop.

- When another role's topic answers 404, read that step's report as item 1 says.

## Commits and pushes

- Before every push, and before you clone a repository under `$LEGION_WORKSPACE`, follow `skill://legion-worker/references/default-branch-check.md`:
  it reads your branch's commits and their messages, and says which trailers the pane writes for you and the one cure you may add.

- A planner whose issue moved back from implementing starts on the remote tip: `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`.

## Pull request and merge gate

- An implementation step's agent opens the pull request with the line `Dispatch: <KEY>` in its body.
  Later steps push to that branch and edit that pull request. An issue none of whose steps opened one has none.

- No deferrals where the issue has a pull request.
  A finding that changes behaviour, hides an error, or breaks a gate is fixed in this pull request.
  So is a wording nit or any clear fix in a file it touches: a later round costs a CI pass and a review.
  `Fast-follow:` reads `none` or names cleanup in files the pull request does not touch.

- The merge gate follows the task's `Steps:` line: each named step runs in order, then retro and, with a pull request, the merger.
  The default task has tester evidence, reviewer approval and architect acceptance.
  With no tester step, an implementation step's evidence serves.
  With no reviewer step, no Legion approval is awaited. The last step's pass starts retro.
  Only the merger runs `gh pr merge`; nobody turns auto-merge on.

- End every pull-request comment you post with `<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->`.

- A Dispatch comment or message never carries that footer: Dispatch records the session that wrote it.

- Never run `gh config set` or `gh alias set`, and leave `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` unset:
  a set variable outranks the gh directory the daemon refreshes.

- Before you report a resource unreachable, or read a GitHub `404` as no access, try the call through `skill://agent-secrets` and read the profiles in `AWS_CONFIG_FILE`.
  Request a secret only for your issue's work.

## Deployment instructions

They override this skill, except:

- no deferrals

- the base merged forward only as `skill://legion-worker/references/conflicts-and-rewrites.md#reintegrating-the-base` allows

- an implementation step's own result for every `before_merge` check with no `ci_check` (`skill://legion-worker/references/results.md#what-a-result-is`)

- the production check after the close (`skill://legion-worker/references/merge-gate.md#the-production-check-after-the-close`)

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
