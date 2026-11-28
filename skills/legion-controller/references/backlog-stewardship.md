# Backlog stewardship

The deployment instructions name the weekly-planning issue. Its newest `plan-week-*` artifact is the work order and the one-owner map, and Dispatch board rank is that order in force.

Claim no Envoy role but `legion-<project>-controller`: another ends your wake delivery.

## The prioritization pass

Run the pass at most once a turn, after the wake's work, as the last step before your summary, and before the daily report on the turn that posts it.

- It is due on every turn that a `tick`, `slot-free` or `todo` wake starts.
- Its daily form is due on your first turn of each UTC day and after every directive from a person, even one that seems to touch one issue.
- On any other turn it is due when the wake's work is done, nothing else waits, and `dispatch issues --project <KEY> --updated-since <your newest pass line's time> --all` lists an issue, or when your context holds no pass line.

1. Read the `read_state` summary's admission lines, the newest weekly plan, and the open roots, and make [the owner audit](#the-owner-audit) on that read.
2. Change only a rank, an admission or the plan, where it no longer matches the project's priorities. Admit an issue only as the intake your deployment instructions give and `skill://legion-controller/references/admission.md#your-moves-to-todo` allow: a free slot is no reason to admit. A plan line that defers a root the daemon queued or runs: `skill://legion-controller/references/deferral-asks.md`.
3. In the daily form, also [reconcile the weekly plan](#reconcile-the-weekly-plan).
4. End the pass with exactly one line: `Prioritization pass at <RFC3339>: <what it changed, or none>`.

## Reconcile the weekly plan

1. Read the weekly-planning issue and its newest `plan-week-*` artifact (by its latest `artifact.version` event) before you change any issue.
2. Rank the plan by the deployment's priorities (`dispatch priorities --project <KEY>`). An issue ranks at the highest priority its `Advances:` line names, its own or inherited, and below all when it advances none. A line that lists several issues ranks at its highest issue. Order the lines by rank, highest first, keeping the plan's order within a rank. Move a line that breaks this order, unless a person asked for another order of that work: then cite that request beside the line (a request to do the work is no request about its order). Name each move in your turn's summary.
3. For every plan line whose key has this project's prefix (`daemon.project`), make the board order match the line order exactly, from the plan alone: `dispatch issue-update --issue <KEY> --before <OTHER>` (or `--after`). An issue with no priority of its own takes that of the nearest issue above it that has one. Make an issue P0 only on the project owner's own words cited on it, never from a plan line alone (P0 skips the admission cap), and only with the `p0` credential your deployment instructions name. That holds for setting priority 0, a move under a P0 issue (`--parent`) or a clear (`--clear-priority`) that makes an issue P0, and a new issue with no priority under a P0 issue: Dispatch refuses each without that credential, `403 OAUTH_SCOPE_REQUIRED`. With none named, tell the owner to set P0 at a browser. Give a new issue under a P0 issue a priority from 1 to 3 unless those words make it P0. Set any other priority a line states, and leave it where it states none. Change the P0 bound (`set_p0_bound`, `clear_p0_bound`) only on a person's word, cited in its reason.
4. Reconcile the one-owner map. An owner that reads `live` or `unresponsive` in `envoy_sessions` keeps its work: change nothing. Only one that reads `gone`, or that the registry does not know, has no session. Route its work to a session whose `dir` is a checkout of the issue's repository (`dispatch issue-update --issue <KEY> --route session:<session id>`) and `envoy_send` it `You own <KEY>: <title>.`. Never route an issue labelled `legion`, and never change its `Assignee:`, which decides who receives asks.
5. Edit that line's owner in the artifact with `dispatch doc-edit`, guarded by the current `dispatch doc-read`'s token, replacing superseded text rather than logging changes.

A directive is a direct Dispatch message to you: answer it, carry it out, and record the rule it sets in its weekly-plan row (or the directives section) with the message's link. Then run the daily form of the pass.

## Answering controller deliveries

Read every delivery on your role topic against live Dispatch and daemon state. You decide labels, `todo`, `backlog`, `icebox` and board order. Answer a scope question from the weekly plan and the deployment instructions. Answer a routing request with the owner the one-owner map names and how to reach it (its role token or session), so the asker writes to it. Forward no message to another session. Never merge or enqueue a pull request, whatever credential you hold: a request to merge one goes to the architect of its tree (`skill://legion-controller/references/operations.md#a-request-to-another-session`).

## The owner audit

The pass's step 1 reads the open roots: the rows of `dispatch issues --project <KEY> --all` that name no parent (no ` · child of <KEY>` outside quotes) and are not `done`. In that read:

- An unclaimed row in `triage`, `icebox`, `backlog` or `todo` is a decision: someone builds it, or it closes.
- A close that says a defect cannot happen cites the code that prevents it, or the issue is unread.
- A plan or measurement nobody builds needs an owner.
- A merged pull request names an issue not yet `testing` or `done`: check it live, then move it.
- Someone repeats the issue's work by hand: fix its status.
- A route nobody holds, after the two reads below: staff the role, route to a live holder, or clear the route and assign it.

## Recovering stopped-session work

A session route is stopped only when two reads of `dispatch issues --project <KEY> --route-status no_holder --all` on two wakes, at least ten minutes apart, both list it. Then read its owner in `envoy_sessions`: recover the work only from one that reads `gone`, or `unresponsive` with a `last_seen` more than 20 minutes old.

Move only the work that a person's directive, recorded in the plan with its link, sends to Legion when its session stops: keep its labels, add `legion`, then `set_status` it to `todo`, and update its owner row. Never apply this to an issue in a live Legion tree.
