# Acceptance, the merge and the close

Read the section for your step of `skill://legion-architect`, sections 4 to 7.
Where it says to ask a person, a child's own architect asks on that child's issue.

## Accepting

A step `acceptance:owner` enters `accepting` as yours, and `acceptance` with no key as a fresh agent's, which ends it by its verdict.
Read, against the goal, every check and every requirement:

1. where the task has a tester step before yours, the handoff of each: `dispatch handoff-read --issue <issue> --phase test` reads the newest, `--version <n>` an earlier one.

2. `dispatch results --issue <issue> --at <head>`: whether a pass stands for each check at the head the steps before yours passed.

3. where the task has a reviewer step before yours, each one's approval of the head its Legion footer names, never its `commit_id` (`skill://legion-worker/references/merge-gate.md`).
   With no pull request or at `reviewer:owner`, read its verdict instead.
   Read each claim of its prose against the code at that head.

4. where the issue has a pull request, `gh pr view <n> --json mergeable,mergeStateStatus`: a `CONFLICTING` head gets no CI (*A conflicting head*, below).

- `sign_off`, or `complete_step` with `pass`, works only in `accepting`, where the Dispatch status reads `needs_review`.
  Your `handoff_complete` there is refused (`ACCEPTANCE_IS_SIGN_OFF`).

- A `catch-up` whose `reason` names an issue waiting for your acceptance is its wake: its `phase-finished` may not have reached you.

- `SIGNOFF_CHILD_IN_WORKFLOW` names each issue beneath still in the workflow: run each to done, or park it under the rule, and sign off again.
  Ask the person about one that can do neither, and hold the acceptance until they answer.

- A finding ask holds the acceptance: one a worker sent you, or one `dispatch read` lists under `Open asks:` whose question opens `Finding from ` or `Clear defect from `.
  Follow it (`dispatch follow --ask <id> --action follow`) and publish its reference to the controller topic.
  Accept only once it is resolved, or carries the controller's reply (`Filed dispatch://`, `Handed dispatch://` or `Not filed:`):
  a closed issue hides its open questions from the controller.

- No `after_merge` result is asked here: the production check records it after a merged issue's close.

## The last round

A parent has a last round only where its task names `acceptance:owner` and it has a pull request.
Sign it off only on a head that carries the merges of every child whose merge its checks need.
Each check needs a pass at that head; one in another repository needs a pass standing since the close of each child it needs, at a commit on the branch that issue's pull request merged into or on its default branch;
a `default_branch` check needs the default branch.
A criterion proven on a deployed system is proven after that child's deploy, as the repository's deploy skill says.
A child's result proves no check of the parent: a child carrying part of a parent's requirement proves it with a check of its own whose `serves` names it.
Nothing builds on the parent's close, which comes last.
Until those children close, leave the issue in `accepting`; that wait is no reason to park a child.
Before you park a child, move its criterion to the parent's pull request or a released, open child.
Never downgrade it.
Then, in one turn:

1. Where the task has a tester step, `envoy_publish` to its tester's role topic:

   "The next round of <KEY> is its last round: a full test round of every check of the issue with no `ci_check`."

   With no tester step, the implementation step's evidence for those checks at the merge head is the full round.

2. `request_backward_move` to the step that opened the pull request, with the reason:

   "Last round: merge the default branch your addressing line names into the branch, push, end your forward-merge comment with `parent's last round: full test round`, and complete."

   Never hard-code `main`.

A child's merge while the parent waits needs no move of its own:
the last round's forward merge takes them all, and the daemon answers a conflict on its head (*A conflicting head*, below).
If the pull request merged before its last round, move nothing back.
Once the children close:

1. Have a fresh `task` subagent run each check with no `ci_check` on the default branch and record each result, and `wait` for its report.

2. Read each `ci_check` result in `dispatch results`.

3. Post the results in the acceptance comment, then `sign_off`.

## After your sign-off

The task's next step runs, or retro where yours was its last step.
Then, with a pull request, the merger merges its head under the repository's own rules.
You start none of it.
A person's request after your sign-off is a requirement of this issue: the role that holds the phase makes the `request_backward_move`.
When the merger reports:

- a failed verification: tell it to move the issue back with `request_backward_move`, naming what failed;

- a merge GitHub refuses for a repository setting or an App permission:
  open a `dispatch ask` naming the pull request, the head and GitHub's refusal, and never bypass a rule;

- a required approving review that no reviewer step gave: open a `dispatch ask` naming the base rule.
  Do not wait for a reviewer phase the task does not have;

- a squash into a base other than the default branch: find each open pull request of the tree built on it.
  Where its issue is yours and still in `implementing`, tell its implementer step's agent to merge that base forward in that round.
  Past `implementing`, the daemon asks for that merge itself (*A conflicting head*, below).

## A conflicting head

A head that conflicts with its base keeps the issue in its phase.
The daemon asks for the merge forward only when the next step needs a mergeable head (*The daemon's ask* in `skill://legion-worker/references/conflicts-and-rewrites.md`).
A conflict it does not act on yet is recorded: the task's steps go on at that head.
A reviewer step's approval of it (your verdict, at `reviewer:owner`) ends its review round.
The daemon acts on the move into `accepting`, `retro` or `merging`.
When it asks away from the task's implementation step, it tells you once by `checks-red`, naming the phase the issue stays in, the head, the base and why; at that step no notice comes.
You start nothing for the merge.

Where a reviewer step ran, the daemon asks its agent to read the merge and approve the new head by its SHA, or request changes.
At `reviewer:owner`, that agent is you.
A reviewer step still to come reviews the head when it runs.
In `accepting`, wait for that approval before `sign_off`, and send the issue back with `request_backward_move` when it requests changes.
A task with no reviewer step has no review to await.

## A review thread GitHub will not resolve

Only the pull request author's App resolves a thread, so the step that opened it resolves them all.
When GitHub refuses its `resolveReviewThread`, open a `dispatch ask` with the thread's URL and GitHub's message, options resolved and could not.
The merger does not complete while it is open.

## The close

The merge closes the issue (`done`), or, with no pull request, its retro's end does.
One whose merge lands while an issue beneath it is still in the workflow stays in `merging` until the last of them is done or parked;
one `pr-merged` notice names what it waits for.
A production check after the close is not yours.
A root's close lingers the tree for `linger_hours`.

## Hand-over at a root's linger

When a root a person closed stays `done` through its linger, it hands each child still in the workflow to Legion as a root.
One that never started goes too.
Only a finished, parked or unreleased child stays.
Ask the person whether a child's work must end with the root.
A parked root hands over only a child in `merging` whose pull request is open.
Your role prompt says what a `handed_over` catch-up asks of you.
