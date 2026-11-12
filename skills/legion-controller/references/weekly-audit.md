# The weekly audit

Read this on your first turn of each UTC week, whatever woke you, and run the three passes below,
and sum the fourth, before you post the day's report. They are the week's own numbers, beside the
daily ones.

## The week

The week's boundary is your first turn on or after Monday 00:00 UTC. The window is the seven days
before that boundary: `date -u +%Y-%m-%dT%H:%M:%SZ` prints the time now, and the window runs from
seven days before it to it. Name the window's two times in the report, so a reader can recount.

You already ran this week when the report issue carries a message of yours from on or after the
boundary whose text names the window. Read the report issue before you start
(`skill://legion-controller/references/daily-report.md`).

## 1. The week's forced creates

A create that went past Dispatch's near-copy refusal with `--force` carries `forced` on its
`issue.created` event, with `past` naming the issues the refusal named. A forced create the
refusal would not have stopped carries nothing, so this is a count of overrides, not of uses of
`--force`. A `past` that is the empty list is a create whose check did not answer: report those
apart, since nobody forced past anything there. Each form reads as itself on the `issue.created`
line: an override ends `forced past <keys>`, and a create whose check did not answer ends
`forced, the near-copy check did not answer`.

```bash
dispatch issues --project <PROJECT> --all
dispatch read --issue <KEY> --all-events   # read the issue.created line
```

`--all-events` is not optional: a bare `dispatch read` prints the last ten events alone, and on
any issue with more than ten the `issue.created` line is not printed at all.

Keep the issues created inside the window, read each one's `issue.created` line, and count by
filer. The line names its writer as `session <id> (<the session's title>)`, and the filer is that
title cut at its last middle dot (`·`), so `Legion implementer · INFRA-1160` counts as
`Legion implementer`, with every person counted together as one filer. A session that carries no
title prints its id alone and is counted by that id. Report, per filer, how many creates forced
past a refusal and which issues each named, then the no-answer creates as their own line.

The recount is the same two commands: name them in the report beside the number.

## 2. The week's tree sizes

A Legion tree is an open issue with the `legion` label and no ancestor carrying it, with its
descendants. A tree closed inside the window counts at its close.

```bash
dispatch issues --project <PROJECT> --label legion --all
dispatch read --issue <KEY>                          # Referenced by: child_of names its children
dispatch issues --project <PROJECT> --parent <KEY>   # the same children, one level at a time
```

`Links: child_of` on that read gives the issue's own parent, never its children, so walk down
instead: read the root, take each `child_of` row under `Referenced by:` as a child, and read each
child the same way until a read names none. `dispatch issues --parent` lists one level in one
request and is the shorter walk on a wide tree.

Count each tree's descendants at any depth, and report the trees in five buckets: 0, 1-2, 3-5,
6-10, and 11 or more. Count the parked descendants of each bucket too: a descendant in `backlog`
or `icebox`. Name the recount commands beside the numbers, since a reader checks them against
Dispatch's own parent links.

The report and, when you lead a stream that keeps a scoreboard, the stream's scoreboard artifact
both carry the two measures, under the keys `forced_creates_by_filer` and `tree_sizes`.

## 3. The nudges

Ask the architect of each tree the counts name to fold what can ride in an open pull request.
Both rules below count only a tree with at least one open descendant, a descendant whose status
is not `done`. A tree whose descendants are all closed has nothing left to fold, and a nudge
sent to one asks its architect to fold work that is already finished.

- a tree with at least one open descendant, and more than 8 descendants in all or more than 2
  levels below its root;
- a tree with at least one open descendant that filed a child no red-team verdict names. The
  verdict is a comment on the root whose first line reads `Red-team verdict for <root key>:`
  (`skill://legion-architect/references/decomposition.md`). Read the root's comments with
  `dispatch read --issue <ROOT> --all-events` and the replies under each verdict comment; a child
  named in none of them was filed without the check.

  Count no child created before that first line existed. The architects write it from the Legion
  release that carries this file, so every child filed before then has no verdict comment to
  find and is not evidence of a skipped check: read the release's own publication time from its
  tag and leave out every child created at or before it. Without that, the first run of this
  pass nudges nearly every tree that has children. Until a release carries this file, there is
  no such tag to read: when `git tag --contains <the commit that added this file>` names no
  release, the cutoff is the time of your run, so every child that exists counts as filed
  before the rule and no tree is nudged for a missing verdict yet.

Each nudge is one `dispatch comment` on the tree's root, naming what you counted and asking the
architect to fold what can ride in an open pull request of the tree. End the comment with
`descendants: <n>, levels: <m>`, the counts you sent it at. When the daemon's state names an
architect for the tree (`read_state`'s `issues.<KEY>`), send the same text to that architect's
role topic with `envoy_publish`.

Send no new comment to a tree whose architect replied in the thread of your newest nudge comment
there, until the tree's descendants pass the number that comment ends with. A tree whose children
were all filed before the verdict's first line existed has no such comment and is nudged once;
the architect's reply then settles it until the tree grows.

List every nudge you sent in the day's report.

## 4. The duplicate audit

The consolidation pass runs it every six hours
(`skill://legion-controller/references/consolidation.md`). The week's report sums the counts of
the week's `Consolidation pass` messages. It serves R8 of dispatch://INFRA-1160.
