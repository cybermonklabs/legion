# Comments on a parked tree's decisions

Read this before you post on a decision that step 4 of `skill://legion-controller/references/parked-decisions.md#deciding` judged. `<KEY>` is the issue where the decision was made, `<root>` its tree's root, `<ref>` the decision's reference and `<login>` the person who made it (`skill://legion-controller/references/parked-decisions-notice.md`).

On a tree whose architect has stopped (step 1 there: `phase` is not `done`, and `architect.state` is `failed` or `retired`), write `<root>'s architect has stopped, so it` for `<root> is parked, so its architect`, `The tree waits.` for `The tree stays parked.`, and `backlog, then todo,` for `todo`, in the reply and in the root's copy.

## A next step

- Your own hold: `Parked decision read: <ref> by <login> gives this tree its next step (<one sentence>). Legion runs it again.`
- Another's park, or one you cannot read, whose words ask Legion to start the tree: `Parked decision read: <ref> by <login> asks Legion to start this tree. Legion runs it again.`
- Another's park, or one you cannot read, otherwise: `Parked decision read: <ref> by <login> gives this tree its next step (<one sentence>).`, then the park's sentence below, then, for an ask's answer or reply, `Its architect then starts with this decision.`

## A question

Reply `Parked decision read: <ref> by <login> asks <the question in a few words>.`, then the answer and where in the record it is (issue, spec, handoffs, pull request) and `The tree stays parked. Set <root> to todo to run it.`; or, when the record does not answer it, `The record does not answer it, and <root> is parked, so its architect cannot answer either. Set <root> to todo to bring its architect back to answer.`

For another's park, or one you cannot read, end either reply with the park's sentence below, with `<root>` for `the tree`, in place of its `Set <root> to todo …` sentence. In the reply the record does not answer, add `Its architect then starts with this question.` after that sentence, except for a comment that is no reply on an ask.

## The park's sentence

- A person's or another session's: `<parker> parked the tree, so it stays parked until a person sets it to todo, approves its spec when it has no parent, or asks Legion to start it.`, with the login for a person and `session <id>` for a session.
- One by `session legion-daemon:<PROJECT>` with no mark: `The tree was parked through Legion (session legion-daemon:<PROJECT>) for no hold of its own, so it stays parked until a person sets it to todo, approves its spec when it has no parent, or asks Legion to start it.`
- One you cannot read: `Its events do not show who parked the tree, so it stays parked until a person sets it to todo, approves its spec when it has no parent, or asks Legion to start it.`

## Where to post

Post on the issue where the decision was made: under its ask, `dispatch comment --issue <KEY> --reply-to-ask <ask id> --turn human --body-file -`; under a reply, `--reply-to <comment id>`; for a change request, a plain comment on that issue.

When that issue is not the root, post on the root too (`dispatch comment --issue <root> --body-file -`): the same comment for a next step, and for a question a short copy, `Parked decision read: <ref> by <login> asks a question, replied to on <KEY>. The tree stays parked.` The decision's own issue holds a comment either way: its events are where a later walk finds the decision.

Several decisions on one tree get one comment naming each `ref`, on the root and on each such issue.
