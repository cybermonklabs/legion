# The consolidation pass

Read this on your first turn of each six-hour UTC block, whatever woke you, and run both steps
below once the wake's work is done, at step 3 of the skill's every-turn steps. Finding issues that
are the same work, or that fail for one cause, and folding them is this pass's job, so that a
person never has to ask for it.

## The block

The blocks start at 00:00, 06:00, 12:00 and 18:00 UTC: `date -u +%Y-%m-%dT%H:%M:%SZ` prints the
time now, and the current block started at the latest of those times at or before it. Read the
report issue (`skill://legion-controller/references/daily-report.md`), then its pass record:

```bash
dispatch doc-read --issue <REPORT KEY> --artifact consolidation.md
```

This block's pass has run when the record has a section headed `## Consolidation pass <block
start>`, whichever controller session wrote it, so a controller that took over from another does
not run the block again. A `not found` answer means no pass has run yet.

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

The record (below) lists every pair you judged the same work, with what you did about it, and
every pair you skipped with what settles it: the event, or the parent link.

## 2. Recurring causes

This step reads every issue changed in the last 24 hours, whatever its status, because a recently
closed issue or a failed attempt can show a cause too. Ask each agent of step 1 also about the
issues of its shard changed in that time, and put the issues closed in that time, which step 1
does not read, in more shards of at most 50:

```bash
dispatch issues --project <PROJECT> --status done --updated-since "$(date -u -d '24 hours ago' +%Y-%m-%dT%H:%M:%SZ)" --all
```

Ask each agent, for each of those issues that fails, what it fails on: the broken tool, check,
rule or component, the evidence in the issue that shows it (an error, a file, a command), and the
records it read. Ask for no groups, because issues that fail for one cause can fall in different
shards. Gather every shard's answers, check each against its records, and group the issues that
fail for one cause yourself; keep a group of three or more. A group that one fix clears entirely
is a set of duplicates: fold it as step 1 says. Keep for this step only a group whose issues each
need their own change beyond the shared cause.

Root each cause you keep once, whichever trees its issues sit in, as
`skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect` says. Post its
evidence comment on the report issue first, its first line `Clear defect from controller`, naming
the cause, each issue of the group and the shared evidence; catch-up step 4 of that reference
brings it back at a later start until it carries a reply. Then take its steps 2 to 5: comment on
the open issue that already tracks the cause, else hand it to the running tree whose code it is,
else file one issue for it, and reply under the evidence comment. The filed issue names each issue
of the group and the shared evidence, and carries the `Filed for` line of `finding-asks.md` for
the person who asked for this pass: `Filed for <them> at their request:
dispatch://INFRA-460/comment/67cf2b86-5c76-41a9-a53a-7bf2dce03602`. It takes the `legion` label
only when the fix needs no choice a person must make (that procedure's step 1); otherwise it waits
in triage for a person, and the reply is still `Filed dispatch://<KEY>`. Then comment on each
issue of the group the cause's issue, or the root of the tree it went to. Close no issue of the
group: each needs its own change as well.

## The record

Record the pass in the report issue's `consolidation.md` artifact, not in a message: a message on
the report issue reaches a person's Inbox, and the day's report posts only while the issue holds
no message from that day. Add at the top of the record you read, or of a new one, a section headed
`## Consolidation pass <block start>` with three counts, each with its keys: the issues read; the
pairs folded (closed, commented or sent to an architect); and the causes rooted (issues commented,
handed or filed for a recurring cause). Then every pair and group you skipped, with what settles
it. Drop sections older than eight days, which the artifact's earlier versions keep, and upload
the whole record, which makes its next version:

```bash
dispatch artifact --issue <REPORT KEY> --name consolidation.md --path <the record file>
```
