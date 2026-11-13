# Daily report

Read this on your first turn of each UTC day, whatever woke you, and when you open, follow or
answer a finding ask: the report issue holds them all.

## Find the report issue

Use the issue the deployment instructions name for Legion's reports. With none named, use the
project's `Legion daily report` issue:
`dispatch search --query '"Legion daily report"' --project <PROJECT>`. When it does not exist,
create it once (the one issue you file unasked) and park it in `icebox`.
The report issue holds no work, which is why it stays in `icebox`, and it is the one issue you
park with neither a person's request nor the end of its work behind it:
you move every other issue to `backlog` or `icebox` only when a person asked for that move, or when the work is done, folded into another issue or no longer needed, and cite that reason in a comment on the issue.
It never carries the `legion` label.
Assign it to the person who answers for the project: the one the deployment instructions name,
else the owner `dispatch whoami` names, because its asks reach that person's Inbox.

```text
dispatch issue --project <PROJECT> --title 'Legion daily report' --assignee '<email>' --spec-file - <<'EOF'
## Summary

Legion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. Legion's agents also post here the evidence of each clear defect they found outside their own work, which the controller hands to the tree that owns the code, or files for Legion when no tree does. They ask here whether a problem that needs a decision needs an issue. This issue is not work, so it carries no `legion` label and stays in icebox.
EOF
legion({ op: "set_status", issue: "<report KEY>", status: "icebox" })
```

With no person known, leave out `--assignee`: the issue then goes to your token's owner, or, under
the shared token, to the project's default assignee when a person set one, and is otherwise
unassigned.

## Keep it answerable

Each time you read it:

- `Status: done` means someone closed it: the close withdrew its open asks (each recorded
  `withdrawn`; a reopen brings them back), and Dispatch refuses an ask there
  (`409 ISSUE_CLOSED`). Reopen it with `set_status` to `icebox`, where the issue holds no work
  and its asks stay answerable, and say so in the day's report.
- `Assignee: unassigned` with a person known: assign it, naming your session (`dispatch whoami`):
  `curl -sS -X PATCH -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" -H 'Content-Type: application/json' -d '{"assignee": "<email>", "actor": {"kind": "session", "id": "<session id>"}}' "$DISPATCH_URL/api/v1/issues/<REPORT KEY>"`.
  An answer that does not print the issue with its new `assignee` left the assignee as it was
  (`400 ASSIGNEE_NOT_ALLOWED` names an email nobody has signed in to Dispatch with): name it, with
  its first line, in your summary.
- While `Assignee:` says `unassigned`, open each report with one sentence asking a person to take
  the issue from its header, because its asks reach only the Inbox's `Unassigned` filter.

## When

After the turn's own work, read the report issue with `dispatch read`, keep it answerable, and
post when its `Events:` show no `message.created` from today.

## What

One `dispatch message --issue <report KEY> --body-file -` of at most 2,000 characters, because a
person reads every report in their Inbox. Write it as `skill://dispatch-writing-specs`'s "Rules for the text" say:
every issue by its `dispatch://` link and what it does for the reader, every pull request by its URL.

- **Finished.** `dispatch issues --project <PROJECT> --status done --updated-since <the previous
  report's time, or 24 hours ago> --all`, keeping the issues `read_state` records
  under `issues` whose `issue.closed` event is after the previous report. `dispatch read` each: the
  pull request under `External links:` is the one that merged.
- **Closed without a change.** Those with no pull request, each with the reason its closing
  message gave (the `message.created` just before `issue.closed` among `Events:`).
- **Running.** Each root in `admission.trees`, the live trees, with its `issues.<KEY>.phase`, and
  under it each issue of that tree in `admission.active`, the slot holders, with its own phase;
  then the issues in `admission.waiting`. Name as waiting on a claim each root that this turn's
  "Trees waiting on a root claim" check found, with the holder its `Claimed by:` line names.
- **Waiting on Dispatch links.** Each root and child in `admission.blocked`, with what it waits
  on: the issues of its `blocked_by` links, or the open children children-first holds it for. A
  link's wait ends when each blocker reaches `done` (a person's close, or the blocker's own
  merge) or its link is taken off (by a person or the tree's architect), and a children-first wait
  ends when the last open child leaves the workflow, so take nothing for them.
- **Phase workers nothing will prompt.** Each entry of the tick's `silent` list, with the role whose
  claim is silent and how long it has been quiet (`skill://legion-controller/references/wakes.md`).
  That list arrives on the tick wake alone and is in no `read_state` field, so the report names the
  most recent tick's list and says nothing of them on a day no tick carried one.
  An entry with `backgroundSince` is a worker whose own background work has stood past `worker_background_hours`,
  which the daemon does not prompt.
  Name it with how long that work has stood, counted from `backgroundSince`.
- **Waiting on a person.** Each tree whose next step waits on a person's answer, with that ask's
  `dispatch://` reference, while the ask is open. Each row of the weekly plan that moved nothing,
  because no person's request stands behind it, with its deferral ask's `dispatch://` reference,
  while that ask is open. Leave out a row whose ask is resolved with no answer, because the next
  bullet names it. Leave out a row whose root fails the check of step 2 of
  `skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers`.
- **Asks resolved with no answer.** For each issue in `admission.active` or `admission.waiting`
  that is the root of its tree (`issues.<KEY>.tree` is `<KEY>`), read its `Asked whether to park:`
  and `Asked whether to defer:` comments with step 1's command of
  `skill://legion-controller/references/wakes.md#ask-whether-to-park-a-tree`, once with each
  prefix. Take the newest comment of each key (the comment's second line). When the ask that its
  first line names is resolved with no answer (`dispatch read --ref '<that reference>'`), name the
  root, the key and that ask's `dispatch://` reference. Leave out a key whose wait has ended: a
  `held` or `architect failed:` key that `read_state` no longer builds (step 4 of that section),
  an `event:` key whose event has happened, and a plan row that the weekly plan no longer defers.
- **Free slots.** Counted as the skill's "Free slots" says, in issues rather than trees; each
  stays free until an issue in the line takes it.
- **Open capabilities.** Each row of `read_state`'s `capabilities` whose `status` is `open`: its
  name, its `detail`, and a deployment row's `configLine`. Say who acts on it, as
  `skill://legion-controller/references/wakes.md` says for `tick`. With none open, say nothing of
  them.
- **The week** (on the report of your first turn of each UTC week only, from
  `skill://legion-controller/references/weekly-audit.md`): the window's two times; the forced
  creates by filer with the no-answer creates apart; the tree sizes in the five buckets with the
  parked descendants; each nudge you sent, by its tree; and the consolidation passes' three counts
  summed over the week, with the link of the report issue's `consolidation.md` artifact, which
  lists each pair and group with what was done about it or what settles it. Name beside each
  number the command that recounts it. When you lead a stream that keeps a scoreboard, the same
  two measures go on it under `forced_creates_by_filer` and `tree_sizes`.

A day with nothing finished says so in one sentence. When the lists do not fit, give each list's
count and name its issues of `--priority 0` and `--priority 1`, in board rank order.
