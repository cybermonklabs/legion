# The consolidation pass

Read this on your first turn of each six-hour UTC block, and run it after the wake's work. Its job is to find issues that are the same work, or that fail for one cause, and fold them, so no person has to ask.

## The block

Blocks start at 00:00, 06:00, 12:00 and 18:00 UTC (`date -u +%Y-%m-%dT%H:%M:%SZ`). Find the report issue as `skill://legion-controller/references/daily-report.md` says, then read its record, `dispatch doc-read --issue <REPORT KEY> --artifact consolidation.md`: a section `## Consolidation pass <block start>`, by any controller session, means this block ran. `not found` means no pass has run yet.

## 1. Duplicates

Shard the open issues into groups of at most 50, dispatch one `task(agent="deep-worker")` per shard, and `wait` for them; a read-only agent cannot check what a finding rests on, so a shard it read does not count: dispatch it again to a full agent. Give each agent its keys and these commands, and ask for each issue's related issues with the reason and the records that show it, and nothing else (no comment, no close, no message):

```bash
dispatch read --issue <KEY>
dispatch doc-read --issue <KEY>
dispatch related --project <PROJECT> --title '<its title>' --text-file - \
  --exclude-issue <KEY> --exclude-issue <each ancestor> --exclude-issue <each descendant>
```

`--exclude-issue` is repeatable and takes up to 50 issues; the issue itself, its ancestors and its descendants all go in one call. The read names both: a `child_of` row under `Links:` is the issue's own parent, read again on that parent for the next ancestor up, and each `child_of` row under `Referenced by:` is a child, read again on each child for the next level down.

Skip a pair the issues already settle: one closed as a duplicate of the other, one reopened after such a close, or one a child of the other. Read the first two from `dispatch read --issue <KEY> --all-events`'s `Events:`: the close's reason is the `message.created` line just before `issue.closed`, and a reopen is an `issue.updated` line that moves the status off `done`. No `Events:` line reports a parent change, so read the third from the same read's `Links:` and `Referenced by:`. Check each finding against its records, then fold each pair that is the same work by the duplicate rule of `skill://dispatch-first`. An issue with the `legion` label, or another holder's claim, gets a comment naming the kept issue instead of a close; a pair inside a Legion tree goes to that tree's architect with `envoy_publish`.

## 2. Recurring causes

Ask each agent also, for each issue of its shard changed in the last 24 hours whose records show it failing (an error, a failed check or command, a failed phase or attempt), what it fails on (the broken tool, check, rule or component), the evidence in the issue that shows it (an error, a file, a command), and the records read. Ask for no groups, because issues that fail for one cause can fall in different shards. Shard the issues closed in that time too (`dispatch issues --project <PROJECT> --status done --updated-since "$(date -u -d '24 hours ago' +%Y-%m-%dT%H:%M:%SZ)" --all`). Check each answer against its records, group the issues that fail for one cause yourself, and keep a group of three or more whose issues each need their own change; a group one fix clears is a set of duplicates: fold it as step 1 says.

Root each cause once, as `skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect` says, with `controller` as the role. First post its evidence comment on the report issue, its first line `Clear defect from controller`, naming the cause, each issue of the group and the shared evidence; catch-up step 4 of that reference brings it back at a later start until it carries a reply. Then take its steps 1 to 5: a cause whose fix needs a choice a person must make becomes a decision ask (step 1); any other is commented on the open issue that tracks it, handed to the tree whose code it is, or filed, with a reply under the evidence. A filed issue names each issue of the group and the shared evidence, and carries `Filed for Legion from controller on <REPORT KEY>: <the evidence comment's reference>`. Comment the result on each issue of the group, and close none of them.

## The record

Add at the top of the record a section `## Consolidation pass <block start>` with three counts and their keys: issues read; pairs folded, each with what you did (closed, commented or sent to an architect); causes rooted. Then every pair and group you skipped, with what settles it: the event, or the parent link. Drop sections older than eight days, and upload the whole record: `dispatch artifact --issue <REPORT KEY> --name consolidation.md --path <the record file>`, with `--force` on every pass after the first, since an upload of a name that exists is refused without it. Never post the pass as a message: that reaches a person's Inbox.
