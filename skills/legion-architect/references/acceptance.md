# Acceptance

Read the section for your step of `skill://legion-architect`, sections 4 to 6. Where it says to
ask a person, a child's own architect sends the question to the architect above it. After your
sign-off (the merger's reports, the close, a root's linger), read
`skill://legion-architect/references/acceptance-close.md`.

## Retro's result

Retro output is at most one `dispatch comment` on the issue with what the work taught, one
`dispatch message` on the issue, and, where the repository's instructions derive pull request body
content from the pull request's changed paths, that content at the approved head. Retro commits
nothing and pushes nothing, writes no handoff and never rewrites the reviewer-approved head.

When the implementer reports that it cannot compute that body content or do the work a line of it
affirms, that its read of the body failed or came back empty, or that GitHub refused the body, it
has pushed nothing and is still in its phase: answer it on its role topic from the deployment
instructions, or open a `dispatch ask` naming the missing instruction.

## Accepting

Read, against the goal, every check and every requirement:

1. the tester's handoff: `dispatch handoff-read --issue <issue> --phase test`;
2. `dispatch results --issue <issue>`: whether a pass stands for each check at the approved head;
3. the reviewer's approval of the head its Legion footer names, never its `commit_id`
   (`skill://legion-worker/references/merge-gate.md`);
4. each claim of the reviewer's prose, against the code at that head and command output, never
   against the finding it ruled on or its own verdict: no other agent reads it;
5. `gh pr view <n> --json mergeable,mergeStateStatus`: a `CONFLICTING` head gets no CI
   (*A conflicting head*, below).

A tree often brings its own shortfall written up; that is no permission: your job is the goal.

- While a child's own architect does not hold its role, the child's acceptance is yours.
- `sign_off` works only in `accepting`, where the Dispatch status reads `needs_review`; your
  `handoff_complete` there is refused (`ACCEPTANCE_IS_SIGN_OFF`).
- Wait for a `ci_check` run still going, or GitHub unread, and move nothing back for it; a name no
  run answers is yours to correct in the check.
- A `catch-up` whose `reason` names an issue waiting for your acceptance is its wake: the
  `phase-finished` may never have reached you.
- `SIGNOFF_CHILD_IN_WORKFLOW` names each issue beneath still in the workflow: run each to done, or
  park it with `park_child` only when a person asked for that move, or when the work is done,
  folded into another issue or no longer needed, and say in a comment which of those it is; then
  sign off again. Ask the person about one that can do neither, and hold the acceptance until they
  answer. One issue beneath never holds it: one the daemon itself holds at `admitted` under an
  ancestor it has already accepted, which starts when that ancestor closes.
- A finding ask holds the acceptance: one a worker sent you, or one `dispatch read` lists under
  `Open asks:` whose question opens `Finding from ` or `Clear defect from `. Follow it
  (`dispatch follow --ask <id> --action follow`) and publish its reference to the controller topic.
  Accept only once it is resolved, or carries the controller's reply
  (`Filed dispatch://`, `Handed dispatch://` or `Not filed:`): a closed issue hides its open
  questions from the controller.
- No `after_merge` result is asked here; the production check records it after the close.

## The last round

Sign off a parent only on an approved head that carries the merges of every child its checks
need. Each check needs a pass at that head; one in another repository, a pass standing since the
close of each child it needs, at a commit on the branch that issue's pull request merged into or
on its default branch, that default branch alone where Dispatch knows of no merge of the issue; a
`default_branch` check, on the default branch, whatever branch the pull request merged into. A
criterion proven on a deployed system is proven after that child's deploy, as the repository's
deploy skill says. A child's result proves no check of the parent: a child carrying part of a
parent's requirement proves it with a check of its own whose `serves` names it. Nothing builds on
the parent's merge, which comes last: a child that waits for it waits forever, so work another
child builds on is a child of its own. Until those children close, leave the issue in
`accepting`; that wait is no reason to park a child. Before you park a child, move its criterion
to the parent's own pull request or a released, open child; never downgrade it. Then, in one turn:

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

Only the pull request author's App resolves a thread, so the implementer resolves every thread
with plain `gh`: the ones it answers and the bot threads the reviewer accepted and named to it.
The reviewer resolves none. When GitHub refuses its `resolveReviewThread`, open a `dispatch ask`
with the thread's URL and GitHub's message, for a person to resolve it, with the options resolved
and could not. The merger does not complete while it is open.
