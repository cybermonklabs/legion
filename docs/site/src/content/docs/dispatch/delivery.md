---
title: Delivery
description: The Delivery page's timeline and measures, the Pipeline page's step timings, what each figure counts, the targets, and the routes an agent reads.
sidebar:
  order: 7
---

The **Delivery** page shows how work reaches production. A timeline of every merge, deploy and
failed run sits under a panel of delivery measures. Both read the facts Dispatch stores from
GitHub, and both follow the same window and filters. The **Pipeline** page beside it shows how
long each step of CI and the deploy takes, day by day. Until the deploy repository and its
workflows are set in **Settings → Delivery timeline**, the Delivery page shows that form instead.

![The Delivery page's measures panel: six target cards over a strip of headline numbers](/legion/media/dispatch/delivery-measures.png)

## The window and filters

The page opens on the last 28 days. **Window**, in the page header, sets it: the last 7, 14, 28 or
84 days, or a custom range of whole days in UTC, both ends included. A preset rolls, so a link to
the last 7 days always shows the latest 7; a custom range keeps its dates. The page reads at most
84 days, and refuses a longer custom range, saying so. Changing the window clears a dragged
stretch.

The window, the search box and every filter (Repository, Parent agent, Session, Dispatch issue,
Priority, Component, Author, rework, and whether a pull request has deployed) are written into the
address, so a link shows the same figures to whoever opens it. Dragging across the timeline narrows
the measures to the stretch you dragged over; the timeline itself keeps the whole window. **clear
brush window** puts the measures back.

## Live updates

An open Delivery or Pipeline page keeps itself current over one connection to Dispatch. A merge
or a deploy appears within seconds, without a reload. Nothing reads again on a timer: Dispatch
sends the page an update only when something it shows has changed, and each update carries only
what changed, such as the pull requests and runs that moved, the counts beside the filters and
the measures' new numbers. A preset window keeps its end at the present, so a pull request or run
leaves the page at the moment it ages out of the window. A deploy shows once its production job
finishes, without waiting for the rest of its run.

Each page's freshness row says **Live** while the connection is open, with how long ago the last
change arrived. A source Dispatch reports as stale, or a failed reconcile, still shows
in red with its age and error. While the connection is lost the row reads **Reconnecting** in red,
with how long it has been lost; the page reconnects by itself and is sent only what changed while
it was away. A page away for more than a day reads its window once, whole. The whole verdict
follows a reconnect — which sources are stale, the last reconcile and its error, and how many pull
requests Dispatch can no longer fetch — so a failure that cleared or appeared while the page was
away shows at once rather than at its next full read.

## Grouping sessions

A pull request's **Parent agent** is the first agent session that wrote it, shown by the session's
title. One agent often runs as several sessions (it restarted, or it was resumed), and a session
group puts them back together: every pull request a member wrote first counts under the group's
name, in the Parent agent filter, the colours, the lanes, the list, the details and the measures.
The Session filter still lists each session on its own.

**Edit groups**, under the Parent agent filter, lists the window's sessions with their titles and
how many pull requests each wrote first. Select sessions and name them to make a group, and rename
a group, add the selected sessions to it, remove a member, or delete it. Groups are shared: everyone
who opens the page sees the same ones. A session can be in one group only, and two groups cannot
share a name. A group may take a session's title as its name only when it holds every session with
that title, so one name never means two things. A session that later takes a group's name as its
title, without joining it, shows with its id beside the title. Deleting a group shows its sessions
on their own again.

Colouring the merges by Parent agent adds **Own colour and lane**: the values you choose there each
get a colour of their own, and a lane of their own when swimlanes are on, and every other value
shares one **Other** colour and lane. With none chosen, the colours and lanes are as usual.

## The measures

Six cards across the top each judge one figure against its target: **met**, **missed**, or **no
data** when the window holds nothing to measure. Below them, one line carries the headline numbers.
**Definitions & alternatives** opens the other definitions of each figure and a per-day table,
newest day first; a day the window only partly covers is marked partial.

- **Deploy**: a run of the deploy workflow on `main` whose production job succeeded: a job named
  in **Production jobs** in **Settings → Delivery timeline** (a renamed job lists its old and new
  names) and, when **Production apply step** names a step, one whose step of that name succeeded
  too, so a run that stood down without applying is no deploy. A run counts on the day it started.
  Runs on other branches count in no figure.
- **Deploys a day**: deploys in the window divided by the window's length in days.
- **Lead time**: from a pull request's merge to the first successful production deploy that
  contains it: the earliest to finish of the deploys whose run GitHub created after the merge. A
  merge queue writes its merge commit before GitHub records the merge, so the run's creation, not
  its commit's time, says which deploy holds a pull request. The cards show the median and the
  longest; the fold adds the 90th percentile and the time from the first commit and from opening
  the pull request.
- **Pull request open → merge**: the median and 90th percentile, over every pull request merged in
  the window.
- **Change failure rate**: confirmed failed changes over deploys (and, in the fold, over pull
  requests). Only a flag the dora role holder has confirmed counts; pending flags are shown
  separately, as the rate if every one were confirmed. Until flags are recorded the card says so
  and shows no rate and no verdict: a rate that measured nothing is not zero.
- **Time to restore**: the median time from a broken pull request's deploy to its fix's deploy,
  over confirmed flags. Until flags are recorded it has no value.
- **Rework share**: pull requests whose title starts with `fix`, `revert` or `hotfix`, over all
  pull requests merged in the window.
- **Deploy runs reaching production**: runs that reached the production job, over runs that
  concluded success or failure. A cancelled run is not an attempt, and the card counts it apart.
- **P0 issues with no owner**: open issues of priority P0 with neither a claim nor a route, in
  every project. It follows neither the window nor the filters, and each issue on the card links
  to it.

The filters narrow every figure drawn from pull requests: lead time, rework share and the per-pull
request failure rate. Deploys and runs follow the window alone, and a deploy counts as one that
shipped a pull request when it is, for at least one pull request, the first production deploy that
contains it, whenever that pull request merged and whatever the filters.

## The targets

Each target is the target of an eval of the project whose default repository is the deploy
repository (**Settings → Projects → Default repository**), so a change to that eval's target, to
whether the target itself counts as met, or to the statistics it judges moves the card's verdict
and its label. Until someone changes one, and where no project or more than one has the deploy
repository as its default, or the project has no such eval yet, the targets are these:

| Figure | Eval | Target |
| --- | --- | --- |
| Deploys a day | `deploy-frequency` | at least 20 |
| Change failure rate, per deploy | `change-fail-rate` | under 5% |
| Merge → production, for every change | `merge-to-production` | under 45 minutes |
| Pull request open → merge, median | `pr-open-to-merge` | under 60 minutes |
| Deploy runs reaching production | `deploy-run-success` | at least 90% |
| P0 issues with no owner | `unowned-p0` | 0 |

"Under" is met strictly below the target. "At least" is met at the target. An eval whose target is
off has a card with no target and no verdict.

A card's label names the statistics its eval's target judges, so it always says what the verdict
beside it is of: "< 45m for every change" for the longest change, "median < 60m", and, for a
`pr-open-to-merge` whose target someone set to judge its 90th percentile, "p90" in place of
"median". A statistic another judged one implies is left out: `merge-to-production` judges its
longest change and its median, and every change under the target puts the median under it, so
the label names the longest change alone and the verdict is the same. A card whose target
judges its own headline figure alone (deploys a day, the failure rate per deploy, run success,
the count of unowned P0 issues) names no statistic.

The met or missed badge is the only verdict of everything the target judges together. Every other
figure, count, bar and table cell is green or red only when the target judges the statistic it
shows, and then by its own value of that statistic, never by the badge: under the catalogue's
target merge to production's median is green or red by the median itself, its longest change by
the longest change, and a figure the target does not judge, such as the failure rate's upper bound
under the catalogue's target, reads plain. A day's bar and a day's cell in the per-day table are
judged by that day's value only for a rate or a median, 90th percentile or longest time; a count
over the window, such as the concluded runs or the deploys, is judged by the window's total, so its
figure on the card takes the colour and no day does. The dashed target line is drawn exactly where
the bars can take a colour. With the target off, nothing takes a colour and no line is drawn.
A coloured figure, and the value a coloured bar writes in its hover title, takes more decimals
where its usual ones would put it on the other side of the target from its own value (on the
target where the value is not, or past it), so it always reads on the side its colour says (4.96%
under a 5% target, never 5.0%, while a rate of exactly 5% keeps 5.0%), and a label writes its
target exactly as it was set.

## For agents

`GET /api/v1/delivery/measures?from=<RFC 3339>&to=<RFC 3339>` answers the same figures, the
targets, whether each is met, and the unowned P0 issues. Each target is
`{"value", "inclusive", "direction", "judges"}`, or null when the eval's target is off:
`inclusive` says whether the value itself is met, `direction` is the eval's, `higher_better` or
`lower_better`, and `judges` lists the statistics the verdict judges, each `<series>.<stat>`, in
the order they are judged (the eval's `target_judges`, or its primary series' headline statistic
when it names none). Each verdict under `status` is true, false, or null when there is no target
or nothing to judge. The verdicts are the eval's own, judged by the same code as
`GET /api/v1/projects/{key}/evals`. Two of them are the verdict of one statistic the eval's target
judges rather than of all of them: `merge_to_production_median` is the median's
(`minutes.median` of `merge-to-production`) and `change_failure_rate_upper_bound` the upper
bound's (`upper_bound.rate` of `change-fail-rate`), each null where the eval's `judges` do not
name that statistic. With the catalogue's judges the median has a verdict and the upper bound has
none. The route takes the timeline's parameters: `from` and `to`
(the last seven days when both are left out, at most 84 days apart), `q` for the search, and any
filter repeated for several values, such as `&repo=acme/widgets&priority=P0`. `from`, `to` and
`q`, and the Pipeline route's `at`, take one value each, and a query naming one of them twice is
`400 INVALID_QUERY`, as it is on the eval routes. Any signed-in person or agent token reads it.
`GET /api/v1/delivery/timeline` carries the same
answer as its `measures` for its own window and filters, so an agent reading both needs only the
timeline. The routes and the page's live updates compute from the same stored facts with the same
code, so they agree. `flags_source` says whether recorded flags fed the change failure rate;
`none` means no flags are recorded yet, so every count in `change_failure_rate` is 0 and its rates
(each `rate` and `upper_bound_rate`) are null, `time_to_restore` has no median, and
`status.change_failure_rate` and `status.change_failure_rate_upper_bound` are null.
`change_failure_rate` itself is always the object, never null, so a Delivery page of an earlier
build, which reads its fields without checking for null, can still read the answer.

`GET /ws/live` is the one websocket each browser tab keeps open for all of its live views. A
signed-in person or agent token may open it; only a person may subscribe to an agent conversation.
Every client message is one JSON object:

```json
{"type":"subscribe","sub":"<client id>","view":{"kind":"timeline","window":{"preset":28},"query":""}}
```

`sub` identifies that subscription in every response. `unsubscribe` with the same `sub` ends it.
A delivery view is `timeline` or `measures` with its window and GET-route query; `pipeline` with
an optional `at`; `legion` with the `project` whose Legion board it shows (the body
of `GET /api/v1/projects/{key}/legion/board`) and nothing else; or `evals` with the `project`
whose evals it shows, its window, and the query `GET /api/v1/projects/{key}/evals` would be sent
without `from` and `to` (`compare_from` and `compare_to`, `repo`, `kind`, `limit`, `offset`). A
delivery view may resume with `{"seq":"…","revs":{…}}`. An event-feed view is
`{"kind":"events"}` and resumes with `{"since":"<event id>"}`. An agent view is
`{"kind":"agent","session_id":"<uuid>"}` and never has a resume. A resume of the wrong shape, or
a resume that carries a member its view does not take, is an `INVALID_FRAME` error. Any other
member a frame does not declare is ignored, so a page of a newer build can reach this build.

Dispatch answers a delivery view with:

- a `snapshot`: the matching GET route's body for the window resolved now, the change it reflects
  (`seq`), when that change was recorded (`changed_at`), and a revision for every key it holds;
- then only `delta` frames: the list items that changed (`items`, each list's `upsert` and the ids
  it `remove`s: the timeline's pull requests, runs and session groups, and an evals view's evals,
  by eval id), the map entries and counts that changed (`maps` and `counts`, a legion view's
  issues by key among the maps, null for a key that left), and the fields replaced whole (`set`,
  which carries `window` whenever it moved, never alone). A legion view sends an issue's entry
  only when its stored row moved, not each time an open issue's hours grow: an entry with a value
  that grows carries `as_of` and `growth`, so a page gives each growing value at its own clock as
  value plus rate times the hours since `as_of`, as the GET route does at its read, and an entry
  none of whose values grows carries null for both;
- `freshness`, the whole freshness verdict - the `freshness` object a timeline, measures or
  pipeline snapshot carries (the last event, the last reconcile, its error and how many pull
  requests cannot be fetched) and the
  `stale` source list - sent right after a subscription's first answer when Dispatch
  holds one, so a socket opened or resumed after the verdict changed still gets it, and to every
  subscriber whenever it changes;
- `error`, with the code the matching GET route would answer (`INVALID_QUERY`,
  `DELIVERY_NOT_CONFIGURED`; `LEGION_NOT_CONFIGURED` for a legion view of a project with no Legion
  daemon, `PROJECT_NOT_FOUND` for an evals view of a project that does not exist) or
  `INVALID_FRAME`; and
- `heartbeat`, every 15 seconds. While nothing the view shows changes, the heartbeat is the only
  frame sent.

An events subscription first receives `events_start` with the decimal id it starts after and
`replay_complete`, then `event` frames with their id and event. A page that has no cursor adopts
`events_start.since`, so the next subscription resumes without a gap. `replay_complete` is true
when the subscription resumed from an id at or below the log's head: Dispatch replays every event
after it, so the page refreshes only what those events change. It is false with no resume, and for
a resume above the head (a log that was reset), whose replay cannot cover the gap: then the page
refreshes everything it holds. An agent subscription receives `agent` frames whose `event` is
`responder`, `replay`, or `frame`; their `data` is the session's JSON. Either kind may receive
`ended` with `behind` or `dropped`; subscribe that view again at once. An events view that ends
with no `events_start` since it was sent is one Dispatch could not start, which it ends a second
after each subscription while its database does not answer: subscribe it again after a backoff
from 1 s, doubling up to 30 s, as after an events `error`, and refresh what the page holds once, at
the next `events_start`, unless that start says its replay is complete. An agent refusal is
terminal for that view (`OAUTH_SCOPE_REQUIRED`, `SESSION_ID_INPUT`, or `STREAM_UNAVAILABLE`).

A client that reconnects subscribes a delivery view again with `"resume": {"seq", "revs"}`, the
last change it applied and the revisions it holds, and is sent a delta of what changed since; a
resume from before the day of changes Dispatch keeps is answered with a snapshot.

Reconnect every subscription when the socket closes. A 4001 `max age` close is routine: Dispatch
chooses an age in [30, 60) minutes, and the page reconnects at once without refreshing. Dispatch
closes every socket with code 1012 when it restarts, and the page reconnects after a random wait of
under two seconds, so the pages one restart closed do not all reach the next server at the same
moment, and resumes; the `events_start` that follows decides what the page refreshes, as above. A
1008 close means the caller no longer resolves; re-check sign-in before retrying. A 1013 full-queue
close, 1009 oversized frame, 1011 internal error, or an ordinary connection loss backs off and
resumes. The page may close a silent socket with 4000 before it opens its replacement.

`packages/dispatch/e2e/delivery-live-probe.ts` is the way to watch one delivery subscription from
a checkout:

```sh
cd packages/dispatch
bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> \
  --view '{"kind":"timeline","window":{"preset":28},"query":""}' --minutes 10 --compare
```

It subscribes under the token in the file, writes one JSON line per frame (`at`, `type`, `bytes`,
and the pull requests, runs, map keys, counts and `set` fields the frame carries), applies each
frame as the page does, and reconnects and resumes as the page does. It writes an `open` line each
time a connection it holds opens and a `close` line each time one it holds closes, whether
Dispatch closed it or the probe replaced it as silent; the one it holds when the watch ends, open
or not, is let go and closed with no close line. A close line carries `wait_ms`: the wait in
milliseconds the probe computed before it would open again, or `null` where it answers that close
with no timer, which is the rotation close it answers at once, the silent socket it replaces at
once, and a first connection Dispatch refused. `--minutes` counts from the
moment its socket first opens. Before that, the first connection has `--open-within <seconds>` to
open; without that flag it has the page's silence rule, `LIVE_SOCKET_SILENT_MS` (45 s). If that
first connection does not open by then, or is refused, the probe ends with exit 1; a later
connection that is refused or does not open is dialled again, as the page does. When `--minutes`
is up, the probe writes an `end` line, `{ at, type: "end" }`; a signal, a refusal or a failure
writes none. When the time is up, `--compare` reads the GET routes for the last window it applied
and writes, for each, whether the pull request ids, run ids and every measure equal what it holds;
it exits 1 when one does not, when no snapshot arrived to compare, or when a read receives nothing
for `--read-within <seconds>`, a time that starts over as each piece of the answer arrives; without
that flag a read has the page's own request deadline, `REQUEST_DEADLINE_MS` (30 s).
`--body-file <file>` writes the body it holds.
`--reconnect-spread-ms <n>` sets the window the probe's first reconnect attempt's wait is drawn
from, uniformly in [0, n), in place of the page's own `RECONNECT_SPREAD_MS` of two seconds; a
later attempt keeps the page's backoff whatever it is set to.
A value above a timer's longest wait, 2147483647 ms, is refused, because the runtime clamps such
a delay to 1 ms and the probe would reopen at once: the opposite of what the option says. That
reason rests on a measurement of the runtime rather than on a row or a code reading: on Bun
1.4.4, `setTimeout` at 2147483648 ms warned `Timeout duration was set to 1` and fired after
118 ms, measured by the tester of dispatch://INFRA-1571 under six busy loops on two CPUs. That
delay is the scheduler's, not the clamp's: the same call fired after 71-110 ms in five more runs
under that load and after 28-42 ms in three idle runs, against the 24.9 days the delay asked for.
A negative value is refused, and so is one that is not a number, and a blank or whitespace one,
which `Number` would otherwise read as a spread of zero. Each of these is a usage error: the
probe writes `--reconnect-spread-ms is a number of milliseconds from 0 to 2147483647, not`
followed by the value it was given, then the usage line, and exits 2 without connecting to
Dispatch. Of what this section says of the probe's lines and of `--reconnect-spread-ms`, these
rest on a reading of the probe's code alone, because no row of its test drives or reads them:
that a frame's line carries `at`; what it says of the silent
socket; that an `open` line comes only for a connection the probe still holds; that the connection
held when the watch ends writes no close line when it had not yet opened; that a negative value
and one that is not a number are refused; and the usage error's text after
`--reconnect-spread-ms`.

A session group's Parent agent value is `group:<id>`, so a rename keeps every filter and link that
names it; the timeline's `session_groups` lists every group with its name and sessions, beside
`session_titles` and `session_pr_counts` for the window's sessions. `POST
/api/v1/delivery/session-groups` (`{name, sessions}`) makes a group, `PATCH
/api/v1/delivery/session-groups/{id}` (`{name?, add?, remove?}`) changes one, and `DELETE` removes
it. Any signed-in person or agent token may write them. A session another group holds is `409
SESSION_IN_GROUP`, a name another group has `409 SESSION_GROUP_NAME_TAKEN`, and a name that is the
title of a session outside the group `409 SESSION_GROUP_NAME_IS_SESSION_TITLE`, each naming what it
refers to. The [HTTP API reference](/legion/dispatch/reference/api/) lists them with every other
route.

## Pipeline

The **Pipeline** page (`/pipeline`, **Pipeline** in the sidebar under **Delivery**) shows how
long each step between a pull request and production takes, and how often it succeeds, over the
last seven UTC days. Today is the last day, drawn faded because it is not over. The page follows
neither the Delivery page's window nor its filters. It updates itself live, as the Delivery page
does, and a deploy card moves as soon as the deploy job it times finishes. A pull request's CI run
reaches the **PR CI** card and its two tables only when the five-minute reconcile records it,
since Envoy relays no event for a workflow run of a pull request.

![The Pipeline page: six timing cards over the slowest-jobs and failing-jobs tables](/legion/media/dispatch/pipeline.png)

Each run counts on the day it started, and each job on its run's day. Only a result that ended in
success or failure is timed; a timed-out result is a failure. A cancelled run or job is counted
beside the timed ones and kept out of the success rate, and anything else (skipped, still
running) is not counted at all. Success is success over success plus failure. Times are wall
clock; a step's median and 90th percentile are rounded to a tenth of a minute.

- **PR CI**: runs of the pull-request checks workflow on a pull request (GitHub's
  `pull_request` event; merge-queue runs are not counted), from the run's start to its
  completion.
- **Staging end-to-end gate**, **Deploy to staging** and **Deploy to production**: the deploy
  workflow's jobs named in **Settings → Delivery timeline**, in its push runs on `main`. The
  production card follows every listed production job name. A run started by hand, or on another
  branch, is not counted. The first two are optional settings; a pipeline that names neither has no
  such card.
- **Merge to production**: the delivery lead time, by the day each pull request merged. A pull
  request still waiting has none yet. It is met or missed as the `merge-to-production` eval judges
  it, on the seven days' longest change and their median until someone changes that: under 45
  minutes for every change.
- **PR open to merge**: every merged pull request, deployed or not, by the day it merged. It is
  met or missed as the `pr-open-to-merge` eval judges it, on the seven days' median until someone
  changes that: a median under 60 minutes.

The verdict on these two cards is the eval's own, judged by the same code as
`GET /api/v1/projects/{key}/evals`. Each card names the statistics its eval's target judges and
shows today's and the seven days' value of each, leaving out one that another implies (merge to
production shows its longest change, which under the target puts the median under it), so a card
whose eval someone set to judge the 90th percentile reads "P90" where it read "Median". Each
figure on these cards, and each day's median bar and 90th-percentile bar, is green or red only
when the eval's target judges the statistic it shows, by its own value, the target's own value
included where the eval counts it as met; today's figures and the bars are a day's, so under a
target of the count of pull requests only the seven days' count takes a colour. The dashed line
marks the target exactly where a bar can take a colour.
A coloured figure, and the value a coloured bar writes in its hover title, takes more decimals
where its usual ones would put it on the other side of the target from its own value (on the
target where the value is not, or past it), so it always reads on the side its colour says (59.9m
under 60 minutes, never 60m, while a median of exactly 60 minutes keeps 60m), and the card writes
its target exactly as it was set.

**PR CI: slowest jobs** ranks the checks workflow's jobs by their seven-day median, slowest first,
and shows five. A job needs a number of timed results in the seven days to be ranked, so a rarely
run job cannot top the table on a handful of results: the `min_n` of the `job:*` series of the
`pr-ci` eval in the project whose default repository is the deploy repository (**Settings →
Projects → Default repository**), which is 50 until someone changes it, and 50 where no project
or more than one has that default, or that project has no `pr-ci` eval yet. The page says the
number it ranked by. The
`pr-ci` eval's items sorted by median list every job ranked the same way. Only jobs of runs that
ended in success or failure are counted.

**PR CI: jobs that failed in the last 24 hours** lists every job that failed in a run completed in
the last day, most failures first, with how many of its results there failed and a link to the run
of its latest failure. A cancelled run's jobs are not counted, and neither is the roll-up job
named in **Settings → Delivery timeline**, which only reports the other jobs' result and fails
whenever any of them does, nor a notification or guard step.

For agents, `GET /api/v1/delivery/pipeline` answers the same numbers. Its optional `at`
(RFC 3339) names the day the seven days end on and the moment the failing jobs look back from;
left out, it is now. `pr_checks.slowest_min_results` is the number of timed results a job needed
to be ranked among the slowest, `targets` holds the same targets as the measures route, and
`status` the verdicts of the two lead-time cards. Any signed-in person or agent token reads it.
