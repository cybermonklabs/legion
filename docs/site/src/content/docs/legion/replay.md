---
title: Replaying a recorded window
description: The developer's tool legion replay, which collects a recorded window of a Legion daemon, replays it under a slot rule you name, and reports what each result measures and assumes.
sidebar:
  order: 4.5
tableOfContents:
  maxHeadingLevel: 2
---

`legion replay` is a tool for a developer who changes how Legion gives out its admission slots.
It replays a recorded window of one project's daemon through the daemon's own admission pass, under a slot rule that you name, on a simulated clock.
It then reports what that rule costs: the issues finished, the started work that waits for a slot, the priority that holders take, and the other delays.
It also reads the same measures from the record of a window, with no replay.

The tool writes nothing to a running daemon, to Dispatch or to GitHub.
`collect` and `collect-merges` read Dispatch and GitHub with `GET` requests alone.
Each replay makes a database of its own on a Postgres server that you name, and drops it at the end.

This page uses the words of [Admission and the `legion` label](/legion/legion/concepts/#admission-and-the-legion-label).
The [CLI reference](/legion/legion/reference/cli/) gives each command and flag as its `--help` prints it.

## The words this page uses

- **Slot**: one of the cap's admission slots.
  An issue that the daemon started holds one in each phase that the slot rule keeps a slot in.

- **Holder**: an issue that holds a slot.

- **The line**: the waiting line, `admission.waiting` in `legion state --json`.
  Its returned part holds the issues whose work came back, and its starting part holds the issues that have not started.

- **Returned issue**: an issue of the returned part of the line.

- **Wait**: a started issue's wait on another issue, an unfinished Dispatch `blocked_by` link or an open child.

- **Slot rule**: the code that decides who holds a slot and how the line gives the slots out.
  It decides which phases keep a slot, which waits give it back, and whether a wait stops the phase under way.
  It also decides the order of the line, what a start may take from the cap, and what a head that conflicts with its base does.

- **Recorded window**: a span of one project's daemon, with the daemon's log lines and the Dispatch events of the project's issues.

- **The record**: what a recorded window shows that the daemon did, under the rule that it ran.

- **Branch**: one value of each knob that the record does not give, such as the time at which an architect takes a link off.

- **Work segment**: one visit of an issue to a phase, from the visit's worker start to the recorded move that ends it.
  The worker start is the later of the move into the phase and the slot that the issue takes for it.
  A visit that the admission pass ends (the design gate opening) ends at its last design registration, or with none at its worker's last turn's end.
  A visit that the issue's leave cuts ends at the leave, and a visit with no move out in the window stays open.

- **Stretch**: one visit of an issue to the phases from testing to merging, in which its head can conflict with its base.

- **Send-back loop**: a move back to an earlier phase, and the work until the issue enters the phase that it left again.

## What a recorded window is

A recorded window is a directory.
`legion replay collect` writes it, and every other command reads it.

| File | What it holds |
| --- | --- |
| `window.json` | The project, the span (`from`, `to`), the reach (`before`), the run-out (`after`, only where the window has one), the cap from each change on, the release, commit and rule of each part of the span, the deployment's settings, the kinds of line that the log holds, the collector's commit, and the sha256 of each input file. |
| `log.jsonl` | The daemon's lines in the reach, the span and the run-out, one JSON object each, as the daemon writes them on standard error: `time`, `level`, `msg` and the attributes. |
| `events/<KEY>.jsonl` | Each issue's Dispatch events, oldest first, as Dispatch's issue event log serves them. |
| `merges/<owner>__<name>.jsonl` | The first-parent commits of each base branch, which the conflict draw reads, each at GitHub's merge event or, where GitHub records none, at its committer date, with its committer date beside it. |
| `git/<owner>__<name>.jsonl` | Git's reading of the repository's pull requests: their heads, the paths that each head and each first-parent commit of the base branch change, and git's verdict of merging each head with each later commit of the base that changes one of its paths, with the count of the paths in conflict ([Add git's verdicts of each repository](#add-gits-verdicts-of-each-repository)). |
| `signals/provider.jsonl` | The deployment's record of the model provider, one line a minute. |
| `launcher/log.jsonl` | The launcher's lines of the project's issue pods, whose memory guard lines are the record of its kills. |
| `queue/<owner>__<name>.jsonl` | The merge queue's events of the repository's pull requests. |
| `settings/events.jsonl` | The project's settings events of Dispatch's event log. |

The last four files hold the records of R11's signals, and each is optional.
The git file of a repository is optional too: a window without one holds no verdict of git's.
A window that `collect` wrote while it still collected the pods' memory series also names `signals/memory.jsonl`.
No model reads that series, so a command checks the file's sha256 and reads nothing else of it.
`collect-merges`, `collect-queue` and `collect-git` leave it as they leave every other input, and `neutralize` drops it with its pod names.
`window.json` also holds, only where the window has them:

- `p0Bounds`: the P0 bound from each change on, `{at, bound}`, as the daemon's line of each change of the bound gives it, each a whole number from 1 to 2147483647.
  A replay writes the change in force at the window's start into its start state and applies each later change at its moment as the daemon's own change of the bound, which records it and makes a pass; a rule that starts a P0 issue outside the cap admits under the bound in force at each pass, none before the first change, as the daemon does.

- `defaultAssignee`: the project's default assignee in Dispatch at the start of the reach, as the deployment gave it.
  A pod's credential cannot read that setting, and a value that a migration filled has no event, so the deployment gives it where no settings event before the reach names one.

- `signals`: for each signal (`idle`, `queue`, `provider`, `kills`, `approvals`), the spans `{from, to}` in which the window holds a record of it, in time order.
  A window holds no record of a signal outside its spans, and a window that names no span of a signal, as every window collected before these records, holds no record of it at all.

- `missingAncestors`: each issue on the Dispatch parent chain of an issue that the window holds, up to 32 levels above it, whose events `collect` could not read, sorted.
  Dispatch answers that it has no such issue, as it answers for an issue that it does not show the reader.
  The priority that Dispatch resolves for an issue below such an ancestor may rest on it (R10).
  A window collected before `collect` read each ancestor names none, and holds the events of each issue's parent alone.

The tool reads these kinds of daemon line:

- the `admission:` lines of each slot given out or taken back, and of each wait

- the `workflow:` lines of each phase move, design gate, hold and handoff

- the `supervise:` lines of each launch, and of the start and the end of each turn

- the `pause:` lines of the project's pause and resume

- the lines that a daemon process writes as it starts

- the `workflow:` lines of a kill that an issue pod's launcher reported, which only the kill matcher of [Signal `kills`](#signal-kills) reads

- the `workflow:` line of each read of a pull request's merge state, with the head that GitHub answered for and the base commit that its answer names, which `collect-git` reads and the replay holds on its issue without deciding anything from it

A window collected before the daemon wrote that last line holds none of it, and the `lines` of its `window.json` do not name it.

From this release, the daemon also writes one `admission: a pass moved slots` line for each admission pass that gives out or takes back a slot.
That line gives the slots in use and the cap.
It also gives each slot given out or taken back, with its issue and reason, and each issue of the line that the pass went past, with its reason.
The tool reads that line where a window holds it, and builds the same slot facts from the other lines where it does not.

The reach is the span from `--from` less `--before` to `--from`.
No reading of a daemon's state exists for a past moment, so the tool rebuilds the state at `--from` from the lines and events of the reach.
Each run gives the number of issues that it could not place at `--from`, and names each one with the reason.

The run-out is the span from `--to` to `--to` plus `--after`.
A replay runs on through the run-out, so the work that the record shows after `--to` runs where the replay moves it.
Every measure reads only the span from `--from` to `--to`.
A window that names no `after` has no run-out.

Every command reads a window only if each input file has the sha256 that `window.json` gives for it.
A window that holds an input file that `window.json` does not name is refused too.
So is a window whose `signals` name a signal that is none of the five, an empty span, spans of one signal out of order or overlapping, or a record of the kills, the provider or the queue with no file of that record.

### The records of the signals

Each line of each file is one JSON object.

- `signals/provider.jsonl`: `{minute, uncached, limit, limit_source, limit_scope, own, provider}`, exactly as the deployment's provider-record script writes it, one line for each minute from `minute`.
  `uncached` is the project's uncached input tokens in the minute, kept as written.
  It can be below zero in a minute where the provider's series counts the cached tokens a minute after their input; the model that reads the record decides what to do with such a minute.
  `limit` is the limit in force: the latest value observed at or before the minute, and `null` before the first observation, where the provider's signal has no record.
  `limit_source` names where that value was observed, as the script names it: a refusal's line in the deployment's log, or a change recorded in Dispatch (`dispatch`).
  `limit_scope` names what the limit covers, as the script writes it: `Claude` for the project's limit over all its Claude models, a model's id for that model's own limit, and `null` where the observation names no one scope.
  `own` counts the refusals that the project's own limit gave in the minute, and `provider` those that the provider gave.

- `launcher/log.jsonl`: `{time, pod, text}`, each line of the launcher of one of the project's issue pods, its text verbatim.
  A line whose text starts `launcher: memory guard: ended` records one kill.

- `queue/<owner>__<name>.jsonl`: `{pullRequest, item}`, each timeline event of a pull request of the kinds `added_to_merge_queue`, `removed_from_merge_queue` and `merged`, exactly as GitHub's REST timeline serves it, with the pull request's number beside it.
  Each event carries its `id` and its `url`, `https://api.github.com/repos/<owner>/<name>/issues/events/<id>`.
  A line that lacks the id, or whose url names no repository, is refused.
  The tool reads all the queue files together: each event under the repository that its url names, as GitHub names it, and each event once, at the first line that carries its id.
  It reads them so because GitHub answers a renamed repository's former name with the repository's own events.
  A window collected under the former name and under the current name then holds some events in both files, and a pull request's events can be split between the two.

- `settings/events.jsonl`: each of the project's events of Dispatch's event log whose type starts `settings.`, `settings.project_default_assignee.updated` among them, verbatim, in the log's order.
  The events before the reach are there too, since the last of them sets what holds at its start.

## Collect a window

1. Get the daemon's log lines for the reach, the span and the run-out.

2. Get the deployment's records of the signals that you hold, for the same reach.

3. Run `legion replay collect`.

4. Run `legion replay collect-merges` once for each base branch.

5. Run `legion replay collect-queue` once for each repository of the project's pull requests.

6. Run `legion replay collect-git` once for each repository of the project's pull requests, after `collect-merges` of its base branch.

7. For test data that this repository keeps, run `legion replay neutralize`.

### Get the daemon's log lines

`collect` reads each file that `--log` names, and each file under a directory that `--log` names, gzipped or not.
It reads the three forms below, and you can give it files of more than one form.
It keeps a line that two files hold once.
It keeps only the kinds of line that the tool reads, so you need not filter the lines first.

#### From the daemon's standard error

The daemon logs JSON lines to standard error.
Where you start it yourself, keep that output in a file:

```sh
legion start --config legion.yaml 2>>daemon.log
```

Where a process supervisor runs the daemon, give `collect` the file in which the supervisor keeps the daemon's standard error.

#### From `kubectl logs`

```sh
kubectl -n <namespace> logs <pod> > daemon.log
kubectl -n <namespace> logs <pod> --previous > daemon-previous.log
```

`collect` removes the time that `--timestamps` writes, and the `[pod/<pod>/<container>]` that `--prefix` writes, before each line.
`kubectl logs` gives only the log file that the node keeps now for the pod that runs now, and `--previous` gives the container that ran before it.
On a busy daemon, that file can hold only the last minutes, so read the time of its first line.
For a window of days, use the log store of your cluster.

#### From a cloud log export

`collect` reads an export of log entries, each with `timestamp` and `jsonPayload`, as JSON lines or as one JSON array.
It reads the daemon's line from `jsonPayload`, and ignores the other members of the entry.
For example, with Google Cloud Logging:

```sh
gcloud logging read '<daemon filter> AND timestamp>="2026-09-28T00:00:00Z" AND timestamp<"2026-10-08T00:00:00Z"' \
  --project=<project> --order=asc --format=json \
  | jq -c '[.[] | {timestamp, jsonPayload}]' > daemon-export.json
```

`<daemon filter>` selects the daemon's own container, for example `resource.type="k8s_container" AND resource.labels.namespace_name="<namespace>" AND resource.labels.container_name="<container>"`.
The `jq` step keeps only the two members that `collect` reads, so the file holds no name of your cluster.
A log store keeps lines for a limited time, 30 days by default in Cloud Logging, so collect a window before the oldest day of its reach expires.

### Get the records of the signals

`collect` reads the deployment's records of R11's signals in the same reach as the lines, and keeps each one's lines from `--from` less `--before` to `--to` plus `--after`.
Each is optional: a window collected without one holds no record of its signal.

- `--provider <file>` names the record of the model provider, one line a minute in the form of `signals/provider.jsonl`, gzipped or not.
  The deployment's provider-record script writes it from its export of the project's per-minute series.
  The window holds a record of the provider's signal in each run of minutes that has a limit in force.

- `--launcher-log <file or directory>` names the launcher's lines of the project's issue pods, given once for each file or directory.
  `collect` reads rows `{timestamp, pod, text}` as an export writes them, Cloud Logging entries with `timestamp`, `resource.labels.pod_name` and `textPayload` as `gcloud logging read --format=json` writes them, and a window's own lines, as JSON lines or one JSON array, gzipped or not.
  It keeps a line that two files hold once, and writes each as `{time, pod, text}`.
  The window then holds a record of the kills over the whole reach, so give it the lines of the whole reach.

- `--default-assignee <login>` gives the project's default assignee at the start of the reach.
  Give it where no settings event before the reach names the default assignee.

Under a directory that `--launcher-log` names, `collect` reads only the files whose names end in `.jsonl`, `.jsonl.gz`, `.json` or `.json.gz`, at any depth.
So an export's other files beside its hour objects, such as its `manifest-<day>.sha256` of their hashes, are left out.
A file that a flag names itself is read whatever its name.
`--log` reads every file under a directory that it names.

### Run `collect`

`collect` reads from Dispatch the events of each issue that the lines name.
It also reads the events of each blocker and child that those events name, in any project.
It lists every issue of the project, of every status, a page at a time (`GET /api/v1/issues?project=<KEY>&limit=&offset=`), and reads the events of each one too, so that the approvals signal reads every ask of the project.
It reads each issue from its first event up to `--to` plus `--after`.
It pages Dispatch's whole event log (`GET /api/v1/events/log`) from its first event to `--to` plus `--after`, and keeps the project's settings events.
A settings event before the reach sets the value at the reach's start, so the read starts at the log's first event; on 2026-11-25 the whole log took 1,148 pages of 1,000 events and about five minutes.
An issue of the project that the event log names and the listing missed, as a listing read a page at a time can, is read too.
Then it follows the Dispatch parent chain of each issue that it read, in any project, and reads the events of each ancestor, up to 32 levels above the issue.
Dispatch resolves an issue's priority from the nearest issue above it that has one, by a walk of the chain that reads 32 levels, so an issue higher up is none of the replay's inputs, and `collect` neither reads it nor names it.
It reads each parent that an issue's events show at any moment, so a parent that the issue left is read too.
Every read is a `GET`, and a read that Dispatch refuses for now (429, a 5xx, or no answer) is tried again with a growing wait.
Set `DISPATCH_URL` to your Dispatch server.
The token comes from the file that `--dispatch-token-file` names, else from the file that `DISPATCH_TOKEN_FILE` names, else from `DISPATCH_TOKEN`.
`collect` never prints the token.

```sh
export DISPATCH_URL=https://<host>.internal.example
legion replay collect \
  --project ACME \
  --from 2026-10-05T00:00:00Z --to 2026-10-08T00:00:00Z --before 168h \
  --log daemon-export.json --log daemon.log \
  --provider provider.jsonl --launcher-log launcher/ \
  --dispatch-token-file <token file> \
  --part 2026-10-05T00:00:00Z,2026-10-08T00:00:00Z,<release>,<commit>,main \
  --out windows/acme-2026-10-05
```

It prints the number of daemon lines that it read and that it kept, the lines that it read twice, the lines that were no daemon line, and the issues whose events it read.
It prints the issues that the project's listing holds, the settings events, and the lines of each record of the signals that it kept.
It names each issue that Dispatch does not have.
It names apart each ancestor among them, which `window.json` names in `missingAncestors`.

The window holds a record of the idle share over the whole reach, from the daemon's lines.
It holds a record of the approvals from the moment the project's default assignee is known: from the reach's start where `--default-assignee` gives it or a settings event before the reach sets it, else from the first settings event in the reach that sets it.

| Flag | What it gives |
| --- | --- |
| `--project <KEY>` | The Dispatch project key of the daemon that wrote the lines (required). |
| `--from <time>`, `--to <time>` | The window's start and end, in RFC 3339 (required). |
| `--before <duration>` | How far before `--from` the inputs reach, so that the tool can rebuild the state at `--from` (required). |
| `--after <duration>` | The run-out: how far after `--to` the inputs reach, so that a replay runs the work that the record shows after `--to` (none by default). |
| `--log <file or directory>` | A file, or a directory of files, of the daemon's lines, given once for each (required). |
| `--provider <file>` | The deployment's record of the model provider. |
| `--launcher-log <file or directory>` | A file, or a directory of files, of the launcher's lines of the project's issue pods, given once for each. |
| `--default-assignee <login>` | The project's default assignee at the start of the reach, where no settings event before the reach names it. |
| `--out <dir>` | The directory that the window goes to, which must be missing or empty (required). |
| `--dispatch-token-file <file>` | The file that holds the Dispatch token. |
| `--concurrency <n>` | How many issues `collect` reads from Dispatch at once (8 by default). |
| `--part <from>,<to>,<release>,<commit>,<rule>` | One part of the span, with the release, the commit and the slot rule that the daemon ran in it, given once for each part. |
| `--collector <commit>` | The commit of cybermonklabs/legion that this `collect` runs from, where the binary carries no commit of its own. |
| `--tick-interval`, `--gate-steps`, `--linger`, `--review-round-cap`, `--stalled-tree-notice`, `--worker-silence`, `--worker-background` | The deployment's `controller_wake_interval_seconds`, `gates.design`, `linger_hours`, `review_round_cap`, `stalled_tree_notice_hours`, `worker_silence_minutes` and `worker_background_hours`, each at the daemon's own default where you give none. |

- Make `--before` as long as the lines that you hold allow.
  If the reach starts while slots are in use and no line names their holders, the tool infers the holders from their phases (assumption 34).

- Give `--after` where you hold lines after `--to`.
  An issue that the replay moves earlier, for example past a conflict loop that the replay does not draw, then runs into its work recorded after `--to`, and does not stand with no work at `--to`.
  Work recorded after the end of the run-out is not in the replay (assumption 3).

- Give one `--part` for each release that the daemon ran in the window, with the name of its rule as `legion replay rules` prints it.
  `scorecard` reads the record under the rule of each moment.
  `validate` and `forecast` with `--from-runs` read a window's rule from its parts where you name none.

- Give each setting of the deployment that differs from its default, so that a replay decides with the values that the record was made under.

### Add the merges of each base branch

The conflict draw reads how fast each base branch moved.
`collect-merges` reads a base branch's first-parent history from GitHub's commit listing, with `GET` alone.
It writes the history into the window as `merges/<owner>__<name>.jsonl`, and writes the file's new sha256 into `window.json`.
It reads the window one line at a time and checks it as every command does.
Of the window, it holds in memory only the merges of `--repo`.
It writes only that file and `window.json`, each to a temporary file that it renames into place.
Every other file of the window stays as it was.
It lists the branch's commits from a day before `--from` to the tip of the branch, with no end time.
A commit's first parent can be committed after the commit, even after `--to`.
So the walk starts at the tip and goes along first parents through every listed commit, whatever its time.
The walk ends at the first commit of the branch, or at a first parent that the listing lacks.
A walk that reaches the day before `--from` has gone beyond the span by committer date.
Where a walk ends at a missing parent while its last commit is inside the span, `collect-merges` reads that parent with one more `GET`.
If that parent's committer date precedes the start of the listing, the walk ends there.
A parent that was committed later is a commit that the listing lacks, so the walk would lose the history before that parent.
`collect-merges` refuses such a walk with an error that names the last commit walked and the parent, each with its time, and writes nothing.

`collect-merges` keeps commits with a landing time at or after `--from` and before `--to`.
A commit lands at GitHub's merge event: the `merged_at` of the pull request that GitHub records as merged as that commit (`merge_commit_sha`) into `--branch` (`base.ref`).
A commit's committer date can come well before that, for example where a merge queue built the commit when the pull request entered the queue.
To find the pull request, `collect-merges` first reads the one that the first line of the commit's message names with GitHub's squash suffix ` (#<n>)` (`GET /repos/<owner>/<name>/pulls/<n>`), and takes it where GitHub's record matches the commit.
Where it does not match, or GitHub answers `404 Not Found`, or the message names none, it reads the pull requests that GitHub lists for the commit (`GET /repos/<owner>/<name>/commits/<sha>/pulls`), so that a merge commit, whose message has no suffix, finds its pull request too.
Where that list leaves out a pull request's `merge_commit_sha`, it reads that pull request whole.
Where GitHub still refuses to read a pull request whole after the tries below, with a `422` whose message says `too many files changed`, `collect-merges` matches that pull request by its entry in the list.
Where the list gives nothing to match that pull request by, the commit keeps its committer date, with no `event` and no `pr`, and the command's summary line counts it, so run the command again later to place it.
It reads nothing of a commit that the walk does not reach.
A commit whose merge GitHub records keeps that pull request as `pr`, and its line says `"event": true`.
A commit whose merge GitHub does not record, or records for more than one pull request, keeps its committer date, which is an estimate and no merge event, and its line has no `event` and no `pr`, whatever its message names.
Every line also holds `committed`, the commit's committer date in RFC 3339, so a line whose merge event came later shows both times.
The replay reads `at` alone.
Older windows contain committer dates alone, with no `committed`, and retain any pull request number from the commit subject.
Commits that land at the same moment keep their first-parent order.

The listing reaches only the commits that were committed from a day before `--from`.
A commit that was committed before that and merged inside the span, such as one that a merge queue held for more than a day, is missing from the history that `collect-merges` writes.
The checks of the walk's end read committer dates, as the listing does, so they do not find such a commit.

A read that GitHub refuses for now, with a 5xx, with no answer, or with a `422 Unprocessable Entity` whose message says `too many files changed`, is tried again with a growing wait of about a minute in all.
A read that GitHub's rate limit refuses is tried again after the wait that GitHub names, as [GitHub's REST documentation](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#exceeding-the-rate-limit) asks.
That is a 429, or a 403 that carries `x-ratelimit-remaining: 0` or a `retry-after`, or whose message names the secondary rate limit.
A `retry-after` of N seconds waits N seconds.
A spent primary limit, with `x-ratelimit-remaining: 0`, waits until the time in `x-ratelimit-reset` and 5 seconds more.
A secondary limit that names no wait waits one minute, then 2, 4, 8 and 16 minutes.
Any other 403, such as a refusal of permission, fails the read at once.
Before each wait, `collect-merges` prints one line to stderr that names the page, GitHub's answer, the wait, the time that the wait ends, and which wait of the read it is:

```text
replay: GitHub's rate limit refused GET https://api.github.com/repos/acme/widgets/commits?per_page=100&sha=main&since=2026-09-27T00%3A00%3A00Z (GitHub answered 403 Forbidden: {"message":"API rate limit exceeded for installation ID 1."}); waiting 22m5s, until 2026-10-05T11:28:05Z, wait 1 of at most 5 for this read
```

One wait lasts at most 65 minutes: an hour, the window of GitHub's primary limit, and a margin.
One read waits at most 5 times.
A read whose wait would last longer, or that GitHub still refuses after its fifth wait, fails with an error that names the time that GitHub gives to try again, and nothing is written.
Run the command again after that time.

GitHub's quota is the installation's, and the daemon and the other roles share it, so a collector that spent it would stop their reads too.
So `collect-merges`, `collect-queue` and `collect-git` each leave them a reserve, and space their own requests.
After any answer whose `x-ratelimit-remaining` is at or below `--github-reserve`, 3000 by default, the collector sends no request before that answer's `x-ratelimit-reset` and five seconds more.
Before that wait, it prints one line to stderr that names the quota left, the reserve, the wait and the time that the wait ends.
A wait past 65 minutes fails the read, as a rate limit's does.
The collector also sends at most `--github-rate` requests a second, 0.6 by default.
A reserve of 0 turns the reserve off, and a rate of 0 turns the pacing off.

```sh
legion replay collect-merges \
  --repo <owner>/<name> --branch main \
  --from 2026-09-28T00:00:00Z --to 2026-10-08T00:00:00Z \
  --github-token-env GITHUB_TOKEN \
  --out windows/acme-2026-10-05
```

Run it once for each base branch that the window's pull requests name.
Give it at least the window's span.
A span that starts at the start of the reach also covers each stretch that began before `--from`.
The draw reads each commit's `pr` to leave a stretch's own merge out of its count.

| Flag | What it gives |
| --- | --- |
| `--repo <owner>/<name>` | The repository (required). |
| `--branch <name>` | The base branch (required). |
| `--from <time>`, `--to <time>` | The span, in RFC 3339 (required). |
| `--github-token-env <VAR>` | The environment variable that holds the GitHub token, which `collect-merges` never prints (required). |
| `--github-api <url>` | GitHub's REST API (`https://api.github.com` by default). |
| `--github-reserve <n>` | The requests of the shared quota left to the daemon and the other roles (3000 by default; 0 for none). |
| `--github-rate <n>` | The most requests a second sent to GitHub (0.6 by default; 0 for no pacing). |
| `--out <dir>` | The directory of the window that `collect` wrote (required). |

### Add the merge queue of each repository

The queue signal reads how long the project's pull requests wait in the merge queue.
`collect-queue` reads, with `GET` alone, the timeline of each pull request of `--repo` that belongs to the project's issues (`GET /repos/<owner>/<name>/issues/<n>/timeline`, every page by its Link header).
A pull request belongs to them where an `issue.pull_request_attribution_changed` event of an issue of the project moves it, or where a line of the window's log names it (`pullRequest`) for an issue whose repository, as its events give it, is `--repo`.
It writes each timeline event of the kinds `added_to_merge_queue`, `removed_from_merge_queue` and `merged`, exactly as GitHub serves it, with its pull request's number, into the window as `queue/<owner>__<name>.jsonl`.
It writes the file's new sha256 into `window.json`, with a record of the queue signal over the window's reach and run-out.
It reads the window one line at a time and checks it as every command does.
Of the window, it holds in memory only the pull requests that the log and the events name, and the repository of each issue.
It writes only that file and `window.json`, each to a temporary file that it renames into place.
Every other file of the window stays as it was.
It names each pull request that a line names for an issue whose repository the window does not give, and reads none of them.
A line of the log names no repository, so `collect-queue` reads the timeline of a pull request that it names in the repository of the line's issue.
The daemon can have watched a pull request of another repository for that issue, which `--repo` does not have.
Where only lines of the log name a pull request, and GitHub answers its timeline with `404 Not Found` in `--repo`, `collect-queue` skips it, names it on one line, and reads the other pull requests.
Where an attribution names the pull request, it is a pull request of `--repo`, so a `404 Not Found` fails the read.
Every other refusal is handled as `collect-merges` handles it ([Add the merges of each base branch](#add-the-merges-of-each-base-branch)): a 5xx or no answer is tried again, GitHub's rate limit is waited out with the same bounds and the same line on stderr, and any other status fails the read.
A read that fails writes nothing.

```sh
legion replay collect-queue \
  --repo <owner>/<name> \
  --github-token-env GITHUB_TOKEN \
  --out windows/acme-2026-10-05
```

Run it once for each repository of the project's pull requests.

| Flag | What it gives |
| --- | --- |
| `--repo <owner>/<name>` | The repository (required). |
| `--github-token-env <VAR>` | The environment variable that holds the GitHub token, which `collect-queue` never prints (required). |
| `--github-api <url>` | GitHub's REST API (`https://api.github.com` by default). |
| `--github-reserve <n>` | The requests of the shared quota left to the daemon and the other roles (3000 by default; 0 for none). |
| `--github-rate <n>` | The most requests a second sent to GitHub (0.6 by default; 0 for no pacing). |
| `--out <dir>` | The directory of the window that `collect` wrote (required). |

### Add git's verdicts of each repository

The replay's spec, R2, has the replay answer a read of a head's merge state from git's own merge of that head with its base.
`collect-git` writes, into the window as `git/<owner>__<name>.jsonl`, what git says of each pull request of `--repo` of the window's issues.
It writes the file's new sha256 into `window.json`.
The pull requests are those that `collect-queue` reads.
It reads the merges file that `collect-merges` wrote of `--branch`, and refuses a window that holds no merges file of `--repo`.
It reads the window one line at a time and checks it as every command does.
It writes only its file and `window.json`.
The file grows by appending: as a phase finishes a pull request, `collect-git` appends that pull request's block to the file, and it appends the commits' block once the git phase has read them.
It writes again no block that the file already holds, but where a block before it changed, as a run on a window whose file exists says below.
After each block it writes `window.json` to a temporary file that it renames into place.

Each line of the file is one JSON object.
The lines stand in blocks, and each block ends with `{"kind":"done","pr"}`, the done line of its pull request, with `pr` 0 for the commits' block.
First comes the GitHub phase's block of each pull request, in ascending number:

- `{"kind":"pull","pr","merge","base","baseChanges","heads"}`, where GitHub gave the pull request, followed by its head lines.
  `merge` is the commit that GitHub records as its merge into `--branch` (`merge_commit_sha`, where `merged` is true and `base.ref` is `--branch`), where the merges file holds that commit, and empty where it does not.
  `base` is the current base branch from GitHub's `base.ref`.
  `baseChanges` lists the times of the timeline's `base_ref_changed` events, oldest first, or `[]` when there are none.
  A read can use the stored verdicts only if `base` is the collected branch and the recorded read is at or after the last base change.
  Otherwise the read is unanswered with kind `missing`.
  `heads` counts the pull request's head lines.

- `{"kind":"head","pr","sha","at","from"}`: each head of the pull request, by `at` and then `sha`.
  `from` is `commit` for a committed event of the pull request's timeline, at its committer's time, `force-push` for the new tip of a `head_ref_force_pushed` event, at the event's time, and `read` for a head that a read of a merge state names and the timeline does not give, at the read's time.
  A committer's time only estimates when the commit was pushed.

- Then each missing line of the pull request whose reason is GitHub's: `pull-not-found`, `pull-unread` or `timeline-unread`.

Then, once the git phase has read anything, come git's blocks.
The first is the commits' block:

- `{"kind":"commit","sha","paths"}`: each first-parent commit of `--branch` that the merges file holds, in that file's order, then each base commit that a read of a merge state names and the merges file does not hold.
  `paths` is the paths that the commit changes against its first parent (`git diff-tree -r --no-commit-id --name-only <sha>^1 <sha>`), sorted.

- Then each missing line with `pr` 0, of a commit that git did not give.

Git's block of each pull request comes next, in ascending number, for each pull request whose block holds a line:

- `{"kind":"reading","pr","sha","tip","mergeBase","paths","verdicts"}`: git's reading of each head that git read, in the order of the pull request's head lines.
  `tip` is the base commit that the head's verdicts run to: the first parent of the pull request's own merge commit, where it merged in the window's inputs, and otherwise the last commit of the merges file.
  `mergeBase` is `git merge-base <head> <tip>`, and `paths` is `git diff --no-renames --name-only <mergeBase> <head>`, sorted: the paths that the head changes, which are its profile.
  `verdicts` holds `{"base","conflict","pathCount"}` for each first-parent commit of the merges file after the merge base, up to the tip (`git rev-list --first-parent <tip> ^<head>`, oldest first), whose commit line shares a path with the head's `paths`.
  Each is git's verdict of merging the head with the base as it stands at that commit (`git merge-tree --write-tree --no-messages --name-only <base> <head>`, or the same merge as a line of `git merge-tree --stdin`): clean, or a conflict.
  So the first verdict that conflicts names its cause, and each later verdict holds the earlier commits.
  `pathCount` counts the distinct paths that git names in the conflict: 0 for a clean merge and at least 1 for a conflict.
  A merge that git calls a conflict and that names no path gives no verdict, and nor does a clean merge that names one: either is a `git-error` missing input.
  The file does not list those paths, since no read of the replay asks which paths conflict, and on a real repository the lists held most of the file.
  A head that git did not read has no reading line.

- Then each missing line of the pull request whose reason is git's.

A missing line is `{"kind":"missing","pr","head","base","reason","detail"}`: an input that the collector could not get, with `pr` 0 for a commit of the base branch.
The missing lines of a block are sorted by `head`, `base`, `reason` and `detail`.
`reason` is `pull-not-found`, `pull-unread`, `timeline-unread`, `head-unreachable`, `base-unreachable`, `git-error`, `memory-cap` or `timeout`.
A missing input is never written as a clean verdict.
A verdict that git cannot give ends the head's verdicts, since each later one would rest on it.
A read refuses a file that is not in this form, so collect the git file again if it predates a member that the form names.

The command runs in two phases.
The GitHub phase reads, with `GET` alone, each pull request (`GET /repos/<owner>/<name>/pulls/<n>`) and its whole timeline (`GET /repos/<owner>/<name>/issues/<n>/timeline`).
It writes each head whose time falls in the window's reach, span or run-out, and the head that stood at the reach's start: the latest at or before it, a force push before a commit of the same second.
It also writes each head that a read of a merge state in the window's log names, as its head or as the head that the record stood at.
It handles GitHub's refusals as `collect-merges` does ([Add the merges of each base branch](#add-the-merges-of-each-base-branch)), but for a `404 Not Found` of a pull request, which is a `pull-not-found` missing input.
A pull request that GitHub still refuses to read after the tries, with the `422` whose message says `too many files changed`, is a `pull-unread` missing input: `collect-git` writes no pull line of it and reads no timeline of it, and a run again reads it again.
A timeline page or event that does not read, a force push that names no commit among them, is a `timeline-unread` missing input: the collector never guesses a head.

The git phase reads no GitHub API.
It clones `--repo` once into `--clones`, as `git clone --no-checkout --bare --single-branch` of `--branch` alone, from `https://github.com/<owner>/<name>.git`.
The clone is whole: it holds every blob of the branch, and each fetch brings the blobs of what it fetches.
Git takes the credential that your git credential helper gives, and `collect-git` never reads, prints or stores it.
It tests which commits the clone holds with lazy fetching off (`GIT_NO_LAZY_FETCH=1 git cat-file --batch-check`), and fetches the commits that it lacks.
It first asks the remote which of the pull requests whose heads the clone lacks have a head ref, `refs/pull/<n>/head` (`git ls-remote`, 200 refs at a time), and fetches each such ref into `refs/pulls/<n>`, 40 at a time.
It then fetches each commit that it still lacks into a ref of its own, `refs/fetched/<sha>`, in batches of up to 40.
A batch whose fetch does not bring all its commits splits in two, and each half is fetched in turn, down to a commit alone.
These refs tell each later fetch which commits the clone holds, so the remote sends no object twice.
A commit that the remote does not give is a `head-unreachable` or `base-unreachable` missing input.
Since the clone holds every blob that it reads, each command that reads it runs with lazy fetching off (`GIT_NO_LAZY_FETCH=1`), and never fetches from the remote.
Git merges a head with each base commit that it reads a verdict at in one `git merge-tree --stdin --write-tree --no-messages --name-only -z`, whose stdin holds a line `<base> <head>` for each base, 200 bases at most.
Where that command fails, or gives fewer verdicts than it has lines, the collector keeps each verdict that it gave whole.
It then merges each base after those alone (`git merge-tree --write-tree --no-messages --name-only -z <base> <head>`), up to the first that git does not give.
Each merge-tree writes the trees and blobs that it merges into an object directory of its own, under `merge-objects` in the clone beside its `objects` (`GIT_OBJECT_DIRECTORY`), and reads the commits from the clone's `objects` (`GIT_ALTERNATE_OBJECT_DIRECTORIES`); no other git command gets these two variables.
Git splits `GIT_ALTERNATE_OBJECT_DIRECTORIES` on `:`, so where the clone's path holds a `:` or a `"`, the collector gives git that path quoted, as `git help git` describes.
The collector removes each merge-tree's directory once the command ends, however it ended, and `merge-objects` whole before the first merge-tree, whatever a stopped run left there, and after the last, so the clone does not grow with the merges.
Git reads a head that several pull requests hold once: each merge base, each list of paths and each verdict that it gives serves every pull request that rests on it.
Under `--parallel`, two pull requests can read the same head at once, and git then gives each the same answer.

Each git command runs under a memory guard of its own: as the command starts and every 200 ms after, the guard reads from `/proc` the summed resident memory of the command's process group, and kills the group once it passes `--memory-cap`, 1.5 GiB by default.
The guard also kills a command past its time: 15 minutes for the clone, 180 seconds for a fetch of a batch, 90 seconds for a fetch of one commit, 120 seconds for a merge-tree, and 60 seconds for any other.
A `merge-tree --stdin` writes each verdict as it makes it, so its 120 seconds are those of one merge, counted from the last verdict that it wrote, or from its start before the first.
So a batch that goes on giving verdicts finishes, and a merge that hangs is killed, while the collector keeps the verdicts that its batch gave before it.
A kill is a `memory-cap` or a `timeout` missing input, any other failure of git a `git-error` one, and the run goes on.
The guard reads Linux's `/proc`, so the git phase runs on Linux.

`--parallel <n>` runs the git reads and merges of up to n heads at once, 1 by default.
Each of n workers takes the next head in the file's order, across its pull requests, so a window of many small pull requests and a few large ones keeps all n busy.
The collector still finishes the pull requests in the file's order: it writes a pull request and prints its line once all its heads are read, never before an earlier one.
So the file, the lines and a run that goes on after a kill are those of `--parallel 1`, byte for byte.
Memory grows with `--parallel`: each of the n git commands can hold up to `--memory-cap`, and a merge of a real repository holds about 350 MiB.
`--github-only` and `--fetch-only` run none of the reads and merges, so they refuse a `--parallel` above 1.

A run on a window whose git file of `--repo` exists asks GitHub and git for nothing that the file holds.
The GitHub phase keeps each pull request that the file holds and no missing input of GitHub's names.
The git phase keeps each commit's paths, each head's merge base where its tip is the same, its paths where its merge base is the same, and each verdict of the same head at the same base commit.
A run writes nothing while each block that it would write is the block that the file holds in its place.
At the first block that differs, it cuts the file there and appends that block and each block after it.
Before it reads the window, a run but `--fetch-only` cuts the file after its last done line, so it drops a block that a write which stopped left without its done line, and that pull request is read again.
It then writes the sha256 of the file as cut into `window.json`, whatever `window.json` named before.
So a run that the pod's release killed goes on where it stopped, from the file as it stood or from a copy of the file taken at any moment.
A run refuses a file whose lines after its last done line do not read in this form, or belong to more than one block, since no write that stopped leaves them.
So a run refuses a git file of an older form that holds more than one block's lines, rather than cut it away.

A build job holds no GitHub token, and the clone of a private repository needs your git credential.
So run the GitHub phase alone with `--github-only` where a GitHub token is, and the git phase alone with `--git-only`, which takes the pull requests and heads from the file that the GitHub phase wrote.
Run a public repository's git phase in a build job, which needs no credential, and a private repository's where your git credential is, under the guard.
The two phases in turn write the same file as one run of both.

```sh
legion replay collect-git \
  --repo <owner>/<name> --branch main \
  --github-token-env GITHUB_TOKEN --github-only \
  --out windows/acme-2026-10-05

legion replay collect-git \
  --repo <owner>/<name> --branch main --git-only \
  --clones <dir outside the window> \
  --out windows/acme-2026-10-05
```

A private repository's merges take hours of CPU, which belong on a machine that holds no git credential.
So the git phase also runs in two halves.
`--fetch-only` runs its fetches alone: it makes or opens the clone in `--clones`, and fetches every commit that the merges will read, as the git phase does.
It reads no head, merges nothing and writes nothing into the window, so it does not cut the git file either: it reads the file only as a run that ended left it.
It prints one line: the commits that the merges read that the clone holds, and the heads and base commits that the remote did not give.
`--merge-only` runs the git phase's reads and merges alone, on the clone that `--clones` holds, and opens no connection to the remote.
It runs no `git clone`, `git fetch` or `git ls-remote`, each git command that it runs has lazy fetching off, and it needs no `origin` remote.
It refuses a `--clones` that holds no clone of `--repo`.
A head or a base commit that the clone does not hold is a `head-unreachable` or `base-unreachable` missing input, whose detail says that the clone does not hold the commit and that this run asks the remote for nothing.
So run `--fetch-only` where your git credential is, and copy the clone's directory, `<clones>/<owner>__<name>`, to a machine that holds no credential and holds the window.
Then run `--merge-only` there.
The two in turn write the same file as `--git-only`.

```sh
legion replay collect-git \
  --repo <owner>/<name> --branch main --fetch-only \
  --clones <dir outside the window> \
  --out windows/acme-2026-10-05

tar -C <dir outside the window> -cf clone.tar <owner>__<name>
# Copy clone.tar to the machine with no credential, and unpack it there.
tar -C <dir outside the window> -xf clone.tar

legion replay collect-git \
  --repo <owner>/<name> --branch main --merge-only --parallel 4 \
  --clones <dir outside the window> \
  --out windows/acme-2026-10-05
```

`collect-git` prints one line for each pull request that a phase finishes, then a summary: the pull requests, heads, verdicts and conflicts, the missing inputs by reason, git's peak memory and the time taken.
`--fetch-only` prints its one line instead, with git's peak memory and the time taken.

| Flag | What it gives |
| --- | --- |
| `--repo <owner>/<name>` | The repository (required). |
| `--branch <name>` | The base branch, whose merges file `collect-merges` wrote (required). |
| `--github-token-env <VAR>` | The environment variable that holds the GitHub token, which `collect-git` never prints (required, but refused with `--git-only`, `--fetch-only` and `--merge-only`). |
| `--github-api <url>` | GitHub's REST API (`https://api.github.com` by default; refused with `--git-only`, `--fetch-only` and `--merge-only`). |
| `--github-reserve <n>` | The requests of the shared quota left to the daemon and the other roles (3000 by default; 0 for none; refused with `--git-only`, `--fetch-only` and `--merge-only`). |
| `--github-rate <n>` | The most requests a second sent to GitHub (0.6 by default; 0 for no pacing; refused with `--git-only`, `--fetch-only` and `--merge-only`). |
| `--out <dir>` | The directory of the window that `collect` wrote (required). |
| `--clones <dir>` | The directory that the repository is cloned into, never inside the window (`clones/` beside the window by default), where `--merge-only` reads the clone and makes none. |
| `--memory-cap <bytes>` | The memory that one git command's processes may hold before the guard kills them (1610612736, 1.5 GiB, by default). |
| `--parallel <n>` | The heads whose git reads and merges run at once, each git command under its own `--memory-cap`, with no change to the file written (1 by default; refused below 1, and above 1 with `--github-only` and `--fetch-only`). |
| `--github-only` | Run the GitHub phase alone. |
| `--git-only` | Run the git phase alone, from the file that the GitHub phase wrote. |
| `--fetch-only` | Run the git phase's fetches alone, where your git credential is: fill the clone in `--clones` with every commit that the merges read, and write nothing into the window. |
| `--merge-only` | Run the git phase's reads and merges alone, on the clone that `--clones` holds, with no connection to the remote. |

### Neutralize a window for test data

This repository is public, so a window that it keeps as test data holds no private name.
`legion replay neutralize` writes a copy of a window with each private name renamed, the same name to the same neutral name in every file.
It renames each issue key, and each claim token that spells one.
It renames each repository but cybermonklabs/legion, each actor's id, each session's id and title, and each person's login and email.
The title of a Legion role's session keeps its role and takes the neutral key of its issue, so the trace reads the same claim from the neutral window.
It renames each host name and machine name, each IP address but a loopback, unspecified or documentation address, and each uuid, link, label and digest.
It also renames the free text of Dispatch's events.
It renames the records of the signals by the same rules.
It renames the issue keys and the people's logins that a pod's name or a pull request's branch spells.
It renames the GitHub accounts of the merge queue's events: each login, numeric id, node id and link.
The `url` of each event keeps its form, with its repository renamed, so that it still names the event's repository.
It renames the project's settings events as Dispatch's events, a token's name and a settings source's last error among them, and the default assignee as a person.
In a git file, it keeps each path and each commit, but for the issue keys of the window's projects that a path spells and the `--deny` words, which it renames in each path alike, so that a head and a commit that share a path still share it.
It renames the detail of each missing input as it renames a daemon line's text.
It keeps every member, JSON type and list length, and the daemon's msgs, ranks, statuses and times.
The window's own project becomes `--project`, and each other project becomes `OTHERA`, `OTHERB` and so on.

```sh
legion replay neutralize \
  --in windows/acme-2026-10-05 \
  --out packages/daemon/internal/replay/testdata/<name> \
  --project ACME \
  --map <dir outside the repository>/names.json \
  --deny <organization>
```

`--map` receives the map of every name that `neutralize` replaced.
That file holds every private name, so keep it outside the repository and never commit it.
`neutralize` refuses a `--map` inside `--out`, and one that exists already.
`--deny` names a word that no file may hold in any case, such as an organization's name.
`neutralize` replaces that word with `withheld` wherever it stands, and fails if one is left.
`neutralize` refuses a deny word that is part of `withheld`, because its replacement would hold that word again.
Before you commit the neutral window, search it for each private name that you know.

| Flag | What it gives |
| --- | --- |
| `--in <dir>` | The window (required). |
| `--out <dir>` | The directory that the neutral window goes to, missing or empty (required). |
| `--project <KEY>` | The key that the window's own project becomes (`ACME` by default). |
| `--map <file>` | The file that the map of names goes to, outside `--out` (required). |
| `--deny <word>` | A word that no file of the neutral window may hold, in any case, given once for each word. |

## Run a replay

Build a `legion` binary from this repository:

```sh
go -C packages/daemon build -o <dir>/legion ./cmd/legion
```

`run`, `validate`, `forecast` and `compare` need a Postgres server on which the user of `--pg-dsn` can create a database.
Each run makes a database of its own there and drops it at the end.
The run copies its database from a template that holds the daemon's migrations and the replay's own tables, `legion_replay_template_<key>`.
The key is a hash of those migrations and tables, so a `legion` binary with other migrations uses a template of its own.
The first run on a server makes the template from `template1`, and every later run copies it.
The template and each run's database run with PostgreSQL's JIT compilation off, as the daemon's store on Cloud SQL runs.
`run --keep-db` keeps the database of its run.
No run writes to the database that `--pg-dsn` names.
Use a test server.
`scorecard`, `signals` and `rules` need no database.

The templates stay on the server.
To remove them, stop every replay on the server, and then drop each database whose name begins with `legion_replay_template_`.
Also drop a database whose name begins with `legion_replay_build_`, which a run that stopped while it made a template leaves.
The next run makes the template again.

A comparison of rules takes these steps:

1. Read the scorecard of each window from its record (`scorecard`).

2. Check the replay of each window under the rule that its daemon ran (`validate`).

3. Check a forecast across a recorded change of the rule (`forecast`).

4. Compare the rules on two windows (`compare`).

Keep the window before a change of the rule held out: record the commit of the tool before its first replay of that window.
If the tool fails on that window, fix the tool and check it again on a third window.

### `legion replay rules`

`legion replay rules` prints every slot rule that a replay can run, the daemon's own first, with its commit and each of its choices.
`--json` prints the rules as JSON.
[The slot rules](#the-slot-rules) gives its output.

### `legion replay run`

```sh
legion replay run \
  --window windows/acme-2026-10-05 \
  --rule keep-past-review \
  --pg-dsn 'postgres://<user>@127.0.0.1:5432/postgres?sslmode=disable' \
  --out runs/acme-keep-past-review
```

`run` replays the window through the daemon's own handler chain: the Dispatch mirror, the workflow engine and the admission pass.
It enters each Dispatch event, each recorded phase move, each tick and each boot at its recorded time or offset.
Where the window has a run-out, the replay runs on through it, and enters each of those in the run-out too.
Every measure of the run reads only the window's span, and `run.json` names the run-out as `after`.
Where the window starts while a daemon process boots, no pass runs before that process's recorded reconcile, and each act before the reconcile waits for it, as an act waits for the next boot while no daemon runs.
The replay reads that reconcile from the line that holds roots for the Dispatch consumer, or else from the process's first slot line before its line `legion daemon started`.
Most Dispatch events that the daemon's own session wrote are its outbox's status writes, which the replay's own outbox makes again, so the replay leaves those events out.
The daemon's API also writes statuses under that session for an agent, and no outbox row writes these: a move to backlog, icebox or triage, a move from one of those or from done to todo that adds no `legion` label, and the controller's todo on a root whose phase is held.
The controller's `set_status`, an architect's `release_children` and `rerun_child` and, in releases that have it, an architect's `park_child` write such moves, and the replay enters each one as recorded.
The replay decides whether a write of the daemon's session is such a move from the issue's event before it in the record, and not from the event that its copy of Dispatch applied last, because the replay can apply an event early or late.
A move to todo from done or from another status out of the workflow waits while the replay's own record holds the issue in the workflow.
It then enters when the replay's issue leaves the workflow, at its recorded offset from the record's leave of the issue before it, so a run that the record starts again after its done starts again after the replay's done.
The replay knows the controller's todo on a held root by the record's release of that hold, whose reason names a todo.
An architect's `release_children` of a root that has left the workflow first adopts the root into the architect's tree, at the generation that the line `workflow: a queued root joined a live tree` names, and parks it at done there, because its status is not todo.
The release's todo then runs the root again at the next generation, so the replay makes the root's recorded moves at that generation, as the record's daemon made them.
An event that a worker's session wrote inside a work segment of that worker's role on that worker's issue enters at its offset in that segment, so it moves with that work.
So does the filing of a child under that issue, because a child's filing moves with the start that the child is filed after.
The filing of a child under an issue that no work segment holds enters at its recorded offset from the replay's first start of that issue in its run, or at its recorded time where that is later, whoever filed it: the issue's own architect, the architect of its tree's root, or a person.
So does an act of an issue's own architect on that issue that no work segment holds, such as a link that the architect sets on it, unless the act answers a question or ends a wait, which keep those anchors.
While the issue stands in `awaiting_children`, its work stopped by the admission pass (cybermonklabs/legion#4592), such a filing or act waits, and enters after the issue's return as long after it as it fell due after the stop, as the issue's own stopped work does.
Each of these moves only where the record shows the issue started in its run and not yet out of the workflow at that moment.
A child's open status and a link hold the issue's first start, so either one applied before the replay starts the issue would hold a start that the record made before it.
Where the replay's issue leaves the workflow before it takes that start, such an event enters when the issue leaves, or at its recorded time where that is later.
An architect's act on a child that it filed, such as an edit of the child's title or a link on the child, enters after the filing, at its recorded offset from it, and never before its recorded time.
An event that the worker wrote on another issue is otherwise part of no segment: an edit of that issue, a link that the worker adds to it or takes off, or a child that the worker moves out.
Such an event keeps its recorded time, because the replay moves a worker's work and not the issues that the work changed.
A write of the daemon's session that follows the filing of a child, such as the architect's release or park of that child, enters after the filing, at its recorded offset from it, and never before its recorded time.
So the architect's writes on a child and the writes that its calls make under the daemon's session keep the order that the record shows.
The record names that session by an id under the title `Legion <role> · <KEY>`, which names the role and the issue of the claim that the session runs.
Where a window's events name a worker's session by its claim token instead, the replay reads the claim from that token.
Any other worker's event outside every work segment of its role is part of no segment, so it enters as an event that no worker wrote enters: at its recorded time, or at its offset from the replay's start of the wait that it ended.
A root's architect writes such events between the phases of its root, for example when it edits another tree's issue while its root is in implementing.
Where the replay does not run the segment of an event, because it passes over that segment or ends its work before the event, the event enters in the same way, at once where its recorded time has passed.
Where the worker's issue leaves the workflow before a segment of its current run opens, each event of that segment enters in the same way when the issue leaves, so a recorded child is filed under every rule.
A segment of a later run of the issue waits for the replay to enter that run.
Where `answer` is `offset`, a person's answer to a worker's question enters at its offset from the moment that the question entered the replay, wherever the question entered, so no answer comes before its question.
Where a drawn conflict loop takes the place of the issue's own recorded loop, each event of the issue's own loop enters at its offset in the drawn loop's segment of the same role and phase, or at the end of that segment where the segment is shorter, and an event of a role and phase that the drawn loop does not hold enters as an event that no worker wrote.
So the replay enters each recorded event once.
No recorded event of an issue enters before the issue's filing, whoever made it and wherever the replay placed it.
An event that would enter first waits for the filing, and then enters at its recorded offset from the filing, or at its recorded time where that is later.
Applied before the filing, an edit of the child would give the replay's copy of Dispatch the whole child as the edit shows it, at a status of the child's later work, and neither the filing nor the architect's release would change that status, so the replay's daemon would never take the child into its workflow.
Each recorded event of an issue changes the replay's copy of Dispatch only in the fields in which it differs from the issue's event before it in the record.
Where the replay applies an event late, the event does not change a field that an event after it in the record set first.
So an architect's edit of its own issue that enters after a person's later edit of the same field leaves the person's value, as the record does.
After each fact, a worker stub runs the outbox rows that the daemon's lanes would take.
An operator's retry of a phase's worker with a fresh session, which `legion claims retry --fresh-session` makes of a suspended claim, shows in the record as a launch of the claim and, a fraction of a second after it, a held phase released by the operator's retry where no hold stands.
The daemon writes the launch first and the release after it, so the replay reads the two by their order in the log and not by their moments.
The retry's launch is the last launch of the phase's role for the issue before the release line, within the visit that the retry restarts.
It counts where the claim launched afresh, ended no hold, and no turn of the role began after it, because a plain retry of a stopped worker writes its release before its launch, and the launch before that release is the worker's own start.
The release `legion-v20.42.5` launched the claim outside the start fence of the outbox and then restarted the phase, and the fence held the start of that restart, for example while the tree's design gate stood closed.
The replay enters such a retry at its recorded offset from the replay's start of the visit that it retries: the stub launches the claim and opens the worker's next work segment in that phase, and the replay then restarts the phase, whose start the fence holds as recorded.
Where the replay's issue stands in another phase at that moment, the replay counts the retry as refused.
A work segment of a visit in which the record shows no launch, no turn and no hold of the phase's role, and which starts at the move into its phase, opens on that move, as an accepting segment does, because no start of the role opened it in the record.
Its recorded end, a conflict among them, then enters at its recorded offset, and does not wait for a start that the replay holds, as it holds a merger's start while the tree's design gate stands closed.
A work segment of a visit that shows a launch, a turn or a hold of its role opens on that role's start.
Where a conflict waits for merging, the issue's segment in merging, whichever way it opens, opens when the merger's merge forward lands, because the merger merges the base forward before the issue's own work in merging.
Each claim that the replay writes runs the session that the record shows that claim running at that moment.
A claim runs a fresh session after a fresh start of its agent, so at each launch the claim takes the latest session whose first event comes at or before that moment, or else the first session after it.
A claim that the record shows no session for runs a session of the replay's own.
So the daemon's reads that compare a session with the sessions of the claims answer as they did in the record: who holds a waiting root's Dispatch claim, who opened an ask, and who wrote a status.
The start state writes the claim of each turn under way at the window's start.
It also writes the claim of the architect of each root that waits on a person at the start, between that architect's turns, where the record shows the architect's session.
The daemon keeps that claim through the wait, and at each pass it keeps the root only while the session that holds the root's Dispatch claim has a claim in the root's tree.
For a seeded claim, a death in the reach carries the runtime's recorded cause and the time of its log line.
These live claims hold no start failure because their workers reached readiness.
The start state's copy of Dispatch holds each issue's Dispatch claim as the newest event before the window's start that shows a claim shows it, as the record's daemon held it then.
The start state opens the lifecycle of the tree of each root in the workflow whose current run took a slot, or had a member start, or opened its design gate, or launched its architect afresh.
The last two show a run that the daemon ran where the log holds no slot line of it.
The daemon marks a re-admitted root whose tree's members it has still to bring back, until that tree first runs in its new generation: at the root's promotion, or at the first admission pass that finds the tree running because its design gate opened first.
A window recorded under a release before that mark holds none of it, so the start state writes it as the daemon's upgrade to the release that keeps it writes it: on each root of its own tree in todo at admitted on a generation past the first, with no linger, no slot and no mark of a slot given back for a wait.
So the members of a tree that the record re-admitted before the window's start come back at its promotion, or at the first pass that finds it running, as the daemon of the commit that the replay was built from brings them back.
The replay's copy of Dispatch also takes the claim of each `issue.claimed` and `issue.released` event, so a later boot or tick reads the claim that Dispatch showed.
Each work segment runs its recorded length times `f^((n − m)/42)`, where n is the number of issues in flight in the replay at the segment's start, and m the number in the record.
The main branch keeps the record's conflicts.
It also keeps the source's conflicts for work that the replay draws for an issue that never started.
The git and drawn branches can change conflicts when a rule changes when work runs.

#### The git branch

`main+conflicts=git` reads the head's merge state at each stretch end and at daemon boots inside a stretch.
The standing head is the latest at or before the recorded move or boot read.
At the same time a force-push wins over a logged read, which wins over a commit.
Within one source at the same time, the later line wins.
For a boot, the replay maps work already done in the running segment back to recorded time by removing the slowdown.
It does not read past that segment's recorded end.

For a repository with git input, each base commit enters at the recorded merge time, not the committer's time.
The exception is the merge commit of a pull request linked to an issue whose work the replay runs.
That commit enters when the replay merges the issue, or not at all if the replay does not merge it.
At a read, Git can answer if the landed commits not in the head that touch its paths are exactly the first n bases in its verdict list, in replay landing order.
With n = 0 the read is clean.
Otherwise the answer is the verdict at the nth base.
An answered conflict acts at that moment through the rule's conflict path.
At a recorded conflict end it keeps the recorded loop.
At an otherwise clean end it takes the loop's work from the generator.
An answered clean end skips the recorded conflict loop, except for an open loop kept under assumption 35.
The replay reads again at the next end of a stretch that went on past a skipped loop.

An unanswered read has the first applicable kind in this order:

- `never-ran`: drawn work or an issue with no recorded pull request.

- `missing`: no usable git input, pull request, standing head or commit path list needed for the read.

- `after-merge`: a touching merge comes after the pull request's own recorded merge on the base branch.

- `order`: the touching merges do not match the recorded prefix.

Unanswered reads use the conflict generator, with at most one draw per live stretch.
A boot in an open stretch with no recorded end can use an answered read but does not draw.
An answered end read overrides a deferred boot draw.
After a loop whose work was drawn, the head holds the replay's base as of the return to the phase it left, or the landing of the merge forward.
Later reads consider only commits that landed after that point.
No touching merges gives a clean read, and touching merges give `order` unless a higher-priority unanswered kind applies.
A kept loop uses the recorded heads again.

The replay logs one `replay: a head's merge state read from git` line per read.
It gives `issue`, `repo`, `head`, `recorded`, `by` (`boot` or `end`), and `n` and `answer` for answered reads, or `answer: "unanswered"` and `kind`.
Neither timeline reads these lines as moves.

#### The conflict generator

`main+conflicts=drawn` draws at each live stretch's first eligible read.
The model fits stretches that begin and end inside the window.
For a repository with git input, an issue's profile is the paths of the standing head at the read, or the last available head's paths.
Drawn work of an issue that never started takes its source's profile but has no head of its own.
The generator counts only landed commits that share a path with the profile and come after the head's merge base.
When the head is unknown, it counts from the later of the first implementing segment's start and the last merge forward in that generation.
After a drawn conflict loop, it counts only commits that landed after the loop ended.
If the issue has no profile, it counts all commits since the stretch's start and reports those draws.
With no touching merges the chance is zero.
The fit counts touching merges at the recorded stretch end and excludes stretches with none or with no profile.
The fit reports the excluded stretches and the recorded conflicts with no touching merge.
Repositories without git input use a separate legacy fit over the whole window.
That fit and its draws count each recorded base commit strictly inside the stretch except the issue's own merge.
The result names the repositories that use each fit and gives both sets of coefficients when both fits apply.
The profiled fit and draws count only known touching commits.
The result counts fitted stretches and draws that encounter a commit with no path list.
A stretch that began in the reach is not in the fit, because the reach can be of a release that recorded a conflict as another move back.
The fit counts the merger's move back for a conflicting head and the merger's merge forward as conflicts.
Each draw of a stretch takes its value from `--seed`, the issue, and the place of the stretch in the record: the phase and the recorded start of the work segment that ends it, and how many times the stretch drew before.
The loop of a conflict that it draws takes its value from the same place.
So a draw never depends on the draws before it, and two rules at one seed draw the same value for each stretch that both run, also where one rule ends a stretch earlier or runs a stretch that the other does not.
A daemon's boot reads each pull request again, so the model gives each phase a chance of its own for a stretch that spans a boot (assumption 3).
At a boot, each stretch under way draws there, whatever work segment of the stretch runs, at that chance of the phase in which the record ends the stretch, with the merges up to the boot: an issue in accepting whose stretch the record ends in merging draws there at merging's chance.
Where the issue stands at the boot in that phase, a conflict that it draws enters there: the issue moves back, or a role merges the base forward and the work goes on after it, or the conflict waits for merging, as the rule's path has it.
Where the issue stands in another phase of the stretch, the conflict waits for the stretch's end in the phase whose chance it drew at, and enters there without a draw of its own.
Where the stretch breaks or ends in another phase first, that conflict is dropped.
A stretch makes this choice at the first boot inside it, and a stretch that no boot reads inside draws at its end.
A stretch draws once.
It keeps that draw through its moves on to the next phase, a hold, a recorded conflict loop that it passes over and a merge forward, so a stretch that draws no conflict, at a boot or at its end, does not draw again, at a later boot or at a later end in another phase.
A kept open-loop conflict enters at its end whatever the live stretch drew before.
Even after an earlier clean draw, this later move is the record's and is not another draw.
A move back, the merge, a move out of the stretch's phases or the issue's leave ends the stretch, and the stretch after it draws its own.
`run` writes a line of its own to `log.jsonl` for each draw, at the moment of the draw, with the msg `replay: a stretch's conflict drawn`.
Its attributes are `issue`, `stretch` (the number of the issue's stretch in the run), `running` (the phase of the work segment that ran at the draw), `fitted` (the phase in which the record ends the stretch, whose chance the draw read), `start` (the replay's start of the stretch), `merges`, `by` (`boot` or `end`), `chance`, `u` (the number drawn) and `outcome`: `clean`, `conflict`, or `deferred` for a conflict that waits for the stretch's end.
No daemon writes that line, and neither timeline reads a move or a measure from it.
`run.json` gives each draw under `draws`, in the order of the draws, with its key and its value: `issue`, `start` (the recorded start of the work segment that ends the stretch), `phase` (the phase in which the record ends the stretch), `n` (how many times the stretch drew before) and `u`.
`compare` reads them to count, for each pair of rules at each seed, the stretches that both runs drew and how many of them drew another value.
`run.json` also gives `unkeyedDraws`: the draws that the run keys to no stretch, `experiments` for the draws of a workflow experiment in the `random` mode ([A limit beside the assumptions](#a-limit-beside-the-assumptions-a-workflow-experiment-drawn-at-random)).
Under a rule whose conflicting head moves back, an issue whose stretch draws a conflict runs a recorded conflict loop, drawn with `--seed` among the loops of the window whose conflict left the same phase.
Where the window records no loop from that phase, the replay draws among the loops that left a later phase, and cuts the drawn loop at its first work segment in that phase or a later one, so the issue returns to the phase that its conflict left.
Where the window records only loops that left an earlier phase, the replay draws among those, and after the loop the issue goes back to the phase that its conflict left through copies of its own last work segments in each phase between.
Where the issue's own work holds no segment in the phase that such a loop returns to, the issue runs no drawn loop and goes on with its own recorded work.
Where the record ended the stretch otherwise than in a conflict, the issue runs the work that ended it again after the loop, and an accepting segment of that work opens on the move into accepting, also where the segment was under way at the window's start.
The replay draws only among the loops whose work, as the drawn conflict takes it, the record shows ending.
A loop whose issue the record shows still in it at the end of the inputs, or leaving the workflow inside it, has no known length, and an issue given it would stand in its last work segment to the end of the inputs and never run its own work after the loop.
A loop that left a later phase counts where the record shows it reaching the phase that the drawn conflict left.
A stretch whose recorded conflict loop runs to the end of the inputs, `--to` or the end of the run-out, with no return to the phase that the conflict left, does not draw: it keeps that conflict, and the issue runs its own loop where the daemon moves it back (assumption 35).
`run` prints the number of recorded conflict loops that it passed over, because their stretch drew no conflict, and the number of conflicts that it drew where the record shows none.
`run.json` counts them as `divergence.skippedLoops` and `divergence.addedLoops`.
The replay runs the daemon of the commit that it was built from, and that daemon answers a conflict past the review by the conflict path of the rule that you name.
Under `main` and each candidate built on it, the daemon answers as cybermonklabs/legion#4979 has it, and leaves that conflict to the merger (assumption 37).
In accepting and retro, the daemon records the head as conflicting and asks nobody, because neither the architect's acceptance nor retro needs a head that merges.
The issue keeps its phase and goes on with its work: past the loop that the record ran, where the record ended the stretch in a conflict, and otherwise by the move that the record shows.
When the issue moves into merging, the daemon asks the merger to merge the base forward, and the replay adds a merge-forward segment of the merger's before the issue's merging work, as long as the implementing round that it drew when the conflict came (assumption 23).
A move back to a phase before accepting ends such a conflict before any merger is asked, and the work goes on from a head of its own, as after a conflict's move back.
In merging, the daemon asks the merger to merge the base forward at once, and the merge-forward segment is the merger's.
In testing and reviewing, the daemon asks the implementer.
Under `legion-v20.42.5` and `legion-v19.57.3`, whose conflicting head moves back, the daemon moves the issue back to implementing from testing, reviewing, accepting, retro and merging, as the releases that ran those rules did.
Under `legion-v22.14.1`, the daemon keeps the phase in every phase and asks the implementer to merge the base forward at once, as release 22.14.1 did, so the merge-forward segment is the implementer's, in accepting, retro and merging too.
`run` asks nothing outside: no Dispatch, no GitHub and no NATS.

The daemon records what each holder of a gated tree's work took of its approved design, and its outbox tells a holder what changed when a person approves a later version.
A window recorded under a release before that record holds none of it, so the start state writes it as the daemon's upgrade to that release writes it: each issue whose work has started took the versions that its tree's gate stands approved at, and so did the architect of each tree whose gate opened.
So the news of a replay is of what a person approves inside the window.
The worker stub runs each reconcile of that news (`design_changes`) as the daemon's outbox runs it, with the replay's copy of Dispatch in place of Dispatch.
That copy holds the versions of each document and the verdicts on them, but not their text.
So it answers which version a person approved before another, and the stub records what changed between two versions as a change that Dispatch could not read, but for a first approval, which names no earlier version.
A merger's start that waits for that record waits on its backoff, as the daemon's does, and starts at its first try after the reconcile, which waits on no read of Dispatch.
The stub finishes each start that the reconcile queues for news alone under assumption 26, once the claim it reaches holds no task, as the daemon's claim takes such a task.
The news that such a start or an entry's start carries counts as taken by its holder at that start, since the replay's workers take each task at their start (assumption 30).
Nothing that moves an issue's work reads what a holder took, but the merger's start, which waits only until the news is recorded.

| Flag | What it gives |
| --- | --- |
| `--window <dir>` | The window (required). |
| `--rule <name>` | The slot rule, as `legion replay rules` names it (required). |
| `--pg-dsn <dsn>` | The Postgres server that the run makes its database on (required). |
| `--out <dir>` | The directory of the run's output (required). |
| `--branch <name>` | The branch, `main` or `main+<knob>=<value>` ([The branches](#the-branches)). |
| `--seed <n>` | The seed of every draw (1 by default). |
| `--slowdown <f>` | The factor f for each 42 issues in flight above the record's number (1 by default). |
| `--cap <n>` | The admission cap that the replay serves under from the window's start to its end, in place of the caps that its record shows (0, the default, keeps the record's), which `run.json` names as `cap`. |
| `--limit <signal>=<value>` | A signal's limit, given once for each signal that has one: `idle`, `queue`, `provider`, `refusals`, `kills` or `approvals`, each in the range that its limit takes ([The signals](#the-signals)), under which `signals` and `signals-backoff` start an issue below P0 only while each signal that has a limit reads under it. |
| `--idle-after <duration>` | d, how long a slot holder has been idle before the idle signal counts it (1h by default). |
| `--probe <time>` | A moment, in RFC 3339, at which `run.json` gives the value of each signal that `--limit` names, given once for each moment. |
| `--token-limit <tokens a minute>` | The project's token limit in uncached input tokens a minute, a whole number above zero, which the replay takes in each minute whose provider record holds no limit of the project's own, a limit that the record holds standing (assumption 49). |
| `--keep-db` | Keep the run's database after the run. |
| `--trace-segments` | Write each work segment's issue, phase, start, n, m, recorded length and replayed length to `segments.jsonl`. |

`run` prints the window, the rule, the branch, the seed, f, the cap and the database.
It prints each move that the daemon's code refused, with the reason, and counts what the replay could not enter as recorded, the recorded conflicts that it kept for a loop that runs to `--to`, and the conflicts past the review that it left to the merger.
It counts the issues that the replay admitted with no work, recorded or drawn, and the slot-hours that they hold to `--to` (assumption 8), which `run.json` gives as `divergence.noWorkAdmissions` and `divergence.noWorkSlotHours`.
Under `neverStarted=drawn` or `drawn-done` it counts how many issues drew their work from issues of their own tree, from issues of their priority and repository, and from the whole window, which `run.json` gives as `divergence.drawnWork`.
It prints the scenario of the work of an issue that never started, the conflict model with the window that it was fitted on and its intervals, and the widest gap between the run's two scorecards.
Last, it prints each assumption that the run rests on, with the number of outbox rows that it finished under each.
It names each signal that `--limit` names whose load model gives no limit, which then has none in the run.
Under `signals` or `signals-backoff` it counts the deadlines that the run set, moved, dropped and fired, and says so where no signal has a limit, since then no signal binds.

`--out` receives these files:

| File | What it holds |
| --- | --- |
| `log.jsonl` | The daemon's lines on the simulated clock and the replay's lines for conflict draws and Git reads. |
| `scorecard.json` | The measures read from the states that the daemon's own admission passes left, the snapshot timeline. |
| `scorecard-line.json` | The same measures read back from `log.jsonl` and the replay's Dispatch events, as the measures of a record are read, the line timeline. |
| `run.json` | What ran, what it counted, the gap of each measure between the two timelines, the conflict model, the read counts (`gitReads`) and conflict hours (`gitLoopHours`) by repository, and what the run read of the signals. |
| `segments.jsonl` | Each work segment, with `--trace-segments` alone. |

`run.json` gives what the run read of R11's signals, for every run:

- `limits` is the limits that `--limit` gave, and `{}` for none; `idleAfter` is d.

- `tokenLimit` is the token limit that `--token-limit` gave, and is absent where it gave none.

- `unlimited` is each signal of `limits` whose load model gives no limit, with the model.
  Under `--token-limit`, the tokens model gives a limit also where it gives no daily test only because the window records no limit of the project's own ([`signals` and `signals-backoff`](#signals-and-signals-backoff)).

- `ceiling` is the cap that the run serves under as a ceiling: `--cap` where it gives one, else the one cap that the window's record serves over its span, and `null` where that cap changes in the span, which `caps` of `window.json` then gives.

- `slotPasses` is each slot that a pass took back (`take`) or gave out (`give`), in order, with the issue, the moment and the kind of the pass (`event`, `tick` or `deadline`), and `[]` for none.

- `deadlines` is each change of the project's deadline under a rule that admits by signals: each `set`, `moved` (with the moment it was due before, `previous`), `dropped` and `fired`, with the moment it happened, the moment it is due and the signal whose crossing it is, or `allowance` for the back-off's rise.

- `signalValues` is, at each moment that `--probe` gives, the value of each signal of `limits` once every act at or before that moment has run, and `null` for a signal with no record there.

- `signalSummaries` is, for each signal of `limits` that the run limited, how the signal read over the window's span, each minute read at its start once every act before it has run: its mean over the minutes where the window records it, its 95th percentile by the nearest rank, its largest value, how many minutes it was recorded in (`recorded`) of the span's (`minutes`), and the share of the span's minutes in which it held admission, at or above its limit (`heldShare`).
  A signal whose load model gives no limit is not limited, so it has none.

- `signals` is, for each signal in `signalSummaries`, its limit and the value that it read at it, the mean of its summary.
  `compare` gives them under "Signal values at their limits".
  It is absent for a run that limited no signal.

- `suppliedRefused` is, by UTC day, the minutes of the window's span in which the tokens model gave a refusal by the project's own limit under the token limit that `--token-limit` gave, in minutes whose provider record holds no limit of the project's own.
  The record cannot confirm them.
  It is `{}` where the run gave none.

`run.json` names the code that made the run, and the time at which the run started:

- `buildCommit` is the commit of the `legion` binary that made the run.
  It is the commit that the build linked in, else the commit that Go recorded from the checkout.
  Where the binary has neither, it is `unknown`.

- `startedAt` is the time at which the run started, on the wall clock and not on the simulated clock, in RFC 3339 and UTC.

- `ruleCommit` is a different commit: the commit at which the rule was first written as a value ([The slot rules](#the-slot-rules)).

A replay's scorecard names the assumptions from 1 to 25 that its measures rest on.
`run.json` and the output of `run` also name the tool's own assumptions, from 26 on.
The gap between the two timelines is the error of reading a record from its lines, which the scorecard of every record rests on.
The daemon writes the line of an issue's wait for its open children once for each generation of its tree.
So a line timeline reads that wait again, with no line, where the issue's last open child closed and another opened before its own first phase started.
The start state marks each such wait that the record told in its tree's current run before the window's start, so the replay's daemon does not tell it again.
A line timeline reads a design gate from the daemon's lines of its openings and closings, so an approval of another document of the issue opens nothing.
An issue that waits for a slot at the window's start began that wait where the record shows, in both timelines, since the start state is the record's.
The merge queue is GitHub's, so no code that the replay runs writes the line of a pull request that leaves it.
Just before the replay enters a recorded move back out of merging that such a line came before in the record, it writes that line to `log.jsonl` itself, so both timelines name the loop `queue_drop`.
The replay's own pull request record of an issue carries the number that a line of the record names for the issue's pull request, where no other issue's record takes that number in the same repository.
An issue that no line gives such a number takes its key's number where no other issue's record takes it in that repository, and otherwise a number above every other there.
GitHub numbers the pull requests of each repository, and the daemon finds a pull request by its repository, compared without case, and its number, so no two records of a run share a number in one repository.

A run is a function of its window, its rule, its branch, its seed and f.
Two runs of the same window, rule, branch, seed and f, made by the same `legion` binary, write the same `log.jsonl`, the same two scorecards and the same `run.json`, byte for byte.
The one exception is the fields of `run.json` that measure or name the run itself: `startedAt`, `wallSeconds`, `meanPassMillis`, `peakMemoryBytes` and `database`.

### `legion replay scorecard`

```sh
legion replay scorecard --window windows/acme-2026-10-05
```

`scorecard` reads every measure of a window from its record alone, by the code that reads a replay's measures, with no replay and no database.
It prints one measure on each line as `<name> <value>`, and then each issue's first slot and done.
`--json` prints the scorecard as JSON.

### `legion replay signals`

```sh
legion replay signals --window windows/acme-2026-10-05 \
  --at 2026-10-06T10:00:00Z --at 2026-10-06T16:00:00Z \
  --limit kills=2 --limit queue=1.5 --idle-after 10m
```

`signals` reads each signal of [The signals](#the-signals) at each moment that `--at` names, from the window's record alone, with no replay and no database.
It prints a block for each moment, with a line for each signal: `idle`, `queue`, `provider`, `refusals`, `kills` and `approvals`.
Each line gives the signal's value, or `unrecorded` where the window holds no record of the signal at that moment.
`idle` and `provider` are shares, `queue` is in hours, and `refusals`, `kills` and `approvals` are counts, with the refusals of the project's own limit and of the provider beside their count.
Under a signal, each `--limit <signal>=<value>` that names it adds a line with the next moment at which the signal would cross that limit if no new event came.
That line says `no crossing due` where none would come, and, for `approvals`, that the signal changes only at events.
`--token-limit` gives the project's token limit, which the provider's headroom reads in each minute whose record holds no limit of the project's own, as a replay that is given it reads it (assumption 49).
Where the window holds the launcher's lines, `signals` then prints how many of the memory guard's kills both the launcher's lines and the daemon's hold, and each kill that one of the two lacks ([Signal `kills`](#signal-kills)).

| Flag | What it gives |
| --- | --- |
| `--window <dir>` | The window (required). |
| `--at <time>` | A moment, in RFC 3339, given once for each (required). |
| `--limit <signal>=<value>` | A limit of `idle`, `queue`, `provider`, `refusals`, `kills` or `approvals`, at or above zero, given once for each. |
| `--idle-after <duration>` | d, how long a holder has been idle before the idle signal counts it (1 hour by default). |
| `--token-limit <tokens a minute>` | The project's token limit in uncached input tokens a minute, a whole number above zero, which the provider's headroom reads in each minute whose record holds no limit of the project's own (assumption 49). |

### `legion replay models`

```sh
legion replay models --window windows/acme-2026-10-05 --against windows/acme-2026-10-12
```

`models` fits the three load models of R11 on a window's record alone, with no replay and no database, and tests each one by R11's daily test and, with `--against`, by R11's test across two records.
A replay keeps each work segment's recorded length, so it cannot show a refusal, a memory kill or a longer merge queue that more work in flight would have caused.
So a replay under another rule takes the provider's signal, the kills and the merge queue's wait from these models (assumption 45).
Each model keeps what the window records, and moves with the work in flight of the replay only as its form says.
No model reads the pods' memory series, and `collect` no longer collects it.
The work in flight is the record's issues in flight at each moment, as the scorecard counts them (`inFlight`).
Each model is fitted and tested only where the window holds its record (assumption 47), and the output names each span that it skips, with the reason.

- **`tokens`**, of the signal `provider`: the project's uncached input tokens a minute.
  Form: a minute's tokens are its recorded tokens for each issue in flight in the record, times the issues in flight in the replay, `tokens(m) = recorded(m) / recordedInFlight(m) × inFlight(m)`, each count of issues the average over the minute.
  A minute with no issue in flight in the record keeps its recorded tokens.
  A minute whose modelled tokens pass the project's own limit in force is a refused minute.
  It counts the record's own refusals per refused minute, or 1 where the record holds no refused minute, rounded so that the first n refused minutes of a replay count n times that number together.
  So a refusal by the project's own limit comes from the model, and a refusal that the provider gives every project is kept as recorded (assumption 46).
  A minute where the window holds no record of the headroom keeps its recorded refusals, since the model has no limit to read there.
  The model is fitted on every minute of the provider's record, with a limit or without one.
  Its test reads only the minutes where the window holds a record of the provider's headroom: a limit of the project's own, from its first observation on (assumption 48).
  A day with no such minute has no test, never a pass.
  Parameters: the minutes fitted, the uncached input tokens for each minute of an issue in flight, the refused minutes recorded, and the refusals per refused minute.

- **`memory`**, of the signal `kills`: the memory guard's kills.
  Form: each recorded kill belongs to the work segment of the issue whose pod it ended, at its offset in that segment, and a replay places it at the same offset of its run of that segment.
  A replay that does not run the segment has none of its kills.
  Work that the record never ran, a drawn scenario's, takes the day's recorded kills for each hour of an issue in flight.
  Each drawn issue counts on its own: its issue-hours in flight, each times the rate of its day, add up, and the issue has a kill each time its sum passes a whole number.
  So no other issue and no rule's timing moves the kills of a drawn issue.
  A kill that the record cannot place in a segment of its issue is counted apart and kept at its recorded time.
  The model is fitted and tested only where the window holds the record of the kills, the launcher's lines, so only where the memory guard ran.
  Parameters: the kills recorded, those placed in a work segment of their issue and those counted apart, the issue-hours in flight, and the kills for each hour of an issue in flight.

- **`queue`**, of the signal `queue`: the merge queue's wait.
  Form: each repository has its own queue.
  A pull request that the record removed before it reached the head of its queue keeps its recorded wait: one that left while a pull request that stood in the queue at its entry, having entered at or before it, still stood.
  Each other pull request leaves in its order of entry, at the record's next exit from the head of its queue after its entry that no pull request before it took.
  While the record's queue stood empty, and after the end of the queue's record, the queue serves at the window's own rate of exits from its head for each hour in which it held a pull request: a pull request that reaches the head then leaves one interval of that rate later, where that comes before the next recorded exit.
  Before the end of the queue's record, the rate serves only while the record's queue stood empty, so a pull request that the record shows still in the queue at its end stands there to that end, as recorded.
  A recorded pull request enters at its recorded offset from the start of the work segment of its issue in which the record shows its entry, the issue that links it in its Dispatch events or whose lines name it.
  One that the record shows in no such segment is counted apart and enters at its recorded time.
  A pull request that no recorded exit and no rate serves stands in the queue, and the model counts it.
  A day on which no pull request left the queue has no test.
  Parameters, for each repository: its exits from the head, its pull requests removed before the head, its hours that held a pull request, and its exits from the head for each such hour; and the pull requests counted apart.

R11's daily test, at the recorded work in flight: on a day with at least 10 refused minutes or kills recorded, the model gives within 30% of the record, and on each other day it gives at most 10; the merge queue's model gives its median wait within 30% of the record on each day.
For the tokens it tests the count of refused minutes, the minutes in which the project's own limit refused a request, and the output gives the project's own refusals beside them.
At the recorded work in flight the models of the kills and of the merge queue give the record by their form, so for them this test shows only that the model keeps the record, and the output says so beside their verdicts.
A model passes where it passes on each day that it tests, fails where it fails on one, and gives no test where no day tests it.

R11's test across two records, with `--against`, holds the rate that each model rests on, on `--window`, within 30% of the same rate on `--against`, which is the reference.
The rates are the tokens for each minute of an issue in flight, the kills for each hour of an issue in flight, counted from `--kills-from` on each record, and, for each repository whose queue has at least 50 exits from its head in each record, those exits for each hour in which the queue held a pull request.
The kills are counted from 2026-11-18 by default, the day after the first full day of the memory guard in one deployment's record, which keeps that day as it is.
A repository with fewer exits has no test, and the output names it with its exits on each record.
A model passes where each rate that it rests on passes, fails where one fails, and gives no test where none is tested.

A model gives a limit for its signal only where it passes the daily test and, with `--against`, the test across two records.
Without `--against`, the output says that the test across records did not run, and a model gives its limit on the daily test alone.

`models` prints, for each model, its form, its parameters, its verdicts, whether it gives a limit, each span of the window's span that it skips with the reason, a table of the days, and its rates by load.
A day is a UTC day of the window's span.
Each row gives the day's issues in flight, the hours of record that its test reads, the recorded and the modelled value with the test, and what the model gives beside it: the project's own refusals, the tokens a minute and the provider's refusals for `tokens`, and the kills counted apart and the day's kills for each hour of an issue in flight for `memory`.
The rates by load give the signal's rate in each range of 20 issues in flight that the window records, such as `[260, 280)`, with the hours that the window spent in it where it holds the model's record.
They are the tokens for each minute of an issue in flight, a minute in the range of its average issues in flight; the kills for each hour of an issue in flight; and, for each repository, the exits from the head for each hour in which its queue held a pull request.
With `--against`, a last section gives each rate on each record, their ratio, the test, and each repository that has no test.
`--json` prints the same as JSON (`LoadModels` in `internal/replay/models.go`), which the limit test reads.

A replay reads each model's signal from the replay's own timeline: its issues in flight at each moment, the stretches in flight of each issue whose work the record never ran, its start of each recorded work segment, and each pull request of work that the record never ran entering the queue.
It gives the provider's headroom and refusals, the kills of the hour before and the queue's wait at a moment, each by the signal's own definition ([The signals](#the-signals)), and `LoadModels.RecordTimeline` gives the record's own timeline, at which the daily test reads the models.
`LoadModels.Replay` (`internal/replay/model_replay.go`) reads a whole timeline at once.
A run reads it as it goes through `LoadModels.Reader` (`internal/replay/model_reader.go`), which models each minute once it ends, each kill once its segment or its drawn issue's own hours bring it, and each pull request once it enters, and gives at each moment what `LoadModels.Replay` gives over the whole timeline.
Each modelled value at a moment reads only the course up to that moment, so nothing that the reader has given changes later.

| Flag | What it gives |
| --- | --- |
| `--window <dir>` | The window (required). |
| `--against <dir>` | A second window, for R11's test across two records. |
| `--kills-from <time>` | The moment, in RFC 3339, from which the test across records counts the kills (`2026-11-18T00:00:00Z` by default). |
| `--json` | The models as JSON. |

### `legion replay validate`

```sh
legion replay validate \
  --window windows/acme-2026-10-05 --rule main \
  --pg-dsn '<dsn>' --out runs --parallel 4
```

`validate` replays the window under the rule that its daemon ran, on every branch, each at f = 1, 1.2 and 1.6.
A branch that draws, a conflict or the work of an issue that never started, runs the seeds 1 to 5.
A branch that draws nothing runs the seed 1 alone.
Up to `--parallel` runs run at once, each in a database of its own.
Each run's files go under `--out` in `<window>/<rule>/<branch>@<f>/seed-<n>/`, and a run whose files are there already does not run again.
`validate` deletes each run's log once it has read it, unless `--keep-logs`.
With `--main-only` it runs only the main branch at f = 1.

It prints, for each branch and each measure, the recorded value, the replayed value, the gap, the allowance and the test.
The replayed value is the mean of the seeds, with the values of the lowest and the highest seed.
It prints each issue's error in its first slot and in its done, with their median and 90th percentile.
It reads the record and each replay from their line timelines, so that it compares like with like.
It prints whether the window's main branch, `main` at f = 1, passes, and which branches give no comparison.
The allowances are:

- 10% of the issues finished, and of the slots given out on each day

- 0.05 of the priority taken by holders

- 60 minutes at the median of the issues' errors in their first slot

- 30% of every other measure

A branch that misses an allowance gives no comparison.
A window whose main branch misses one gives no comparison at all.
`priorityTakenBanded`, priority taken in today's line's order, is printed beside the other measures and is not one of these tests.
They read priority taken by rank alone (`priorityTaken`), the order of the recorded windows' lines.
R7 judges a rule on it by its error across the change of the rule, not by an allowance of these tests.
The wait of urgent work (`urgentWaits`) is not one of these tests either, and `validate` does not print it.

Beside the tests, `validate` prints diagnostics that do not affect the gaps:

- For each branch and seed, the number of issues that the replay gave a first slot in the window and the record did not, the number that the record gave one and the replay did not, and the seed's own median error of the first slot, over the issues that the record or that seed gives one, a missing one standing at `--to`.
  It stands beside the first-slot test's statistic, the mean over the seeds of each issue's error (assumption 44), in the table and in `--json` (`firstSlotMedianMinutes`).

- For each branch, the trees whose design gate stood closed in the replay more than 30% longer or shorter than in the record (`gateClosedHours`).

On R5's branch of the recorded conflicts, the replay of the recorded rule should start no issue that the record did not start in the window, and file each issue that the record filed in the window.
`validate` names, for each f and each seed, the issues that break either, in the section "The issues that break R5's branch of the recorded conflicts":

- each issue that the replay gives a first slot in the window and the record gives none

- each issue of the window's project that the record files in the window and the replay does not file in it, read from each scorecard's `filed`

The names gate nothing: no test of R5, no verdict of the window and no decision of a comparison reads them.
`--json` gives them as `breakers` on the main branch at each slowdown, with `startedNotRecorded` and `filedLeftOut` for each seed.

For each git run, `validate` gives a table by repository and seed.
The table crosses Git's answer or unanswered kind with the recorded outcome at each end read.
It counts boot reads separately, with no recorded outcome.
It gives the share of reads Git could not answer and the share of conflict hours started by draws rather than answered reads.
Each git and drawn run reports the hours spent in conflict loops by source.
`validate` also says whether the drawn branch passes the window.
A failing drawn branch does not stop a comparison on the passing branches, but the tool is not finished and the pull request does not merge while it fails on window one.

A validation whose replays give no issue a first slot or a done, where the record gives some, is refused, with an error that names the window, because each issue's error would stand at `--to` less the record's time.
`--from-runs <dir>` reads the validation from the runs that a `validate` or a `compare` wrote, and runs nothing.
Each run keeps the token limit that it ran with in its `run.json`, so `--token-limit` does not apply there.
`--token-limit <tokens a minute>` gives every run the project's token limit, as `run` takes it, and a run that `--out` holds already that ran with another token limit, or with none, is refused, as one that ran at another cap is.
A window whose record names no rule, because its parts name none or more than one, keeps each issue's times in every part that it is validated under.
`--json` prints the validation as JSON.

With `--cap-backtest --cap <n>`, `validate` runs the cap backtest of R5 in place of every branch.
It runs the main branch at f = 1, with its seeds, twice: at the caps that the record shows, into `--out`, and at the one cap `--cap` from the window's start to its end, into `cap-<n>` in `--out`.
It then counts the slots that each replay and the record give out in each span of one cap that the daemon served.
A span runs from the boot of a daemon process to the next boot, at the cap that the process served under, and the spans of adjacent processes of one cap are one span.
It prints the validation of the main branch at the recorded caps, and after it the backtest, with a row for each span.
The backtest passes when both of these hold:

- at the recorded caps, each span gives out the record's slots within 10%

- at the cap `--cap`, each span whose cap is not `--cap` misses the record's slots by more than 10%, and a span at `--cap` is not read

A backtest that fails either, or that reads no span, says that the tool cannot tell a change of the cap.
A span of a cap that the daemon served for a few hours can give out as few as about five slots, and there a miss of one slot fails the span.
So the backtest names each span in which one slot is more than 10% of the record's count.
It also names each span whose cap is not `--cap` in which the record gave out no slot and the replay at `--cap` gave out none either: that replay did not miss the record there by more than 10%, so the span fails.
Collect the window of a cap backtest with no run-out, so that its replays end at `--to`.
`--json` gives the backtest as `capBacktest`, with each span's `from`, `to`, `cap`, `recorded`, `atRecordedCaps`, `atFlatCap`, `recordedPass`, `flatRead` and `flatPass`.

### `legion replay forecast`

```sh
legion replay forecast \
  --before windows/acme-2026-09-20 --after windows/acme-2026-10-05 \
  --rule main --pg-dsn '<dsn>' --out runs --parallel 4
```

`forecast` replays the window recorded before a change of the daemon's rule under the rule after the change, on the main branch.
It compares that forecast with the record of the window after the change.
On the same branch, it also replays the window before under the rule that its daemon ran, the window after under the rule that its daemon ran, and, as the forecast's control, the window after under the rule before the change.
A window whose parts name no one rule that its daemon ran is refused.

It compares rates per hour, averages and shares, never totals, because the two windows differ in length and in work.
An average is over time, or, for the wait of urgent work, over the issues.
Each number passes within twice its allowance, because a forecast carries the model's error twice: once for the window and once for the rule.
A send-back cause that the change of the workflow defines again is scored only inside the total of all send-back loops, `loops[all]`, and inside the total of those causes, `loops[redefined]`.
Those causes are red checks or a conflict, a conflict, and a send-back by the architect or the merger.
Each cause that both releases define the same way is scored apart.

Beside the numbers that R5 gives an allowance, the forecast gives one number for each measure of R4 that has no rate, average or share of its own:

- `<count>.max daily mean`, for `waitOnlyForSlot`, `startedWithoutSlot`, `pastReviewHolding`, `pastReviewNotHolding` and `inFlight`: the mean over the window's UTC days of the day's largest count, which each scorecard's `dailyMaxima` gives

- `loops[redefined].rounds per hour` and `loops[redefined].hours per hour`: the loops of the causes that the change defines again, together

- for P0 and for P1 apart, `urgentWaits[<P>].issues per hour`, `urgentWaits[<P>].firstSlot.meanMinutes` and `urgentWaits[<P>].done.meanMinutes`, the mean wait over the issues with a cut wait at its length, which each scorecard's `urgentMeans` gives, and `urgentWaits[<P>].firstSlot.cut share` and `urgentWaits[<P>].done.cut share`, the cut waits as a share of the issues

The gap of each of these numbers is the replay's own error on its measure, which R7 reads.
That error is the larger of the two windows' errors, each window replayed under its own rule against its own record.
The output marks these numbers as measured, and `--json` gives the error as `error`.

A number that the change moves is one where the forecast stands more than twice its gap from the window before replayed under its own rule.
On such a number, where the control too stands within twice its gap of the record after the change, the forecast cannot tell the two rules apart.
The output names each such number, and gives, for every number, the window before under its own rule, whether the change moves it, and the control.
The control gates nothing: the forecast passes or fails on its own numbers alone.
The forecast also names the workflow commits between the two windows' daemons, which it carries beside the rule.
With `--from-runs`, `--before` and `--after` name the windows by the names of their directories, the runs must hold all four replays, and `--token-limit` does not apply.
A forecast into an `--out` that holds runs already, a `validate`'s or an earlier forecast's, runs none of them again where the same `legion` binary made them, and refuses a run that another binary made.
It names each part that it so found whole, with the commit of the binary that made it.
A run into the directory of a `validate` of every branch keeps each branch of that part in its `part.json`.
`--json`, `--parallel`, `--keep-logs` and `--token-limit` work as in `validate`.

### `legion replay compare`

```sh
legion replay compare \
  --window windows/acme-2026-09-20 --window windows/acme-2026-10-05 \
  --pg-dsn '<dsn>' --runs runs --out comparison.md --parallel 8
```

`compare` compares the slot rules on two windows, and writes the result as one Markdown document.
Beside the document, it writes the issues file, which gives the times of each issue under each rule.
Give the window before the change of the rule first.
It compares each `--rule`, or every rule that the repository keeps where you give none, and adds `main` and each window's own rule where you leave them out.
Under each rule, it replays each window on every branch, and on the main branch at each f from 1 to 2 in steps of 0.1 for the break-even search.

The document gives:

- the build commit of each run, the commit of the `legion` binary that made it, with the number of runs that each build commit made

- each window's scorecard, read from its record, with the issues done by anyone (`doneByAnyone`) beside Legion's `done`, and the wait of its P0 and P1 work ([The wait of urgent work](#the-wait-of-urgent-work))

- for each rule with a grace, what the grace keeps on each window, read from the window's record: how many of the waits on another issue that gave a slot back the grace keeps the slot through, of how many such waits, and the slot-hours that it keeps ([`wait-grace` and its grace](#wait-grace-and-its-grace))

- each window's validation under its own rule, with the branches that give no comparison

- the forecast across the change, with the workflow commits that it carries too, its control and each number on which it cannot tell the two rules apart

- which window measures how a rule changes conflicts

- the value of every measure for every rule, window and branch, with the lowest and the highest seed, the wait of P0 and P1 work among them, and below each table the issues done by anyone, which only each run's line timeline gives

- below the table of each branch that passes the validation, each rule's difference from `main` on each measure that a rule is judged on: its mean over the seeds with its 95% interval, or a single run's difference, its verdict, the error that it is held to, and for a tie the seeds that a difference of its size would need

- each run's admissions of an issue that holds its slot with no recorded work, and the slot-hours that those issues hold

- for each run that limited a signal, the limit of each signal and the value that it read at it, under "Signal values at their limits"
  A comparison whose runs limit no signal gives none.

- for each window and seed, the pairs of rules compared, the stretches that the two runs of a pair both drew, and how many of them drew another value, which must be none, with each pair, branch and stretch at which one did

- the name of the issues file, and what it gives

- the gap of each measure between the snapshot and the line timelines

- each rule's break-even factor: the f at which its rank against `main` changes, or that no f up to 2 changes it

- the rule that it names best, or that no rule is best, with whether each window lists an experiment, the replay's own error that each difference is held to, each rule's trade, whether the rule keeps the owner's direction, and, beside the trade of each candidate that was added to the rules of the comparison's spec, the reason for that candidate

- the assumptions and the scenario that each run used

A rule is judged against `main` on twelve measures, each read from the snapshot timeline.
They are the issues finished (`done`), the hours of started work that waits only for a slot (`waitOnlyForSlot.hours`), the priority taken by holders in today's line's order (`priorityTakenBanded`), the hours of the other delays (`otherDelays.hours`), and the wait of urgent work ([The wait of urgent work](#the-wait-of-urgent-work)).
The wait of urgent work gives eight of them: for P0 and for P1 apart, the median and the 90th percentile of the wait to the first slot and to done, in hours, such as `urgentWaits[P0].firstSlot.medianHours`.
Every rule is read in today's line's order, whatever order its own line keeps, so that the rules stand on one scale.
Both windows were recorded before the line put priority first, so each table gives priority taken by rank alone (`priorityTaken`) beside it.

On each branch that passes the validation, a rule's difference from `main` is taken seed by seed: the rule's run less `main`'s run at the same seed.
Each draw is keyed to the seed, the issue and the stretch, so the two runs draw the same value at each stretch that both of them reach.
Over the n seeds that both ran, the mean difference d has the 95% interval d ± t·s/√n.
Here s is the standard deviation of the differences, and t is the 0.975 quantile of Student's t at n − 1 degrees of freedom, 2.7764 at the 5 seeds of a branch with a draw.
A rule is better or worse than `main` on a measure only where that interval excludes zero and its near end is farther from zero than the replay's own error on that measure.
That error is the larger of the two windows' errors: each window replayed under its own rule on `main@1`, the mean over its seeds, against its record, as the forecast measures it.
A branch that draws nothing runs one seed, and its difference is a single run's: it is better or worse only where it is farther from zero than the error.
Each other difference is a tie, and the document gives the seeds that a difference of its size would need, ⌈(1.96 s / d)²⌉, or `never` where d is 0.
That count is the number of seeds at which the interval would exclude zero, so a tie within the error stays a tie at any number of seeds.

A rule is best when it keeps the owner's direction of 2026-11-24 and, in every branch of both windows that passes the validation, it is worse than `main` on no measure and better on at least one.
The owner's direction is the start of a P0 issue outside the cap, and below P0 the next issue in priority order while each signal is under its limit, with the cap as a ceiling only.
Of the rules that the repository keeps, `signals` and `signals-backoff` keep it.
Every other rule is measured beside them, and `before-p0`, the rule before the start of a P0 issue outside the cap, is measured for reference and never named.
A rule that admits issues that the record never started is read on `main+neverStarted=drawn` and `main+neverStarted=drawn-done` as on every other branch.
On a window whose `window.json` lists a workflow experiment in the `random` mode, no difference is paired, so no rule is better or worse than `main` there, and no rule is named best while that window is compared ([A limit beside the assumptions](#a-limit-beside-the-assumptions-a-workflow-experiment-drawn-at-random)).

The part on the graces gives one row for each window and each rule with a grace.
For the fixture windows `one-minute-wait` and `two-waiters-one-blocker` of the tool's tests, it reads:

```text
## What each grace keeps, read from each window's record

| window | rule | grace | waits it keeps the slot through | slot-hours it keeps |
| --- | --- | --- | --- | --- |
| one-minute-wait | `wait-grace` | 5m0s | 1 of 1 | 0.0167 |
| one-minute-wait | `wait-grace-1h` | 1h0m0s | 1 of 1 | 0.0167 |
| two-waiters-one-blocker | `wait-grace` | 5m0s | 0 of 2 | 0.1667 |
| two-waiters-one-blocker | `wait-grace-1h` | 1h0m0s | 0 of 2 | 2 |
```

Between the heading and the table, the document has a paragraph that says how to read the rows, which the example leaves out.

The issues file is at the path of `--out`, with the extension replaced by `-issues.md`.
For `--out comparison.md`, the issues file is `comparison-issues.md`, and the document names it.
For each window and each rule, the issues file gives each issue that held a slot in the record or on the main branch of the rule's replay.
For each such issue, it gives the first slot and the done in the record and in the replay, in UTC, to the minute.
Each replayed time is the median over the seeds of the main branch.
A seed that gives an issue no time counts as later than every time, so the median gives no time where half of the seeds or more give none.
Where the median gives no time, the file writes `no slot` or `not done`.
The rows follow the recorded first slot, and the issues that hold no slot in the record come last.
For a comparison of the tool's tests, one section of the issues file reads:

```text
## window-1 under `returned-by-first-slot` (seeds: 3)

| issue | first slot, recorded | first slot, replayed | done, recorded | done, replayed |
| --- | --- | --- | --- | --- |
| ACME-2 | 2001-03-05T09:00Z | 2001-03-05T09:40Z | 2001-03-05T11:00Z | 2001-03-05T12:20Z |
| ACME-1 | 2001-03-05T09:30Z | 2001-03-05T09:00Z | 2001-03-05T12:00Z | 2001-03-05T10:40Z |
| ACME-3 | 2001-03-05T10:00Z | no slot | not done | not done |
| ACME-5 | no slot | 2001-03-05T10:40Z | not done | not done |
```

Each window's scorecard and each table of every measure give the wait of P0 and P1 work in fourteen rows, in hours ([The wait of urgent work](#the-wait-of-urgent-work)).
A table gives, under each rule, the mean over the branch's seeds, with the lowest and the highest seed beside it for more than one.
A seed that has no issue of a priority counts 0 in its issues and its cut waits, and counts in no median or percentile, which read `none` where no seed has one.
The record's own rows cut each wait at the end of the window's record.
For a comparison of the tool's tests, one window's rows on the main branch read:

```text
| P0 issues | 3 | 3 |
| P0 to first slot, median | 5 | 0.9167 |
| P0 to first slot, 90th percentile | 5 | 5 |
| P0 to first slot, cut waits | 2 | 1 |
| P0 to done, median | 5 | 2.9167 |
| P0 to done, 90th percentile | 5 | 5 |
| P0 to done, cut waits | 2 | 1 |
| P1 issues | 0 | 2 |
| P1 to first slot, median | none | 0.375 |
| P1 to first slot, 90th percentile | none | 0.75 |
| P1 to first slot, cut waits | 0 | 0 |
| P1 to done, median | none | 2.375 |
| P1 to done, 90th percentile | none | 2.75 |
| P1 to done, cut waits | 0 | 0 |
```

To split the work, run each part with `--only-window <name>` and `--only-rule <name>` into one `--runs` directory, with no `--out`.
Then write the document and the issues file from the parts' directories with `--from-runs <dir>... --out <file>`, which runs nothing, and to which `--token-limit` does not apply.
`--token-limit <tokens a minute>` gives every run the project's token limit, as in `validate`.
`validate`, `forecast` and `compare` print on standard error, as each run ends, its facts, passes, seconds and peak memory, so that you can size a job.

### `legion replay limits`

```sh
legion replay limits \
  --window-one windows/acme-2026-10-05 --window-two windows/acme-2026-09-20 \
  --models runs/models-one.json --models-reverse runs/models-other.json \
  --baseline runs --runs limits --token-limit 6000000 --ceiling 171 \
  --plan limits-plan.json
```

`limits` is the limit test of R11.
It plans the replays that name each signal's limit and, given the directories of their runs, names each limit by R11's rule and prints its table.
It runs no replay.
Each entry of its plan is one `legion replay run`, one index of an indexed Job, and the runs go to `--runs`, which `limits` reads back.

The list of limits of each signal is its 50th, 75th, 90th, 95th and 99th percentile over `--window-one`'s record, read at each minute where the window records the signal, and off.
`--window-one` is the window whose record the lists come from, and whose main branch ranks the limits by the issues they finish.
The idle share is read at d of an hour.
Every limit passes through the shared validator of the signals' ranges (`signals.CheckLimit`), the same that `legion replay run --limit` applies.
A percentile outside the signal's range is left out of the list with the validator's reason, and is never clamped into it.
A percentile that repeats a lower percentile's limit is left out as its repeat.
`off` is the reference row and is never named.

A signal that a load model gives has steps only where its model passed R11's tests.
`--models` is the output of `legion replay models --against --json` for `--window-one` against the other record, and `--models-reverse` the output for the other record against `--window-one`.
The model of a signal must fail the daily test on neither record, pass it on at least one, and pass the test across the two records in both outputs.
A window that records no token limit of the project's own has no daily test for the tokens model, so the tokens model gives a limit for the provider's headroom and refusals only under `--token-limit`, the project's token limit, which every run of the plan takes (`legion replay run --token-limit`, [assumption 49](#assumption-49-the-projects-token-limit-is-the-one-that-the-replays-caller-gives-in-each-minute-whose-record-holds-none)).
The provider's headroom and its refusals each have a limit and a sweep of their own, since each has its own range, and the table groups them under the provider.
The idle share and the approvals come from the replay itself, have no model and always have steps.
A signal whose model failed has no step, and the output says why.

The steps are these.

- **Step A** runs each signal alone, with the others off, at each limit of its list, and runs off, on the main branch of both windows at f 1.
  The main branch draws nothing, so each run has the seed 1 alone.

- **Step B** takes, for each signal, the limit with the most issues finished on window one's main branch among those that no other measure is worse at than under today's rule on step A's branches.
  A tie goes to the loosest limit.
  It runs that limit and the next tighter one on every branch of both windows that passes R5, with the seeds of the baseline's runs.
  Where the first is worse on a branch, the next tighter limit is run on every branch, until one holds or the list ends, and the result then names no limit.
  The idle share's named limit is also run at d of 10 minutes and of 4 hours, and the output gives each beside the hour's.

- **Step C** runs `signals` and `signals-backoff` with every signal at its named limit on every branch of both windows.

Each step runs at two ceilings, the recorded cap and `--ceiling`, the most issues in flight that any replayed span records.
Under the recorded cap alone, a rule that can only admit fewer would make the loosest limit the best.

A limit holds where no measure other than the issues finished, of the owner's three costs and the wait of urgent work, is worse than under today's rule, `main`, in R7's sense, on any branch that passes R5.
The comparison is the one `compare` makes: the paired difference of each measure over the seeds, held to the replay's own error on the measure.
A verdict of `better`, `tie`, `none` or `not paired` is not worse.
The issues finished rank the limits and are never a reason to reject one, and the table gives their verdict beside each limit.
The branches that pass R5 are those that the validation of each window under its own rule says give a comparison.
`--baseline` names the directories of runs of `validate` and `compare` that hold both windows' records and their runs of `main` and of each window's own rule.

The output gives, for each signal and ceiling, a row for off and for each limit, with the issues finished, the three costs and the signal's own reading on window one's main branch, the mean over the seeds.
The signal's reading is its mean over the minutes where the window records it, its 95th percentile, its largest value and the share of the window's minutes in which it held admission, from `run.json`'s `signalSummaries`.
The row also gives the most issues in flight that any run of the limit held, and whether the limit holds, or the first measure it is worse on.
Below the table is the limit named for the signal, with the issues it finished on window one's main branch and what off finished, or that no limit holds.
Step C is a table of the two rules at each ceiling with how many branches they stood better, worse, as a trade and level on.

The output names the range of work in flight that the windows record, from the lowest day's average of issues in flight in the models' days to the larger of the two windows' scorecard maxima, the most at any instant.
A limit holds only inside that range.
A named limit whose runs hold more issues in flight than the range's top is an extrapolation, and the output says so beside it.

The output also says what each limit rests on.
For a signal that a model gives, it lists each tested day of the daily test with the recorded and the modelled value and the record it is in, and the ratio of the test across the two records.
For the provider's signals it also gives the token limit, the span of each window's provider record, and the refused minutes by day that the model gives under the token limit in minutes whose record holds no limit of the project's own, which the record cannot confirm (`run.json`'s `suppliedRefused`).

`limits` reads what the runs hold, so it is run again after each step.
Step B waits for the runs of step A, and step C for the runs of step B, and the output says which step it waits for.
`--plan` writes the entries still to run, as a JSON list, to the file named.
Each entry gives its step, signal, window, rule, branch, slowdown, seed, limits, d, cap, token limit, the directory under `--runs` that its run goes to (`out`), and the arguments of `legion replay run` that make it, but for `--pg-dsn`, which the Job adds.
A run whose directory holds its three files is not planned again, and a run that ran with other limits, d, cap or token limit than its entry names is refused.

| Flag | What it gives |
| --- | --- |
| `--window-one <dir>` | The window whose record the lists of limits are read from (required). |
| `--window-two <dir>` | The other window of the sweep (required). |
| `--models <file>` | `legion replay models --against --json` of window one against the other record (required). |
| `--models-reverse <file>` | The same of the other record against window one (required). |
| `--baseline <dir>` | A directory of runs of `validate` or `compare`; repeat for more (required). |
| `--ceiling <n>` | The second ceiling, a cap above zero, beside the recorded cap (required). |
| `--runs <dir>` | The directory that the sweep's runs are under. |
| `--plan <file>` | Write the entries still to run to this file. |
| `--token-limit <n>` | The project's token limit in uncached input tokens a minute, which every run takes. |
| `--json` | Print the result as JSON. |

## The measures

Every scorecard gives these measures over the window, from `--from` to `--to`, also where the window has a run-out after `--to`.
Nothing after `--to` counts, and a stretch, a wait or a loop that goes on past `--to` is cut at `--to`, but for the wait of urgent work, which runs on through the run-out.
A replay reads them from its own run, and `legion replay scorecard` reads them from the record, by the same code.
So a difference between the two is a difference of the replay, not of two readings.

An average is the integral of a count over the window, divided by the window's length.
`hours` is that integral in issue-hours, and `max` is the largest count at any moment.
Each name below is the name that `scorecard` prints and that a scorecard's JSON holds.

### Issues finished

- `done`: the issues whose move to done falls in the window, each once.
  `doneAt[<KEY>]` gives the time of each.

- `doneByAnyone`: the issues of the window's project that Dispatch shows reaching done in the window, by any actor, each once.
  It holds Legion's own done and each close that a person or a session outside Legion made.
  Only a trace gives it: the record's scorecard and a replay's `scorecard-line.json` give it, and `scorecard.json` does not (assumption 41).

- `workLeft`: the issues in flight at `--to`.
  An issue that reached done and then reopened counts in both.

### Started work that waits for a slot

The tool gives two readings side by side, and the issues past their review apart.
Each has an `.average`, a `.max` and `.hours`.

- `waitOnlyForSlot` is reading (a): the returned issues that hold no slot and that no wait holds back.
  It counts an issue from the end of its wait, or from the owed start of an issue that waits on nothing, to its slot.
  `legion state` calls it `wait only for a slot`.

- `startedWithoutSlot` is reading (b): every returned issue that holds no slot, its waits on other issues included.
  It counts an issue whose start is owed, an answered root, and an issue that gave its slot back for a wait, until the pass that gives it a slot.
  Under `cover`, a covered waiter counts too.
  A started parent in `awaiting_children` that holds no slot counts too while it stands there, though it stands in no line (cybermonklabs/legion#4592).
  `legion state` calls it `in all`.

- `pastReviewHolding` and `pastReviewNotHolding`: the issues in accepting, retro or merging, or held from one of them, with a slot and without one.

Three more numbers describe the slots given out:

- `slotsGivenByDay[<day>]`: the slots given out on each UTC day, which are each promotion, each child's first phase, each slot that a returned issue takes, and each cover.

- `medianSlotWaitMinutes`: the median, over the slots given out, of the minutes from the moment that the issue which takes a slot began to wait for one to the slot.

- `firstSlot[<KEY>]`: each issue's first slot in the window.

### Priority taken by holders

`priorityTaken` and `priorityTakenBanded` read, at each moment, every pair of a holder and an issue that waits for a slot.
Each is the share of those pairs in which the holder ranks below the waiting issue, as an average over the window.
An issue that waits for a slot is a member of the line, in either part, that holds no slot, that no wait holds back, and that `admission.blocked` and `admission.held` do not name.
Each reads every rule in one order, so that rules whose own lines order differently are read on one scale.
`priorityTaken` ranks by `record.RankLess` over the Dispatch rank of each issue's tree root, and then over the issue's own rank: the order of the lines that the recorded windows ran.
`priorityTakenBanded` ranks in today's line's order: first by the band of the priority Dispatch resolves for each issue ([The priority Dispatch resolves](#the-priority-dispatch-resolves)), placed as the line places an issue (`record.PriorityOf` over the resolved priorities), P0 first and an issue with none after P3, and then as `priorityTaken` ranks.
It reads that priority under every rule, a recorded rule among them, so the rules compare on one banding of the line.
`validate` tests `priorityTaken` and the forecast reads it, and `compare` judges the rules on `priorityTakenBanded`.
At a moment with no pair, each share is 0.

### Other delays

- `loops[<cause>]`: the `.rounds` and `.hours` of the send-back loops of each cause.
  Each move back to an earlier phase is one round.
  Its hours run from the move back to the issue's next entry into the phase that it left, or to `--to`.

- `waits[<word>]`: the `.count` and `.hours` of the waits on other issues, by what ended each.
  A wait runs while an issue in flight, before its review, waits on an unfinished `blocked_by` link, or, at admitted under an open design gate, on an open child.
  A started parent that the admission pass moves into `awaiting_children` (cybermonklabs/legion#4592) waits there on its open children, from the move until the last of them closes or moves out.
  Under `cover` a started parent with an open child keeps its phase instead, and its wait on its open children counts in each phase from `planning` through `accepting`, from the first pass that reads an open child under it until the last of them closes or moves out.
  That is the wait that a cover takes, whether or not a child starts on the parent's slot.
  A child at admitted that has not started has no wait here: its wait is the line's.
  A wait goes on while the root of its tree lingers.

| Cause | The move back |
| --- | --- |
| `review_changes` | A review that asks for changes, from reviewing back to implementing. |
| `send_back` | A send-back by the architect or the merger, a move back out of accepting, retro or merging that no drop from the merge queue came before. |
| `conflict` | A head that conflicts with its base, moved back to implementing, or kept in its phase and merged forward, and a merger's move back out of merging where the daemon's post of its reason names a conflict. |
| `red_checks_or_conflict` | Red checks or a conflict, which the release at 9542c6fb did not tell apart. |
| `test_failed` | A failed test, from testing back to implementing. |
| `queue_drop` | A move back out of merging after the issue's pull request left the merge queue. |
| `other_backward` | A worker's own move back before the review, out of planning, implementing, testing or reviewing. |

| Word | What ended the wait |
| --- | --- |
| `blocker_done` | The last blocker reached done. |
| `link_removed` | Someone took the last unfinished link off. |
| `child_done` | The last open child closed as done or icebox. |
| `child_moved_out` | The last open child moved from under the issue. |
| `gate_closed` | A new spec version closed the design gate. |
| `moved_on` | The waiting issue moved past its review, or out of the phase its open children held. |
| `left_workflow` | The waiting issue left the workflow. |
| `open` | Nothing, because the issue still waits at `--to`. |

### Slots

- `slotHoursInTurn` and `slotHoursNoTurn`: the slot-hours held while a worker of the holder, of any role, is in a turn, and the rest.
  Each slot counts from the pass that gives it to its release, or to `--to`.

- `inFlight`: the issues in flight, with `.average`, `.max` and `.hours`.
  An issue is in flight from the pass that first gives it a slot to its move to done or out of the workflow, and again from a reopened issue's next start.
  An issue that gave its slot back stays in flight.

### The wait of urgent work

`urgentWaits[P0]` and `urgentWaits[P1]` give the wait of P0 work and of P1 work apart, each where an issue counts in it.
An issue counts only where Legion works it: an issue of the window's project that enters the workflow in the record, a root handed to Legion or a child of a tree in the workflow.
An issue that nobody handed to Legion, which a window holds for its events alone, counts in neither.
An issue counts as P0, or as P1, where the priority Dispatch resolves for it is that one at its first todo in the window, or becomes that one before its first slot, at the slot's moment included: a person raises it, or an issue above it changes.
It reads that priority under every rule, so the rules compare on one set of urgent issues.
It counts in the highest of those.
Its first todo is the first Dispatch event in the window that shows it todo where the event before it did not.
An issue that was todo before the window and stands todo at its start counts from the window's start.
Its waits run from that moment to its first slot after it and to its first move to done after it.
These waits run on past `--to` through the run-out: an issue with no first slot, or no done, by the end of the run-out counts with its wait to that end, a cut wait.
The record's scorecard cuts at the end of the window's record, the same way.

- `.issues`: how many issues count.

- `.firstSlot` and `.done`: the wait to the first slot and the wait to done, each with `.medianMinutes` and `.p90Minutes`, the median and the 90th percentile by nearest rank over the issues, cut waits included, and `.cut`, how many of the waits are cut.

A replay reads each issue's first todo from the record's Dispatch events and its priority as Dispatch resolves it from them, as it reads the ranks, and its first slot and its done from its own run.
`firstDayApart` gives no wait of urgent work.
A window that lacks the history of an ancestor on which such a priority rests gives no wait of urgent work and no `priorityTakenBanded`, which stands at 0; the run names the missing input, and no report prints either or judges a rule on it.

### What else a scorecard holds

- `unplaced`: the issues that the start state could not place.

- `firstDayApart`: the same measures over the window's first 24 hours alone, for a window longer than a day (assumption 6).

- `gateClosedHours[<KEY>]`: for each tree, by its root's key, the hours of the window in which its design gate stood closed, from the gate's first fact on.
  A tree whose gate never stood closed has no entry.
  Like `doneByAnyone`, only a trace gives it, the record's and a replay's own log, by one code, so `validate` can compare them.

- `filed[<KEY>]`: each issue of the window's project that Dispatch shows filed in the window, at its filing.
  R5's branch of the recorded conflicts reads it ([`legion replay validate`](#legion-replay-validate)).

- `slotsGiven`: each moment in the window at which a pass gave out a slot, in order, the moments that `slotsGivenByDay` counts.
  The cap backtest reads it.

- `dailyMaxima[<count>]`: for each count that has a `.max`, the mean over the window's UTC days of the day's largest count, a day that the window cuts read over its part in the window.

- `urgentMeans[P0]` and `urgentMeans[P1]`: the wait of urgent work as means over its issues, `.firstSlotMinutes` and `.doneMinutes`, a cut wait counted at its length.
  The forecast reads them and `dailyMaxima` ([`legion replay forecast`](#legion-replay-forecast)).

- `rule`, `branch`, `seed` and `slowdown`: what a replay ran.
  A record's scorecard carries none of them.

- `assumptions`: the numbers of the assumptions from 1 to 25 that the measures of a replay rest on.
  A record's scorecard names none, because a record rests on no assumption of a replay.

- `scenario`: the work that the run gave an issue that never started, `none`, `drawn` or `drawn-done`.

Like `doneByAnyone`, only a line timeline gives `filed`, `slotsGiven`, `dailyMaxima` and `urgentMeans`, the record's scorecard and a replay's `scorecard-line.json`, by one code.
`scorecard.json` gives none of them, nor does `firstDayApart`.
A report refuses a record or a run written before its scorecard held them, and says to run it again.

## The slot rules

A slot rule is code in the daemon, not a plugin, and a new candidate is a change to the daemon's code.
The repository keeps eighteen rules: the daemon's own (`main`), the rules of three recorded windows (`legion-v20.42.5`, `legion-v19.57.3` and `legion-v22.14.1`), and fourteen candidates.
The daemon runs `main`, and a replay runs the rule that you name through the same admission pass.

| Rule | What it is | Commit |
| --- | --- | --- |
| `main` | The daemon's own rule, with no slot past the review, the slot given back on a link wait and a children wait, a started parent with an open child moved into `awaiting_children`, which keeps no slot (cybermonklabs/legion#4592), the line by priority first and returned issues first within one priority, a P0 issue started outside the cap under the P0 bound (cybermonklabs/legion#5488), and a conflicting head that keeps its phase. | `0d3786da` |
| `legion-v20.42.5` | The rule of releases 20.25.1 and 20.42.5, with the slot choices of `main` but no open child that stops a started parent's phase, the line by no priority, no P0 issue started outside the cap, and a conflicting head that moves back to implementing. | `958173da` |
| `legion-v19.57.3` | The rule of release 19.57.3, with a slot kept in every phase until the merger's ready mark in merging, no slot given back on a wait, no open child that stops a started parent's phase, the line by no priority, no P0 issue started outside the cap, and red checks that move back to implementing. | `9542c6fb` |
| `legion-v22.14.1` | The rule of release 22.14.1, with the slot choices of `main` but no open child that stops a started parent's phase, the line by no priority, no P0 issue started outside the cap, and a conflicting head that keeps its phase in every phase, accepting, retro and merging too, while the implementer merges the base forward at once (cybermonklabs/legion#4464 without #4979). | `bafd3618` |
| `keep-past-review` | `main` with the slot kept in accepting, retro and merging, so that a send-back past the review finds its issue with its slot; a parent with an open child still moves into `awaiting_children`, from accepting too, and gives its slot back, as under `main`. | `0d3786da` |
| `blockers-first` | `main` with each issue that a started issue waits on placed first among the issues that have not started. | `0d3786da` |
| `returned-by-rank` | `main` with a returned issue placed in the line by its board rank, not by the time at which it gave its slot back. | `0d3786da` |
| `cover` | A slot kept until the work is done, through every wait on another issue, for its children and for a link alike, and an issue that a waiter waits on started on the waiter's slot, one cover for each holder: a parent with an open child keeps its phase, with no move into `awaiting_children`, and the child starts on the parent's slot and holds it for the parent while the cover stands, as an issue it waits on through a `blocked_by` link does. | `0d3786da` |
| `wait-grace` | `main` with a link wait, or the children wait of a first phase, that gives the slot back only after five minutes, for the reason that a deployment reported: a started issue whose `blocked_by` link was set and taken off within a minute lost its slot and stood last of 79 in the line's returned part; a started parent with an open child moves into `awaiting_children` at once, as under `main`. | `0d3786da` |
| `wait-grace-1h` | `wait-grace` with a grace of one hour, so that the comparison shows beside it what a longer grace keeps and what it costs; a started parent with an open child moves into `awaiting_children` at once there too. | `0d3786da` |
| `returned-by-first-slot` | `main` with a returned issue placed in the line by its first slot in its current generation, not by the time at which its work came back, for the reason that a deployment reported: a started issue sent back from merging for one forward merge stood last of 76 in the returned part, and the issue of the one-minute wait last of 79. | `0d3786da` |
| `rework-first` | `main` with the line in first in, first out order for started work, for the reason that an owner doubted that the board's rank is kept up to date: the returned issues first, by their first slot in their current generation, then the issues of trees that have started, each tree by its root's first slot and each issue by its arrival in the line, and the trees that have not opened last, by rank. | `0d3786da` |
| `rework-first-keep` | `rework-first` with the slot kept in accepting, retro and merging, as `keep-past-review` keeps it, so that the comparison reads the cost of returning work under the owner's order with and without that slot; a parent with an open child still moves into `awaiting_children` and gives its slot back, as under `main`. | `0d3786da` |
| `arrival-order` | `main` with the line as one queue in the order in which its issues arrived, with no part ahead of another and no rank, which shows what the board's order changes. | `0d3786da` |
| `before-p0` | `main` with no P0 issue started outside the cap, so that a P0 issue waits in the line as any issue does and the cap counts every holder, measured for reference. | `0d3786da` |
| `idle-return-1h` | `main` with the slot of a holder that waits on its children or on a person given back once none of its roles had a turn for an hour, the holder keeping its place in the line, for the reason that an owner asked why a P0 issue cannot start at once; it now differs from `main` only for a holder that waits on a person, since under `main` a started parent already gives its slot back while it waits for its children (`awaiting_children`). | `0d3786da` |
| `signals` | `main` with R10's start of a P0 issue outside the cap and admission by signals below P0: the next issue in the line's priority order starts while each signal that has a limit is under it and the holders below P0 are fewer than the ceiling, which the cap becomes, at the pass of an event or of a signal's deadline and never at the tick. | `0d3786da` |
| `signals-backoff` | `signals` with an allowance that falls to 90% at each project-limit refusal or kill (rounded down, minimum 1) and rises by 1 each quiet hour up to the ceiling. | `0d3786da` |

The commit of `main` is the commit at which the rule was first written as a value.
`main` is the daemon's rule at the tool's commit, which each run records, so since cybermonklabs/legion#4592 it moves a started parent with an open child into `awaiting_children`.
A rule built on `main` that changes one choice carries the same commit, the code that its other choices come from.
`legion replay rules` prints:

```text
main (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

legion-v20.42.5 (commit 958173da0ea4cd03290a1964edf9eb92e6685200)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: false
  the line's order: returned_oldest_first
  the line stands by priority first: false
  a start takes: own_slot
  a conflicting head: moves_back_conflict
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: false
  an idle wait on children or a person gives the slot back after: 0s

legion-v19.57.3 (commit 9542c6fbbecd184ccee73523ab3defa39d6cf23a)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, accepting, retro, merging, held
  a link wait gives the slot back: false
  a children wait gives the slot back: false
  a wait gives the slot back after: 0s
  a wait stops the phase under way: false
  the line's order: returned_oldest_first
  the line stands by priority first: false
  a start takes: own_slot
  a conflicting head: moves_back_checks_red
  the slot goes back at the ready mark: true
  a P0 starts outside the cap: false
  an idle wait on children or a person gives the slot back after: 0s

legion-v22.14.1 (commit bafd3618eb58722419c77f34174c1b77525c891b)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: false
  the line's order: returned_oldest_first
  the line stands by priority first: false
  a start takes: own_slot
  a conflicting head: implementer_merges_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: false
  an idle wait on children or a person gives the slot back after: 0s

keep-past-review (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, accepting, retro, merging, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

blockers-first (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: blockers_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

returned-by-rank (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_by_rank
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

cover (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, accepting, retro, merging, held
  a link wait gives the slot back: false
  a children wait gives the slot back: false
  a wait gives the slot back after: 0s
  a wait stops the phase under way: false
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: cover
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

wait-grace (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 5m0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

wait-grace-1h (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 1h0m0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

returned-by-first-slot (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_by_first_slot
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

rework-first (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: rework_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

rework-first-keep (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, accepting, retro, merging, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: rework_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

arrival-order (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: arrival
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s

before-p0 (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: false
  an idle wait on children or a person gives the slot back after: 0s

idle-return-1h (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 1h0m0s

signals (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s
  admits below P0 by signals: ceiling

signals-backoff (commit 0d3786da536722f2eb26a793ab1b9827b6e90f47)
  keeps a slot in: admitted, awaiting_person, planning, implementing, testing, reviewing, held
  a link wait gives the slot back: true
  a children wait gives the slot back: true
  a wait gives the slot back after: 0s
  a wait stops the phase under way: true
  the line's order: returned_oldest_first
  the line stands by priority first: true
  a start takes: own_slot
  a conflicting head: merge_forward
  the slot goes back at the ready mark: false
  a P0 starts outside the cap: true
  an idle wait on children or a person gives the slot back after: 0s
  admits below P0 by signals: backoff
```

Each line of a rule gives one choice:

- `keeps a slot in`: the phases in which a started issue keeps its slot.
  A phase that the list leaves out holds no slot, and the start of its worker waits for no slot.

- `a link wait gives the slot back` and `a children wait gives the slot back`: whether a started issue gives its slot back while it waits on an unfinished `blocked_by` link, or on its open children.

- `a wait gives the slot back after`: how long such a wait lasts before it gives the slot back.
  A holder whose wait is younger than this keeps its slot, and the first pass at or after the wait's start plus this time gives the slot back.
  `0s` gives it back at the pass that finds the wait.

- `a wait stops the phase under way`: whether an open Dispatch child of a started parent stops the phase under way (cybermonklabs/legion#4592).
  Where it does, a parent in `planning` through `accepting` whose pull request has not merged moves into `awaiting_children`, which keeps no slot, in place of each start an open child holds and in the middle of a phase the admission pass finds it in, and comes back once no child of it is open.
  Where it does not, the parent's phases go on while a child is open, and a `blocked_by` link stops no phase under any rule.

- `the line's order`: `returned_oldest_first`, the returned issues first, oldest first, and then the issues that have not started by rank.
  `returned_by_rank` places a returned issue by its board rank.
  `returned_by_first_slot` places a returned issue by the time of its first slot in its current generation.
  `blockers_first` places first, among the issues that have not started, each issue that a started issue waits on.
  `rework_first` places the returned issues by their first slot, then the issues of trees that have started, and then the trees that have not opened, by rank.
  `arrival` places every issue of the line by the time at which it arrived in the line, with no part ahead of another and no rank.

- `the line stands by priority first`: whether the line is banded by priority before its order: each issue stands in the band of its own Dispatch priority, or of its tree root's where it has none, P0 first and an issue with neither after P3, and within one band the line keeps its order.
  `main` bands the line, as the daemon does since cybermonklabs/legion#5365, and so does each candidate built on it; the rules of the recorded windows do not, because their releases came before that change.

- `a start takes`: `own_slot`, a slot of its own while the cap has one free.
  `cover` lets an issue that a waiter waits on start on that waiter's slot, whatever the cap says, one cover for each holder.

- `a conflicting head`: `merge_forward` keeps the phase and asks the implementer, or in merging the merger, to merge the base forward.
  Under `merge_forward`, a conflict in accepting or retro keeps its phase and waits for merging, where the merger merges the base forward (assumption 37).
  `moves_back_conflict` moves the issue back to implementing by the trigger `conflict`, and `moves_back_checks_red` by the trigger `checks_red`, from testing, reviewing, accepting, retro and merging.
  `implementer_merges_forward` keeps the phase in every phase and asks the implementer to merge the base forward at once, in accepting, retro and merging too.

- `the slot goes back at the ready mark`: whether an issue in merging gives its slot back at the merger's ready mark, under a rule that keeps a slot in merging.

- `a P0 starts outside the cap`: whether an issue that Dispatch resolves to P0 starts outside the cap, whatever the number of slots in use, under the P0 bound, and whether the rule reads the priority Dispatch resolves for its line's order and its holder counts too ([The priority Dispatch resolves](#the-priority-dispatch-resolves)).
  `main` starts one there since cybermonklabs/legion#5488, and so does every rule built on it; the rules of the recorded windows and `before-p0` start none, and a P0 issue waits in the line under them as any issue does.

- `an idle wait on children or a person gives the slot back after`: how long a holder that waits on its children or on a person goes with no turn of any of its roles before it gives its slot back.
  `0s` gives no slot back for that.

- `admits below P0 by signals`: printed only for a rule that admits by signals, with its mode: `ceiling`, where the cap becomes a ceiling, or `backoff`, where an allowance replaces it.
  `signals` and `signals-backoff` name one; the rules before them print no such line.

### `wait-grace` and its grace

`wait-grace` keeps a holder's slot through a wait on another issue for five minutes.
The admission pass that first finds the holder waiting marks the start of the wait on its slot, as it marks every wait that a holder keeps its slot through.
A wait that ends inside the five minutes frees no slot, and its issue stands in no place of the line.
The first pass at or after the wait's start plus five minutes gives the slot back, as `main` gives it back at once.

No pass is asked for at the end of the grace, because the daemon has no wake that an admission pass can ask for at a time of its choosing.
The slot goes back at the next pass after the grace: the next fact of the project or the next controller tick, at most `controller_wake_interval_seconds` later.
That delay is the rule's own, in the daemon and in a replay alike.

The repository keeps the rule at two graces: `wait-grace` at five minutes, and `wait-grace-1h` at one hour, so that the comparison shows what a longer grace keeps and what it costs.
`compare` runs both, and for each rule with a grace it names what the grace keeps on each window, read from the window's record and not from a replay.
It takes each wait on another issue that gave a slot back in the record.
Such a wait runs from its start to the end of the last wait of its issue on a link or on children that follows it with no gap.
A wait that is still open at the window's end counts at its length to the end.
The grace keeps the slot through each wait that lasts no longer than the grace, because the pass at the end of the grace finds such a wait over.
`compare` names how many of the waits the grace keeps the slot through, and of how many.
Each wait holds its slot under the grace for the shorter of its length and the grace, and `compare` names the sum over the waits as the slot-hours that the grace keeps, which the line goes without.
The lengths of the kept waits alone would leave out the grace that each longer wait holds, where most of the cost falls.
The slot of a longer wait goes back at the first pass after the end of the grace, a little later, which the slot-hours leave out.

The value of five minutes comes from one deployment's recorded window of 94 hours at a cap of 157:

| Reading of the window | Value |
| --- | --- |
| Slots taken again in the pass that freed them | 776 of 819 |
| Median time from a slot's release to the next taking of a slot | 19 microseconds |
| Median time from one pass that frees a slot to the next | 4 minutes 44 seconds, over 760 such passes |
| Waits on another issue that gave a slot back | 114: 61 on a link and 53 on children, 62 still open at the window's end |
| Median length of those waits | 20 hours 24 minutes, an open wait counted to the window's end; 26 hours 22 minutes over the ended waits alone |
| The shortest of those waits | 15 seconds, then 14 minutes, 15 minutes and 25 minutes |
| Median time from a release for a wait to the issue's next slot | 18 hours 18 minutes, over 49 |
| Median time from the end of such a wait to the issue's next slot | 7 minutes 48 seconds, over 49 |

Each grace below is read as `compare` reads it, over the same 114 waits:

| Grace | Waits that it keeps the slot through | Of those, still open at the window's end | Slot-hours that it keeps |
| --- | --- | --- | --- |
| 1 minute | 1 (0.9%) | 0 | 1.89 |
| 2 minutes | 1 (0.9%) | 0 | 3.77 |
| 5 minutes, `wait-grace` | 1 (0.9%) | 0 | 9.42 |
| 10 minutes | 1 (0.9%) | 0 | 18.84 |
| 15 minutes | 2 (1.8%) | 0 | 28.24 |
| 30 minutes | 5 (4.4%) | 1 | 55.89 |
| 1 hour, `wait-grace-1h` | 9 (7.9%) | 1 | 109.28 |
| 2 hours | 15 (13.2%) | 3 | 210.24 |
| 4 hours | 22 (19.3%) | 7 | 399.98 |

The median time from a slot's release to its next taking names no grace: the cap was full, and the pass that freed a slot gave it out at once in 776 of 819 releases.
The time from one pass that frees a slot to the next, 4 minutes 44 seconds, is when a slot next comes free, and five minutes is that time rounded up.
Five minutes keeps the slot through the one-minute class, the 15-second wait of the window, and through no other wait of the window, because the next shortest lasted 14 minutes.
Each longer wait holds its slot five minutes more, 9.4 slot-hours of the window's 14,768.
Over the window's waits, the next pass that a replay makes after the grace's end, an event, a recorded move, a slot moved or a tick, came a median 2.6 seconds later, at most 31.5 seconds.
An hour keeps the slot through 8 more waits than five minutes: 7 that ended within the hour and 1 still open at the window's end after less than an hour.
It keeps 109.28 slot-hours against 9.42, nearly all of them the hour that each of the other 105 waits holds: 105 slot-hours.

### `returned-by-first-slot` and the first slot

`returned-by-first-slot` places each issue of the returned part by the time of its first slot in its current generation, the earliest first.
An issue that the daemon started early and that gave its slot back stands, when its work comes back, ahead of the returned issues that the daemon started after it.
The daemon records no first slot once a slot goes back, because the slot's row goes with it.
A replay keeps the first slots in a table of its own database: each pass keeps the slot of each holder as the first of its generation where the table holds none, and the start state keeps the first slot that the record shows for each issue in its current run.
A deployment that adopts this rule needs that record in the daemon's own store first.

### `rework-first`, `rework-first-keep`, `arrival-order` and the arrival

`rework-first` admits started work first, in the order in which it started.
The returned part comes first, each issue by the time of its first slot in its current generation, as under `returned-by-first-slot`.
Then come the issues of trees that have started and that have not started themselves.
Among them, a tree whose root took its first slot earlier comes first, and within a tree, an issue that arrived in the line earlier comes first.
The trees that have not opened come last, by the board's rank, so rank decides only after the started work.
`rework-first-keep` orders the line as `rework-first` does, and keeps a started issue's slot in accepting, retro and merging as `keep-past-review` does, so that work sent back past the review never waits for a slot.

`arrival-order` places every issue of the line by the time at which it arrived in the line in its current generation, the earliest first.
No part stands ahead of another and no rank counts.
Each issue keeps its part and its place in that part, which say what kind of work it is.

An issue arrives in the line when the daemon first records it in its current generation.
The daemon records no arrival, because the time at which an issue last moved changes with each move.
The daemon also records no first slot once a slot goes back.
So a replay under any of the three rules keeps both in tables of its own database, `replay_first_slots` and `replay_arrivals`.
Each pass keeps the arrival of each issue that it reads as the time at which its record last moved, where the table holds no arrival for the issue's generation, and the first pass after the record is written keeps the time of that write.
The start state keeps, for each issue in the workflow, the time at which the record shows it entered the run that it stands in.
An issue with no arrival kept is placed by its first slot, and then by the time at which its record last moved.
A deployment that adopts any of them needs the daemon to record each issue's arrival in a generation and each issue's first slot in a generation, kept after the slot goes back, in its own store first.

Under each rule that keeps one of these tables, `returned-by-first-slot` among them, the replay reads the start state as a pass reads it, in a transaction that writes nothing.
That read keeps no first slot and no arrival, and reads the ones that the start state kept.
The passes keep the rest.

### `before-p0`, `idle-return-1h` and the priority

An owner asked why a P0 issue cannot start at once.
`main` now carries that start, and these candidates give the comparison the reference without it and the idle return beside it.
Each is `main` with one choice changed, so each carries the commit of `main`.

The record keeps each issue's own Dispatch priority, P0 to P3 or none (`record.Issue.Priority`), as the newest Dispatch event, listing or detail read shows it.
Under a recorded rule and `before-p0`, a replay gives each issue its own priority as its recorded Dispatch events show it: the start state writes each record with the priority of its newest event before the window's start, each event that the replay applies carries its own and no resolved priority, and each boot's listing carries the priority of the replay's copy of Dispatch.
So `main` and every candidate order the line by priority in a replay, as the daemon does, and the rules of the recorded windows do not.
Under a rule with the new P0 start, `main` and every candidate built on it but `before-p0`, the replay gives each issue the priority Dispatch resolves for it in place of its own ([The priority Dispatch resolves](#the-priority-dispatch-resolves)).

`main` orders the whole line by priority first.
An entry stands in the band of the priority that places it: the issue's own priority, or its tree root's priority where it has none.
P0 comes first, and an entry that has neither priority comes after P3.
Within one band, the line keeps the rule's order, under `main` the returned issues first, oldest first, and then the issues that have not started, by rank.
The comparison ran this order as the candidate `priority-first` before that change reached `main`, and `main` is that rule now.

`main`, since cybermonklabs/legion#5488, and every candidate built on it but `before-p0`, start an issue that Dispatch resolves to P0 outside the cap.
The cap counts only the holders that did not start outside it, so where the admission pass would start such an issue but for the cap, it starts it, whatever the number of slots in use.
The P0 bound limits how many such issues hold a slot at once: with a bound in force, such an issue starts only while fewer of them than the bound hold a slot, and at the bound it waits in the line where its priority places it.
With no bound, every such issue that waits only for a slot starts.
A replay reads the bound in force at each pass as the daemon does: the window's `p0Bounds` give its changes, the one in force at the window's start in the start state and each later one at its moment, and none stands before the first change.
Every other hold on a start holds it as before: a wait on another issue, a hold of the Dispatch consumer or of the boot's catch-up, and a tree's cleanup.
The priority Dispatch resolves decides, for the start, the line's order and the holders the cap counts: a child that has no priority of its own takes that of the nearest issue above it that has one, and the root of a Legion tree is no substitute.
Below P0, the cap holds as under `main`.
The stall measure and the read of a free slot count the holders as the promotion counts them, so a P0 that the promotion starts is a line that moves, and a P0 holder fills no slot of the cap.
The pass logs each such start as `admission starts a P0 issue outside the cap`, and each such issue held at the bound as `admission holds a P0 issue at the P0 bound`, each with the issue, the P0 holders, the other holders, the cap and the bound.

`main` starts a P0 issue outside the cap since cybermonklabs/legion#5488, which brought that start to the daemon: one implementation of it, `record.Issue.StartsNow` and the holders `record.HoldersOf` counts, which the rule's field turns on or off (`record.SlotRule.StartsNow`).
`before-p0` is the reference without that start, and the rules of the recorded windows start none, so under them the cap counts every holder, a P0 holder too.
Before cybermonklabs/legion#5488 reached `main`, the comparison ran that start as the candidate `p0-at-once` and together with `idle-return-1h` as `p0-idle`; once `main` and `idle-return-1h` carried it, those two rules went.

`idle-return-1h` gives back the slot of a holder that waits on its children or on a person, when none of its roles had a turn for an hour.
A holder waits on its children while an issue beneath it in its tree is still in the workflow.
A holder waits on a person while it is a root in `awaiting_person` that nobody has answered.
The turns are the last turn start and the last turn end of the holder's claims, read as the holder state reads them, and a slot taken less than an hour ago keeps it.
A holder with a turn, a start or a task under way, or that is owed a start, keeps its slot.
A root whose architect writes its design at `admitted`, and a root that arrived with the slot of another tree, keep their slots, as a wait on another issue keeps them.
The holder keeps its place in the line by the time of the release, as a release for a wait keeps it, so it stands ahead of every slot given back after it.
While it waits on its children, it stands in the line and the promotion passes it over.
A root that waits on a person stands in no line until a person answers it, and then it stands at the time of its release.
When its children are done, or when a worker of it is owed a start, it takes a slot again at its place.

The daemon gave a slot back for these waits once, and then took that release away.
cybermonklabs/legion#2751 gave a slot back while an issue waited on its children or on a person.
cybermonklabs/legion#3206 took those releases away.
Its rule asked whether any agent of an issue had work, which read six states, and on one day it judged wrong five times: slots stood with their architects idle and their next step with a person, while other issues waited.
The cap was then made to count the work that a project has in hand, a spec in a person's queue among it.
`idle-return-1h` asks no state of the work: it reads the time since the holder's last turn, and only of a holder that waits on its children or on a person.

The admission pass logs a slot that an idle holder gave back with the reason `idle_return`.
The line timeline places such a holder in the returned part as it places an issue after a release for a wait.
It does not read the children or the person that the holder waits on, so it counts the holder as waiting only for a slot.
The snapshot timeline counts the same holder as passed over while its children work, and a root that waits on a person as in no line, and `compare` gives that difference as the gap between the two timelines.

### The priority Dispatch resolves

Dispatch resolves a priority for each issue: its own, else that of the nearest issue above it on its Dispatch parent chain that has one, in any project, up to 32 levels, else none (`resolved_priority`, spec R10).
A rule with the new P0 start, whose field `a P0 starts outside the cap` is true, reads it for the start of a P0 issue outside the cap, the line's priority order and the holders the cap counts.
Each recorded rule keeps its release's own admissions and order, by each issue's own priority.
The measures read it under every rule: the wait of urgent work and `priorityTakenBanded`.

A replay reads it from the record as the measures and the rules need it (`resolvePriorities`):

- Where an event of an issue carries `resolved_priority`, the value it carries stands from that event on, a null one as none, and nothing of the issue is reconstructed after it.

- Before an issue's first such event, as in every event of a Dispatch release before `resolved_priority`, the replay reconstructs it from the window's own events: each issue above it at its own priority, read at the window's start and at each change of a priority, a parent or a recorded value of the issue or of an issue above it (assumption 51).

Under a rule with the new P0 start, the replay's copy of Dispatch serves that priority as each issue's priority, so the daemon's mirror holds it.
Under a rule with no P0 start, the copy serves each issue's own priority with no `resolved_priority` or `priority_from`, as a Dispatch release before them serves it, so a recorded value of `resolved_priority` reaches no such rule.
The start state records each issue at the one standing just before the window's start, each event the copy applies shows the one standing at its moment, and where an issue above an issue changes, the copy applies at that moment an `issue.updated` of the issue, as Dispatch appends one on each issue whose resolved priority a write to another issue moves.
The daemon's own rule, `record.PriorityOf` and the cap's counts then read the resolved priority with no change of the daemon: an issue that resolves to none has a tree root that resolves to none, its root standing on its chain.
Once the daemon's record keeps the resolved priority that Dispatch serves, the copy serves it in that field in place of the own priority, the one place of the replay that supplies it.

A window that lacks the history of an ancestor on which the priority of an issue of its project rests is refused for every rule with the new P0 start, `main` among them since cybermonklabs/legion#5488: `legion replay run` names each such ancestor with the issues resting on it and says to collect the window again with their history, and `validate`, `forecast`, `compare` and `limits` make no run of such a rule on the window and read none.
`collect` names in `missingAncestors` each ancestor Dispatch did not show, and the replay also finds one that a window collected before `collect` read each ancestor lacks.
A recorded rule still runs on such a window, and its run and the window's record name the missing input (`missingAncestors` in `run.json` and `record.json`) and read no measure that rests on it; its validation and the forecast across the change give no such measure and name the missing input.
A comparison needs a run of `main` on each window, so it makes none of such a window: its refusal names `main`, the window and the ancestors it lacks, and says to collect the window again with their history, since a window of the comparison that `main` cannot run is a gap of its collection, not a result.

### `signals` and `signals-backoff`

The tool's spec asks whether signals can regulate admission in place of the cap, so these two candidates are `main` with R10's start of a P0 issue outside the cap and a mode of admission by signals.
Below P0, the next issue in the line's priority order starts only while each signal that has a limit reads under it and the holders below P0 are fewer than the ceiling, which the cap becomes.
A signal that has no limit holds no start, and neither does a signal that has no record at the moment of the pass.
No pass takes a slot from a holder because a signal is over its limit, and every other hold on a start and every release stand as under `main`.

The rule decides from the signals of the pass, their limits, the cap and the kind of the fact whose pass it is (`record.SlotRule.SignalsLet` and `signals.Decide`).
An admission pass reads the signals once, from a snapshot that its caller supplies (`admit.Admission.UseSignals`).
Where the caller supplies none, or says that admission by signals is off, the pass admits under the cap, whatever rule it judges by.
The daemon's own rule admits by signals in the same way once its caller supplies a snapshot that names a mode, and the daemon supplies none in this version.
The kind of the pass is an event for every fact but two: the controller's tick, and the signal deadline, `intake.SignalDeadline`, which is the moment at which a signal would cross its limit with no event.
At the pass of a tick, the rule gives back each slot that `main` gives back, applies the P0 bound and starts each P0 issue that R10 starts, and starts no issue below P0 and returns none to a slot.
At the pass of an event or of a deadline, it starts the next issue below P0 as the paragraph above says.
No daemon emits the deadline fact in this version.
A replay supplies the signals to the passes of a run under either rule (`legion replay run --limit`), from its own course and never from the record.
The idle share comes from the replay's own slot holders: the states that its pass observer reads over the newest pass's view and the stub's claims, with the moment each holder has stood idle since.
The replay's course up to the window's start is the record's, so a holder stands idle no earlier than the newest end of a turn of its issue that the record shows before the start.
The approvals come from the replay's copy of Dispatch: each ask event, each comment on an ask and each change of an issue's assignee that it applies, and each move of the project's default assignee at its recorded moment.
The provider's headroom and refusals, the kills and the merge queue come from the load models at the replay's own issues in flight, its starts of each recorded work segment and the pull requests of its drawn work ([`legion replay models`](#legion-replay-models)).
The models read the course as the run goes: each minute when it ends, each kill once its segment or its drawn issue's own hours bring it, and each pull request once it enters, so the cost of a pass does not grow with the window.
A model that gives no limit for its signal leaves that signal with none in the run, and `run.json` names it under `unlimited`.
Under `--token-limit`, the tokens model also limits the provider's headroom and its refusals where it gives no daily test only because the window records no limit of the project's own, and the run models each minute whose record holds no such limit against the token limit (assumption 49).
A model that fails its daily test gives no limit, with the token limit or without it.
A signal with no record at a moment holds no start there (assumption 47).
Under `signals-backoff` the pass also folds the setbacks: the project's own refusals of each minute, at the minute's end, and each kill, in time order, of a model that limits its signal in the run.

After every pass, the run computes the project's deadline: the earliest crossing of each signal that has a limit, with no event, and under `signals-backoff` the allowance's next rise (`signals.Earliest`).
It does so again at each moment that a modelled input changes with no pass: a kill, a pull request entering or leaving the merge queue, and the end of a minute that may hold a refusal.
It also does so at each turn that starts or ends with no pass, where the idle share has a limit.
A kill runs no pass: the replay leaves the kill's fact out (assumption 31), and only computes the deadline again.
The run keeps one deadline, as the daemon keeps one timer.
A new moment replaces the deadline that stands (`moved`), a deadline no longer due is `dropped`, and at its moment the deadline applies `intake.SignalDeadline`, whose pass is a deadline's (`fired`).
The timer is the daemon's memory, so a deadline that falls while the daemon is down does not fire.
A boot drops the deadline that stands, and the boot's first pass computes it again.
`run.json` gives each change of the deadline under `deadlines`, and each slot each pass gave out or took back, with the kind of its pass, under `slotPasses`.

`signals-backoff` replaces the ceiling by an allowance.
The allowance starts at the ceiling.
At each refusal by the project's own limit and at each kill, it becomes 90% of itself in whole numbers, rounded down, and at least 1.
For each whole hour after the last of them, it rises by 1, up to the ceiling.
The pass folds the history of refusals and kills up to the pass (`signals.Allowance`), so the same history gives the same allowance, and the moment of the next rise is one that a deadline can name (`signals.NextRise`).

## The branches

A branch is one value of each knob that the record does not give.
A result that rests on a knob runs under each value of that knob, so that you read the values side by side.

| Knob | Values, the main branch's first | What it decides | Assumption |
| --- | --- | --- | --- |
| `timing` | `offset`, `wall` | When a link's removal, a child's move out, a gate's closing and an outside blocker's close happen, at their recorded offset from the start of the wait that they end, or at their recorded time. | 4 |
| `answer` | `offset`, `wall` | When a person answers an ask or reviews a spec version, at the recorded offset from the question, or at the recorded time. | 5 |
| `afterSendBack` | `recorded`, `less-slot-wait` | How long the first work after a send-back takes, its recorded length, or that length less its recorded wait for a slot. | 13 |
| `firstStarts` | `recorded`, `rank` | The order of first starts, the order in which the record started the issues in the window, or the board's rank. | 36 |
| `neverStarted` | `none`, `drawn`, `drawn-done` | The work of an issue that never started, none, or the work of an issue drawn from those that the record started, or from those that it finished: from the issues of its own tree where the tree has any, else from those of its priority and repository, else from the whole window. | 8 |
| `conflicts` | `recorded`, `git`, `drawn` | Keep the record's conflicts, use Git's stored verdicts with the generator for unanswered reads, or draw every conflict from the generator. | 3, 44 |

Beside the first six knobs, the slowdown factor f is the seventh, with the values 1, 1.2 and 1.6 (assumption 1).
The main branch uses the first value of each knob.
Validation and comparison run `main` and the eight one-knob changes at f = 1, 1.2 and 1.6, for 27 branch and slowdown pairs.
`run` accepts a single change, such as `main+timing=wall`, or several different knobs, such as `main+conflicts=git+neverStarted=drawn`.
A run's directory names its branch as `<branch>@<f>`, for example `main@1.2`.
Under the rule that made the record, both values of `timing` and of `answer` give the record's times, because every anchor falls at its recorded time.

Under `firstStarts=recorded`, the first start of an issue is the first slot in the window of a run that had taken no slot before it.
A slot that the issue held at the window's start is not a first start, nor is a later slot of a run that held a slot at the window's start or took one before it.
A slot that the record gives to work that came back to a started issue is not a first start either: a return after a send-back or a conflict, the end of a wait, or the restart of a held phase.
Nor is a slot that the trace infers (assumption 34).
Each issue that the record started in the window takes a rank in the order of these first starts, ahead of every rank on the board.
The line orders issues by the rank of their tree's root first.
So the root of a tree takes the rank of the earliest recorded first start among the issues of its tree that can still take their first start in the replay, the root's own first start among them.
An issue can still take its first start while the daemon's own test of the line's starting part says that its first phase may start now.
That test asks that the issue stands at admitted, waits on no person, and is in the workflow by its status.
It also asks that nothing of the starting part holds the issue: its tree is running, its root waits on no person, its tree's design gate is open, and no Dispatch blocker, open child or accepted ancestor holds it.
An issue that the replay has not filed yet cannot take its first start, and neither can an issue that the replay has finished, moved past its first phase, or taken out of the workflow without its first start.
The replay reads the test in each fact of its daemon, just before the admission pass of that fact, over the line as that pass reads it.
Each time one of these issues takes its first slot, cannot take it any longer, or can take it again, the replay moves the root's rank to the earliest of those that can still be taken.
When the test gives a different answer, the replay writes the new rank of the root into the daemon's record before the pass, so that pass orders the line by it.
The move also reaches the replay's copy of Dispatch as a change of the root's rank, and then the daemon's mirror.
No measure counts that change.
For example, a root that holds its slot at the window's start takes the rank of its first child that starts, and then the rank of its next child.
So the next child of a tree waits where its own recorded start puts it among the work of every tree, and the replay starts the children of different trees in the order in which the record started them (assumption 36).
A tree in which no issue can still take its recorded first start shows its root at its board rank, unless the root has not yet taken its own first slot, in which case the root keeps the rank of its own first start, since a tree whose root has not started has no member that can start.
Each other issue keeps its board rank, and waits after the ranked issues.
So an issue that the record never started waits behind the recorded starts of its own tree that can still be taken.
Among the other trees, it waits at the rank of its root at that moment: the rank of the next recorded start of its tree that can still be taken, or the root's board rank when none is left.
A change of a rank in the record does not move these ranks, and the measures read the board's ranks.
A recorded start that a hold keeps at admitted, such as a link, open children or the tree's design gate, holds no rank.
So a tree whose next recorded start a hold keeps back ranks by its next start that can be taken, and another tree's issue that the record started later goes ahead of it.
So the pass in which a hold begins or ends gives its slots by the rank that the hold leaves.

## The states of a slot holder

`legion state` says what each holder of a slot does now, in one line under its admission line:

```text
slots: 4 of 4 in use (0 P0): 2 working (oldest 2026-10-05T10:12Z), 0 stopped, 1 person (oldest 2026-10-05T08:20Z), 0 issue, 1 idle (oldest 2026-10-05T09:30Z). Started work without a slot: 1 wait only for a slot (oldest 2026-10-05T09:58Z), 2 in all (oldest 2026-10-05T07:41Z). 2 holders rank below ACME-14, which waits for a slot.
```

[Running Legion](/legion/legion/running-legion/#observe) says what each state and each count of that line means, and what `legion state --json` carries of it.
A replay measures from the same states, so a reading of production and a replay use one set of words.
`waitOnlyForSlot` is the line's `wait only for a slot`, `startedWithoutSlot` its `in all`, and `priorityTaken` places holders and waiting issues in the order of the line before the priority band, `priorityTakenBanded` in the order of today's line ([The measures](#the-measures)).

## The signals

The tool's spec names, in its R11, five signals that a rule can admit by: the idle share, the merge queue's wait, the model provider's headroom with its refusals, the memory guard's kills, and the approvals that wait on the project's default assignee.
Each has one definition in the tool's code (`internal/signals`), over plain values that its caller supplies at a moment: the replay reads them from a collected window's own record (`internal/replay/signals.go`), and the daemon's admission pass reads them from its own events.
Package `signals` imports the standard library alone, so the daemon imports no replay code.
It also holds the pairing of the merge queue's events into waits and the turn of each ask by its events (`signals.WaitsOf` and `signals.NextAsk`), for a caller that reads the events one at a time (`signals.MergeQueue` and `signals.ApprovalState`), and the legal limit of each signal (`signals.CheckLimit`).
A limit is legal where it is above 0 and at most 1 for the idle share, at most 10 for the provider's headroom and at most 168 for the queue's hours, and where it is a whole number from 1 to 1,000,000 for the refusals, the kills and the approvals.
`legion replay signals --limit` is not a setting: it reads any limit at or above zero, and prints where a signal would cross it.
Lower is better for each.

A signal binds where the window holds a record of it and its value is at or above its limit, since a rule admits while each signal is under its limit.
A signal with no record at a moment is unrecorded, never zero, and binds nothing there (assumption 47).
Each signal but the approvals has a next crossing: the next moment after a moment at which it would start or stop binding if no new event came.
The end of a signal's record is such a moment for a signal that binds there.
It is the moment at which a rule that admits by the signals needs its next admission pass when no event comes first (R11), and each event moves it again.

A replay keeps each work segment's recorded length, so it cannot show the load that more work in flight would cause.
So a replay under another rule takes the merge queue's wait, the provider's headroom and refusals, and the kills from a model of the window that keeps what the window records and moves with the work in flight of the replay (assumption 45).
[`legion replay models`](#legion-replay-models) fits the three models and tests each one by R11's daily test and by R11's test across two records.
The idle share and the approvals come from the replay itself.
The definitions below read the record, and [`legion replay signals`](#legion-replay-signals) prints them.

### Signal `idle`

**Definition**: the share of the slot holders whose state is `idle` with its time at least d before the moment.
d is one hour, and the comparison also reads it at 10 minutes and at 4 hours (`--idle-after`).
A holder's state is the one of [The states of a slot holder](#the-states-of-a-slot-holder), read by the same rule as `legion state`.
An idle holder's time is the later of the newest end of a turn of its claims and the moment it took the slot that it holds.
A moment with no holder has a share of 0.

**Next crossing**: with the holders as they stand at the moment, the moment at which a holder idle since s reaches s + d.
A holder leaves `idle` only at an event, so the share falls at no moment with no event.

**Inputs**: `log.jsonl`, its `admission:` slot lines and its `supervise:` lines of each turn's start and end.
Live, the same share comes from each slot's `state` and `since` in `legion state --json`, which a test of the tool holds to the window's reading of the same slot.

**Source**: the replay itself.
A replay reads its own slot holders and the stub's claims.
Its course up to the window's start is the record's, so a holder's idle time begins no earlier than the newest end of a turn of its issue that the record shows before the start, as this definition reads the record there.

### Signal `queue`

**Definition**: the larger of two waits, in hours, the median wait of the project's pull requests that left the merge queue in the 6 hours before the moment, and the wait so far of the project's pull request that has been in the merge queue longest at the moment.
A wait runs from a pull request's `added_to_merge_queue` to the `merged` or `removed_from_merge_queue` that ends it, so a drop and a re-add are two waits.
The removal that GitHub records beside a merge ends no wait.
With no exit in the 6 hours and no pull request in the queue, the wait is 0.

**Next crossing**: with the waits as they stand at the moment, the moment at which the wait of the pull request longest in the queue reaches the limit, or at which an exit leaves the 6 hours.

**Inputs**: `queue/<owner>__<name>.jsonl`, which `collect-queue` writes, all files together, each event under the repository that its url names and each event once ([The records of the signals](#the-records-of-the-signals)).

**Source**: the merge queue model fitted on the window (assumption 38).

### Signal `provider`

**Definition**: the share of the project's limit that its uncached input tokens used in the minute before the moment, the tokens of the last whole minute that ended at or before the moment over the limit in force in that minute.
The tokens are as the record writes them, below zero too.
The limit holds from one observation to the next, as the record gives it (assumption 48).
The headroom has no limit of the project's own in a minute before the limit's first observation, in a minute with no limit or a limit of 0, and in a minute whose limit is one model's own (a `limit_scope` other than `Claude`), which is no limit on the project's tokens.
There it reads the project's token limit that the caller gives (`--token-limit`, assumption 49), and with none it has no record and binds nothing.
A minute outside the record of the provider has no record, whatever the caller gives (assumption 47).

**Refusals**: part of the provider's signal, the refusals of each minute of the record that ended in the hour before the moment, each by the limit that gave it, the project's own (`own`) or the provider's (`provider`), as the record splits them (assumption 46).
A limit of the refusals reads their count (`--limit refusals=<n>`).
The refusals have a record at a moment where the record holds the minute before it, whatever the limit.

**Next crossing**: for the headroom, the first minute boundary at which the record's series starts or stops binding.
For the refusals, with the refusals as they stand at the moment, the moment at which an old refusal leaves the hour.

**Inputs**: `signals/provider.jsonl`.

**Source**: the tokens model fitted on the window, for the headroom and the refusals by the project's own limit.
A refusal that the provider gives every project is kept as recorded.

### Signal `kills`

**Definition**: the count of the memory guard's kills of the project's issue pods in the hour before the moment, one for each launcher line whose text starts `launcher: memory guard: ended`, counted from its own moment to an hour later.

**Next crossing**: with the kills as they stand at the moment, the moment at which an old kill leaves the hour.

**Inputs**: `launcher/log.jsonl`, the record of the kills.
No model reads the pods' memory series, and `collect` no longer collects it.

**Source**: the memory model fitted on the window.

**The daemon's lines of the kills**: the daemon writes a line of each kill that a launcher reports, `workflow: the role whose process the memory guard ended is told why`, or, for an issue that it does not record, `workflow: a memory guard kill names an issue the daemon does not record, so no role is told`.
The tool reads those lines beside the launcher's and matches the two kill by kill, on the pod, the role, the process's pid, and a time at most a minute apart (`MatchKills` in `internal/replay/kills.go`).
`legion replay signals` prints how many kills both hold, and each kill that one of them lacks.
A window that `collect` wrote before it kept the daemon's lines of the kills holds none of them, so collect it again to read both.
The trace reads nothing else of those lines, so a window that holds them replays as one that does not.

### Signal `approvals`

**Definition**: the count of the asks open at the moment, approval requests among them, that wait on the project's default assignee then.
An ask waits on its `asked_of` person, else on its issue's assignee, else on the project's default assignee at the moment.
The default assignee is the one that the newest `settings.project_default_assignee.updated` at or before the moment names, else `window.json`'s `defaultAssignee`.
An ask counts while it is open and waits on a person, as the daemon reads the turn of an ask (`internal/intake/decode.go`) and as Dispatch's contract gives it (`packages/contracts/src/dispatch-api-events.ts` and `dispatch-api-asks.ts`):

- `ask.opened` opens it waiting on a person, and an ask event whose payload's `state` is `open` opens a closed ask again so.
  `ask.answered` and `ask.resolved` close it, a decline among them (`resolution.kind` is `declined`).

- An `ask.edited` that moves an approval request, its `requested_version` below its `version`, leaves it waiting on its agent, and any other edit leaves it as it was.
  Its `ask.handed_back`, the agent handing it back to the person, leaves it waiting on the person.

- A comment on an open ask (`comment.created`, `comment.answered`) leaves it waiting on whom its `ask_waiting_on` names: `agent` after a person's reply, and `human` after an agent's.

- `asked_of`, which the payload of each ask event carries, `ask.opened`, `ask.asked_of` and `ask.edited` among them, names the person that the ask is asked of.
  An issue's assignee comes from its `issue.created` and `issue.updated` events.

The signal reads the ask events of every issue of the project, which `collect` lists.

**Next crossing**: none, since the approvals change only at events.

**Inputs**: `events/<KEY>.jsonl`, `settings/events.jsonl`, and `window.json`'s `defaultAssignee`.

**Source**: the replay itself.

## The assumptions

A replay under a rule other than the one that made the record rests on assumptions about what the window would show under that rule.
Each run names the assumptions that it used.
Numbers 1 to 18 are the assumptions that the review of the first replay, below, judged.
Numbers 19 to 22 map the release at 9542c6fb onto today's workflow, 23 is the conflict path of the rule `main`, 24 is a daemon's downtime, and 25 is its reading of outside blockers.
Numbers 26 to 37 are the tool's own.
Numbers 38 to 44 come from a later review of the tool's own replays of one deployment's recorded window, which read the replays, the daemon's code and the window's record.
Numbers 45 to 48 are those of [The signals](#the-signals): what the models of three of them rest on, how the refusals and a signal with no record are read, and how the provider's limit is read between two observations of it.
Number 49 is the project's token limit that a replay takes from its caller where the record holds none.
Each section below gives the assumption as the tool states it, and its verdict.
It then gives the evidence in a recorded window, the direction in which the assumption moves each measure if it is wrong, and the reading of a window that would test it.

Each bullet that follows the label `If it is wrong.` starts with a direction:

- **Up**: the replay gives a value above the true value under that rule.

- **Down**: the replay gives a value below it.

- **None**: the assumption does not move the measure.

- **Unknown**: a window does not show which way the measure moves.

The direction is that of a replay under a rule other than the one that made the record.
Where it depends on the rule, the bullet names the rule for each direction.
Under the rule that made the record, `validate` measures how far a replay stands from the record.

A first replay, a prototype outside this repository, replayed one deployment's record under eight rules.
A review of it by a model of another family named the faults that the tool fixes.
For assumptions 1 to 18, sound, doubtful and wrong are that review's verdicts, but for assumption 12, whose verdict the tool's spec gives from a later reading of a deployment.
For assumptions 19 to 49, a verdict is the tool's own.
Numbers from the prototype's record come from that deployment's daemon log over four days, in which every slot was in use for 99.95% of the time.
That record is not in this repository.

### A limit beside the assumptions: a workflow experiment drawn at random

A window may list workflow experiments in its `window.json` (`experiments`), as the daemon's configuration declares them.
`collect` writes none, so the replay of a collected window draws no experiment arm, and only a window written by hand lists one.
An experiment in the `random` mode draws an arm for each issue or tree that the daemon records, from one source of the run's seed.
That source starts afresh at each boot and is read in the order in which the engine and the admission pass record their issues and trees, so a draw is keyed to no issue and no stretch.
A rule that records issues in another order, or at another boot, draws another arm for one of them.
So the runs of two rules at one seed are not paired on such a window, and the tool does not key those draws, because keying them would change the daemon's own experiment code.
Each run counts those draws in `run.json`, as `unkeyedDraws.experiments`.
`compare` says, for each window, whether its `window.json` lists an experiment.
On a window that lists one in the `random` mode, `compare` calls no rule better or worse than `main`: it gives each difference there as not paired, and names no rule best while that window is compared.
The fixture window `random-experiment` is such a window.

### Assumption 1: A work segment takes its recorded time at any start and at any number in flight

Doubtful.
If load slows work, the replay favours a rule with more issues in flight.
The tool takes the slowdown factor f: a segment that starts with n issues in flight in the replay and m in the record runs its recorded length times `f^((n − m)/42)`.
At f = 1, or where n = m, a segment keeps its recorded length.

**Evidence in a recorded window.** _One step of load, read across a change of the rule._
The prototype's record shows no slowdown from 186 to 228 issues in flight.
That step came with a change of the slot rule, so it does not separate the load from the rule.
f is the slowdown for 42 more issues in flight, which is the size of that step.

**If it is wrong.**
If work slows as more issues are in flight, a replay at f = 1 moves the measures so.

- Issues finished: Up for a rule with more issues in flight than the record, because its segments end sooner in the replay than they would.
  Down for a rule with fewer.

- Started work that waits for a slot: Down for a rule with more issues in flight, because shorter segments bring each holder sooner to the moment at which it gives its slot back.
  Up for a rule with fewer.

- Priority taken by holders: Unknown.
  Segment lengths change which issues hold slots at each moment, but not by their rank.

- Other delays: Down for a rule with more issues in flight, because each round of a send-back loop, and the work of each blocker, keeps its shorter recorded length.
  Up for a rule with fewer.

- Slots: Down for a rule with more issues in flight, because slower work would hold more slot-hours in a turn and keep more issues in flight.
  Up for a rule with fewer.

**The reading that would test it.** _Segment lengths against the issues in flight, under one rule._
Run with `--trace-segments`, and read the phase, m and recorded length of each segment in `segments.jsonl`.
In a span with one rule, a recorded length that grows with m, for the segments of one phase, shows a slowdown.
`compare` gives, for each rule, the f at which its rank against `main` changes, or that no f up to 2 changes it.

### Assumption 2: A stretch past the review keeps its recorded length and its outcome

Doubtful.
If architects and mergers are slower with more issues past the review, the replay favours a rule that keeps slots past the review.
The tool keeps each stretch past the review at its recorded length and outcome, and the comparison says so.

**Evidence in a recorded window.** _Stretches past the review, with what ended each._
The phase lines give each visit to accepting, retro and merging, with its time and the trigger that ended it.
In the prototype's record, the architect or the merger sent work back within a median of 15 to 83 minutes.
442 of 682 entries into accepting came back after a send-back.

**If it is wrong.**
Suppose that architects and mergers are slower when more issues stand past the review.
Then, for a rule that keeps slots past the review and puts more issues there, the replay moves the measures so.

- Issues finished: Up, because each stretch past the review ends at its recorded length, sooner than it would.

- Started work that waits for a slot: Down, because each holder past the review reaches done, and frees its slot, sooner in the replay.

- Priority taken by holders: Unknown.
  The stretches change how long the holders past the review hold their slots, not their rank.

- Other delays: Down, because an issue that waits on an issue past its review ends its wait sooner in the replay.

- Slots: Down, because longer stretches would hold more slot-hours and keep more issues in flight.

**The reading that would test it.** _Stretch lengths against the issues past the review._
For each stretch past the review in the record, read its length and the number of issues past the review at its start.
A length that grows with that number shows the slowdown.
A window recorded under a rule that keeps slots past the review gives a second reading.

### Assumption 3: Send-backs keep their count and cause

Doubtful for conflicts.
The first replay saw more conflicts after a longer wait for a slot.
If a longer wait brings more conflicts than the fitted rate gives, the replay favours a rule under which branches wait longer.
The tool draws conflicts from a rate that it fits on the stretches inside the window.
The rate has a base, a term for each phase in which a stretch can end, a term for each release part of the stretches that end in merging, a term for each phase's stretches that span a boot, and a merges term, all fitted together.
The base stands for the stretches that end in merging in the window's first release part, and each other phase's term moves the rate of the stretches that end in that phase.
A phase or a part takes a term of its own only where both groups that the term sets apart hold a conflict and a stretch without one: the phase's stretches and the base's, a later part's stretches and the first part's, a phase's stretches that span a boot and those that span none.
Every other group draws at the rate of the group that it would be set apart from.
The main branch keeps recorded conflicts.
The git branch uses stored verdicts, and the drawn branch uses the fitted model for every draw.
On those branches the timing and order of touching merges can change the result.

Merging's rate is each release part's own.
A release that posts the merger's conflicts records each as a conflict, and a release that does not records it as another send-back, which the replay keeps as a recorded loop.
In window one the merger sent work back on about 47% of its visits in both parts: 93 send-backs and 18 conflicts of 238 visits under `legion-v20.25.1`, and 8 and 66 of 158 under `legion-v20.42.5`.
Only the split between send-backs and conflicts moved, so one rate over both parts would count the earlier part's conflicts twice.
Under every rule, a stretch that ends in merging draws at the rate of the part in which its end falls.

A daemon's boot reads each pull request again.
So a stretch that a boot read the pull requests inside has a chance of its own in its phase, and the tool reads that phase's stretches that span a boot apart.
At a boot, each stretch under way draws there, once, at its phase's chance for a stretch that spans a boot, whatever work segment of the stretch runs.
A conflict that it draws there enters at the boot where the issue stands in the phase in which the record ends the stretch, and the stretch after its loop takes a draw of its own.
Where the issue stands in another phase, the conflict enters at the stretch's end in that phase, without a draw of its own, and not at all where the stretch breaks or ends in another phase first.
A stretch that draws none at a boot draws no more.
A stretch that spans no boot draws at its end at its phase's other chance.
A draw reads no boot after its own moment.
A stretch keeps its one draw through its moves on to the next phase, a hold, a recorded loop that it passes over and a merge forward; only a move back, the merge, a move out of the stretch's phases or the issue's leave ends it.

A drawn conflict's work is a recorded loop that the tool draws among every loop of the record that ends before the end of the run-out, the loops of the reach before the window's start among them.
A loop still open at the end of the run-out has no known length and is left out, so the longest loops are drawn a little less often than the record ran them.
In window one that leaves out 6 loops from testing, 9 from reviewing, 8 from accepting, 17 from merging and 1 from retro.
The tool draws no length that the record does not show.

A draw does not depend on the record's outcome.
So a stretch that the record ended in a conflict can draw none, and its issue then goes on past its recorded loop into its later recorded work.
The acts of the loop that it passes over on other issues keep their recorded times: a child's filing, its release, a link, and an edit of another issue.
Its issue does not close before a child that the record filed under it before its own close has entered, as the daemon holds a merged issue's close while an issue beneath it is in the workflow.
A stretch that the record ended otherwise can draw a conflict, and its loop pushes the issue's later work later.
A window's run-out holds the record's work after `--to`, so an issue that goes on past a loop runs into that work.
Work recorded after the end of the run-out is not in the replay.
So an issue that goes on past a loop longer than the run-out stands in its last recorded work from the end of that work to the end of the run-out.
An approval that an added loop pushes past `--to` is not counted.
It keeps every other send-back loop at its recorded count and length.

**Evidence in a recorded window.** _A conflict rate fitted on the window, and loops kept as recorded._
The phase lines give each move back with its trigger, and the trace names its cause.
`run` prints the conflict model: the window, and the stretches and conflicts, that it was fitted on, the base, the term of each phase with the stretches and conflicts of that phase, each phase's term for the stretches that span a boot with those stretches, the term of each release part for the stretches that end in merging with that part's stretches, conflicts and the chance at no merges, the merges term, each with its interval, and the odds ratio for each doubling of the merges.
`compare` prints the same terms for each window that measures conflicts, each part's rate among them.
In the prototype's record, that odds ratio was 1.21, with a 95% interval of 0.92 to 1.35, so the record does not show the merges term.
In one deployment's window, a rate with no phase term gave the stretches that end in merging about twice the share of conflicts that the record shows for them, so the rate takes a term for each phase.
In window one, merging's conflicts were 16 of 230 stretches under `legion-v20.25.1` and 51 of 145 under `legion-v20.42.5`.
There, accepting's stretches conflicted in 30 of 52 that spanned a boot, against 7 of 175 that spanned none, and testing's in 29 of 136 against 95 of 680.
`run` prints the number of recorded conflict loops that it passed over and the number of conflicts that it drew where the record shows none, and `run.json` counts them as `divergence.skippedLoops` and `divergence.addedLoops`.

**If it is wrong.**
If a longer wait brings more conflicts than the fitted rate gives, the replay moves the measures so for a rule under which branches wait longer.
If a boot raises the chance of a conflict more or less than the window shows, the replay misstates how many returned issues wait for a slot at once.
If the loops still open at the end of the run-out are the longest, the drawn loops are a little short.
If the run-out is shorter than a loop that the replay passes over, the issue of that loop stands with no recorded work before the end of the run-out, and holds its slot with no work there.

- Issues finished: Up, because the replay draws fewer conflicts for that rule than it would meet.

- Started work that waits for a slot: Down where a conflict moves its issue back from a phase that keeps no slot, because each missed conflict is a start that would wait for a slot.

- Priority taken by holders: Unknown.
  The missed conflicts change which issues wait, not their rank.

- Other delays: Down, because the replay counts fewer rounds and hours of `conflict` than the rule would bring.

- Slots: Down, because each missed conflict is work, with its turns and its time in flight, that the replay does not run.

**The reading that would test it.** _Conflicts against the wait before each stretch._
For each stretch of the record, read whether it ended in a conflict, the phase that it ended in, the merges into its base during it, and how long its issue waited for a slot before it.
At the same phase and the same number of merges, a higher rate of conflict after a long wait shows the effect that the rate leaves out.
For each phase, the share of the stretches that end there in a conflict in a window that the rate was not fitted on tests the phase terms.
A window that records no conflict of its own, such as one of a release that records a conflict as red checks (assumption 22), tests neither the phase terms nor the merges term.
Compare the git and drawn branches against the recorded conflicts on `main`.
For each phase, the share of the stretches that span a boot and end in a conflict, against the share of those that span none, in a window that the model was not fitted on, tests the boot term.
For each release part, the share of the merging stretches that end in a conflict, in a window of the same releases, tests the part terms.
The number of recorded loops that a run passes over and the number that it adds, over the seeds, show how far the draws move the record's loops.

### Assumption 4: A link removal, a child moved out, a gate closing and an outside blocker's close happen at their recorded time, or at their recorded offset from the wait's start

Doubtful, and the direction is unknown.
The tool runs both as branches.
`main` places each such event at its recorded offset from the start of the wait that it ends, and `main+timing=wall` at its recorded time.
Under the rule that made the record, both give the record's times.

**Evidence in a recorded window.** _The events that end each wait, and who made them._
Dispatch's events give each link's removal, each child's move out and close, and each blocker's close.
The admission lines and the design gate lines give the start of each wait.
In the prototype's record, a Legion architect took the link off in 14 of the 21 link waits that ended, after a median of 43.8 hours.
In the other 7, the blocker reached done, after a median of 36.0 hours.
14 waits on children ended because a new spec version closed the parent's design gate.

**If it is wrong.**
If such an event follows neither its recorded time nor its offset from the wait's start under another rule, the replay moves the measures so.

- Issues finished: Unknown.
  The window does not show whether an event follows the clock or the start of its wait.

- Started work that waits for a slot: Unknown.
  An event that comes sooner than the replay gives makes its issue wait for a slot sooner, and a later one later.

- Priority taken by holders: Unknown.
  The events change which issues wait, not their rank.

- Other delays: Unknown.
  The hours of the waits on other issues move with these events, earlier or later.

- Slots: Unknown.
  A wait that ends at another time changes when its issue holds a slot.

**The reading that would test it.** _The offset of each end from the start of its wait._
For each wait that ended in the window, read its start and the event that ended it.
Ends that keep their offset when the start of the wait moves support `offset`, and ends at times of their own support `wall`.
Two windows recorded under two rules show whether the offsets stay the same when the starts move.
`validate` gives `main` and `main+timing=wall` side by side.

### Assumption 5: A person answers at the recorded time, or at the recorded offset

Doubtful, and the direction is unknown.
The tool runs both as branches.
`main` places each answer at its recorded offset from the question, and `main+answer=wall` at its recorded time.
The question is the ask's opening, or the registration of the spec version that the person reviews.

**Evidence in a recorded window.** _Each question and its answer._
Dispatch's events give each ask's opening and its answer, and each spec version's registration and its review.
The daemon's line `workflow: root waits on a person and keeps its admission slot` gives each root that waits on a person.

**If it is wrong.**
If a person answers at neither the recorded time nor the recorded offset under another rule, the replay moves the measures so.

- Issues finished: Unknown.
  An answer that comes sooner or later than the replay gives moves the root's next start the same way.

- Started work that waits for a slot: Unknown.
  An answered root counts in reading (b) from its answer to its slot, so that count moves with the answer.

- Priority taken by holders: Unknown.
  The answer changes when the root enters the line, not its rank.

- Other delays: Unknown.
  A wait on a person is not one of the other delays, but an issue that waits on the root ends its wait when the root's work ends.

- Slots: Unknown.
  A root that waits on a person holds its slot in the state `person`, so its slot-hours with no worker in a turn move with the answer.

**The reading that would test it.** _The delay of each answer against the time of its question._
For each ask and each spec review in the window, read the delay from the question to the answer, and the hour of the answer.
Delays that stay the same at any hour support `offset`, and answers grouped at certain hours support `wall`.
`validate` gives `main` and `main+answer=wall` side by side.

### Assumption 6: The issues in flight at the window's start enter in their recorded state

Sound for one window, and not for a steady state.
The start state carries the recorded rule's choices into every rule's first day, which favours the recorded rule.
The tool reports the first day apart: each scorecard of a window longer than a day carries `firstDayApart`.

**Evidence in a recorded window.** _The start state and the issues that it could not place._
The tool rebuilds the state at `--from` from the lines and events of the reach.
Each run gives the number of issues that it could not place, and `run.json` names each with its reason.
A scorecard counts them as `unplaced`.

**If it is wrong.**
If the issues in flight at `--from` would stand otherwise under another rule, the replay moves the measures so, most in the first day.

- Issues finished: Down for a rule other than the recorded one, because its first day starts from the recorded rule's choices.

- Started work that waits for a slot: Up for a rule other than the recorded one, for the same reason.

- Priority taken by holders: Up for a rule other than the recorded one, for the same reason.

- Other delays: Up for a rule other than the recorded one, for the same reason.

- Slots: Unknown.
  The start state fixes which issues hold the slots at `--from`, so the first day's slot-hours follow the recorded rule.

**The reading that would test it.** _The first day against the rest of the window._
For each rule, compare each measure of `firstDayApart` with the same measure over the whole window.
A rule whose first day differs from its later days more than the recorded rule's first day does carries the start state.
A later `--from` on the same lines shows whether the difference gets smaller.

### Assumption 7: An issue may start once its todo, its links, its gate, its ancestors and its tree allow it, at the times recorded

Wrong in the first replay, which read only `todo` and links.
A hold that the replay misses starts an issue early, which favours a rule that starts more issues.
The tool reads every hold at its recorded time: a design gate, a link on a child, an ancestor, an accepted ancestor, a tree, a held phase, the project's pause and a boot's catch-up.

**Evidence in a recorded window.** _The holds that the record names._
The daemon's lines give each design gate's opening and closing, each held phase and its release, each pause and resume, each tree that lingers, and each boot's catch-up.
A window of a release that writes no line where a phase is held reads each hold from the failed claim of the phase's worker.
That hold ends at its release line, at the next launch of a claim of the issue in the failed role, at the issue's next move, or at its leave, whichever comes first.
A relaunch or a move with no release line shows the record working the phase again, and the replay releases the hold there as it releases a recorded one.
Dispatch's events give each link, label and status.
The test `TestEveryHoldHasASourceInTheReplay` builds one small window for each hold of the daemon, and fails where a start does not wait from the start of the hold to its end.

**If it is wrong.**
If a hold stands that the replay misses, or at another time than the record gives, the replay moves the measures so.

- Issues finished: Up for a rule that starts more issues, because a missed hold lets an issue start early.

- Started work that waits for a slot: Unknown.
  A missed hold lets an issue take a slot sooner, but the line gives the returned issues their slots first.

- Priority taken by holders: Unknown.
  A held issue does not wait for a slot, so a missed hold adds a waiting issue at its own rank.

- Other delays: None.
  A hold on a first start is not a wait of a started issue, and no send-back loop reads it.

- Slots: Up for a rule that starts more issues, because each issue that starts early is in flight sooner.

**The reading that would test it.** _Each first start against the holds on it._
For each issue that the record started in the window, read the holds that stood before its first slot, and the moment at which the last one ended.
In a replay of the recorded rule, a first slot before that moment shows a missed hold, and `validate` gives each issue's error in its first slot.
A run's report names each issue that it could not place, with its reason.

### Assumption 8: The work of an issue that never started in the window is unknown

A scenario that gives such an issue work that finishes favours a rule that starts more issues.
The scenario that gives it no work disfavours that rule.
The tool names the scenario that each run used: `none`, `drawn` or `drawn-done`.
Under `drawn` and `drawn-done`, an issue draws the work of an issue of its own kind and its own tree where the record started one in the window, or finished one for `drawn-done`; else of its kind, its priority and its repository; else of its kind in the whole window.
Each run counts how many issues drew from each of these three, and how many it admitted with no work at all, with the slot-hours that they hold.
Drawn work keeps none of the Dispatch events of the issue that it copies, so a drawn issue never waits on a person.
So for a rule that admits more issues than the record did, `none` and `drawn` bracket the answer: `none` gives the admitted issues no work, and `drawn` gives them work that waits on nobody.
Read a result of such a rule under both, never under one alone.

**Evidence in a recorded window.** _The issues that never start._
A window shows each issue handed to Legion, and whether the record ever gave it a slot.
In the prototype's record, 394 issues waited to start at one moment, 194 children and 200 roots.
A new root waited a median of 16.7 hours for its first slot.
In one deployment's recorded window replayed under `none`, the extra slots of a higher cap went almost all to children that the record never started, and each held its slot with no turn to the window's end.

**If it is wrong.**
If the work that a scenario gives differs from the work that the issue would do, the replay moves the measures so for a rule that starts more issues than the record did.

- Issues finished: Up under `drawn-done` if the real work finishes less often than the drawn work.
  Down under `none` if any of the real work finishes.

- Started work that waits for a slot: Unknown.
  Drawn work brings the send-backs of the issue that it copies, and each can wait for a slot.

- Priority taken by holders: Unknown.
  The scenario changes the work of an issue, not its rank.

- Other delays: Unknown.
  Drawn work brings the loops and the waits of the issue that it copies.

- Slots: Up for the slot-hours with no worker in a turn under `none`, because such an issue holds its slot to the window's end with no turn.

**The reading that would test it.** _The three scenarios side by side, and a later window._
`validate` and `compare` give `main`, `main+neverStarted=drawn` and `main+neverStarted=drawn-done` side by side.
A later window that starts the same issues shows their real work.
Compare it with the work drawn for issues of the same kind, a root for a root and a child for a child.

### Assumption 9: The line orders returned work first and then by rank

Sound.
A replay runs the line's own code, so it orders the line as the daemon does.
A rule that orders the line otherwise does so in its own code.

**Evidence in a recorded window.** _The slots taken, in their order._
The `admission: slot taken` lines give each slot taken, with its issue and its time.
The admission lines that give a slot back to an issue whose work came back, or whose wait ended, name that issue.
In the prototype's record, each slot that came free went first to the oldest returned issue whose wait had ended, and otherwise to new work.

**If it is wrong.**
If the daemon gave slots out in another order than its code gives, the replay moves the measures so.

- Issues finished: Unknown.
  Another order changes which issue takes each slot, not how many slots come free.

- Started work that waits for a slot: Unknown.
  A returned issue that stands first in the line waits less than one that does not.

- Priority taken by holders: Unknown.
  An order closer to the board's rank takes less priority, and one further from it takes more.

- Other delays: Unknown.
  A wait on another issue ends when the work of its blocker ends, and the line decides when that work starts.

- Slots: Unknown.
  The order chooses which issue takes a free slot, not whether the slot is given.

**The reading that would test it.** _Each slot taken against the first issue of the line._
For each slot taken in the record, compare its issue with the first issue of the line at that moment in a replay of the recorded rule.
`validate` gives each issue's error in its first slot, and an order that differs shows as errors of opposite sign for pairs of issues.

### Assumption 10: A link or a child has the lifetime that its events give it

Wrong in the first replay, which merged them over the window.
A lifetime merged over the window makes each wait on it longer, which favours a rule under which a wait gives its slot back.
The tool keeps the lifetime of each link and of each child.

**Evidence in a recorded window.** _Each link and each child, with its events._
Dispatch's events give each `blocked_by` link's addition and removal, and each child's filing, its moves out and its status.
The fixture windows `link-added-and-removed` and `child-moved-out` replay one of each.

**If it is wrong.**
If a link or a child lasts longer in the replay than it would, the replay moves the measures so.
If it lasts less long, each measure moves the other way.

- Issues finished: Down for every rule, because each waiting issue finishes later.
  It moves down most for a rule that keeps the slot through a wait, such as `cover` and `legion-v19.57.3`.

- Started work that waits for a slot: Up for reading (b) under a rule under which a wait gives its slot back, because the waiting issue counts there for the whole wait.

- Priority taken by holders: Unknown.
  A longer wait changes which issues wait, not their rank.

- Other delays: Up, because the hours of each wait on another issue grow with the lifetime.

- Slots: Up for the slot-hours with no worker in a turn under a rule that keeps the slot through a wait, because the waiting holder keeps its slot longer.

**The reading that would test it.** _Each wait and what ended it._
A scorecard gives the count and the hours of the waits by what ended them, from `blocker_done` to `open`.
`validate` gives the gap of each `waits[<word>].hours` between the record and the replay of the recorded rule.

### Assumption 11: A slot that comes free is given out in the same pass

Sound for the record.
A slower hand-out would make the waits for a slot of every rule longer alike.

**Evidence in a recorded window.** _The time from a slot coming free to its hand-out._
Each `admission: slot released` and `admission: slot taken` line gives its time and the slots in use.
In the prototype's record, the median time from a slot coming free to its hand-out was 18 microseconds, and the longest was 88 seconds.

**If it is wrong.**
If the daemon gave a free slot out later than the pass that freed it, the replay moves the measures so.

- Issues finished: Up, a little, for every rule alike, because each slot goes to the next issue sooner in the replay.

- Started work that waits for a slot: Down, a little, for every rule alike, for the same reason.

- Priority taken by holders: Unknown.
  A later hand-out changes the number of holders at each moment, not their rank.

- Other delays: Up, a little, for every rule alike, because a blocker that starts later ends the waits on it later.

- Slots: Up, a little, for every rule alike, because a slot that the replay gives out at once is held for longer.

**The reading that would test it.** _Each slot released against the next slot taken._
Pair each slot released with the next slot taken while an issue waited, and read the time between them.
From this release, one `admission: a pass moved slots` line names each slot that one pass gave out and took back.

### Assumption 12: A waiting issue costs no pod after the idle release

Doubtful.
The review of the first replay judged it sound after 10 minutes, and a later reading of one deployment found waiting issues that kept their pod much longer.
The replay counts no pod, so the result names the pods of waiting issues as a cost that it does not measure.

**Evidence in a recorded window.** _The pods of the issues that waited for a slot._
The daemon releases an issue pod that is idle for 10 minutes.
In the prototype's record, it released 2,266 idle pods in 25 hours, which gave back 2,247.53 pod-hours, and the median time from a wake to the role's first turn was 160 seconds.
On 2026-11-24, 43 of the 110 waiting issues with a pod in one deployment's daemons had had no role turn for more than 10 minutes.

**If it is wrong.**
If a waiting issue keeps its pod past the idle release, the replay moves the measures so.

- Issues finished: None, because the measures count slots and work, not pods.

- Started work that waits for a slot: None, for the same reason.

- Priority taken by holders: None, for the same reason.

- Other delays: None, for the same reason.

- Slots: None, because a waiting issue holds no slot, and the measures count an idle slot as slot-hours with no worker in a turn.
  The cost of the pod of a waiting issue stands outside the measures, and the result names it.

**The reading that would test it.** _The waiting issues against their pods._
At moments of a window, count the issues that wait for a slot and still have their pod, and read from the daemon's log when each of those pods was released.
A waiting issue whose pod stays up for more than 10 minutes with no role turn is a cost that the replay does not count.

### Assumption 13: Work after a send-back takes its recorded time, or that time less its wait for a slot

Doubtful.
The recorded time holds the recorded rule's own wait for a slot, which favours that rule.
The tool runs both as branches.
`main` keeps each recorded length.
`main+afterSendBack=less-slot-wait` takes the visit's recorded wait for a slot, from the move back to its worker start, off the first piece of work after a send-back.
That is the most time that a worker which never went cold could save.

**Evidence in a recorded window.** _The wait before the work after a send-back._
The phase lines give each move back, and the slot lines and the `supervise: launched` lines give the worker start after it.
In the prototype's record, a send-back past the review waited a median of 16.8 to 18.0 minutes for its slot, and at most 142.4 minutes.
The daemon releases an issue pod that is idle for 10 minutes, so the worker after a longer wait can start in a new pod.

**If it is wrong.**
If the work after a send-back is shorter than its recorded length under a rule that removes the wait, `main` moves the measures so for that rule.
If the work is as long, `less-slot-wait` moves each measure the other way.

- Issues finished: Down for a rule that removes or shortens the wait for a slot after a send-back, such as `keep-past-review`, because its work keeps the length that the wait gave it.

- Started work that waits for a slot: Up for such a rule, because each visit holds its slot for the longer work, and other issues wait longer.

- Priority taken by holders: Unknown.
  The longer visits change which issues hold slots, not their rank.

- Other delays: Up for such a rule, because a round's hours run to the next entry into the phase that it left, and the longer work is inside them.

- Slots: Up for the slot-hours held under such a rule, because each visit holds its slot for the longer work.

**The reading that would test it.** _First segments after a send-back, with and without a wait._
Run with `--trace-segments`, and read from the record which segments are the first after a send-back, and how long each issue waited for a slot before it.
Compare the recorded length of such a segment after a long wait with its length after a short wait.
A length that grows with the wait shows the time that `less-slot-wait` takes off.

### Assumption 14: A cover under spec v4's rule

Wrong in the first replay, which left out some covers and kept others too long.
Either fault misstates the rule with covers.
The tool runs the rule's own code, the rule `cover`, whose start may take a cover.
Spec v4's rule is the candidate `cover` of [The slot rules](#the-slot-rules), whose choices `legion replay rules` prints.

**Evidence in a recorded window.** _No record holds a cover._
No daemon ran the rule `cover`, so no window shows a cover.
The fixture windows `send-back-at-full-cap` and `two-waiters-one-blocker` replay covers that their authors worked out by hand from the rule.
In a replay under `cover`, each cover counts as a slot given out in `slotsGivenByDay`.
Under `cover` a started parent with an open Dispatch child is a waiter as one with an open `blocked_by` link is: it keeps its phase, with no move into `awaiting_children`, and a child of it that has not started starts on its slot and holds it for the parent while the cover stands (`record.SlotRule.StartedParents`, `blockers.CoverWaits`).

**If it is wrong.**
If the replay leaves out a cover that the rule takes, or keeps one longer than the rule does, the replay of `cover` moves the measures so.

- Issues finished: Unknown.
  A cover left out delays the issue that a waiter waits on, and a cover kept too long keeps a slot from the line.

- Started work that waits for a slot: Unknown.
  A covered waiter counts in reading (b), so each fault moves that count in the direction of the fault.

- Priority taken by holders: Unknown.
  Each fault changes which issue holds a slot, not the ranks.

- Other delays: Unknown.
  The waits on other issues end when the covered issue's work ends, which each fault moves.

- Slots: Unknown.
  Each fault moves the slot-hours of the covers in the direction of the fault.

**The reading that would test it.** _Each cover's start and end against the rule._
In a replay under `cover`, read from `log.jsonl` each slot given out and each `admission gives a slot back to an issue whose cover ended` line.
Each cover must begin when a waiter that holds a slot waits on an issue that has not started.
It must end when that issue is done, or when the waiter no longer waits on it.

### Assumption 15: An issue that stops working at the window's end would not have finished

Wrong: it counts the end of the window as a stop, which disfavours a rule that leaves more work under way at the end.
The tool reports the issues finished (`done`) and the work left (`workLeft`) apart.

**Evidence in a recorded window.** _The work under way at `--to`._
A window shows each segment that the record leaves unended at `--to`.
A replay does not end such a segment before the window does, and the scorecard counts its issue in `workLeft`.

**If it is wrong.**
It is wrong, so a reader who takes `done` alone as the work that a rule finishes reads the measures so.

- Issues finished: Down for a rule that leaves more work under way at `--to`, because some of that work would finish soon after.

- Started work that waits for a slot: None, because the measure reads the window's span alone.

- Priority taken by holders: None, for the same reason.

- Other delays: Down, a little, because a loop or a wait that stands at `--to` counts its hours only up to `--to`.

- Slots: None, because the slot-hours of every rule end at `--to`.

**The reading that would test it.** _Done and work left, and a later end._
Read `done` beside `workLeft` for each rule.
Replay a window that ends later on the same lines, and count how much of the first window's work left reaches done after its end.

### Assumption 16: An issue that reopens keeps its later work

Wrong in the first replay, which dropped it.
Dropping the later work makes the issues finished and the slots held too low under every rule.
The tool keeps the later work.

**Evidence in a recorded window.** _A reopen, and the work after it._
Dispatch's events show an issue that leaves done, and the phase lines show its next start.
The fixture window `reopen-works-again` replays one, in which the issue counts in `done` and again in `workLeft`.

**If it is wrong.**
If the later work would not come under another rule, the replay moves the measures so.
If the rule would bring more later work, each measure moves the other way.

- Issues finished: Up, because the replay counts the done of the later work again.

- Started work that waits for a slot: Up where the later work waits for a slot in the replay.

- Priority taken by holders: Unknown.
  The later work changes which issues hold slots, not their rank.

- Other delays: Up, because the replay counts the loops of the later work.

- Slots: Up, because the replay counts the slot-hours and the time in flight of the later work.

**The reading that would test it.** _The reopens of two windows._
For each reopen in the window, read the event that reopened the issue, and who made it.
The number of reopens for each issue finished, in two windows recorded under two rules, shows whether the rule changes it.

### Assumption 17: The links and children of an issue are the same under every rule

Doubtful, because a planner files children after its parent starts.
Children filed at their recorded times favour a rule that starts parents later.
The tool moves the filing of a child with its parent's start.
A filing that falls in the parent's work moves with that work.
A filing that falls in no work of the parent, made while the record shows the parent started in its run, enters at its recorded offset from the replay's first start of the parent, or at its recorded time where that is later, whoever filed it, because the child's open status holds the parent's first start.
An act of an issue's own architect on the issue that falls in no work of the issue, such as a link that it sets there, moves with the issue's first start in the same way.
An architect's act on a child that it filed follows the filing, at its recorded offset from it, and no recorded event of a child enters before the child's filing.
It never loses a recorded child: where the replay passes over, drops or never opens the work that a child is filed in before the parent leaves the workflow, or the parent leaves before its first start, the filing enters at its recorded time, or then where that time has passed.
An act of work that runs later in the replay than in the record can fall past the end of the run-out, and then it does not enter.
In window one, an architect filed a child 53 hours into a piece of its work in accepting that the record shows still open at the end of the inputs, and a seed that brings that piece more than about 42 hours late files no such child.
A worker's other events on another issue keep their recorded times: an edit of that issue, a link that the worker adds to it or takes off, and a child that the worker moves out.

**Evidence in a recorded window.** _Children filed after their parent started._
Dispatch's events give each child's filing, and the phase lines give its parent's start.
The fixture window `child-filed-after-parent-start` replays one under `main` and `keep-past-review`.
The tests of the built windows replay a filing in work that runs late, a filing in work that never opens before its issue leaves, an architect's filing under its own issue that its issue's late start moves, and a link that an architect adds to another issue in work that the replay opens early.
They also replay a filing under a member of a tree, by the tree root's architect and by a person, that the member's late start moves, an architect's link on its own issue that the issue's late start moves, an architect's edit of a child that it filed, which follows the filing, and another issue's architect's edit of a child, which waits for the child's filing.
In window one, 189 architect filings of a child under the architect's own issue fall in no work of the issue, and 252 fall in its work.

**If it is wrong.**
If an issue would have other links or children under another rule, or a child's filing does not follow its parent's start, the replay moves the measures so.

- Issues finished: Up for a rule that starts parents earlier, if a child's filing does not follow its parent's start, because the replay files the child too soon.
  Unknown if the links and the children themselves differ.

- Started work that waits for a slot: Unknown.
  The window shows only the links and the children that the recorded rule met.

- Priority taken by holders: Unknown.
  Other children would stand at ranks that the window does not show.

- Other delays: Unknown.
  The waits on other issues are the waits on these links and children.

- Slots: Unknown.
  Other children would hold slots that the window does not show.

**The reading that would test it.** _The offset of each child's filing from its parent's start._
For each child filed after its parent started, read the offset from the start to the filing, and who filed it.
Offsets that stay close for one role support the move with the start.
Two windows under two rules show whether the number of children and links for each parent differs.

### Assumption 18: A conflict under the release that a project runs next behaves as recorded

Wrong for a project that moves to a release with cybermonklabs/legion#4464, where a conflicting head keeps its phase.
Its recorded conflict loops then make the conflicts of that release look too high.
The tool takes what a conflicting head does as part of the rule: `merge_forward`, `moves_back_conflict` or `moves_back_checks_red`.

**Evidence in a recorded window.** _The conflict moves of the record._
A window of a release whose conflicting head moves back holds phase lines with the trigger `conflict`, or `checks_red` in older releases.
Under `main`, a conflict keeps its phase, and the replay adds a merge-forward segment (assumption 23).
In the prototype's record, 359 conflict rounds took 2,315 hours in four days.

**If it is wrong.**
If a replay kept a recorded conflict loop for a release where a conflicting head keeps its phase, it would move the measures so.

- Issues finished: Down, because each kept loop sends its issue back to implementing.

- Started work that waits for a slot: Up where the loop leaves a phase that keeps no slot, because its issue then waits for a slot to go back to work.

- Priority taken by holders: Unknown.
  The kept loops change which issues wait, not their rank.

- Other delays: Up, because the replay counts the rounds and hours of `conflict` of each kept loop.

- Slots: Up for the slot-hours held and the issues in flight, because each kept loop is more work.

**The reading that would test it.** _A window of the release that the project runs next._
Collect a window recorded under that release, and read its conflicts, each a merge forward that keeps its phase under `main`.
Compare its `loops[conflict]` with a replay of the earlier window under `main`.

### Assumption 19: Under the release at 9542c6fb, whose workflow has the phase awaiting_merge, the time in awaiting_merge is folded into merging

This assumption and the three after it apply only to a window recorded under the release at 9542c6fb, release 19.57.3, from which the rule `legion-v19.57.3` comes.
A slot held in awaiting_merge counts as held in merging.
So a rule that keeps a slot in merging is charged that time, and a rule that keeps none is not.

**Evidence in a recorded window.** _The visits to awaiting_merge._
That release's workflow has the phase `awaiting_merge`, which today's workflow does not have, and it kept no slot there.
Its phase lines give each move into and out of `awaiting_merge`, and the trace reads that time as merging.

**If it is wrong.**
If the time in awaiting_merge should not count as merging, the replay moves the measures so in a window of that release.
It does so for a rule that keeps a slot in merging and does not give it back at the ready mark, such as `keep-past-review` and `cover`.

- Issues finished: Down, because the slot that such a rule holds through that time keeps another issue from starting.

- Started work that waits for a slot: Up, because other issues wait for the slot that it holds.

- Priority taken by holders: Unknown.
  The time changes how long the issue holds its slot, not its rank.

- Other delays: None, because the time in merging is in no send-back loop, and the issue's done does not move.

- Slots: Up for the slot-hours held, because the replay holds the slot through that time.

**The reading that would test it.** _The work in awaiting_merge._
In a window of that release, read the turns of each issue's roles in `awaiting_merge`.
A visit with a merger in a turn is work of merging, and a visit with no turn is a wait for the merge.

### Assumption 20: Under the release at 9542c6fb, the merger's ready move is a mark inside merging, where the rule legion-v19.57.3 gives the slot back

If that release gave the slot back at another moment of merging, the slot-hours past the review under `legion-v19.57.3` move by the time between the two moments.

**Evidence in a recorded window.** _The ready move and the slot line._
That release's phase lines give the merger's `ready` move into `awaiting_merge`, and its slot lines give the slot given back.
The trace reads the move as a mark inside merging, and `legion-v19.57.3` gives the slot back at that mark.

**If it is wrong.**
If that release gave the slot back later than the mark, the replay of `legion-v19.57.3` moves the measures so.
If it gave the slot back earlier, each measure moves the other way.

- Issues finished: Up, because the replay frees the slot sooner, and another issue starts sooner.

- Started work that waits for a slot: Down, because the issues that wait for that slot take it sooner in the replay.

- Priority taken by holders: Unknown.
  The moment changes who holds the slot, not the ranks.

- Other delays: None, because the moment at which the slot goes back moves no send-back loop and no wait on another issue.

- Slots: Down for the slot-hours held past the review, by the time between the mark and the real release.

**The reading that would test it.** _The slot line at each ready move._
In a window of that release, compare the time of each `admission: slot released` line of an issue in merging with its `ready` move.
A release at the same moment supports the mark.

### Assumption 21: Under the release at 9542c6fb, checks_red from any phase is a move back to implementing

A `checks_red` move that did not send the work back would count a send-back loop that never ran, which disfavours that release's rule.

**Evidence in a recorded window.** _The checks_red moves._
That release's phase lines give each move by the trigger `checks_red`, with the phase that it left and the phase that it entered.
The trace reads each as a move back to implementing, with the cause `red_checks_or_conflict`.

**If it is wrong.**
If a `checks_red` move did not send the work back, the replay moves the measures so in a window of that release.

- Issues finished: Down for a rule that keeps no slot in the phase that the move left, because the replay makes the issue wait for a slot before work that never stopped.

- Started work that waits for a slot: Up for such a rule, for the same reason.

- Priority taken by holders: Unknown.
  The move changes which issues wait, not their rank.

- Other delays: Up, because the replay counts a round of `red_checks_or_conflict`, with its hours, that never ran.

- Slots: Up for a rule that keeps no slot in the phase that the move left, because the issue holds a slot in implementing that it would not hold.
  None under `legion-v19.57.3`, which keeps a slot in every phase of the workflow.

**The reading that would test it.** _The work after each checks_red move._
In a window of that release, read the next turn of the issue after each `checks_red` move.
An implementer's turn supports a move back, and a turn of the role of the phase that the move left does not.

### Assumption 22: Under the release at 9542c6fb, red checks and conflicts are one cause of a send-back

A window of that release cannot tell how a rule changes conflicts.
So a forecast across the change scores this cause only inside the total of all send-back loops, and the result says so.

**Evidence in a recorded window.** _One trigger for two causes._
That release moved an issue back by `checks_red` for red checks and for a conflict alike, so its phase lines do not say which.
A window of that release records no `conflict` move, so it fits a chance of conflict of 0 and draws no conflict.
Its loops of `red_checks_or_conflict` stay as recorded under every rule.

**If it is wrong.**
If conflicts grow with the wait while red checks do not, the replay of a window of that release moves the measures so for a rule under which branches wait longer.

- Issues finished: Up, because the replay keeps the record's rounds of the cause where the rule would bring more.

- Started work that waits for a slot: Down where the loops leave a phase that keeps no slot, for the same reason.

- Priority taken by holders: Unknown.
  The missed rounds change which issues wait, not their rank.

- Other delays: Down, because the replay keeps the record's rounds and hours of the cause.

- Slots: Down for the slot-hours held, because the replay leaves out the work of the missed rounds.

**The reading that would test it.** _Each checks_red move against GitHub's record._
For each `checks_red` move in a window of that release, read from GitHub whether the pull request's head conflicted with its base at that moment.
The share of conflicts among the moves, against the wait before each, shows how the cause would change with the rule.

### Assumption 23: Merge-forward work under main lasts as long as the implementing round of a recorded conflict loop

A merge forward shorter than a conflict's implementing round makes the conflict cost of `main` too high, which disfavours `main`.

**Evidence in a recorded window.** _The recorded conflict loops._
A window of a release whose conflicting head moves back holds each conflict loop: the move back by `conflict`, and the implementing round after it.
Under `main`, a drawn conflict keeps its phase, and the replay adds a merge-forward segment, with the worker that the daemon asks in a turn: the implementer in testing and reviewing, and the merger in merging.
Under `main`, a conflict in accepting or retro waits for merging, and its merge-forward segment, the merger's, runs when the issue moves into merging, with a length that the replay draws when the conflict comes (assumption 37).
Under `legion-v22.14.1`, the segment is the implementer's in every phase, and it runs at once.
The replay draws the length of that segment from the implementing rounds of the recorded conflict loops that left the same phase, among the loops whose work the record shows ending.
Where the window records no loop from that phase, the replay draws from the loops that left a later phase, and where it records none of those either, from the loops that left an earlier phase.
The fixture window `conflict-from-fitted-rate` replays one under `main`, `legion-v22.14.1` and `legion-v20.42.5`.

**If it is wrong.**
If a merge forward is shorter than the implementing round of a conflict loop, the replay moves the measures so for `main` and each rule built on it.

- Issues finished: Down, because each drawn conflict holds its issue longer than a merge forward would.

- Started work that waits for a slot: Up where the conflict comes in a phase that keeps a slot, because its issue holds the slot longer and others wait.

- Priority taken by holders: Unknown.
  The longer segments change how long holders hold their slots, not their rank.

- Other delays: Up, because a drawn conflict's round counts the merge-forward segment as its hours.

- Slots: Up for the slot-hours in a turn, because the worker that the daemon asks is in a turn for the whole merge-forward segment.

**The reading that would test it.** _Merge forwards in a window of `main`._
In a window recorded under a release with #4464, read each merge forward from the conflict to the push that ends it.
Compare those lengths with the implementing rounds of the conflict loops in a window of an earlier release.

### Assumption 24: A daemon down between two boots runs no pass

Sound for the record.
What arrives while the daemon is down waits for the next boot.
A replay that ran a pass then would start issues earlier than any daemon could.

**Evidence in a recorded window.** _The boots and the spans between them._
Each daemon process writes its start lines, such as `legion daemon started`, and writes no line after it ends.
The replay enters each boot and each tick at its recorded time, and no fact while the daemon was down.

**If it is wrong.**
If a pass ran in a span that the record shows as down, for example because the lines of a running process were lost, the replay moves the measures so.

- Issues finished: Down, because the replay starts nothing in that span.

- Started work that waits for a slot: Up, because the issues that waited through the span wait longer in the replay.

- Priority taken by holders: Unknown.
  The span changes when issues take slots, not their rank.

- Other delays: Up, a little, because a wait that ends in the span ends at the next boot in the replay.

- Slots: Unknown.
  The span changes which issues hold slots at the next boot.

**The reading that would test it.** _Spans with no line, against the daemon's own boots._
List each span of the window with no daemon line for longer than a tick interval.
A span between a process's last line and the next start lines is time down.
A span inside one process's life is a gap of the log, which a second source of lines, such as a cloud log export, may fill.

### Assumption 25: An outside blocker's close is seen at the next tick or boot, as the daemon sees it

A close that the daemon saw sooner would end link waits sooner.
So reading the close at the tick makes link waits longer, which disfavours a rule that keeps its slot through a wait.

**Evidence in a recorded window.** _Each outside blocker's close, and the next tick._
The daemon learns the status of a blocker in another project at each tick and each boot.
A tick comes once each `--tick-interval`, the deployment's `controller_wake_interval_seconds`.
Dispatch's events give the blocker's close.
The replay gives each tick the status, at that moment, of each outside issue that a waiter waits on.
A blocker of another project that runs a daemon of its own is written by that daemon, and the replay applies those writes as recorded, its close among them: only the writes of the window's own project's daemon are the ones that the replay makes again.

**If it is wrong.**
If the daemon saw such a close sooner than the next tick or boot, the replay moves the measures so.

- Issues finished: Down for a rule that keeps its slot through a wait, because its waiters hold their slots longer in the replay.

- Started work that waits for a slot: Up for reading (b) under a rule under which a wait gives its slot back, because the waiter counts there until its wait ends.

- Priority taken by holders: Unknown.
  The later close changes when the waiter leaves its wait, not its rank.

- Other delays: Up, because the hours of the link waits that such a close ended (`blocker_done`) grow by up to one tick interval.

- Slots: Up for the slot-hours with no worker in a turn under a rule that keeps its slot through a wait, because the waiter holds its slot longer.

**The reading that would test it.** _Each close against the end of the wait._
For each link wait that an outside blocker's close ended, read the time of the close from Dispatch.
Read the last `admission waits for what this issue waits on` line that names that blocker for the waiter.
A gap close to the tick interval supports the assumption.

### Assumption 26: A notice, a message or a prompt the daemon sends a worker or posts on Dispatch changes nothing the record's workers did after it: their work is the record's segments

The tool's own.
A notice that changed what a worker did, or how soon, would move that work by the replay's own timing of the notice.
Under the recorded rule, each notice falls where the record's did.
Under another rule, the work that the notice would have moved stays where the record put it, which favours the recorded rule's timing.

**Evidence in a recorded window.** _The rows that the replay finishes without acting._
The replay's worker stub finishes each Dispatch message, notice, controller notice, role message, hook prompt, update ask and merge-forward request without acting, and each start queued for news of the approved design alone that reaches its claim with news to tell.
The daemon gives the worker of such a start a task of news and no work, and launches its claim where an idle pod's release or the daemon's own rule suspended it; the replay leaves the claim as it stands.
`run` prints, beside this assumption, how many rows it finished under it, and `run.json` counts them under `divergence.finished`.

**If it is wrong.**
If a notice moved the work after it, the replay moves the measures so for a rule other than the recorded one.

- Issues finished: Unknown.
  A notice that comes sooner under the rule would move work sooner, and a later notice later.
  The replay moves neither.

- Started work that waits for a slot: Unknown, for the same reason.

- Priority taken by holders: Unknown, for the same reason.

- Other delays: Unknown, for the same reason.

- Slots: Unknown, for the same reason.

**The reading that would test it.** _Turns after a notice._
In the daemon's log, read each notice that the daemon sent a worker, and the start of that worker's next turn.
Turns that start soon after a notice more often than at other moments show that notices move work.
Compare the rows that `run.json` counts under this assumption between rules, because a rule that sends many more notices rests more on it.

### Assumption 27: What the daemon publishes on Dispatch for people and tools to read, the gated steps, the design gates, the role holders and the step and plan records, is read back by no decision a replay runs

The tool's own.
Sound: the daemon decides from its own record and from Dispatch's events.
A person who acted on a publication did so in a recorded event, which the replay applies.

**Evidence in a recorded window.** _The publication rows that the replay finishes without acting._
The replay finishes each publication of the gated steps, the design gates and the role holders, and each step record and plan record, without acting.
The workflow can attach an experiment and variant from a simulated assignment to a step record, but the replay does not publish that pair or use it as start-state evidence.
Collected windows do not supply prior experiment assignments.
`run.json` counts them under `divergence.finished`.
A person's act after a publication is a Dispatch event of the window.

**If it is wrong.**
If a decision of the daemon read a publication back, the replay moves the measures so.

- Issues finished: Unknown.
  Such a decision would read the record's publication, not one at the replay's moment.

- Started work that waits for a slot: Unknown, for the same reason.

- Priority taken by holders: Unknown, for the same reason.

- Other delays: Unknown, for the same reason.

- Slots: Unknown, for the same reason.

**The reading that would test it.** _Decisions after a publication._
In a window, read each decision of the daemon that follows a publication.
A decision with a Dispatch event between the two follows the event, which the replay applies, as it applies an answer (assumption 5).
A decision with no event between the two would show that the daemon reads the publication back.

### Assumption 28: The work of a pod or a workspace, a branch made, a pod stopped, bundled, released or removed, and a role that a pause or an idle pod's release stops, lies inside the recorded work segments and the record's own pause

The tool's own.
A rule that starts more issues at once may wait longer for pods and workspaces than the record did.
The recorded lengths do not carry that wait, which favours such a rule.
The slowdown factor f is the tool's reading of it.

**Evidence in a recorded window.** _The pod and workspace rows that the replay finishes without acting._
The replay finishes each release and each pause stop of a role, and each issue branch, issue suspend, bundle, issue release, workspace removal and claim release, without acting.
The replay runs no Sandbox teardown, so it neither notes a teardown on an issue release mark nor writes or reads the `pod_releases` records keyed by those marks.
`run.json` counts them under `divergence.finished`.
The replay's pause queues its stop rows on the simulated clock, as the daemon queues them on its own, so each pause stop finishes at the moment the record shows the pause.
A segment's recorded length runs from its worker start, so the launch of its pod is inside it.
In the prototype's record, the median time from a wake to the role's first turn was 160 seconds.

**If it is wrong.**
If pods and workspaces take longer when more issues start at once, the replay moves the measures so for a rule that starts more issues at once.

- Issues finished: Up, because its segments keep their recorded length where they would take longer.

- Started work that waits for a slot: Down, because its holders come sooner to the moment at which they give their slot back.

- Priority taken by holders: Unknown.
  The longer launches change how long holders hold their slots, not their rank.

- Other delays: Down, because each round of a send-back loop keeps its recorded length.

- Slots: Down for the slot-hours with no worker in a turn, because a wait for a pod holds a slot with no worker in a turn.

**The reading that would test it.** _The time to the first turn against the issues in flight._
For each `supervise: launched` line in the record, read the time to the first `supervise: a turn started` line of its claim, and the issues in flight then.
A time that grows with the issues in flight shows a cost that the segments do not carry.
The break-even factor of `compare` says how large f must be to change a rule's rank.

### Assumption 29: The replay makes no live GitHub requests

The tool's own.
A rule that changes how long a head waits may change what GitHub reports of it, a conflict above all.
The git branch uses stored verdicts for mergeability, with the generator for reads it cannot answer.
The main branch keeps recorded conflicts, and the drawn branch uses the generator.
All branches keep other GitHub outcomes through the recorded moves.

**Evidence in a recorded window.** _The GitHub facts that the replay leaves out._
The replay leaves out each fact of a pull request's opening, reviews, checks, closing, merge queue, pushes and mergeability.
It finishes each merge queue read, merge queue removal and conflict confirmation without acting.
The record's phase moves carry what GitHub caused, and the replay's own pull request record stands in for the issue's.

**If it is wrong.**
If GitHub reports more red checks, drops from the merge queue or other faults for a head that waits longer, the replay moves the measures so for a rule under which heads wait longer.

- Issues finished: Up, because the replay misses the work that those reports would cause.

- Started work that waits for a slot: Down where a missed report would send work back from a phase that keeps no slot.

- Priority taken by holders: Unknown.
  The missed reports change which issues wait, not their rank.

- Other delays: Down, because the replay leaves out the loops that those reports would cause.

- Slots: Down for the slot-hours in a turn, because the replay leaves out the work after those reports.

**The reading that would test it.** _GitHub's reports against the wait of each head._
For each pull request of the window, read from GitHub its checks and its merge queue events, and how long its head waited before its merge.
A rate of red checks or drops that grows with the wait shows a report that the replay does not draw.

### Assumption 30: A production check after a merge, its looks and its wakes, changes nothing of a replay: it runs after the issue is done

The tool's own.
Sound for the measures, which end at an issue's done.
A check that reopened its issue made the record's own reopen, which the replay applies.

**Evidence in a recorded window.** _The check rows and turns that the replay leaves out._
The replay leaves out each fact of a production check, its report, its looks, its wakes and its time limit, and finishes its rows without acting.
This includes the wakes postponed after a checking role fails, as well as older wakes superseded by the postponement.
`run.json` counts the check's turns that the replay left out under `divergence.turnsLeftOut`.

**If it is wrong.**
A production check runs after its issue's done, so a check that changed what came after it would move the measures so.

- Issues finished: None, because a check runs after its issue's done.
  A reopen that the check caused enters as the record's reopen (assumption 16).

- Started work that waits for a slot: None, because a closed issue does not wait for a slot.

- Priority taken by holders: None, because a closed issue neither holds a slot nor waits for one.

- Other delays: None, because each loop and each wait on the issue ends at its done.

- Slots: None, because a closed issue holds no slot and is not in flight.

**The reading that would test it.** _Reopens after a production check._
In the window, read each issue that reopened after a look of its check, and the time of the reopen.
Each must be a Dispatch event of the window, which the replay applies (assumption 16).

### Assumption 31: A worker's claim settles as the record shows it: it launches, is ready and confirms its task at its start, its turns are the record's, and no silence, lost session, memory kill or full disk of supervision's moves the issue

The tool's own.
A supervision fault that the record holds shows as the claim failure, retry or hold that it led to, which the replay enters from the record.
A fault that a rule would bring on more or less often is not drawn, which favours a rule that runs more issues at once.

**Evidence in a recorded window.** _The supervision facts that the replay leaves out._
The replay leaves out the facts of a claim's readiness, of its task's confirmation, of a lost session and of a fault of the session store.
It also leaves out a silent worker, a stalled background job, an entry sweep, a memory kill in an issue's pod or in the controller's, a full volume, a listener's wake and a pod's backstop.
It enters each recorded `supervise: claim failed` line at its offset in the segment of its issue.
The trace also keeps the logged cause and time of each process death or refused start it can read.
These observations do not apply a fact, spend a launch budget, or change the schedule.
The replay leaves each memory kill's fact out under every rule, the rules that admit by signals among them.
At the moment of each kill that the kill model gives, a run under such a rule computes the project's deadline again and runs no pass, so a kill holds back a start only through the kills signal that a later pass reads, and never moves an issue.

**If it is wrong.**
If faults come more often when more issues run at once, the replay moves the measures so for a rule that runs more issues at once.

- Issues finished: Up, because the replay misses the faults that would delay its work.

- Started work that waits for a slot: Down, for the same reason.

- Priority taken by holders: Unknown.
  The missed faults change how long holders hold their slots, not their rank.

- Other delays: Down, a little, because an issue that waits on one with a missed fault ends its wait sooner in the replay.

- Slots: Down for the slot-hours with no worker in a turn, because a fault holds a slot with no worker in a turn.

**The reading that would test it.** _Faults against the issues in flight._
From the daemon's log, count the claim failures, memory kills and full volumes in each hour against the issues in flight.
A rate that grows with the issues in flight shows faults that the replay does not draw.

### Assumption 32: A worker's completion passes the checks a live one passes before its move, its claim's run, its handoff record and its review's head, since the record shows it passed them; the move enters at the end of its segment

The tool's own.
Sound under the recorded rule.
Under another rule, a completion whose check would fail on a later head is entered as passed, which favours a rule under which more completions meet a moved head.

**Evidence in a recorded window.** _The completions that the record shows passed._
The replay leaves out each handoff's completion fact, and enters the recorded move at the end of its segment.
The record shows that each such completion passed its checks: its claim's run, its handoff record and its review's head.

**If it is wrong.**
If a completion would fail on a head that moved under another rule, the replay moves the measures so for a rule under which more completions meet a moved head.

- Issues finished: Up, because the replay misses the extra work that each refused completion would cause.

- Started work that waits for a slot: Unknown.
  A refused completion keeps its issue in its phase, where it holds or waits as before.

- Priority taken by holders: Unknown.
  The extra work changes how long holders hold their slots, not their rank.

- Other delays: Down, a little, because an issue that waits on one with a refused completion ends its wait sooner in the replay.

- Slots: Down for the slot-hours in a turn, because the replay leaves out the work after each refused completion.

**The reading that would test it.** _Refused completions against the merges during the work._
From the daemon's log, count the completions that it refused for a moved head, against the merges into the base while the worker ran.
A count that grows with the merges shows the refusals that a rule with longer work would meet.

### Assumption 33: The repairs a boot or a read of the repository mapping makes, a merger the cutover left released, a design gate's request found again, a repository's mapping changed, change nothing of a window

The tool's own.
Sound for a window recorded under one release and one repository mapping.
A repair that the record made shows as its own move or start, which the replay enters as recorded.

**Evidence in a recorded window.** _The repair facts that the replay leaves out._
The replay leaves out the cutover's repair of a released merger, and a design gate's request found again.
The parts of `window.json` give the release that ran each part of the window.

**If it is wrong.**
If a repair at a boot, or a change of the mapping, gave or took a slot that no recorded move shows, the replay moves the measures so.

- Issues finished: Unknown.
  The repair could free a slot or take one.

- Started work that waits for a slot: Unknown, for the same reason.

- Priority taken by holders: Unknown, for the same reason.

- Other delays: Unknown.
  A repair that changed a design gate would move the waits under it, earlier or later.

- Slots: Unknown, because the repair could free a slot or take one from that boot on.

**The reading that would test it.** _The boots inside the window._
Read each process start in the window, and the release that its part gives.
After a boot that starts a new release, look in the record for a slot or a move that no fact of the window explains.

### Assumption 34: An issue the record first shows in a phase that keeps a slot, whose first slot line gives a slot back or which no slot line names, held a slot from before the window's reach, until that line or, with none, until the rule gives the slot back; and an issue that stands at the window's start in a worker's phase that keeps a slot, with no slot line holding it there and no wait that gives the slot back, whose worker started in that run of phases after its last slot went back, held one from its last entry into that run until the rule gives it back

The tool's own.
Doubtful where the reach begins with slots in use and no line names their holders, because the slot is then read from the phase alone.
Doubtful too where a slot is taken again by a way that writes no slot line, because the slot at the window's start is then read from the worker's start alone.
The daemon starts the worker of a phase that keeps a slot only once the issue holds one, so a worker that started after the issue's last slot went back is the record's sight of a slot that no line names.
An issue whose work came back with no slot and whose worker has not started owes its start, and is given no slot.

**Evidence in a recorded window.** _The slot line at the window's start._
The first `admission:` slot line at or after the window's start gives the slots in use where the start state places them.
A window whose log holds no slot line from its start on is read at the last slot line before it, whose count stands at the start.
The start state reports the difference between that count and the slots that the trace holds right after the line, and the holders that it inferred.
`run.json` carries both, as `divergence.slotCountDifference` and `divergence.inferredSlots`.

**If it is wrong.**
If such an issue held no slot, the replay moves the measures so.
A holder that the inference misses moves each measure the other way.

- Issues finished: Down, because its slot keeps another issue from starting until the slot goes back.

- Started work that waits for a slot: Down, because the tool reads its time as held, not as waiting.

- Priority taken by holders: Up where it ranks below an issue that waits for a slot.

- Other delays: Up, because a link wait that it opens counts from its slot, which makes the waits longer.
  The loops are read from their own lines, and do not move.

- Slots: Up for the slots held, with no worker in a turn above all, by the span of the inferred slot.

**The reading that would test it.** _The slot count against the trace._
Compare the slots in use that the slot line at the window's start gives with the slots that the trace holds right after that line.
A difference of zero, with no inferred holder, means that the window does not rest on this assumption.
A longer `--before` moves the reach back, which can make the difference smaller where the lines exist.

### Assumption 35: A stretch whose recorded conflict loop runs to the window's end keeps that conflict, and its issue runs its own loop

The tool's own.
Doubtful, and the direction is unknown.
A stretch ends in a recorded conflict whose loop runs to `--to` when the record shows the issue moved back by `conflict` and never back in the phase that the conflict left.
The record then holds no work of the issue after the stretch but that loop.
Drawn as any other stretch, a draw of no conflict would take the issue past a loop after which the record shows nothing, and a drawn conflict would give it another loop in place of its own, after which it has nothing either: either way the issue would stand idle, with no worker, to the window's end.
The replay keeps this conflict at its end regardless of the Git answer or any earlier draw.
The move belongs to the record, even if the stretch drew clean earlier.
The git branch logs the end read but does not act on it, and skips boot reads in the stretch.
Under a rule whose conflicting head moves back, a kept conflict in testing, reviewing, accepting, retro or merging moves the issue back, and it then runs its own loop as recorded.
Under a rule whose conflicting head keeps its phase, the merge-forward segment that the daemon asks at once for a kept conflict lasts as long as that loop's implementing round, and has no end where the round has none.
Under `main`, in accepting or retro, the daemon at the head moves nothing and asks nobody, and the record holds no work of the issue after the stretch but the loop, so the issue stands in its phase with no worker to `--to` (assumption 37).

**Evidence in a recorded window.** _Stretches that a recorded conflict loop runs past `--to`._
In a replay of one deployment's recorded window of four days under the rule that its daemon ran, with such stretches drawn as any other, six issues stood with no worker, in accepting, merging or reviewing, from the end of the stretch to the window's end, where the record shows each one working in its loop.
`run` prints the number of conflicts that it kept, and `run.json` counts them as `divergence.keptOpenLoops`.

**If it is wrong.**
If such a stretch would not have conflicted under the rule, the replay moves the measures so.

- Issues finished: Down, because the issue runs its loop to `--to`, where without the conflict it might have finished.

- Started work that waits for a slot: Up where the loop goes back to a phase that keeps a slot, because the issue takes or waits for a slot that it would not have needed.

- Priority taken by holders: Unknown.
  The kept loop changes which issues hold slots, not their rank.

- Other delays: Up, because each kept conflict counts as a round of `conflict`, its hours running to `--to`.

- Slots: Up for the slot-hours in a turn, because the loop's workers are in their turns to `--to`.

**The reading that would test it.** _A later end of the same lines._
Replay a window that ends later on the same lines.
For each conflict that the first window kept, read whether its loop came back, and how long it ran.
Compare the share of those stretches that the later window's model gives a conflict with the share that the first window kept.

### Assumption 36: Under the recorded order of first starts, a tree's root ranks at the earliest recorded first start among the issues of its tree that can still take their first start in the replay

The tool's own.
It is a choice of how the main branch makes its ranks under `firstStarts=recorded`, not a fault of the record.
The line orders the issues whose first phase can start by the rank of their tree's root first, and by their own rank second.
So the rank of a root decides where each child of its tree waits among the work of every other tree.
Only the first slot of a run is a first start, so a slot that the record gives to work that came back takes no rank, such as a return after a conflict.
The replay moves the rank of a root each time an issue of its tree takes its first slot, to the next recorded first start of the tree that can still be taken ([The branches](#the-branches)).
It moves the rank too where an issue of the tree can no longer take its first start in the replay, and where such an issue can take it again.
An issue cannot take its first start where the replay has not filed it, has finished it, has moved it past its first phase, or has taken it out of the workflow.
It cannot either while a hold of the line's starting part keeps it at admitted: its tree not running, its root waiting on a person, its tree's design gate closed, a Dispatch blocker, its open children or an accepted ancestor.
The replay reads these holds with the daemon's own test of the line's starting part, just before each admission pass of its daemon, over the line as that pass reads it.
So a start that the replay cannot make holds no root at the rank of that start, and a tree whose next recorded start a hold keeps back ranks by its next start that can be made.
The next child of a tree then takes the place that its own recorded start gives it among the work of every tree.
So the branch starts children in the order in which the record started them, ahead of a root's later work where the record did so.
The branches under `firstStarts=rank` keep the board's ranks, and show how much the choice moves the result.

**Evidence in a recorded window.** _A root held at a start that the replay does not make._
In a replay of one deployment's recorded window under the rule that its daemon ran, each root first kept the rank of its tree's earliest first start for the whole window.
The replay then gave a slot to a child that the record started much later, ahead of the issues of other trees that the record started earlier, which waited.
Once the replay moved a root's rank at each first slot of its tree, a few roots still kept an early rank for the whole window.
Their first slot in the window was a return after a conflict or after the end of a wait, and the replay never made the return, since that replay moved no issue back for a conflict past the review (assumption 37, which then covered every rule).
Children that the record never started, in the trees of those roots, took slots and held them with no work while issues that the record started waited, so the replay gave fewer slots and finished fewer issues than the record on the window's last days.
A later replay of the same window showed a root that kept the rank of a child that waited for its own children, which its architect filed at their recorded times.
That child never started in the replay, and children of its tree that the record never started took slots ahead of the recorded starts of other trees.
A root now ranks by a start that such a hold keeps back no longer.

**If it is wrong.**
If the record's daemon started the children of different trees in their order for reasons that the replay does not carry, such as the board's ranks of the moment or a hold, the replay moves the measures so for a rule other than the one that made the record.

- Issues finished: Unknown.
  The order changes which issue takes a slot first, not how many slots there are or how long the work takes.

- Started work that waits for a slot: Unknown.
  The order changes which issues start first, and so which of them wait for a slot later.

- Priority taken by holders: Up, because the line follows the order in which the record started the issues rather than the board's ranks, by which the measure places holders and waiting issues.

- Other delays: Unknown.
  The order changes which issues wait on others, not how long a loop or a wait takes.

- Slots: Down for the slot-hours with no worker in a turn under `neverStarted=none`, because an issue that the record never started waits behind every recorded start that can still be taken in the replay, where a line ordered by the board could start it sooner, to hold its slot with no turn.

**The reading that would test it.** _The two orders side by side, and the board at each start._
`validate` and `compare` give `main` and `main+firstStarts=rank` side by side.
For each child that the record started after a child of another tree, read the ranks that the board gave the roots of the two trees at the moment of each start, and the holds on each child.
A start that those ranks and holds explain is one where the order of the starts and the board agree.
A start that they do not explain is one that only the order of the starts keeps.

### Assumption 37: Under main and the candidates built on it, a conflict past the review is answered as the daemon that the replay runs answers it, which leaves it to the merger

The tool's own.
It covers `main` and each candidate built on it, whose conflict path is the path of `main`.
The replay runs the daemon of the commit that it was built from, not the daemon that made the record.
That daemon answers a conflict past the review by the conflict path of the rule that it runs.
Under the path of `main`, since cybermonklabs/legion#4979, it leaves a conflict past the review to the merger.
In accepting and retro, it records the head as conflicting and asks nobody, and it asks the merger to merge the base forward when the issue moves into merging.
In merging, it asks the merger to merge the base forward at once.
A rule that names another path answers by that path, as the release that ran the rule did.
`legion-v20.42.5` and `legion-v19.57.3` move the issue back to implementing from accepting, retro and merging, as releases 20.42.5 and 19.57.3 did.
`legion-v22.14.1` keeps the phase and asks the implementer to merge the base forward at once, as release 22.14.1 did before cybermonklabs/legion#4979.
So a replay of a window under the rule that recorded it plays each conflict past the review that it draws or keeps as the record does, and this assumption does not move it.
Under `main` or a candidate built on it, a conflict in accepting or retro keeps its phase and runs no loop.
The work that the issue goes on with after the loop then opens at once, earlier than the record shows it.
A version or a registration of the root's design that its architect made in that work keeps its recorded time and does not move with the work.
Such an act opens or closes the tree's design gate, which holds the first start of every member of the tree and the start of its merger.
So the gate of the tree opens and closes at the record's times, and not with the work that the replay runs early.
Where the record shows no work of the issue after a kept conflict but its loop (assumption 35), the issue stands in its phase with no worker to `--to`.

**Evidence in a recorded window.** _The conflict moves past the review, against the run's count._
A window recorded under `legion-v20.42.5` holds a phase line with the trigger `conflict` for each conflict out of accepting, retro or merging.
A replay of that window under `legion-v20.42.5` moves the issue back at each such conflict that it draws or keeps, as the record does.
`run` prints the number of conflicts past the review that it left to the merger, and `run.json` counts them as `divergence.conflictsToMerger`.
In a replay of one deployment's recorded window under its own rule, made while this assumption covered every rule, a root that stood in accepting at the window's start went past two recorded conflict loops, and its architect's spec versions moved with its next work in accepting.
A new version closed the tree's design gate 19 hours earlier than in the record, and the gate stayed closed for 56 hours against the record's 37, which held the tree's merger starts and first starts.
Since then, each such version keeps its recorded time.

**If it is wrong.**
If a project ran a candidate built on `main` on a daemon from before cybermonklabs/legion#4979, that daemon would ask the implementer to merge the base forward at once in accepting and retro, as `legion-v22.14.1` does.
A merge forward lasts as long whichever worker runs it, and the issue waits for it once either way, so the direction is None for `main` and each candidate built on it.

- Issues finished: None, because the issue waits for one merge forward of the same length either way.

- Started work that waits for a slot: None, because the merge forward takes no slot, and the issue holds the same slots either way.

- Priority taken by holders: None, because the holders and the issues that wait are the same either way.

- Other delays: None, because the conflict's round is one merge forward of the same length either way.

- Slots: None, because the merge forward takes no slot and its worker is in a turn for the same time either way.

**The reading that would test it.** _A window of a release with cybermonklabs/legion#4979._
Collect a window recorded under a release with cybermonklabs/legion#4979, and read each conflict past the review in it.
A conflict in accepting or retro writes the line `workflow: a confirmed conflict needs no merge yet`, and a merge forward in merging writes the line `workflow: merge forward asked`.
Compare the time from each conflict to its merge with the same time in a window of release 22.14.1 replayed under `legion-v22.14.1`.

### Assumption 38: A stretch in merging keeps its recorded length and outcome

Doubtful.
A replay keeps each stretch in merging as the record shows it, with its length, its drops from the merge queue and its send-backs.
It does not read how many pull requests of the same repository wait in merging at the same time.
So a rule or a cap that brings more work to merging at once is favoured.
The merge queue's wait under another rule comes from the model fitted on the window ([Signal `queue`](#signal-queue)), which only a signal rule reads: it admits by the modelled wait, and the stretch keeps its length.

**Evidence in a recorded window.** _Merging against the depth of the queue._
In one deployment's recorded window of four days, a pull request that entered merging behind more of its project's pull requests on the same repository was sent back more often, and a merged one took longer to merge.
Close to half of all the visits to merging ended in a send-back.
The share of the merge queue's runs that were cancelled was about 36% in that window, and about 63% on a later night, so the queue of one window is not the queue of another.

**If it is wrong.**
If more pull requests in merging make each one slower or less likely to merge, the replay moves the measures so for a rule that brings more work to merging at once.

- Issues finished: Up, because the merges keep the recorded lengths and outcomes that a shorter queue gave.

- Started work that waits for a slot: Down, because fewer send-backs from merging come back to wait for a slot.

- Priority taken by holders: Unknown.
  The queue changes when work comes back, not the rank of a holder.

- Other delays: Down, because the loops of `queue_drop` and `send_back` keep their recorded count.

- Slots: Unknown.
  Merging holds no slot under most rules, and a rule that keeps a slot in merging holds it for the recorded time.

**The reading that would test it.** _The depth of the queue at each entry into merging._
For each visit to merging in a recorded window, count the pull requests of the same repository in merging when the visit began.
Compare the time to the merge and the share of send-backs at a low depth and at a high depth, and do the same on a second window.

### Assumption 39: A person answers at any hour of the day, and answers any number of questions at once

Doubtful.
A replay places each answer at its recorded time or at its recorded offset from its question (assumption 5).
It does not read the hours at which the person answers, or how many questions wait for that person.
So a rule that moves questions into hours with no answers, or that asks more of them at once, is favoured.

**Evidence in a recorded window.** _The hours of the answers, and the questions that wait._
In one deployment's recorded window, nearly every answer came from one person.
No answer came in four hours of each day, and a question asked in the hours before them waited a median of 9 to 14 hours, where a question asked in the person's other hours waited less than an hour.
A question also waited longer while more questions were open.

**If it is wrong.**
If a person answers only in some hours, one question at a time, the replay moves the measures so for a rule that moves questions into other hours or asks more of them at once.

- Issues finished: Up, because the answers that the work waits for come sooner than the person could give them.

- Started work that waits for a slot: Unknown.
  An answer that comes sooner ends a wait sooner, and the issue can then wait for a slot.

- Priority taken by holders: Unknown.
  The hours of the answers change when a root moves on, not its rank.

- Other delays: Down, because each wait on a person ends at its recorded offset, however late in the person's day the question comes.

- Slots: Down for the slot-hours with no worker in a turn, because a root that keeps its slot while it waits on a person waits no longer than it did in the record.

**The reading that would test it.** _A second window's answers._
From one window, read the hours at which the person answers and how the wait grows with the questions that are open.
Use them to predict when each question of a second window is answered, and compare the predicted times with the recorded ones.

### Assumption 40: The slowdown factor f cannot be fitted on a window whose cap did not move, so f comes from outside the window

Doubtful.
A segment's length scales by `f^((n − m)/42)` (assumption 1).
A window whose cap stayed the same has no change of load that the cap made, so it cannot show f.
The tool runs each branch at f = 1, 1.2 and 1.6 and searches the f at which a rule's rank against `main` changes, and it fits no f.

**Evidence in a recorded window.** _A window at one cap._
In one deployment's recorded window of four days, the cap stayed at one value from before the window to its end.
Inside the window, the mean length of a turn rose where more turns ran at once, but long turns make more turns run at once by themselves, so this shows no slowdown either way.
The cap of that deployment changed several times in one day before the window, under one release.

**If it is wrong.**
If load slows work, a replay at f = 1 moves the measures so for a rule or a cap with more issues in flight than the record had.

- Issues finished: Up, because the work keeps its recorded length at the higher load.

- Started work that waits for a slot: Down, because holders give their slots back sooner than slower work would.

- Priority taken by holders: Unknown.
  The slowdown changes how long each holder works, not its rank.

- Other delays: Down, because the loops keep their recorded lengths.

- Slots: Down for the slot-hours in a turn, because each turn keeps its recorded length.

**The reading that would test it.** _A span in which the cap moved under one release._
Collect a window over a span in which the cap moved under one release.
Compare the lengths of each phase's segments that started at the lower cap with those that started at the higher cap, at the same hours of the day.

### Assumption 41: Work done outside Legion is the same under every rule: an issue the record shows closed outside Legion closes at its recorded time

Doubtful, and the direction is unknown.
A person or a session outside Legion that closes an issue in the record closes it at the same time in every replay.
A person who does work that waits for a slot does less of it under a rule that starts that work sooner, and the replay gives no rule that credit or that cost.
The scorecard gives `doneByAnyone` beside `done`, so a reader can weigh Legion's work against all of the project's.

**Evidence in a recorded window.** _The issues closed outside Legion._
In one deployment's recorded window, Legion closed about 200 issues, and about 40 issues were closed outside Legion.
Nearly all of the 40 never held a slot, and some were of the highest priority.

**If it is wrong.**
If the work done outside Legion depends on how long Legion makes work wait, the replay moves the measures so for a rule that starts more issues.

- Issues finished: Unknown.
  A rule that starts more issues would take work that a person did outside, and Legion would finish it in place of the person.

- Started work that waits for a slot: Unknown.
  Work that a person no longer takes waits for a slot in the line.

- Priority taken by holders: Unknown.
  The issues closed outside leave the line at their recorded times.

- Other delays: None, because an issue closed outside has no loop or wait in Legion.

- Slots: Unknown.
  A rule that starts the work in Legion holds slots for it.

**The reading that would test it.** _Outside closes against the wait for a slot._
Across the days of several windows, compare how many issues of the highest priorities were closed outside Legion with how long the line made new work wait for a slot.

### Assumption 42: Under main, the merge forward of a drawn conflict reads no rules of the base branch, so the engine asks for it at once

Wrong where the base branch's rules let a conflicting head wait.
The daemon reads the rules of the base branch with each conflicting read, and it asks no merge forward while every required check is green and no rule asks the branch to be current.
A replay gives the engine no such rules, so under `main` it asks a merge forward of each drawn conflict before merging at once.
So `main` and the rules built on it are charged a merge forward that the daemon asks later or not at all, which disfavours them.

**Evidence in a recorded window.** _The conflicts that need no merge yet._
The daemon writes the line `workflow: a confirmed conflict needs no merge yet` where it asks no merge forward of a conflict.
A window recorded under a release with the rules read holds those lines beside the lines `workflow: merge forward asked`.

**If it is wrong.**
If the base branch's rules let a conflicting head wait, the replay moves the measures so under `main` and the rules built on it.

- Issues finished: Down, because each merge forward delays the issue's work.

- Started work that waits for a slot: None, because a merge forward under `main` keeps the issue's phase and slot.

- Priority taken by holders: None, because the holders are the same.

- Other delays: Up, because each merge forward is a round of `conflict`.

- Slots: Up for the slot-hours in a turn, because the implementer is in a turn for each merge forward.

**The reading that would test it.** _The two lines side by side._
In a window of a release that reads the base branch's rules, count the lines `workflow: a confirmed conflict needs no merge yet` in testing and reviewing against the lines `workflow: merge forward asked`.

### Assumption 43: An answer at its recorded offset from its question can come at an hour at which the record shows no answers

Doubtful, and the direction is unknown.
Under `answer=offset`, an answer comes at its recorded offset from the replay's own question, wherever the question came (assumption 5).
A question that a rule moves into the person's night is then answered at a time of the night at which the record shows no answers, and one moved out of it later than it would be.
Run R0, the replay of the rule that made the record, also on `main+timing=wall` and `main+answer=wall`, where each answer and each end of a wait keeps its recorded time, and compare the two validations.

**Evidence in a recorded window.** _An answer that came many hours early._
In a replay of one deployment's recorded window under the rule that made it, a person's request for changes to a spec version came 19 hours before its recorded time, because the replay's version came earlier and the answer kept its offset.

**If it is wrong.**
If answers come at the person's own hours whatever the question's time, the replay moves the measures so.

- Issues finished: Unknown.
  An answer moved earlier lets work go on sooner, and one moved later holds it.

- Started work that waits for a slot: Unknown.
  The answers change when work comes back to wait for a slot.

- Priority taken by holders: Unknown.
  The answers change when a root moves on, not its rank.

- Other delays: Unknown.
  The waits on a person and the gate's waits change with the answers, both ways.

- Slots: Unknown.
  A root that waits on a person keeps its slot for as long as the wait lasts.

**The reading that would test it.** _R0 on both timings, and the hours of the answers._
`validate` gives `main`, `main+timing=wall` and `main+answer=wall` side by side under the rule that made the record.
Compare their tables, above all the waits on links and on blockers and the first-slot error.
Count the share of the replay's answers that fall in hours at which the record shows almost none.

### Assumption 44: Redrawing conflicts can change the first-slot test

Doubtful.
On the drawn branch a replay draws each stretch's conflict from the fitted rate, except for kept open loops.
On the git branch it draws only for unanswered reads.
So most of the record's conflicts are drawn again, on other stretches, and two seeds give an issue first slots that differ by about half as much as the replay differs from the record.
The first-slot test can then pass or fail on the draws alone.
The main branch keeps the record's conflicts, so this source of noise does not affect its first-slot test.
All three branches are subject to the same validation gaps.

**Evidence in a recorded window.** _Two seeds against the record._
In a validation of one deployment's recorded window under its own rule, for the issues that every seed gave a first slot, the median difference between two seeds was about 45 minutes, and the median error against the record about 90 minutes.

**If it is wrong.**
If the redrawn conflicts move the first slots more than the rule does, the replay moves the measures so under the rule that made the record.

- Issues finished: Unknown.
  A redrawn conflict can fall on an issue that the record finished, or move from one that it did not.

- Started work that waits for a slot: Unknown.
  A redrawn conflict sends other work back to wait for a slot.

- Priority taken by holders: Unknown.
  The draws change which issues hold slots, not their ranks.

- Other delays: Unknown.
  The rounds of `conflict` fall on other stretches.

- Slots: Unknown.
  The loops of redrawn conflicts hold slots at other times.

**The reading that would test it.** _The drawn branch beside the main branch._
Compare the first-slot errors on `main` and `main+conflicts=drawn`, and the spread over the seeds on the drawn branch.

### Assumption 45: Each modelled signal moves with the work in flight only as its model's form says

Doubtful.
A replay under another rule takes the merge queue's wait, the provider's headroom with the refusals by the project's own limit, and the memory guard's kills from models of the window ([`legion replay models`](#legion-replay-models)).
Each model keeps what the window records, and moves with the replay's work in flight only as its form says.
A minute's tokens are its recorded tokens for each issue in flight, times the replay's issues in flight.
A kill moves with the work segment of its own issue, and work that the record never ran takes the day's recorded kills for each hour of an issue in flight.
Each repository's queue is served by the record's own exits from its head, and by its rate of them for each hour in which it held a pull request.
So a cause that the record holds and the work in flight does not, such as a release, the hour of the day or a queue's checks, stays as recorded, and a cause that more work in flight would bring beyond these forms is not modelled.
At the recorded work in flight the models of the kills and of the merge queue give the record by their form, so R11's daily test shows only that they keep it, and R11's test across two records tests the rate that each model rests on.
Under a rule that admits by no signal, the models move nothing.

**Evidence in a recorded window.** _The kills of one window at a full cap, and the same rate in a later record._
Even at a full cap, so at one level of work in flight, one deployment's window had 299, 88, 57 and 110 memory kills on successive days, the first of them the first full day of the memory guard.
From the next day on, its kills were 0.0144 for each hour of an issue in flight, as in a later record of the same deployment with more work in flight, while one repository's queue grew from 2 to 90 pull requests at about the same work in flight.

**If it is wrong.**
If a modelled signal moves with the work in flight otherwise than its model's form says, the replay moves the measures so under a rule that admits by the signals.

- Issues finished: Unknown.
  A model that reads a signal too high holds back starts that the true signal would let through, and one that reads it too low lets through starts that it would hold back.

- Started work that waits for a slot: Unknown, for the same reason.

- Priority taken by holders: None, because the signals hold back starts below P0, not ranks.

- Other delays: Unknown.
  More or fewer starts change how many stretches run at once, and so the conflicts that the merges term draws.

- Slots: Unknown, for the same reason as the issues finished.

**The reading that would test it.** _Each model's rate on two records and by load._
Compare the rate that each model rests on in two records whose work in flight differs, as R11's test across two records does, and read the signal's rate in each range of 20 issues in flight beside the model.
A rate that moves between the records, or from one range of issues in flight to the next, shows load that the model does not read.

### Assumption 46: A refusal by the provider, or one the record does not split, is kept as recorded and counted as R11 says

The record splits the refusals of each minute into those of the project's own limit and those of the provider ([Signal `provider`](#signal-provider)).
A replay under another rule takes the project's own refusals from the tokens model and keeps the provider's as recorded, since the provider gives them to every project.
A refusal that the record does not split counts as the project's own, and the result says so.
A minute in which the window holds no record of the project's limit keeps its recorded refusals of both kinds, since the model has no limit to read there.

**Evidence in a recorded window.** _Refusals of both kinds in one minute._
In one deployment's record of 2026-11-23 from 16:28, the provider refused 70 calls in the first minute while the project's own limit refused none, and in the third minute the project's own limit refused 127 beside the provider's 53.

**If it is wrong.**
If some of the refusals counted as the project's own came from the provider, the replay moves the measures so under a rule that holds back starts on the project's own refusals.

- Issues finished: Down, because the rule holds back starts for refusals that more work in flight did not cause.

- Started work that waits for a slot: Up, for the same reason.

- Priority taken by holders: None, because the refusals hold back starts below P0, not ranks.

- Other delays: None, because the refusals move no loop and no wait on another issue.

- Slots: Down, because the starts held back hold no slot.

**The reading that would test it.** _A record that separates the counts._
Check the minutes whose refusals the record does not split against a record of the same minutes that separates the project's own refusals from the provider's.

### Assumption 47: A signal with no record in a span does not bind there

A signal that the window holds no record of at a moment is unrecorded, never zero, and a rule that admits by the signals lets it hold back nothing there.
The provider's headroom has no record before the first observation of the project's limit and where the project had no limit (assumption 48), the kills none where the deployment gave no launcher's lines, and the approvals none before the project's default assignee is known.
The replay reads the idle share from its own slot holders and the approvals from its own copy of Dispatch, but each of them is recorded only where the window holds a record of it, so a window that names no span of a signal reads that signal unrecorded at every moment, whatever the replay's own state.
A load model gives a recorded value only where the window holds its signal's record, and a model that gives no limit for its signal leaves the signal with none in a run.

**Evidence in a recorded window.** _A window with no limit of the project's._
In one deployment's first window, the project's own limit on the provider's tokens was taken away at 2026-11-16T22:07Z, so the headroom has no record from then on.

**If it is wrong.**
If a signal with no record would have stood at or above its limit, the replay moves the measures so under a rule that admits by the signals, in the span with no record.

- Issues finished: Up, because the rule starts work that the signal would have held back.

- Started work that waits for a slot: Down, for the same reason.

- Priority taken by holders: None, because the signals hold back starts below P0, not ranks.

- Other delays: Unknown.
  More starts run more stretches at once, and so change the conflicts that the merges term draws.

- Slots: Up, because the starts let through hold slots.

**The reading that would test it.** _The decision where the signal is recorded._
Check the rule's decisions on a span where that signal is recorded, and compare the starts that it makes there with those that it makes where the signal is unrecorded.

### Assumption 48: The provider's limit holds from one observation to the next

Doubtful.
The record observes the project's limit where a refusal's line in the deployment's log names it, the "of its N" of a 429, and where a change of it is recorded, each value with its source.
Between two observations, the tool takes the earlier one to hold, so a change between them is read late, and the headroom is misread over that stretch: high after a cut, and low after a raise.
Before the first observation, the headroom has no record and does not bind (assumption 47), and the tokens model keeps the minute's tokens and tests no refused minute there.

**Evidence in a recorded window.** _Few observations of the limit._
In one deployment's record of nine days, the limit was observed in 290 of 12,150 minutes, and one recorded change took it away at 2026-11-16T22:07Z.

**If it is wrong.**
If the limit changed between two observations, the replay moves the measures so under a rule that admits by the headroom, over that stretch.

- Issues finished: Unknown.
  It is up after a cut read late, since the share of the limit in use then reads below its true share, and down after a raise read late.

- Started work that waits for a slot: Unknown: down after a cut read late, and up after a raise.

- Priority taken by holders: None, because the headroom holds back starts below P0, not ranks.

- Other delays: None, because the headroom moves no loop and no wait on another issue.

- Slots: Unknown: up after a cut read late, and down after a raise.

**The reading that would test it.** _Each recorded change against the next 429._
Check each recorded change of the limit against the N of the next 429 line: an N that differs from the limit that the record holds then is a change that the record read late.

### Assumption 49: The project's token limit is the one that the replay's caller gives, in each minute whose record holds none

Doubtful.
The provider's headroom is the project's uncached input tokens of a minute over the project's own limit in force in that minute, and that limit is a value of the deployment's configuration.
A window can record no limit of the project's own in many minutes, or in none, so the headroom has no value there, and the tokens model gives no limit for the provider's signal.
`--token-limit` gives the replay the project's limit, which it takes in each minute whose provider record holds no limit of the project's own.
A limit that the record holds stands, and a minute that the record of the provider does not hold stays unrecorded (assumption 47).
The replay then models the headroom and the project's own refused minutes there against that limit, as it models them against a recorded one.
The window's work then runs under a limit that the record did not have.
The replay keeps each work segment's recorded length (assumption 1), so it shows no slowdown that those refusals would have caused, and no record confirms the refused minutes that it models there.
R11's daily test compares the model with the record only in the minutes where the record holds the limit, so `legion replay models` gives the same verdicts with any token limit, and a window that records no limit of the project's own keeps `no test`.
Under a rule that admits by no signal, the token limit changes no admission.

**Evidence in a recorded window.** _A window with no limit of the project's own._
In one deployment's first window, the project's own limit was taken away at 2026-11-16T22:07Z, and no later minute records one, so the headroom has no value in any minute of that window without a token limit.
One deployment's record holds the project's limit of 6,000,000 tokens a minute for 1.8 hours of 2026-11-23, and nowhere else.

**If it is wrong.**
If the refusals under that limit would have slowed the window's work, the replay moves the measures so under a rule that admits by the provider's signal: its measures look better than they would by that slowdown.

- Issues finished: Up, because the work that runs through the refused minutes that the replay models keeps its recorded length, where the refusals would have slowed it.

- Started work that waits for a slot: Down, because shorter work brings each holder sooner to the moment at which it gives its slot back.

- Priority taken by holders: Unknown.
  Slower work would change which issues hold slots at each moment, but not by their rank.

- Other delays: Down, because each round of a send-back loop, and the work of each blocker, keeps its recorded length through those minutes.

- Slots: Down, because slower work would hold more slot-hours in a turn and keep more issues in flight.

**The reading that would test it.** _A window that records the limit throughout._
Replay a window whose record holds the project's limit in every minute, once as recorded and once with the limit taken out of the record and given as `--token-limit`, and compare the refused minutes and the measures of a signal rule.
A difference shows what the assumption moves.

### Assumption 50: No root's design-gate step rests on an ancestor's approval

Sound for every window recorded before cybermonklabs/legion#4592.
Since that release a root's design-gate step can open on an approval of an ancestor's document, or wait on an ancestor's open approval request, and a root that a person makes a child of a live tree can rest on its new parent's document.
The daemon then reads Dispatch for the ancestor's request (the pending-basis seed) and for the new parent's document (the re-parent seed).
A replay's copy of Dispatch holds no document's requirement blocks and no basis, so the worker stub finishes both reads without acting, and every step opens on the root's own approval, as the record shows it.

**Evidence in a recorded window.** _No basis in any window the tool has replayed._
Every window the tool has replayed was recorded before the release that brought the basis, so no root of them registered a step on one, and the daemon wrote neither seed there.

**If it is wrong.**
A step that the record opens on an ancestor's approval opens in the replay at the root's own approval, which is earlier or later than recorded.

- Issues finished: Unknown, because the tree starts earlier or later.

- Started work that waits for a slot: Unknown, for the same reason.

- Priority taken by holders: Unknown.
  The tree's members enter the line at another moment, by the same ranks.

- Other delays: Unknown.

- Slots: Unknown.

**The reading that would test it.** _A window that holds a step on a basis._
Replay a window recorded after cybermonklabs/legion#4592 in which a root registered a step on an ancestor's approval, and compare the moment its gate opens, and its tree's first start, with the record's.

### Assumption 51: Where Dispatch recorded no resolved priority, the window's parent chains give it

A window recorded before Dispatch served `resolved_priority` holds each issue's own priority alone.
The replay then reconstructs the priority Dispatch resolves for each issue by Dispatch's own rule: the issue's own priority, else that of the nearest issue above it on its Dispatch parent chain that has one, in any project, up to 32 levels, else none.
It reads the chain again at the window's start and at each change of a priority or a parent of the issue or of an issue above it.
Where an event carries Dispatch's own value, that value stands from the event on, and nothing is reconstructed after it.
The measures read the priority this gives under every rule, and a rule with the new P0 start its admissions too ([The priority Dispatch resolves](#the-priority-dispatch-resolves)).

**Evidence in a recorded window.** _The fixture windows of R10, no recorded window yet._
The fixture windows `p0-inherited-across-projects`, `p0-inherited-from-the-nearest-ancestor` and `one-free-slot-opposite-order` hold children whose priority comes from a parent in another project, worked by hand by this rule, and `p0-recorded-descendant-update` holds Dispatch's recorded value, which the replay takes as it stands.

**If it is wrong.**
An ancestor that Dispatch hid from the daemon's reader, but that the collector read, gives an issue a priority that Dispatch would not have given it, and a chain that moved with no event in the window gives an issue the old chain's priority.

- Issues finished: Unknown, because a rule with the new P0 start may start an issue outside the cap where Dispatch would not have.

- Started work that waits for a slot: Unknown, for the same reason.

- Priority taken by holders: Up or down where an issue stands in another band of today's line's order than Dispatch would give it.

- Other delays: Unknown, for the same reason as the issues finished.

- Slots: Up for a rule with the new P0 start, where it starts an issue outside the cap on a priority that Dispatch would not have given it.

**The reading that would test it.** _A window recorded after Dispatch served `resolved_priority`._
Strip `resolved_priority` from the events of such a window, reconstruct each issue's priority from the window's parent chains, and compare it with Dispatch's recorded value at each event.
