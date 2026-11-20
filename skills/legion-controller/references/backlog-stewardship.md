# Backlog stewardship

Read this for the weekly-plan reconciliation (first turn of each UTC day, and after each directive a person gives), a scope question or routing request on your role topic, and stopped-session work. The deployment instructions name the weekly-planning issue; its newest `plan-week-*` artifact is the work order and the one-owner map, and Dispatch board rank is that order in force.

Claim no Envoy role but `legion-<project>-controller`: another ends your wake delivery.

## Reconcile the weekly plan

1. Read the weekly-planning issue and its newest `plan-week-*` artifact (by its latest `artifact.version` event) before you change any issue.
2. Rank the plan by the deployment's priorities (`dispatch priorities --project <KEY>`). An issue takes the rank of the highest priority its `Advances:` line names, its own or inherited; an issue that advances none ranks below all. Order the plan's lines by rank, highest first, keeping the plan's order within a rank; a line that lists several issues takes its highest issue's rank. Move a line that breaks this order, unless a person asked for a different order of that work: then cite that request beside the line (a request to do the work is no request about its order). Name each move in your turn's summary.
3. For every plan line whose key has this project's prefix (`daemon.project`), make the board order match the line order exactly, from the plan alone: `dispatch issue-update --issue <KEY> --before <OTHER>` (or `--after`). When one plan line lists several issues, keep them in that order. Set the priority a line states; leave it where it states none.
4. Reconcile the one-owner map. A `live` or `unresponsive` owner in `envoy_sessions` still owns its work: change nothing. Only an owner that reads `gone`, or that the registry has no record of, has no session. Route such unowned work to a session whose `dir` is a checkout of the issue's repository (`dispatch issue-update --issue <KEY> --route session:<session id>`) and `envoy_send` it `You own <KEY>: <title>.`. Never route an issue that carries the `legion` label, and never change its `Assignee:`, which decides who receives asks.
5. Edit that line's owner in the artifact with `dispatch doc-edit`, guarded by the token of the current `dispatch doc-read`, replacing superseded text rather than logging changes. A comment or message does not update the one-owner map.

A directive is a direct Dispatch message to you: answer it, carry it out, record the rule it sets in its weekly-plan row (or the directives section) with the message's link, then run the whole pass.

## Answering controller deliveries

Read every delivery against live Dispatch and daemon state. You decide labels, `todo`, `backlog`, `icebox` and board order. Answer a scope question from the weekly plan and the deployment instructions; send anything else with `envoy_send` to the session the one-owner map names for that issue.

## The owner audit

At each pass, read `dispatch issues --project <KEY> --priority 0 --priority 1 --all`. A row that an open ask already names (`dispatch open-asks --project <KEY>`) needs no second ask.

- An unclaimed row in `triage`, `icebox`, `backlog` or `todo` is a decision a person makes: someone builds it, or it closes. Ask it as a finding (`skill://legion-worker/references/findings.md`), unless the one-owner map names a `live` or `unresponsive` owner for it or `read_state` records it under `issues`. A claimed or in-flight row already has an owner.
- A close that says a defect cannot happen cites the code that prevents it. Without that, the issue is unread, not foreclosed: ask a person, as a finding, whether to reopen it, quoting the close's message.
- A plan or measurement nobody builds (no claim, and no route that reaches a holder) needs an owner: route it as step 4 of the reconciliation routes unowned work; with no such session, ask a person who owns it, as a finding.
- A merged pull request names an issue not yet `testing` or `done`: check it live, then move it.
- Your own earlier messages, or the issue's comments, show someone doing its work by hand more than once: set the status that work has reached (`in_progress` while it goes on, `done` once it is finished) with `dispatch issue-update --issue <KEY> --status <status>`. Leave an issue that carries the `legion` label or sits under one that does: the daemon owns its status and undoes yours.
- A route nobody holds, after the two reads below: staff the role, route to a live holder, or clear the route and assign it.

## Recovering stopped-session work

A session route is stopped only after two reads of `dispatch issues --project <KEY> --route-status no_holder --all`, at least ten minutes apart, on two wakes, both list it. Then read its owner in `envoy_sessions`: recover the work only from one that reads `gone`, or `unresponsive` with a `last_seen` more than 20 minutes old.

Only work that a person's directive, recorded in the plan with its link, moves to Legion when its session stops is yours to move: keep its labels, add `legion`, then `set_status` it to `todo`, and update its owner row. Never apply this to an issue in a live Legion tree.

## A root the plan defers

A person's `legion` label outranks a plan's deferral: the daemon queues the root at once, since a person's label on an unrecorded root in `triage` is the root's `todo`, written by the daemon. A labelled root the daemon queued or runs, which the weekly plan says to defer, is parked at your next pass only when the plan's line names a person's request: `set_status` to `backlog` (`icebox` for longer), citing that request in a comment. A plan row with no such request moves nothing; ask instead:

1. Find your earlier ask: step 1 of `skill://legion-controller/references/park-asks.md`, with `Asked whether to defer: ` for `Asked whether to park: `, its second line the plan row. A comment or reply from any session counts as yours. Open or resolved with no answer, or your reply starts `Deferred` or `Let run`: do nothing. Answered with no reply of yours: step 4. Your reply starts `Not deferred`: go on to step 2.
2. Check that Legion runs the root: it carries the label, stands in `admission.active` or `admission.waiting`, and its `phase` is not `done`. If not, do nothing. Else open a `dispatch ask` on the report issue naming the root and the plan row, options `Defer it` and `Let it run`, follow it, and comment on the root: first line `Asked whether to defer: dispatch://<REPORT KEY>/ask/<id>`, second line the plan row exactly.
3. Name the row and the ask in the day's report while it is open.
4. On `Defer it` while step 2's check holds, park the root as above, citing the answer. On `Defer it` in any other case, move nothing, and say in your reply what you checked. On `Let it run`, move nothing and take the deferral off that plan row with `dispatch doc-edit`. Reply under the ask, first line exactly `Deferred dispatch://<root>`, `Not deferred dispatch://<root>` or `Let run dispatch://<root>` (`dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`).
