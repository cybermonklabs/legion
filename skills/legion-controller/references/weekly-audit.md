# The weekly audit

Run it on your first turn of each UTC week, before that day's report, which carries its numbers. Put each number beside the command that recounts it.

## The week

The window is the seven days before your first turn on or after Monday 00:00 UTC. Name its two times in the report. The audit ran already when the report issue holds a message of yours, since that boundary, that names the window.

## 1. The week's forced creates

A create that went past Dispatch's near-copy refusal with `--force` ends its `issue.created` line `forced past <keys>`. A create whose check did not answer ends `forced, the near-copy check did not answer`. Read each issue created in the window with `dispatch read --issue <KEY> --all-events` (a bare read prints only the last ten events).

Count by filer: the title of the line's `session <id> (<title>)`, cut at its last `·`. People count together as one filer, and an untitled session counts by its id. Per filer, give the overrides with the issues each named, then the no-answer creates apart. The report key is `forced_creates_by_filer`.

## 2. The week's tree sizes

A tree is an open `legion`-labelled issue with no labelled ancestor, with its descendants. A tree closed in the window counts at its close. Walk down from each root with `dispatch issues --project <PROJECT> --parent <KEY> --all`, level by level. Report the trees by descendant count at any depth in five buckets, 0, 1-2, 3-5, 6-10 and 11 or more, with each bucket's parked (`backlog`, `icebox`) descendants. The report key is `tree_sizes`.

## 3. The nudges

Nudge the architect of each tree that has at least one open descendant and more than 8 descendants or more than 2 levels below its root.

A nudge is one `dispatch comment` on the root that mentions the tree's architect (`--mention 'role:<token>'`, the token that `skill://legion-controller/references/operations.md#a-request-to-another-session` names). It names what you counted, asks the architect to fold what can ride in an open pull request, and ends `descendants: <n>, levels: <m>`. Send the architect nothing else. Send no new nudge to a tree whose architect replied to your newest one until its descendants pass that `<n>`. List each nudge in the report.

## 4. The duplicate audit

Sum the three counts of the week's sections of the report issue's `consolidation.md` record (`skill://legion-controller/references/consolidation.md`), with the artifact's link.
