# The consolidation pass

Read this on your first turn of each six-hour UTC block, and run it after the wake's work. Its job is to find issues that are the same work, or that fail for one cause, and fold them, so no person has to ask.

## The block

Blocks start at 00:00, 06:00, 12:00 and 18:00 UTC (`date -u +%Y-%m-%dT%H:%M:%SZ`). Read the report issue's record, `dispatch doc-read --issue <REPORT KEY> --artifact consolidation.md`: a section `## Consolidation pass <block start>`, by any controller session, means this block ran. `not found` means no pass has run yet.

## 1. Duplicates

Shard the open issues into groups of at most 50, dispatch one `task(agent="deep-worker")` per shard, and `wait` for them; a read-only agent cannot check what a finding rests on, so a shard it read does not count. Give each agent its keys and these commands, and ask for each issue's related issues with the reason and the records that show it, and nothing else (no comment, no close, no message):

```bash
dispatch read --issue <KEY>
dispatch doc-read --issue <KEY>
dispatch related --project <PROJECT> --title '<its title>' --text-file - \
  --exclude-issue <KEY> --exclude-issue <each ancestor> --exclude-issue <each descendant>
```

Skip a pair the issues already settle: one closed as a duplicate of the other, one reopened after such a close (both in `dispatch read --issue <KEY> --all-events`), or one a child of the other (`Links:`, `Referenced by:`). Check each finding against its records, then fold each pair that is the same work by the duplicate rule of `skill://dispatch-first`. An issue with the `legion` label, or another holder's claim, gets a comment naming the kept issue instead of a close; a pair inside a Legion tree goes to that tree's architect with `envoy_publish`.

## 2. Recurring causes

Ask each agent also, for each issue of its shard changed in the last 24 hours that fails, what it fails on (the broken tool, check, rule or component), its evidence, and the records read. Shard the issues closed in that time too (`dispatch issues --project <PROJECT> --status done --updated-since "$(date -u -d '24 hours ago' +%Y-%m-%dT%H:%M:%SZ)" --all`). Group the issues that fail for one cause yourself, and keep a group of three or more whose issues each need their own change; a group one fix clears is a set of duplicates.

Root each cause once, as `skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect` says, with `controller` as the role: post its evidence comment on the report issue, then comment on the open issue that tracks it, hand it over, or file it, and reply under the evidence. Comment the result on each issue of the group, and close none of them.

## The record

Add at the top of the record a section `## Consolidation pass <block start>` with three counts and their keys: issues read, pairs folded, causes rooted; then every pair and group you skipped, with what settles it. Drop sections older than eight days, and upload the whole record: `dispatch artifact --issue <REPORT KEY> --name consolidation.md --path <the record file> --force`. Never post the pass as a message: that reaches a person's Inbox.
