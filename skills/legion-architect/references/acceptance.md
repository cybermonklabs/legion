# Acceptance, the merge and the close

Read the section for your step of `skill://legion-architect`, sections 4 to 7. Where it says to
ask a person, a child's own architect sends the question to the architect above it.

## Retro's result

Retro posts one `dispatch comment` of learnings and one `dispatch message`, brings the pull
request body up to date, and commits nothing. When it reports that it cannot compute, read or
write that body, it is still in its phase: answer it on its role topic from the deployment
instructions, or open a `dispatch ask` naming the missing instruction.

## Accepting

Read, against the goal, every check and every requirement:

1. the tester's handoff: `dispatch handoff-read --issue <issue> --phase test`;
2. `dispatch results --issue <issue>`: whether a pass stands for each check at the approved head;
3. the reviewer's approval of the head its Legion footer names, never its `commit_id`
   (`skill://legion-worker/references/merge-gate.md`);
4. each claim of the reviewer's prose, against the code at that head;
5. `gh pr view <n> --json mergeable,mergeStateStatus`: a `CONFLICTING` head gets no CI
   (*A conflicting head*, below).

- `sign_off` works only in `accepting`, where the Dispatch status reads `needs_review`; your
  `handoff_complete` there is refused (`ACCEPTANCE_IS_SIGN_OFF`).
- A `catch-up` whose `reason` names an issue waiting for your acceptance is its wake: the
  `phase-finished` may never have reached you.
- `SIGNOFF_CHILD_IN_WORKFLOW` names each issue beneath still in the workflow: run each to done, or
  park it under the rule, and sign off again. Ask the person about one that can do neither, and
  hold the acceptance until they answer.
- A finding ask holds the acceptance: one a worker sent you, or one `dispatch read` lists under
  `Open asks:` whose question opens `Finding from ` or `Clear defect from `. Follow it
  (`dispatch follow --ask <id> --action follow`) and publish its reference to the controller topic.
  Accept only once it is resolved, or carries the controller's reply
  (`Filed dispatch://`, `Handed dispatch://` or `Not filed:`): a closed issue hides its open
  questions from the controller.
- No `after_merge` result is asked here; the production check records it after the close.

## The last round

Sign off a parent only on an approved head that carries the merges of every child its checks
need. Each check needs a pass at that head, or one recorded below it where every push since
changes no code that runs; one in another repository, a pass standing since the
close of each child it needs, at a commit on the branch that issue's pull request merged into or
on its default branch; a `default_branch` check, on the default branch. A criterion proven on a
deployed system is proven after that child's deploy, as the repository's deploy skill says. A
child's result proves no check of the parent: a child carrying part of a parent's requirement
proves it with a check of its own whose `serves` names it. Nothing builds on the parent's merge,
which comes last. Until those children close, leave the issue in `accepting`; that wait is no
reason to park a child. Before you park a child, move its criterion to the parent's own pull
request or a released, open child; never downgrade it. Then, in one turn:

1. `envoy_publish` to the tester's role topic: "The next round of <KEY> is its last round: a full
   test round of every check of the issue with no `ci_check`."
2. `request_backward_move` to `implementing`, with the reason "Last round: merge the default
   branch your addressing line names into the branch, push, end your forward-merge comment with
   `parent's last round: full test round`, and complete." Never hard-code `main`.

A child's merge while the parent waits needs no move of its own: the last round's forward merge
takes them all, and the daemon answers a conflict on its head (*A conflicting head*, below). If
the pull request merged before its last round, move nothing back: once the children close, have
a fresh `task` subagent run each check with no `ci_check` on the default branch and record each
result, read each `ci_check` result in `dispatch results`, post them in the acceptance comment,
then `sign_off`.

## After your sign-off

Retro runs, then the merger merges the approved head under the repository's own rules; you start
none of it. A person's request after your sign-off is a requirement of this issue: the role that
holds the phase makes the `request_backward_move`. When the merger reports:

- a failed verification: tell it to move the issue back with `request_backward_move`, naming what
  failed;
- a merge GitHub refuses for a repository setting or an App permission: open a `dispatch ask`
  naming the pull request, the head and GitHub's refusal, and never bypass a rule;
- a squash into a base other than the default branch: tell the implementer of each open pull
  request of the tree built on that branch whose issue is yours and still in `implementing` to
  merge that base forward in its implementing round. Past `implementing`, the daemon asks for that
  merge itself (*A conflicting head*, below).

## A conflicting head

A head that conflicts with its base keeps the issue in its phase. The daemon asks the implementer
to merge the base forward only when the next step needs a mergeable head (*The daemon's ask* in
`skill://legion-worker/references/conflicts-and-rewrites.md`). A conflict it does not act on yet
is recorded: the tester and the reviewer go on at that head, an approval of it ends the review
round, and the daemon acts when the issue enters `accepting`. When it asks outside
`implementing`, it tells you once by `checks-red`, naming the phase the issue stays in, the head,
the base and why; in `implementing` no notice comes. You start nothing for the merge.

Past review, the daemon also asks the reviewer to read the merge and approve the new head by its
SHA, or request changes. In `accepting`, wait for that approval of the new head before
`sign_off`, and send the issue back with `request_backward_move` when the reviewer requests
changes.

## A review thread GitHub will not resolve

Only the pull request author's App resolves a thread, so the implementer resolves them all. When
GitHub refuses its `resolveReviewThread`, open a `dispatch ask` with the thread's URL and GitHub's
message, for a person to resolve it, with the options resolved and could not. The merger does not
complete while it is open.

## The close

The merge closes the issue (`done`). One whose merge lands while an issue beneath it is still in
the workflow stays in `merging` until the last of them is done or parked; one `pr-merged` notice
names what it waits for. The production check after the close is not yours. A root's close
lingers the tree for `linger_hours`.

## Hand-over at a root's linger

A root a person closed that stays `done` through its linger hands each child still in the
workflow, a child that never started included, to Legion as a root of its own; only a finished,
parked or never-released child stays. Ask the person whether a child's work must end with the root.
A parked root hands over only a child in `merging` whose pull request is open. Your role prompt
says what a `handed_over` catch-up asks of you.
