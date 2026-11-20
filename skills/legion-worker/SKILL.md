---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, or merger — booted from the daemon's LEGION_* environment.
---

# Legion Phase Worker

Your role prompt gives the workspace, the push, the handoff, `handoff_complete`, the waits and the pod release; this skill adds the rest. The architect your `Legion addressing` line names owns your issue. When an architect above it tells you that it takes your issue's work while your issue's own architect does not run, send to that architect instead.

## Every phase

1. Read the issue, then its `open-items.md` where one exists (`dispatch doc-read --issue <issue> --artifact open-items-md`): the architect records decisions there that it does not message. Then read each earlier phase's handoff (`dispatch handoff-read --issue <issue> --phase <plan|implement|test|review>`). Before you read another phase's results with `dispatch results --issue <issue>`, read `skill://legion-worker/references/pr-body.md#what-a-result-is`, which defines a check and a result. Where a record disagrees with your memory or a transcript, the record wins: it is the copy that survived. At each later assignment read them again, your own included, since other phases may have recorded since.
2. Before the turn's first `jj new` or commit, run the two path lists of `skill://legion-worker/references/foreign-path.md` and do what it says for a path you did not write.
3. Commit only your phase's paths: `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`.
4. Before you write the handoff, run `jj -R "$LEGION_WORKSPACE" status` and remove or restore every file you copied in that is not your phase's work: the working copy is the next role's first commit.
5. Before you run a build or a test suite in your pod, read `skill://legion-worker/references/tools.md`. Every role writes the one workspace, so a file can change under your check. Run checks from a copy of the head (`git archive`, or a clone under `$TMPDIR`), which no sibling writes; run a check in the workspace only when it reads what a copy lacks, such as jj's state or the uncommitted working copy. Run `git diff --quiet <head> -- <files the check reads>` before you record a result. Before you report a script that failed in the workspace, or a run whose output you did not capture, read `skill://legion-worker/references/shared-workspace.md`.
6. Record the handoff your role prompt names, push any work, then complete. A handoff is never a commit, and no handoff carries the evidence: what you ran is a result you record against one of the issue's checks with `dispatch result`, and `skill://legion-worker/references/pr-body.md#what-a-result-is` defines a check and a result. Before you record the handoff or complete, read `skill://legion-worker/references/handoff-and-completion.md`.

## Stop rules

- A refused completion is information, not a retry loop. Quote the answer verbatim to the architect, then do what `skill://legion-worker/references/handoff-and-completion.md` says for its code.
- The workspace is read-only for you from the moment your phase ends, however it ended: read `skill://legion-worker/references/handoff-and-completion.md#when-your-phase-ends` for what that stops and its one exception.
- Never start another Legion role: the daemon starts every phase worker.
- These waits stand rather than proceed, each ending the turn with a `WAITING` line naming what it waits on:
  - a planner on a decision the plan itself needs, because a plan completed around an open decision carries that gap into every later phase;
  - a reviewer on a half of the round that has no review, on a required check with no run at the head, or on the architect's rewrite of a check whose `run` is a whole test suite (`skill://legion-worker/references/merge-gate.md`);
  - retro on the architect's answer to a defect it reported;
  - a pushing role whose second push still did not carry the pull request onto its commit.
- Send any other decision someone else owns (product, scope, design, lifecycle, another phase's) to the owning architect with the issue, phase, verified observation, what you tried and the decision needed, then go on. A human-only to-do is a `dispatch ask`.
- No surface reaches the changed path: report it to the architect, never complete (`skill://legion-worker/references/pr-body.md#when-no-surface-reaches-the-changed-path`).
- A released pod is not a stop. Your session, phase and task are kept, and the next pod restores your unpushed tracked work; nothing else of the disk survives.
- When another role's topic answers 404, read its handoff with `dispatch handoff-read`.

## References

Read each by its `skill://` link: a read by path stops at 300 lines.

| When | Read |
| --- | --- |
| You write or edit the PR body, record a result against a check, read another phase's results, or run the simplify pass | `skill://legion-worker/references/pr-body.md` |
| A check carrying `ci_check` | `skill://legion-worker/references/ci-checks.md` |
| You review or approve, merge the pull request, or take a look of a closed issue's production check | `skill://legion-worker/references/merge-gate.md` |
| You reply on, list or resolve review threads, or judge a bot's finding | `skill://legion-worker/references/review-threads.md` |
| A conflict, the daemon's merge-forward message, a need for the base, a retarget, a parent's last round, a wrong pushed commit | `skill://legion-worker/references/conflicts-and-rewrites.md` |
| A plan you write or review | `skill://legion-worker/references/plan-tests.md` |
| The issue renames a repository, package, or URL across the codebase | `skill://legion-worker/references/systematic-rename.md` |
| The issue deletes code, a command, or documentation | `skill://legion-worker/references/cleanup-deletion.md` |
| Before you run a check's command, a build, a test suite or any long job in your pod; before you read a shared database, a session store or another role's transcript among them; you need a tool the image may not carry, before you install anything; before you measure Oh My Pi's binary; or a test fails in your pod or needs a database | `skill://legion-worker/references/tools.md` |
| Before you unpack or install what the image lacks | `skill://legion-worker/references/withheld-capability.md` |
| Before you report a resource unreachable, or read a GitHub `404` as no access | `skill://legion-worker/references/unreachable-resource.md` |
| A problem outside your issue, or a failed production check | `skill://legion-worker/references/findings.md` |
| Your text would reach a person outside the company: a comment, issue or pull request on a repository the company does not own, an email, a message to another project's maintainers | `skill://outside-message` |
| A deployment instruction conflicts with this skill | `skill://legion-worker/references/deployment-instructions.md` |
| Before a push, after the trailer check in `skill://legion-worker/references/default-branch-check.md`; before you cure a commit that lacks a trailer line or clone a repository; as a planner whose issue moved back from implementing | `skill://legion-worker/references/commits.md` |

## Pull request and merge gate

- No deferrals. A finding that changes behaviour, hides an error, or breaks a gate is fixed in this pull request. So is a wording nit, or any other fix in a file it touches that needs no one else's decision: a later round costs a CI pass and a review. `Fast-follow:` reads `none` or names cleanup in files the pull request does not touch.
- Before you open or comment on a pull request, write a GitHub issue, or change `gh`'s config, its aliases or its token variables: read `skill://legion-worker/references/pull-request.md`.
- The merger merges the head it read as `skill://legion-worker/references/merge-gate.md` says; read it before you merge.
