# Acceptance, the merge and the close

Read the section for your step of `skill://legion-architect`, sections 4 to 7.
Where it says to ask a person, a child's own architect asks on that child's issue.

## Accepting

A step `acceptance:owner` enters `accepting` as yours, and `acceptance` with no key as a fresh agent's, which ends it by its verdict.
Read, against the goal, every check and every requirement:

1. where a tester step comes before your acceptance, the handoff of each such step: `dispatch handoff-read --issue <issue> --phase test` reads the newest, and `--version <n>` an earlier step's;

2. `dispatch results --issue <issue> --at <head>`: whether a pass stands for each `before_merge` check at the head the step before your acceptance passed;

3. where a reviewer step comes before your acceptance, the approval of the last such step, read by the head its Legion footer names, never its `commit_id` (`skill://legion-worker/references/merge-gate.md`), or its verdict with no pull request or at `reviewer:owner`, and each claim in the review of each such step, against the code at that head; no reviewer step approved the commits of an implementer step between that step and your acceptance (`implementer:a reviewer implementer:a acceptance:owner`);

4. where the issue has a pull request, `gh pr view <n> --json mergeable,mergeStateStatus`: a `CONFLICTING` head whose checks passed is accepted as any other (*A conflicting head*, below).

- `sign_off`, or `complete_step` with `pass`, works only in `accepting`, where the Dispatch status reads `needs_review`.
  Your `handoff_complete` there is refused (`ACCEPTANCE_IS_SIGN_OFF`).

- A `catch-up` whose `reason` names an issue waiting for your acceptance is its wake: its `phase-finished` may not have reached you.

- `SIGNOFF_CHILD_IN_WORKFLOW` names each issue beneath still in the workflow: run each to done, and sign off again.

- A finding ask holds the acceptance: one a worker sent you, or one `dispatch read` lists under `Open asks:` whose question opens `Finding from ` or `Clear defect from `.
  Follow it (`dispatch follow --ask <id> --action follow`) and publish its reference to the controller topic.
  Accept only once it is resolved, or carries the controller's reply (`Filed dispatch://`, `Handed dispatch://` or `Not filed:`):
  a closed issue hides its open questions from the controller.

- No `after_merge` result is asked here: the production check records it after a merged issue's close.

## The last round

A parent has a last round only where its task names `acceptance:owner` and it has a pull request.
Sign it off only on a head at which whose merge its checks pass.
Each check needs a pass at that head; one in another repository needs a pass standing since the close of each child it needs, at a commit on the branch that issue's pull request merged into or on its default branch;
a `default_branch` check needs the default branch.
A criterion proven on a deployed system is proven after that child's deploy, as the repository's deploy skill says.
A child's result proves no check of the parent: a child carrying part of a parent's requirement proves it with a check of its own whose `serves` names it.
Nothing builds on the parent's close, which comes last.
Until those children close, leave the issue in `accepting`.
Then start the last round: `request_backward_move` to `testing`, or, where no tester step comes before your acceptance, to the step that opened the pull request, its reason naming the children that closed and the checks of the parent that need their code.
The tester sizes that round like any other; with no tester step before your acceptance, the implementation step's evidence for those checks at its head is the round.
The last round merges the base only on a trigger every pull request has: GitHub reads the pull request `CONFLICTING` (*A conflicting head*, below), or a check that needs a child's merged code fails at the head without it, and the implementation step's agent merges the base on that trigger.

A child's merge while the parent waits needs no move of its own:
the last round tests them all, and the daemon answers a conflict on its head (*A conflicting head*, below).
If the pull request merged before its last round, move nothing back.
Once the children close:

1. Run each check with no `ci_check` on the default branch and record each result, yourself or by a subagent you dispatch; `wait` for a subagent's report.

2. Read each `ci_check` result in `dispatch results`.

3. Post the results in the acceptance comment, then `sign_off`.

## After your sign-off

The steps after your acceptance in the task run, then retro, then, with a pull request, the merger merges the last step's head under the repository's rules; you start none of it, but a step after it whose key is `owner` is yours.
A person's request after your sign-off is a requirement of this issue: the role that holds the phase makes the `request_backward_move`.
When the merger reports:

- the issue moved back with `request_backward_move`: its reason says why the fix is a change to the change's own logic that no merge of the base resolves; you start nothing;

- a merge GitHub refuses for a repository setting or an App permission:
  open a `dispatch ask` naming the pull request, the head and GitHub's refusal, and never bypass a rule;

- a required approving review that no reviewer step gave: open a `dispatch ask` naming the base rule.
  Do not wait for a reviewer phase the task does not have;

- a squash into a base other than the default branch:
  tell the implementer step's agent of each open pull request of the tree built on that branch whose issue is yours and still in `implementing` to merge that base forward.
  Past `implementing`, the daemon asks for that merge itself (*A conflicting head*, below).

## A conflicting head

Until the task's last step has run, the daemon asks the agent of the task's implementation step (the implementer, in the default steps) to merge the base forward only when the next step needs a mergeable head (*The daemon's ask* in `skill://legion-worker/references/conflicts-and-rewrites.md`).
A conflict it does not act on yet is recorded: the task's steps go on at that head, and a reviewer step's approval of it (your verdict, at `reviewer:owner`) ends its review round.
Once every step has run, in `retro` or in `accepting` with no step after yours, the conflict waits, so accept the head as any other; once the issue enters `merging` the daemon asks the merger to merge the base forward,
and a check the base's newer code breaks is the merger's to fix in that merge too.
When it asks, the daemon tells you once by `checks-red`, naming the phase the issue stays in, the role asked, the head, the base and why; at the task's implementation step no notice comes.

Where a reviewer step of the task already ran, the daemon asks the agent of the reviewer step that started last (you, at `reviewer:owner`) to read the merge and approve the new head by its SHA, or request changes; the merger merges the new head only once that approval is on the pull request.
In `accepting`, wait for that approval before `sign_off`, and send the issue back with `request_backward_move` when the reviewer requests changes.
A task with no reviewer step has no review to await.

## A review thread GitHub will not resolve

Only the pull request author's App resolves a thread, so the step that opened it resolves them all.
When GitHub refuses its `resolveReviewThread`, open a `dispatch ask` with the thread's URL and GitHub's message, options resolved and could not.

## The close

The merge closes the issue (`done`), or, with no pull request, its retro's end does.
A production check after the close is not yours.
A root's close lingers the tree for `linger_hours`.

## Hand-over at a root's linger

Ask the person whether a child's work must end with the root.
Your role prompt says what a `handed_over` catch-up asks of you.
