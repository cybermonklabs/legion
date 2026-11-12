# The consolidation pass

Read this on your first turn of each six-hour UTC block, whatever woke you, and run both steps
below before anything else that turn. Finding issues that are the same work, or that fail for one
cause, and folding them is this pass's job, so that a person never has to ask for it.

## The block

The blocks start at 00:00, 06:00, 12:00 and 18:00 UTC: `date -u +%Y-%m-%dT%H:%M:%SZ` prints the
time now, and the current block started at the latest of those times at or before it. You already
ran this block's pass when the report issue carries a message of yours from on or after the
block's start whose first line is `Consolidation pass <block start>`. Read the report issue before
you start (`skill://legion-controller/references/daily-report.md`).

## 1. Duplicates

For each open issue of the project, read the related items of its title and spec through the
no-write related route, leaving the issue, its ancestors and its descendants out, and judge
whether each pair the two issues' own records do not already settle is the same work.

The project holds hundreds of open issues and one related read takes about a second, so a serial
pass is ten minutes of HTTP before any judging. Shard the open list and dispatch one full agent per
shard of at most 50 issues, `task(agent="task")` or `task(agent="deep-worker")`, which can run
commands and read Dispatch, GitHub and the workspace, then `wait` for them: each returns the pairs
it found with the records it read for each, and you check each finding against those records
before you fold it. A read-only `scout` cannot look up what a finding rests on, so a shard a scout
read does not count: dispatch it again to a full agent.

Give each agent the shard's keys and these commands:

```bash
dispatch read --issue <KEY>                    # title, spec artifact, Links:, Referenced by:
dispatch doc-read --issue <KEY>                # the spec text the related read uses
dispatch related --project <PROJECT> --title '<its title>' --text-file - \
  --exclude-issue <KEY> --exclude-issue <each ancestor> --exclude-issue <each descendant>
```

`--exclude-issue` is repeatable and takes up to 50 issues; the issue itself, its ancestors and its
descendants all go in one call. The read names both: a `child_of` row under `Links:` is the
issue's own parent, read again on that parent for the next ancestor up, and each `child_of` row
under `Referenced by:` is a child, read again on each child for the next level down. Ask each
agent for, per issue, every related item that is an issue with the reason it looks like the same
work and the records that show it, and nothing else: no comment, no close, no message.

A pair is settled, and is skipped rather than proposed, when the two issues already say so: one
was closed as a duplicate of the other, one was reopened after such a close, or one was made a
child of the other. Read the first two from `dispatch read --issue <KEY> --all-events`'s
`Events:`: the close's reason is the `message.created` line just before `issue.closed`, and a
reopen is an `issue.updated` line that moves the status off `done`. No `Events:` line reports a
parent change, so read the third from the same read's `Links:` and `Referenced by:`.

Fold each pair you judge the same work by the duplicate rule of `skill://dispatch-first`: keep the
issue with the spec, copy to it what only the other has, and close the other
(`--status done --reason 'Duplicate of dispatch://KEY.'`). An issue carrying the `legion` label,
or another holder's claim, is not yours to close: comment the kept issue's name on it instead. A
pair inside a Legion tree goes to that tree's architect with `envoy_publish`, which decides it.

The report lists every pair you judged the same work, with what you did about it, and every pair
you skipped with what settles it: the event, or the parent link.

## 2. Recurring causes

Ask each agent of step 1 also for the groups of issues in its shard opened or changed in the last
24 hours that fail for one cause: the same broken tool, check, rule or component, shown in each
issue by evidence they share (an error, a file, a command), with that evidence. Judge each group
yourself, and keep only a group of three or more. A group that one fix clears entirely is a set of
duplicates: fold it as step 1 says. Keep for this step only a group whose issues each need their
own change beyond the shared cause.

For each group you keep, search Dispatch for an issue that already owns the cause
(`dispatch search --query '<the cause in two or three words>'`). When one does, comment its key on
each issue of the group. When none does, file one issue for the cause, naming each issue of the
group and the shared evidence, with a `Filed for` line naming
dispatch://INFRA-460/comment/67cf2b86-5c76-41a9-a53a-7bf2dce03602, the owner's request for this
pass; then comment its key on each issue of the group. Close no issue of the group: each needs its
own change as well. A group inside one Legion tree goes to that tree's architect with
`envoy_publish` instead, which decides it.

## The record

Post one message on the report issue whose first line is `Consolidation pass <block start>`, then
three counts, each with its keys: the open issues read; the pairs folded (closed, commented or sent
to an architect); and the causes rooted (issues filed or named for a recurring cause). Then every
pair and group you skipped, with what settles it.
