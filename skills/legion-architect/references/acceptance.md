# Acceptance, the merge and the close

Read the section for your step of `skill://legion-architect`, sections 4 to 7.
Where it says to ask a person, a child's own architect sends the question to the architect above it.

## Retro's result

Retro's output is in `skill://legion-retro`, steps 3 to 5.
When it reports that it cannot compute, read or write the pull request body, it is still in its phase:
answer it on its role topic from the deployment instructions, or open a `dispatch ask` naming the missing instruction.

## Accepting

Only a task whose steps name `acceptance:owner` enters `accepting`.
Read, against the goal, every check and every requirement:

1. where a tester step comes before your acceptance, the tester's handoff: `dispatch handoff-read --issue <issue> --phase test`;

2. `dispatch results --issue <issue>`: whether a pass stands for each `before_merge` check at the head the step before your acceptance passed;

3. where a reviewer step comes before your acceptance, the reviewer's approval of the head its Legion footer names, never its `commit_id` (`skill://legion-worker/references/merge-gate.md`), and each claim of its prose, against the code at that head;

4. `gh pr view <n> --json mergeable,mergeStateStatus`: a `CONFLICTING` head whose checks passed is accepted as any other (*A conflicting head*, below).

- `sign_off` works only in `accepting`, where the Dispatch status reads `needs_review`; your `handoff_complete` there is refused (`ACCEPTANCE_IS_SIGN_OFF`).

- A `catch-up` whose `reason` names an issue waiting for your acceptance is its wake: the `phase-finished` may never have reached you.

- `SIGNOFF_CHILD_IN_WORKFLOW` names each issue beneath still in the workflow: run each to done, and sign off again.

- A finding ask holds the acceptance: one a worker sent you, or one `dispatch read` lists under `Open asks:` whose question opens `Finding from ` or `Clear defect from `.
  Follow it (`dispatch follow --ask <id> --action follow`) and publish its reference to the controller topic.
  Accept only once it is resolved, or carries the controller's reply (`Filed dispatch://`, `Handed dispatch://` or `Not filed:`):
  a closed issue hides its open questions from the controller.

- No `after_merge` result is asked here; the production check records it after the close.

## The last round

A parent has a last round only where its task names `acceptance:owner`.
Sign it off only on a head at which its checks pass.
Each check needs a pass at that head; one in another repository needs a pass standing since the close of each child it needs, at a commit on the branch that issue's pull request merged into or on its default branch;
a `default_branch` check needs the default branch.
A criterion proven on a deployed system is proven after that child's deploy, as the repository's deploy skill says.
A child's result proves no check of the parent: a child carrying part of a parent's requirement proves it with a check of its own whose `serves` names it.
Nothing builds on the parent's merge, which comes last.
Until those children close, leave the issue in `accepting`.
Then start the last round: `request_backward_move` to `testing`, or to `implementing` where no tester step comes before your acceptance, its reason naming the children that closed and the checks of the parent that need their code.
The tester sizes that round like any other; with no tester step before your acceptance, the implementer's evidence for those checks at its head is the round.
The last round merges the base only on a trigger every pull request has: GitHub reads the pull request `CONFLICTING` (*A conflicting head*, below), or a check that needs a child's merged code fails at the head without it, and the implementer merges the base on that trigger.

A child's merge while the parent waits needs no move of its own:
the last round tests them all, and the daemon answers a conflict on its head (*A conflicting head*, below).
If the pull request merged before its last round, move nothing back. Once the children close:

1. Run each check with no `ci_check` on the default branch and record each result, yourself or by a subagent you dispatch; `wait` for a subagent's report.
2. Read each `ci_check` result in `dispatch results`.
3. Post the results in the acceptance comment, then `sign_off`.

## After your sign-off

The steps after your acceptance in the task run, then retro, then the merger merges the last step's head under the repository's rules; you start none of it.
A person's request after your sign-off is a requirement of this issue: the role that holds the phase makes the `request_backward_move`.
When the merger reports:

- the issue moved back with `request_backward_move`: you start nothing;

- a merge GitHub refuses for a repository setting or an App permission:
  open a `dispatch ask` naming the pull request, the head and GitHub's refusal, and never bypass a rule;

- a required approving review that no reviewer step gave: open a `dispatch ask` naming the base rule.
  Do not wait for a reviewer phase the task does not have;

- a squash into a base other than the default branch:
  tell the implementer of each open pull request of the tree built on that branch whose issue is yours and still in `implementing` to merge that base forward.
  Past `implementing`, the daemon asks for that merge itself (*A conflicting head*, below).

## A conflicting head

A head that conflicts with its base keeps the issue in its phase.
Until the task's last step has run, the daemon asks the implementer to merge the base forward only when the next step needs a mergeable head (*The daemon's ask* in `skill://legion-worker/references/conflicts-and-rewrites.md`).
A conflict it does not act on yet is recorded: the task's steps go on at that head, and a reviewer step's approval of it ends its review round.
Once every step has run, in `retro` or in `accepting` with no step after yours, the conflict waits, so accept the head as any other; once the issue enters `merging` the daemon asks the merger to merge the base forward,
and a check the base's newer code breaks is the merger's to fix in that merge too.
When it asks, the daemon tells you once by `checks-red`, naming the phase the issue stays in, the role asked, the head, the base and why; in `implementing` no notice comes.
You start nothing for the merge.

Where the task's reviewer step already ran, the daemon asks the reviewer to read the merge and approve the new head by its SHA, or request changes; the merger merges the new head only once that approval is on the pull request.
In `accepting`, wait for that approval before `sign_off`, and send the issue back with `request_backward_move` when the reviewer requests changes.
A reviewer step after yours reviews the new head when it runs.
A task with no reviewer step has no review to await.

## A review thread GitHub will not resolve

Only the pull request author's App resolves a thread, so the implementer resolves them all.
When GitHub refuses its `resolveReviewThread`, open a `dispatch ask` with the thread's URL and GitHub's message, for a person to resolve it, with the options resolved and could not.

## The close

The merge closes the issue (`done`).
The production check after the close is not yours.
A root's close lingers the tree for `linger_hours`.

## Hand-over at a root's linger

Ask the person whether a child's work must end with the root.
Your role prompt says what a `handed_over` catch-up asks of you.
