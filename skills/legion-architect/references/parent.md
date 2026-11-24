# An issue with children

Part of `skill://legion-architect`, sections 3 and 4.
Read it while a parent waits in `awaiting_children`, before you file children for an issue or set a parent link, on a code defect in an issue with children, and before you accept a parent.

## While the children run

While a parent is in `awaiting_children`, act only on its children (release them, write `done`, `icebox` or `backlog` on them in Dispatch under the rule for each move, or move an unreleased one out of the tree), and start nothing for its return.
Move an unreleased child that should not run now to no parent, never under another issue of the tree, since an open child holds every phase start of its parent.
When the person says to run that child, move it back under its parent and release it; until they answer, sign off neither that parent nor any issue above it.

## An issue with children has no code of its own

Every change of an issue with children, and a fix found in its work, goes into an open child's pull request or a new child.
Where an issue whose branch already carries code needs children, file a child for that code, whose work starts from those commits.

On an issue with children, the fix of a code defect, a red check's included, goes to a new child that you file and release, never to the issue's implementer.
After a tester fail or a review's changes request there, file that child and tell the implementer to change no code until the daemon stops its phase.

## A root with no code of its own

When every change lands through children, the root's implementer opens its pull request on an empty commit.
Plan no code for the root, and create no child only to give it a commit.

## Class roots and links

A class root (an issue whose spec states a class of work, each member its child) that must take new members is a Legion root with no code of its own, never an issue that holds only a design.
A mention of a class in prose is no link: only a Dispatch `child_of` link (`--parent`) is one, and a root counts only once a person approved its spec.

- A new member of your root's class, new work in your tree's unfinished work, or such a defect the controller sends becomes a child of the tree or part of an open issue's work.

- Once your spec is approved, link each issue already filed that belongs to your root's class or unfinished work: `dispatch issue-update --issue <KEY> --parent <ROOT>`.
  Release a linked issue that has not run.
  One that runs as a root of its own stays its own tree: `envoy_publish` to its architect's role topic your root's key and that `skill://legion-architect/references/view.md` now applies to it.

- Work outside the tree that another approved root holds is a finding for the controller.

## A parent's head

You accept a parent only where its task names `acceptance:owner`.
Accept it on its head, which need not carry its children's merges, once each check a role runs has a pass at that head whose observed line names a base commit that carries the merge of every child the check needs (`skill://legion-worker/references/results.md`, "The rules every phase's evidence follows").
Read a `ci_check` check, an `after_merge` check and a check in another repository as for any issue: one in another repository needs a pass standing since the close of each child it needs, at a commit on the branch that issue's pull request merged into or on its default branch;
a `default_branch` check needs the default branch.
A criterion proven on a deployed system is proven after that child's deploy, as the repository's deploy skill says.
A child's result proves no check of the parent: a child carrying part of a parent's requirement proves it with a check of its own whose `serves` names it.
Nothing builds on the parent's merge, which comes last.
Before a child goes to the backlog, move its criterion to a released, open child or a new child; never downgrade it.

Send the tester no message for the children's merges, wait in `accepting` for no child, and move nothing back for a child's merge, a park, the last close or a conflict during the wait.
Move a parent whose pass names an older base commit, or none, back to `testing` with `request_backward_move`, naming the children whose merges that base lacks, and ask for no forward merge.

If the parent's pull request merged before its children's, move nothing back. Once the children close:

1. Have a fresh `task` subagent run each check with no `ci_check` on the default branch and record each result, and `wait` for its report.
2. Read each `ci_check` result in `dispatch results`.
3. Post the results in the acceptance comment, then `sign_off`.
