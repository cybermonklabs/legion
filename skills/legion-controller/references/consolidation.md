# The consolidation pass

Run it on your first turn of each six-hour UTC block, after the wake's work. It finds issues that are the same work, or that fail for one cause, and folds them, so that no person has to ask.

## The block

Blocks start at 00:00, 06:00, 12:00 and 18:00 UTC. Read the report issue's record, `dispatch doc-read --issue <REPORT KEY> --artifact consolidation.md`. A section `## Consolidation pass <block start>`, by any controller session, means that this block ran. `not found` means that no pass has run yet.

## 1. Duplicates

Shard the open issues into groups of at most 50. Dispatch one `task(agent="deep-worker")` per shard, and `wait` for them. A read-only agent cannot check what a finding rests on, so a shard that one read does not count. Give each agent its keys and these commands. Ask it for each issue's related issues, with the reason and the records that show it, and for nothing else (no comment, no close, no message):

```bash
dispatch read --issue <KEY>
dispatch doc-read --issue <KEY>
dispatch related --project <PROJECT> --title '<its title>' --text-file - \
  --exclude-issue <KEY> --exclude-issue <each ancestor> --exclude-issue <each descendant>
```

Skip a pair that the issues already settle: one closed as a duplicate of the other, one reopened after such a close (both in `dispatch read --issue <KEY> --all-events`), or one a child of the other (`Links:`, `Referenced by:`). Check each finding against its records. Then fold each pair that is the same work by the duplicate rule of `skill://dispatch-first`. An issue with the `legion` label, or with another holder's claim, gets a comment that names the kept issue instead of a close. Send a pair inside a Legion tree to that tree's architect with `envoy_publish`.

## 2. Recurring causes

Ask each agent also, for each issue of its shard that changed in the last 24 hours and fails, what it fails on (the broken tool, check, rule or component), its evidence, and the records read. Shard the issues closed in that time too (`dispatch issues --project <PROJECT> --status done --updated-since "$(date -u -d '24 hours ago' +%Y-%m-%dT%H:%M:%SZ)" --all`). Group the issues that fail for one cause yourself. Keep a group of three or more whose issues each need their own change. A group that one fix clears is a set of duplicates.

Root each cause once, as `skill://legion-controller/references/finding-asks.md#hand-over-a-clear-defect` says, with `controller` as the role. Comment the result on each issue of the group, and close none of them.

## The record

Add at the top of the record a section `## Consolidation pass <block start>` with three counts and their keys: issues read, pairs folded, causes rooted. Then list every pair and group that you skipped, with what settles it. Drop sections older than eight days. Upload the whole record: `dispatch artifact --issue <REPORT KEY> --name consolidation.md --path <the record file> --force`. Never post the pass as a message, because a message reaches a person's Inbox.
