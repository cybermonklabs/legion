# The weekly audit

Read this on your first turn of each UTC week, and run it before that day's report, which carries its numbers. Each number goes beside the command that recounts it.

## The week

The window is the seven days before your first turn on or after Monday 00:00 UTC (`date -u +%Y-%m-%dT%H:%M:%SZ`); name its two times in the report. It ran already when the report issue holds a message of yours since that boundary that names the window.

## 1. The week's forced creates

A create that went past Dispatch's near-copy refusal with `--force` ends its `issue.created` line `forced past <keys>`; one whose check did not answer ends `forced, the near-copy check did not answer`. Read the issues created in the window, each line from `dispatch read --issue <KEY> --all-events` (a bare read prints only the last ten events):

```bash
dispatch issues --project <PROJECT> --all
dispatch read --issue <KEY> --all-events
```

Count by filer, the line's `session <id> (<title>)` title cut at its last `·` (people count together as one filer, an untitled session by its id): per filer, the overrides with the issues each named, then the no-answer creates apart. The report key is `forced_creates_by_filer`.

## 2. The week's tree sizes

A tree is an open `legion`-labelled issue with no labelled ancestor, with its descendants; one closed in the window counts at its close. Walk down from each root with `dispatch issues --project <PROJECT> --parent <KEY> --all`, level by level (`Links: child_of` names the parent, never a child). Report the trees by descendant count at any depth in five buckets, 0, 1-2, 3-5, 6-10 and 11 or more, with each bucket's parked (`backlog`, `icebox`) descendants. The report key is `tree_sizes`.

## 3. The nudges

Nudge the architect of each tree with at least one open descendant that has more than 8 descendants or more than 2 levels below its root.

A nudge is one `dispatch comment` on the root naming what you counted and asking the architect to fold what can ride in an open pull request, ending `descendants: <n>, levels: <m>`; send the same text to the architect's role topic when `read_state` names one. Send no new nudge to a tree whose architect replied to your newest one until its descendants pass that `<n>`. List each nudge in the report.

## 4. The duplicate audit

Sum the three counts of the week's sections of the report issue's `consolidation.md` (`skill://legion-controller/references/consolidation.md`), with the artifact's link.
