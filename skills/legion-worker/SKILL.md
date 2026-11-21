---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, or merger — booted from the daemon's LEGION_* environment.
---

# Legion Phase Worker

Your role prompt gives the workspace, the push, the handoff, `handoff_complete`, the waits and the pod release; this skill adds the rest. Where it disagrees with the code or a command's `--help`, follow those and tell your architect which line is wrong.

## Every phase

1. Read the issue, then its `open-items.md` where one exists (`dispatch doc-read --issue <issue> --artifact open-items-md`): the architect records decisions there that it does not message. Then read each earlier phase's handoff (`dispatch handoff-read --issue <issue> --phase <plan|implement|test|review>`). A record beats your memory.
2. Before the turn's first `jj new`, list `jj -R "$LEGION_WORKSPACE" diff -r @ --name-only` and `jj -R "$LEGION_WORKSPACE" diff --from "legion/<issue>@origin" --to @ --name-only`. A path you did not write in either list: read `skill://legion-worker/references/foreign-path.md`.
3. Commit only your phase's paths: `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`.
4. Before you write the handoff, run `jj -R "$LEGION_WORKSPACE" status` and remove or restore every file you copied in that is not your phase's work: the working copy is the next role's first commit.
5. Every role writes the one workspace, so a file can change under your check. Run checks from a copy of the head (`git archive`, or a clone under `$TMPDIR`) where you can, and run `git diff --quiet <head> -- <files the check reads>` before you record a result. A script that fails in the workspace is a finding only when it fails again with `sha256sum <script>` unchanged just before and after. A run whose output you did not capture can be neither confirmed nor cleared.
6. A codebase-wide rename or deletion leaves historical records (transcripts, test snapshots, changelog entries) unchanged.

## Stop rules

- A refused completion is information, not a retry loop. Quote the answer verbatim to the architect.
  - `HANDOFF_STALE_GENERATION` or `HANDOFF_NO_RUN`: push nothing more and end the turn without completing. When the task then arrives with its work and handoff done, call `handoff_complete` again in its turn; never redo the work.
  - `HANDOFF_STALE_ENTRY`: end the turn without completing; the daemon sends the new entry's task by itself. Do what it asks at the head it names, with a handoff of its own.
  - `HANDOFF_HEAD_MERGED_FORWARD`: a merge forward the daemon asked for has landed above the head your completion names, or your completion names no head; nothing was recorded. Read the merge (`skill://legion-worker/references/conflicts-and-rewrites.md`), then complete again at the new head with a handoff that names it.
  - `HANDOFF_NOT_CURRENT_PHASE`: report it; never complete again. `HANDOFF_ALREADY_RECORDED`: the first answer stands.
- These waits stand rather than proceed, each ending the turn with a `WAITING` line naming what it waits on:
  - a planner on a decision the plan itself needs, because a plan completed around an open decision carries that gap into every later phase;
  - a reviewer on a half of the round that has no review, on a required check with no run at the head, or on the architect's rewrite of a check whose `run` is a whole test suite (`skill://legion-worker/references/merge-gate.md`);
  - retro on the architect's answer to a defect it reported;
  - a pushing role whose second push still did not carry the pull request onto its commit.
- Send any other decision someone else owns (product, scope, design, lifecycle, another phase's) to the owning architect with what you observed, what you tried and the decision needed, then go on.
- No surface reaches the changed path: report it to the architect, never complete (`skill://legion-worker/references/pr-body.md`).
- A released pod is not a stop. Your session, phase and task are kept, and the next pod restores your unpushed tracked work; nothing else of the disk survives.
- When another role's topic answers 404, read its handoff with `dispatch handoff-read`.

## Commits and pushes

- Before a push, run the trailer check in `skill://legion-worker/references/default-branch-check.md`. Then, with the `$default` it set, read your branch's full commit messages: `jj -R "$LEGION_WORKSPACE" log -r "${default}@origin..@" -T 'description ++ "\n"'`. Earlier phases' commits in it are expected; `description.first_line()` shows no trailer.
- Write no trailer yourself. The pane's jj adds `Omp-Session:` and `Legion-Role:`, and `Dispatch: <KEY>` in a repository the deployment names; the git wrapper adds the first two to a `git commit`. A merge, revert, cherry-pick or rebase that git makes gets none. Cure a commit that lacks a line by describing it again in the pane's shell; where that adds no issue line and `LEGION_ISSUE_TRAILER` is set, add the trailer line `Dispatch: $LEGION_ISSUE_TRAILER` yourself.
- A repository you clone under `$LEGION_WORKSPACE` is stamped with this issue's key: clone one you will push elsewhere outside it.
- A planner whose issue moved back from implementing starts on the remote tip: `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`.

## Pull request and merge gate

- The implementer opens the pull request. The branch `legion/<KEY>` ties it to the issue, so no pull request title or body and no commit subject names a Dispatch key or link. Later phases push to that branch and edit that pull request.
- No deferrals. A finding that changes behaviour, hides an error, or breaks a gate is fixed in this pull request. So is a wording nit or any clear fix in a file it touches: a later round costs a CI pass and a review. `Fast-follow:` reads `none` or names cleanup in files the pull request does not touch.
- The merge gate, in order: the tester's evidence green → the reviewer's approval of the head → the architect's acceptance → retro, which commits nothing → the merger's merge of the head it read, which closes the issue → the production check after the close, the tester's unless the project's `production_check` is `off`. Only the merger runs `gh pr merge`; nobody turns auto-merge on.
- End every pull-request comment you post with `<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->`.
- A Dispatch comment or message never carries the `<!-- legion: … -->` footer. Dispatch records the session that wrote it, and only a pull-request comment or review needs the footer.
- Never run `gh config set` or `gh alias set`, and leave `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` unset: a set variable outranks the gh directory the daemon refreshes.
- Before you report a resource unreachable, or read a GitHub `404` as no access, try the call through `skill://agent-secrets` and read the profiles in `AWS_CONFIG_FILE`. Request a secret only for your issue's work.

## Deployment instructions

They override this skill, except: no deferrals; the base merged forward only as `skill://legion-worker/references/conflicts-and-rewrites.md#reintegrating-the-base` allows; the implementer's own result for every `before_merge` check with no `ci_check` (`skill://legion-worker/references/pr-body.md#what-a-result-is`); and the production check after the close (`skill://legion-worker/references/merge-gate.md#the-production-check-after-the-close`).

## References

- PR body, a check's result, the simplify pass: `skill://legion-worker/references/pr-body.md`.
- A check carrying `ci_check`: `skill://legion-worker/references/ci-checks.md`.
- Review, approval, merge, production check: `skill://legion-worker/references/merge-gate.md`.
- Review threads, a bot's finding: `skill://legion-worker/references/review-threads.md`.
- A conflict, the daemon's merge-forward message, a need for the base, a retarget, a wrong pushed commit: `skill://legion-worker/references/conflicts-and-rewrites.md`.
- A plan you write or review: `skill://legion-worker/references/plan-tests.md`.
- Builds, tests, tools or a database in the pod: `skill://legion-worker/references/tools.md`; before you unpack or install what the image lacks: `skill://legion-worker/references/withheld-capability.md`.
- A problem outside your issue, or a failed production check: `skill://legion-worker/references/findings.md`.
- Text that reaches a person outside the company: `skill://outside-message`.
