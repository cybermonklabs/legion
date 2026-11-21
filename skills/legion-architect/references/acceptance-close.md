# After your sign-off: the merge and the close

Read the section for your step after your `sign_off` (`skill://legion-architect`, sections 6 and
7). Where it says to ask a person, a child's own architect sends the question to the architect
above it.

## After your sign-off

Retro runs, then the merger merges the approved head under the repository's own rules; you start
none of it. A person's request after your sign-off is this issue's work. It becomes a requirement
once it is in this issue's spec: on a root, at a version the person approves; on a child, at a
version the architect above you reviews. Then the role that holds the phase makes the
`request_backward_move`. A merge the merger cannot make arrives while the merger holds `merging`,
so the merger makes the move and you tell it which; from `accepting` you send an issue back only
to `implementing` or `testing`. When the merger reports:

- a failed verification: tell it to move the issue back with `request_backward_move`, naming what
  failed; never bypass;
- a merge GitHub refuses for a repository setting or an App permission: open a `dispatch ask`
  naming the pull request, the head and GitHub's refusal, and never bypass a rule;
- a squash into a base other than the default branch: tell the implementer of each open pull
  request of the tree built on that branch whose issue is yours and still in `implementing` to
  merge that base forward in its implementing round. Past `implementing`, the daemon asks for that
  merge itself (*A conflicting head* in `skill://legion-architect/references/acceptance.md`).

## The close

The merge closes the issue (`done`). One whose merge lands while an issue beneath it is still in
the workflow stays in `merging` until the last of them is done or parked; one `pr-merged` notice
names what it waits for. The production check after the close is not yours.

- A child's close reaches you as `child-closed`. Where the project's `production_check` names the
  role that works the check (the tester, unless the setting is `off`), the daemon retires five of
  its six claims and leaves that role running, because the issue still owes the production check;
  where the setting is `off` no check opens, and the close retires all six and releases the pod.
- A root's close writes `done`, releases any admission slot the tree's issues still held, and
  lingers the tree for `linger_hours` with every member suspended. The linger's end, the tree's
  last step, waits until every member's open production check has closed.
- `read_record` shows a closed issue's open check (`check`: the pull request, the merge commit, the
  role the check records as the one that works it, the looks, what woke the last one, and
  `nextWakeAt`, when the next look is due: the time the checking agent named for itself on a
  `pending`, the retry of a look nobody reported, or the check's own bound where neither stands).

## Hand-over at a root's linger

A root a person closed that stays `done` through its linger hands each child still in the
workflow, a child that never started included, to Legion as a root of its own: `todo` with the
`legion` label, its phase, gate, phase records and pull request carried into a new generation, and
that phase's role started. A child that had its own architect keeps that architect's session: the
root's architect start resumes the same claim, and its next launch gives it the root architect's
prompt. A child that ran without one gets a new architect session. Only a finished, parked or
never-released child stays. The hand-over runs by itself and needs no action from you. Ask the
person whether a child's work must end with the root. A parked root hands over only a child in
`merging` whose pull request is open; ask the person whether that child's merge must wait for the
root.

If you were that child's own architect, your session is the same, and your role prompt is now the
root architect's: follow it from your next `catch-up`, which says `handed_over`. Your role prompt
says what a `handed_over` catch-up asks of you. The approval request of a spec you then register
waits with the person like any other, but it starts no wait of the root's own: a root your
catch-up calls `handed_over` stands at a phase it carried, never `admitted`, and only a root at
`admitted` waits on a person.
