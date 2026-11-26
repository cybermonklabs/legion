# Backlog stewardship

Read this for the prioritization pass (each turn it is due, and its daily form on the first turn of each UTC day and after each directive a person gives), a scope question or routing request on your role topic, and stopped-session work. The deployment instructions name the weekly-planning issue; its newest `plan-week-*` artifact is the work order and the one-owner map, and Dispatch board rank is that order in force.

Claim no Envoy role but `legion-<project>-controller`: another ends your wake delivery.

## The prioritization pass

The pass keeps the order of the work aligned with the project's priorities. Run it at most once a turn, after the wake's work, as the last step before your summary, and before the daily report on the turn that posts it.

- It is due on every turn that a `tick`, `slot-free` or `todo` wake starts.
- Its daily form is due on your first turn of each UTC day and after every directive from a person, even one that seems to touch one issue.
- On any other turn it is due when the wake's work is done, nothing else waits, and `dispatch issues --project <KEY> --updated-since <the time of your newest pass line> --all` lists an issue, or when your context holds no pass line.

1. Read the `read_state` summary's admission lines, the newest weekly plan, and the open roots, and make [the owner audit](#the-owner-audit) on that read.
2. Change only a rank, an admission or the plan, where it no longer matches the project's priorities, and change nothing else. Admit an issue only as the intake your deployment instructions give and `skill://legion-controller/references/admission.md#your-moves-to-todo` allow: a free slot is no reason to admit. A plan line that defers a root the daemon queued or runs: `skill://legion-controller/references/deferral-asks.md`.
3. In the daily form, also [reconcile the weekly plan](#reconcile-the-weekly-plan).
4. End the pass with exactly one line: `Prioritization pass at <RFC3339>: <what it changed, or none>`.

## Reconcile the weekly plan

1. Read the weekly-planning issue and its newest `plan-week-*` artifact (by its latest `artifact.version` event) before you change any issue.
2. Rank the plan by the deployment's priorities (`dispatch priorities --project <KEY>`). An issue takes the rank of the highest priority its `Advances:` line names, its own or inherited; an issue that advances none ranks below all. Order the plan's lines by rank, highest first, keeping the plan's order within a rank; a line that lists several issues takes its highest issue's rank. Move a line that breaks this order, unless a person asked for a different order of that work: then cite that request beside the line (a request to do the work is no request about its order). Name each move in your turn's summary.
3. For every plan line whose key has this project's prefix (`daemon.project`), make the board order match the line order exactly, from the plan alone: `dispatch issue-update --issue <KEY> --before <OTHER>` (or `--after`). Set priority P0 only where the project owner's own words, cited on the issue, ask for it, and never from a plan line alone: a P0 issue starts at once, outside the admission cap. Set any other priority a line states; leave it where it states none. Change the P0 bound (`set_p0_bound`, `clear_p0_bound`) only on a person's word, cited in the change's reason.
4. Reconcile the one-owner map. A `live` or `unresponsive` owner in `envoy_sessions` still owns its work: change nothing. Only an owner that reads `gone`, or that the registry has no record of, has no session. Route such unowned work to a session whose `dir` is a checkout of the issue's repository (`dispatch issue-update --issue <KEY> --route session:<session id>`) and `envoy_send` it `You own <KEY>: <title>.`. Never route an issue that carries the `legion` label, and never change its `Assignee:`, which decides who receives asks.
5. Edit that line's owner in the artifact with `dispatch doc-edit`, guarded by the token of the current `dispatch doc-read`, replacing superseded text rather than logging changes.

A directive is a direct Dispatch message to you: answer it, carry it out, record the rule it sets in its weekly-plan row (or the directives section) with the message's link, then run the daily form of the prioritization pass.

## Answering controller deliveries

Read every delivery against live Dispatch and daemon state. You decide labels, `todo`, `backlog`, `icebox` and board order. Answer a scope question from the weekly plan and the deployment instructions. Answer a routing request with the owner the one-owner map names and how to reach it (its role token or session), so the asker writes to it: you forward no message to another session. Never merge or enqueue a pull request, whatever credential you hold: a request to merge one goes to the architect of the tree that owns it (`skill://legion-controller/references/operations.md#a-request-to-another-session`).

## The owner audit

At each pass, read the open roots, the same read as the pass's step 1: `dispatch issues --project <KEY> --all`, each row that names no parent (no ` · child of <KEY>` outside quotes) and whose status is not `done`. In that read:

- An unclaimed row in `triage`, `icebox`, `backlog` or `todo` is a decision: someone builds it, or it closes.
- A close that says a defect cannot happen cites the code that prevents it, or the issue is unread.
- A plan or measurement nobody builds needs an owner.
- A merged pull request names an issue not yet `testing` or `done`: check it live, then move it.
- Someone repeats the issue's work by hand: fix its status.
- A route nobody holds, after the two reads below: staff the role, route to a live holder, or clear the route and assign it.

## Recovering stopped-session work

A session route is stopped only after two reads of `dispatch issues --project <KEY> --route-status no_holder --all`, at least ten minutes apart, on two wakes, both list it. Then read its owner in `envoy_sessions`: recover the work only from one that reads `gone`, or `unresponsive` with a `last_seen` more than 20 minutes old.

Only work that a person's directive, recorded in the plan with its link, moves to Legion when its session stops is yours to move: keep its labels, add `legion`, then `set_status` it to `todo`, and update its owner row. Never apply this to an issue in a live Legion tree.
