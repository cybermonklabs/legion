# Acceptance, the merge and the close

## Accepting

No worker holds `accepting`. Read, against the goal:

1. the tester's handoff, where a tester step comes before yours: `dispatch handoff-read --issue <issue> --phase test`;
2. `dispatch results --issue <issue>`, at the head that the step before yours passed;
3. the reviewer's approval, where a reviewer step comes before yours, and each claim of its text against the code at that head.
   The approval is of the head that its `<!-- legion:` footer names.
   GitHub rewrites a review's `commit_id` to the current head, so never read the head from it.

- A head that GitHub reads `CONFLICTING` is accepted as any other ("A conflicting head").
- A finding ask holds the acceptance: one a worker sent you, or one that `dispatch read` lists under `Open asks:` whose question starts `Finding from ` or `Clear defect from `.
  Follow it (`dispatch follow --ask <id> --action follow`), and publish its reference to the controller topic.
  Accept only when it is resolved, or when it carries the controller's reply (`Filed dispatch://`, `Handed dispatch://` or `Not filed:`).
  A closed issue hides its open questions from the controller.
- An `after_merge` check gets its result after the close. It holds no acceptance.

## The last round

A parent has a last round only where its task names `acceptance:owner`.
A child's result proves no check of the parent.
A child that carries part of a parent's requirement proves that part with a check of its own whose `serves` names it.

1. Leave the parent in `accepting` until its children close.
   A child's merge while the parent waits needs no move of its own: the last round tests them all.
2. Start the last round with `request_backward_move` to `testing`, or to `implementing` where no tester step comes before yours.
   Its reason names the children that closed and the checks of the parent that need their code.
3. Sign off only on a head at which each check of the parent passes.
   A check in another repository needs a pass since the close of each child it needs, at a commit on the branch that child's pull request merged into, or on its default branch.
   A `default_branch` check needs the default branch.
   A requirement proven on a deployed system is proven after that child's deploy, as the repository's deploy skill says.

Where the parent's pull request merged before its last round, move nothing back.
Once the children close, run each check with no `ci_check` on the default branch and record each result, yourself or by a subagent whose report you `wait` for.
Read each `ci_check` result in `dispatch results`.
Post the results in the acceptance comment, then `sign_off`.

## A conflicting head

A head that conflicts with its base keeps the issue in its phase.
The daemon asks a role to merge the base forward when a step needs a mergeable head, and tells you with `checks-red`.
In `merging`, it asks the merger.
You start nothing for the merge.

Where that merge comes after the task's reviewer step ran, the reviewer reads the merge and approves the new head.
In `accepting`, wait for that approval before `sign_off`.
When the reviewer requests changes, send the issue back with `request_backward_move`.

## After your sign-off

The steps after yours, the retro and the merge run without you.
When the merger reports:

- a merge that GitHub refuses for a repository setting or an App permission: open a `dispatch ask` that names the pull request, the head and GitHub's refusal.
  Never bypass a rule.
- a required approving review that no reviewer step gave: open a `dispatch ask` that names the base's rule.
  Do not wait for a reviewer step that the task does not have.
- a squash into a base other than the default branch: tell the implementer of each open pull request of the tree built on that branch, whose issue is yours and still in `implementing`, to merge that base forward.
  Past `implementing`, the daemon asks for that merge itself.
- the issue moved back with `request_backward_move`: start nothing.

## The close

The merge closes the issue (`done`).
The production check after the close is not yours.
A root's close keeps its tree for `linger_hours`.
At the end of the linger, each child still in the workflow goes on as a root of its own.
Ask the person whether such a child's work must end with the root.
