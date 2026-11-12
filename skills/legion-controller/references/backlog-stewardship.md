# Backlog stewardship

The controller keeps the Dispatch backlog's facts aligned with the deployment's current weekly
plan. The plan is the source of the work order and its one-owner map; Dispatch board rank is the
executable form of that order. The deployment instructions name the weekly-planning issue and its
`plan-week-*` artifact.

## Controller routing address

Claim no Envoy role other than `legion-<project>-controller`, your startup role, because changing
roles ends the controller's wake delivery. READY packets, scope questions and routing requests
reach you on `notifications.role.legion-<project>-controller`.

## Reconcile the weekly plan

Run this pass on the first turn of every UTC day and after every directive from a person:

1. Read the weekly-planning issue and select its newest `plan-week-*` artifact by its latest
   `artifact.version` event. Read that artifact before changing any issue.
2. For every plan line whose issue key carries this project's prefix (`daemon.project` in
   `read_state`), make the project's board order match the line order exactly, because board rank
   is the order Legion admits by: an issue whose first phase can start takes the rank of its
   tree's root, and an issue whose work came back goes ahead of all of them. Set the priority the
   plan line states, and leave the priority where it states none; priority changes no admission
   order. When one plan line lists
   several issues, keep them in that order. Take the order from the plan alone, never from a
   title or a route. Reorder with
   `dispatch issue-update --issue <KEY> --before <OTHER>` or `--after <OTHER>`.
3. Reconcile the plan's one-owner map. `envoy_sessions` gives each session a `state` (`live`,
   `unresponsive` or `gone`) and a `last_seen`; read the state, never the presence or absence of
   the row. A `live` or `unresponsive` owner still owns its work: change nothing for it. Only an
   owner that reads `gone`, or that the registry has no record of, is without a session. For
   unowned work, find an owner whose `envoy_sessions` `dir` is a
   checkout of the issue's repository, route the issue to it
   (`dispatch issue-update --issue <KEY> --route session:<session id>`) and `envoy_send` it
   `You own <KEY>: <title>.`. Routing starts nothing on Legion and sets no label. Never route an
   issue that carries the `legion` label, because Legion's own roles work it. Do not change the
   human `Assignee:` field: it decides who receives asks, not who works the issue.
4. Edit that issue's line in the plan artifact (its owner column) with `dispatch doc-edit`,
   guarded by the document token from the current `dispatch doc-read`. Replace superseded text
   instead of keeping a change log in it, because the artifact states the current rule. A comment
   or message does not update the one-owner map.

A directive from a person is a direct Dispatch message to this controller. Answer it first, carry out
its directive on scope or priority, then record the resulting current rule in the affected weekly-plan
row (or its directives section when it has no one line). Link the message when Dispatch gave it a
reference.
A directive triggers the full reconciliation pass even when it appears to affect only one issue.

## Answering controller deliveries

Read every delivery to `notifications.role.legion-<project>-controller` against live Dispatch and
daemon state. You decide labels, the statuses `todo`, `backlog` and `icebox`, and board order.
Answer a scope question from the weekly plan and the deployment instructions. Anything else goes
with `envoy_send` to the session the plan's one-owner map names for that issue.

For a READY packet, confirm the Dispatch record and leave the merge to the daemon. Never merge or
use a GitHub credential to merge from the controller, because the daemon submits the merge itself
at READY's head.

## The owner audit

At each pass, read `dispatch issues --project <KEY> --priority 0 --priority 1 --all`:

- An unclaimed row in `triage`, `icebox`, `backlog` or `todo` is a decision: someone builds it, or
  it closes. A claimed or in-flight row already has an owner.
- A close that says a defect cannot happen cites the code that prevents it; without that, the issue
  is unread, not foreclosed.
- A plan or measurement nobody builds needs an owner.
- A merged pull request names an issue not yet in `testing` or `done`: check it live, then move it.
- Someone repeats the issue's work by hand: fix its status.
- A route nobody holds (`--route-status no_holder`, any priority), after the two reads that
  "Recovering stopped-session work" names: staff the role, route the issue to a live holder, or
  clear the route and assign it.

## Recovering stopped-session work

Treat a session route as stopped only after two reads of
`dispatch issues --project <KEY> --route-status no_holder --all` at least ten minutes apart
both list it, as `dispatch issues --help` says for `route_status`, because a
restarting session is absent for minutes. Do not poll to create the second read; use the next
controller wake. Then read the owner's state with `envoy_sessions` (the session the route names,
or one that took it over). A `live` owner still holds the work. So does an `unresponsive` one,
which is past the live window but inside the gone horizon: it keeps its claims, and a message you
send it waits for it. Recover the work only from an owner that reads `gone`, or that reads
`unresponsive` with a `last_seen` more than 20 minutes old, which is as long as an issue claim is
protected.

For work that a person's directive, recorded in the plan with its link (the directives section
above), says moves to Legion when its session stops, preserve its existing labels, add `legion`,
then set it `todo` with the `legion` tool's `set_status`. A plan row you wrote with no such directive
behind it moves nothing. The label comes first: it is what
lets the daemon admit the root. Do not apply that recovery to an issue already in a live Legion
tree (a root in `admission.trees`, or an issue whose `tree` is one); its architect and daemon own its lifecycle. Update the weekly-plan owner row after the move.

## A root the plan defers

A person's `legion` label outranks a plan's deferral. The daemon queues the root at once: a
person's label on an unrecorded root in `triage` is the root's `todo`, written by the daemon. A
labelled root the weekly plan says to defer is parked by you at your next backlog pass, not
before, and only when the plan's line names a person's request to defer it: `set_status` it to
`backlog`, or to `icebox` for a longer deferral, and cite that request in a comment on the issue.
Park a root only when a person asked for that move, or when the work is done, folded into another issue or no longer needed.
A plan row you wrote with no such request behind it moves nothing. Ask the person whether to
defer that root instead:

1. Look for your earlier ask for that row. Read the root's comments with the command of
   `skill://legion-controller/references/wakes.md#ask-whether-to-park-a-tree` step 1, with
   `Asked whether to defer: ` in place of `Asked whether to park: `, and take the newest whose
   second line is exactly that plan row. Compare that line alone. A comment or a reply from any
   session counts as yours. When its ask is open, open nothing and post nothing. When it is
   answered, and no reply of yours under it starts `Deferred dispatch://`,
   `Not deferred dispatch://` or `Let run dispatch://`, act on its answer now (step 4). When your
   reply under it starts `Deferred dispatch://` or `Let run dispatch://`, the person decided
   already: open nothing and post nothing. When your reply under it starts
   `Not deferred dispatch://`, that ask is spent, because its `Defer it` came when the root was
   `done` or Legion no longer ran it. Go on to step 2.
2. Otherwise open a `dispatch ask` on the report issue that names the root and the plan row and
   asks the person whether to defer that root, with the options `Defer it` and `Let it run`.
   Follow it (`dispatch follow --ask '<ref>' --action follow`). Post one comment on the root
   (`dispatch comment --issue <root> --body-file -`). Make its first line
   `Asked whether to defer: dispatch://<REPORT KEY>/ask/<id>`. Make its second line the plan row,
   exactly as the plan gives it: that line is the key that step 1 compares.
3. Name the row and that ask in the day's report until the person answers.
4. On `Defer it`, when the root's status is not `done` and Legion still runs it, park the root as
   above, and cite the answer in a comment on the issue. On `Defer it` in any other case, move
   nothing, and say in your reply what you checked. On `Let it run`, move nothing, and take the
   deferral off that plan row with `dispatch doc-edit`, as step 4 of
   [Reconcile the weekly plan](#reconcile-the-weekly-plan) edits a row, so that the next backlog
   pass asks nothing. In each case, reply under the ask with what you did, its first line exactly
   `Deferred dispatch://<root>`, `Not deferred dispatch://<root>` or `Let run dispatch://<root>`:
   `dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`.

The cost is the architect's work between the daemon's `todo` and your park.
