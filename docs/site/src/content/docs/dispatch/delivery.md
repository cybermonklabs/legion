---
title: Delivery
description: Each project's delivery settings, the record of its deploys and failed changes, the Delivery page's timeline and measures, the Pipeline page's step timings, what each figure counts, the targets, and the routes an agent reads.
sidebar:
  order: 7
---

The **Delivery** page shows how work reaches production.
A timeline of every merge, deploy and failed run sits under a panel of delivery measures.
Both follow the same window and filters, and both read the facts Dispatch stores from GitHub for every project that has delivery on, each repository's runs judged by the rule of the deploy source that reads that repository.
The three failure measures among them read the record of deploys and failed changes instead (below).
The **Pipeline** page beside it shows how long each step of CI and the deploy takes, day by day.
Each project turns delivery on, and names its deploy sources, on its own **Settings** tab.
While no project has delivery on, the routes of both pages answer `404 DELIVERY_NOT_CONFIGURED`, and the message names `PUT /api/v1/projects/{key}/settings/delivery`.

![The Delivery page's measures panel: six target cards over a strip of headline numbers](/legion/media/dispatch/delivery-measures.png)

## Delivery settings

Each project holds its own delivery settings.
Its **Settings** tab, `/projects/<KEY>/settings`, edits them through `GET` and `PUT /api/v1/projects/{key}/settings/delivery`.
They are:

- **Delivery**, on or off.
  With delivery off, Dispatch reads none of the project's deploy sources and builds none of its failure measures, and the other fields stay as they were saved.

- **Deploy sources**, at least one while delivery is on, in their order.
  A `github_workflow` source reads the runs of one deploy workflow.
  It may name its repository (`repo`, `owner/name`).
  A source that names none reads the project's default repository, which **Settings → Projects** sets, and moves when the default repository moves.
  It names its deploy workflow, its production jobs (a renamed job lists its old and new names), an optional production apply step, the optional staging end-to-end and staging deploy jobs, the pull-request checks workflow, and an optional roll-up job of that workflow.
  An `events` source reads the deploys a system posts to the project's record (The record of deploys and failed changes, below), and names no repository and no workflow.
  A project holds one `events` source at most.

- **The population rule**: the GitHub logins whose merged pull requests count (`population_authors`) and the repositories whose merges do not (`excluded_repos`).
  Dispatch stores a pull request once, so it admits a pull request whose author any delivery-on project names, unless its repository is one that any delivery-on project excludes.

- **Failure sources** (`failure_sources`): any of `check_results`, `reverts`, `incident_label` and `events`, the signals the project's failure measures count (The failure measures, below).
  `failures_measured_since` is the first UTC day they count, and a save whose settings hold a failure source must hold that day too, named in its body or kept from the stored settings.

- **Targets** (`targets`): the target of each of the project's evals `deploy-frequency`, `change-fail-rate`, `merge-to-production`, `pr-open-to-merge`, `deploy-run-success`, `failed-deploy-recovery` and `unowned-p0`, a number or `"off"`.
  No settings column holds a target.
  A save writes each target it changes as a new version of the project's eval row, in the save's own transaction, and every read takes the target back from that row.
  A target the save leaves out, or one the eval holds already, writes nothing.

A repository is the deploy source of one delivery-on project at most, and of one source of that project, so each repository has one rule and one owner.
The save checks this under a lock of each repository involved, taken in name order, so two saves cannot both take one repository.
A move of the default repository of a delivery-on project whose `github_workflow` source names no repository, and so reads the default, is held to the same rule: `PUT /api/v1/projects/{key}/default-repository` refuses a move onto a repository that another delivery-on project's source reads or that another source of the project names (`409 DELIVERY_REPOSITORY_SHARED`), and refuses to clear it (`409 DELIVERY_NEEDS_REPOSITORY`).
Any other project moves its default repository freely.

A save that names `sources` replaces the project's sources with that list, and a save that leaves `sources` out keeps them.
A source whose repository and rule are both unchanged keeps its id, its bookkeeping and its repository's reconcile progress.
Every other source's repository has its progress cleared, so the next reconcile imports that repository's runs of the last 28 days again.
The answer, like `GET`, adds to each source its `id`, its `resolved_repo` (the repository it reads, null where it names none and the project has no default repository) and its `reconciled_through`.
`reconciled_through` is how far the reconcile has imported that repository's deploy runs, pull-request check runs and merged pull requests, and it is null for an `events` source, for a project with delivery off, and for a repository whose first import has not finished.
A reader that wants a source's numbers after a save waits until its `reconciled_through` is later than the save.

`GET` answers every reader of the project, a read-only token included, and answers `null` for a project that has saved no settings.
`PUT` writes each top-level field its body names and keeps the stored value of each one it leaves out, as the settings stand when the save is written, so a save that waited for another save of the project keeps what that one wrote.
`null` clears a field that may be empty.
`PUT` answers the settings as `GET` does.
The settings belong to the deployment rather than to one project, so a credential the broker scoped to one project is refused `403 PROJECT_SCOPE_DEPLOYMENT`, the project its path names included.
A person saves them, and so does an agent token that no broker scoped to a project; a broker token that acts for a service and no person is refused `403 SIGN_IN_REQUIRED`, as it is refused every write.
A refused save writes nothing, and its body is `{code, error, field?, source?, project?}`:

- `400 DELIVERY_SETTINGS_INPUT`: a field no reader could read, named in `field`, with the source's index in `source` for a field of a source.
  Delivery on with no source, more than 50 sources, a kind that is neither `github_workflow` nor `events`, a `github_workflow` source with no deploy workflow, no production job, an empty production job name, a production job named twice or no pull-request checks workflow, a repository that is not `owner/name`, a second `events` source or one that names a repository or a workflow field, two sources that read one repository, a failure source that is not one of the four or is named twice, a failure source with no `failures_measured_since` or a day that is not `YYYY-MM-DD`, and a target of another eval, of an eval the project does not hold, or that is neither a number nor `"off"` are each refused this way.

- `409 DELIVERY_SETTINGS_ACCESS`: with delivery on, a `github_workflow` source whose repository Dispatch's GitHub App cannot read, named in `source`.
  Dispatch asks GitHub before it writes anything, and a save whose settings have delivery off asks nothing, so a project whose repository the App can no longer read can still turn delivery off.

- `409 DELIVERY_NO_DEFAULT_REPOSITORY`: with delivery on, a `github_workflow` source that names no repository, of a project with no default repository.
  The message says to name the source's repository or to set the project's default repository in **Settings → Projects**.

- `409 DELIVERY_REPOSITORY_SHARED`: with delivery on, a repository that a source of another delivery-on project reads, that project named in `project`.

- `409 DELIVERY_SETTINGS_CHANGED`: another write moved what the save rests on under each of its two attempts: the repository a source reads, which the save has then not proved the App can read, or a target the save names.
  Read the settings again and save again.

- `404 PROJECT_NOT_FOUND`: no project has the key.

An agent may save them on the tab itself, in a headless browser that sends its agent bearer on every request to Dispatch and has no sign-in: `GET /auth/whoami` answers such a bearer as an agent, and the dashboard draws its shell for it.
An agent whose credential proves no session opens the tab at `/projects/<KEY>/settings?session=<its session>`, and its save names that session as the writer (`actor`), as a bearer's write must; a broker-issued token's own session is named without the parameter.
Opened with neither, the tab sends no save: **Save delivery settings** says to open the page with `?session=<your session>`, since Dispatch would refuse the save `400 ACTOR_KIND`.
The agent's shell has no **Inbox** link, no **Peek**, no **Sign out** and no **Settings** link, and shows none of a person's own lists (the Inbox's asks, threads and credential requests, pinned issues and pinned agents) and asks for none of them, since each needs a person's sign-in or the `person` scope.
Its Inbox page, `/`, says that an agent has no Inbox.

The deployment-wide delivery record of the release before this one, `GET` and `PUT /api/v1/settings/delivery`, still answers for one release, because a route is removed only in the release after the one that stops calling it; nothing in the web app calls it, this release reads that record nowhere else, and the next release removes the route.

## The window and filters

The page opens on the last 28 days.
**Window**, in the page header, sets it: the last 7, 14, 28 or 84 days, or a custom range of whole days in UTC, both ends included.
A preset rolls, so a link to the last 7 days always shows the latest 7; a custom range keeps its dates.
The page reads at most 84 days, and refuses a longer custom range, saying so.
Changing the window clears a dragged stretch.

The window, the search box and every filter (Repository, Parent agent, Session, Dispatch issue, Priority, Component, Author, rework, and whether a pull request has deployed) are written into the address, so a link shows the same figures to whoever opens it.
Dragging across the timeline narrows the measures to the stretch you dragged over; the timeline itself keeps the whole window.
**clear brush window** puts the measures back.

## Live updates

An open Delivery or Pipeline page keeps itself current over one connection to Dispatch.
A merge or a deploy appears within seconds, without a reload.
Nothing reads again on a timer: Dispatch sends the page an update only when something it shows has changed, and each update carries only what changed, such as the pull requests and runs that moved, the counts beside the filters and the measures' new numbers.
A preset window keeps its end at the present, so a pull request or run leaves the page at the moment it ages out of the window.
A deploy shows once its production job finishes, without waiting for the rest of its run.

Each page's freshness row says **Live** while the connection is open, with how long ago the last change arrived.
A source Dispatch reports as stale, or a failed reconcile, still shows in red with its age and error.
While the connection is lost the row reads **Reconnecting** in red, with how long it has been lost; the page reconnects by itself and is sent only what changed while it was away.
A page away for more than a day reads its window once, whole.
The whole verdict follows a reconnect — which sources are stale, the last reconcile and its error, and how many pull requests Dispatch can no longer fetch — so a failure that cleared or appeared while the page was away shows at once rather than at its next full read.

## Grouping sessions

A pull request's **Parent agent** is the first agent session that wrote it, shown by the session's title.
One agent often runs as several sessions (it restarted, or it was resumed), and a session group puts them back together: every pull request a member wrote first counts under the group's name, in the Parent agent filter, the colours, the lanes, the list, the details and the measures.
The Session filter still lists each session on its own.

**Edit groups**, under the Parent agent filter, lists the window's sessions with their titles and how many pull requests each wrote first.
Select sessions and name them to make a group, and rename a group, add the selected sessions to it, remove a member, or delete it.
Groups are shared: everyone who opens the page sees the same ones.
A session can be in one group only, and two groups cannot share a name.
A group may take a session's title as its name only when it holds every session with that title, so one name never means two things.
A session that later takes a group's name as its title, without joining it, shows with its id beside the title.
Deleting a group shows its sessions on their own again.

Colouring the merges by Parent agent adds **Own colour and lane**: the values you choose there each get a colour of their own, and a lane of their own when swimlanes are on, and every other value shares one **Other** colour and lane.
With none chosen, the colours and lanes are as usual.

## The measures

Six cards across the top each judge one figure against its target: **met**, **missed**, or **no data** when the window holds nothing to measure.
Below them, one line carries the headline numbers.
**Definitions & alternatives** opens the other definitions of each figure and a per-day table, newest day first; a day the window only partly covers is marked partial.

- **Deploy**: a run of a deploy source's workflow on `main` whose production job succeeded: a job that the source names among its production jobs (a renamed job lists its old and new names) and, where the source names a production apply step, one whose step of that name succeeded too, so a run that stood down without applying is no deploy.
  A run counts on the day it started.
  Runs on other branches count in no figure.

- **Deploys a day**: deploys in the window divided by the window's length in days.

- **Lead time**: from a pull request's merge to the first successful production deploy that contains it: the earliest to finish of the deploys whose run GitHub created after the merge.
  A merge queue writes its merge commit before GitHub records the merge, so the run's creation, not its commit's time, says which deploy holds a pull request.
  The cards show the median and the longest; the fold adds the 90th percentile and the time from the first commit and from opening the pull request.

- **Pull request open → merge**: the median and 90th percentile, over every pull request merged in the window.

- **Change failure rate**: the deploys with a counted failure over the deploys, per deploy (The failure measures, below).
  Where the window holds no stored row of it, the card says so and shows no rate and no verdict: a rate that measured nothing is not zero.

- **Failed deployment recovery time** (**Recovery time** in the line of headline numbers): the median time from a failed deploy to the failure's restore, over the failures restored in the window, beside how many are not restored yet.

- **Deployment rework rate** (**Deploy rework**): the deploys that restored a failure over the deploys.

- **Rework share**: pull requests whose title starts with `fix`, `revert` or `hotfix`, over all pull requests merged in the window.

- **Deploy runs reaching production**: runs that reached the production job, over runs that concluded success or failure.
  A cancelled run is not an attempt, and the card counts it apart.

- **P0 issues with no owner**: open issues of priority P0 with neither a claim nor a route, in every project.
  It follows neither the window nor the filters, and each issue on the card links to it.

The filters narrow every figure drawn from pull requests: lead time and rework share.
Deploys and runs follow the window alone, and a deploy counts as one that shipped a pull request when it is, for at least one pull request, the first production deploy that contains it, whenever that pull request merged and whatever the filters.
The three failure measures follow the UTC days the window touches, and no filter.

## The targets

Each target is the target of an eval of the project whose deploy sources read every deploy repository the page reads, so a change to that eval's target, to whether the target itself counts as met, or to the statistics it judges moves the card's verdict and its label.
A project sets the targets of its delivery evals on its settings tab (Delivery settings, above).
Each project's evals start with these targets, and these targets stand in where two or more projects own the deploy repositories, where no repository is read, and where that project has no such eval yet:

| Figure | Eval | Target |
| --- | --- | --- |
| Deploys a day | `deploy-frequency` | at least 20 |
| Change failure rate, per deploy | `change-fail-rate` | under 5% |
| Merge → production, for every change | `merge-to-production` | under 45 minutes |
| Pull request open → merge, median | `pr-open-to-merge` | under 60 minutes |
| Deploy runs reaching production | `deploy-run-success` | at least 90% |
| P0 issues with no owner | `unowned-p0` | 0 |

"Under" is met strictly below the target.
"At least" is met at the target.
An eval whose target is off has a card with no target and no verdict.

A card's label names the statistics its eval's target judges, so it always says what the verdict beside it is of: "< 45m for every change" for the longest change, "median < 60m", and, for a `pr-open-to-merge` whose target someone set to judge its 90th percentile, "p90" in place of "median".
A statistic another judged one implies is left out: `merge-to-production` judges its longest change and its median, and every change under the target puts the median under it, so the label names the longest change alone and the verdict is the same.
A card whose target judges its own headline figure alone (deploys a day, the failure rate per deploy, run success, the count of unowned P0 issues) names no statistic.

The met or missed badge is the only verdict of everything the target judges together.
Every other figure, count, bar and table cell is green or red only when the target judges the statistic it shows, and then by its own value of that statistic, never by the badge: under the catalogue's target merge to production's median is green or red by the median itself, its longest change by the longest change, and a figure the target does not judge, such as the change failure rate's count of deploys with a failure under the catalogue's target, reads plain.
A day's bar and a day's cell in the per-day table are judged by that day's value only for a rate or a median, 90th percentile or longest time; a count over the window, such as the concluded runs or the deploys, is judged by the window's total, so its figure on the card takes the colour and no day does.
The dashed target line is drawn exactly where the bars can take a colour.
With the target off, nothing takes a colour and no line is drawn.
A coloured figure, and the value a coloured bar writes in its hover title, takes more decimals where its usual ones would put it on the other side of the target from its own value (on the target where the value is not, or past it), so it always reads on the side its colour says (4.96% under a 5% target, never 5.0%, while a rate of exactly 5% keeps 5.0%), and a label writes its target exactly as it was set.

## The record of deploys and failed changes

Each project keeps one record of its deploys and failed changes, whatever wrote them: the table `production_events`, read and written through `/api/v1/projects/{key}/production-events` (For agents, below).
A deploy row is one deploy that reached production.
A failure row is one failure signal, and it names the deploy it blames, which may be a deploy row of any project.
Each row carries `source_ref`, its writer's own key, and a project holds one row for each `source_ref`, so a signal that arrives twice writes one row.

Deploy rows come from two writers:

- The GitHub adapter writes one row for each run of a `github_workflow` source's repository that the source's rule calls a deploy: a run of the deploy workflow on `main` whose production job succeeded, and whose apply step succeeded too where the source names one.
  The row belongs to the project whose source reads the repository, with `source` `github`, `source_ref` `github:<run id>`, `status` `succeeded`, `started_at` the run's creation, `finished_at` the production job's completion, `commits` the run's head and the merge commit of each pull request it ships, and `pull_requests` the pull requests it ships first.
  The production signals loop writes the row in a transaction of its own after the run's write commits, and moves it in place where the run moves, as when GitHub runs it again.
  When a repository's production jobs, its production apply step or the project whose source reads it change, the loop derives every deploy row of that repository again: a run the rule calls a deploy gains or keeps its row, and every other GitHub deploy row of the repository is deleted.
  A row that a failure blames, or that a restore names, is kept instead with `status` `dropped` and the reason in its `note`, and each counted failure that blames it is rejected with that reason, which names the rule.
  A `dropped` row counts in no failure measure, and no signal finds it by a commit, a run or a pull request.
  Where the rule calls its run a deploy of the same project again, the row is `succeeded` again, and the failures it lost stay rejected until someone counts them again.

- A system posts a deploy, `kind: deploy`, to `POST /api/v1/projects/{key}/production-events` with an agent or service token; its `source` is `api`.

Failure rows come from five signals.
A project's failure measures count each signal where the project has the failure source named beside it on, and a person's flag under any failure source:

- `check_fail` (failure source `check_results`): a `fail` result of a check whose `when` is `after_merge`.
  It blames the deploy that first shipped the result's commit, of any project, and is written under the project of the issue the result was recorded on, with that issue's pull requests, the result's actor, and the result's time as `detected_at`.
  Its `source_ref` is `check:<check document id>/<check block id>@<deploy id>`: block ids are per document, so two documents' checks of one block id on one deploy write two rows.
  The result route writes it as soon as the result commits, and the production signals loop, which reads the `result.recorded` event, writes it where that write was lost.
  A result whose commit no deploy row holds yet makes its row when such a deploy row lands, and a result that no deploy ever ships makes none.
  A retraction of the result (`POST /api/v1/results/{id}/retract`) rejects the row with the retraction's reason and actor, unless another `fail` of the same check that is not retracted, whose commit the same deploy first shipped, still stands behind it.

- `revert` (failure source `reverts`): a merged pull request of a source repository whose body says `Reverts <owner>/<repo>#<n>`, the line GitHub's revert button writes, whoever wrote it.
  The pull-request intake records the revert as it merges, and the reconcile's search records one the intake missed.
  Once the deploy that ships the revert has its deploy row, the loop writes the failure under the project whose source reads the repository.
  It blames the deploy row that shipped the reverted pull request, or else the one whose commits hold that pull request's merge commit, and it is detected when the revert merged.
  The revert's own deploy restores it: `restored_at` is that deploy's finish, and `restored_by_deploy` is that deploy's row.
  Its `source_ref` is `revert:<owner>/<repo>#<n>`, the revert's own pull request.
  A revert whose reverted pull request no deploy row holds makes no row.

- `incident_issue` (failure source `incident_label`): an issue labelled `incident`, in any case, whose own links name a GitHub pull request page or a workflow run page (`/actions/runs/<id>`).
  It blames the earliest deploy among those its links name: the deploy that shipped the pull request, or the run's deploy row.
  It is written under the issue's project, with `source_ref` `incident:<issue key>`, and its actor and `detected_at` are those of the event that made the issue an incident or gave it its link.
  An incident whose links name no deployed pull request and no deploy run makes its row when such a link is added.
  A `bug` label makes no failure.

- `api` (failure source `events`): a failure a system posts with an agent or service token, naming the deploy it blames by `deploy_id`, by the GitHub run (`cicd.pipeline.run.id` with `repo`), or by a commit the deploy shipped (`commit_sha`).

- `person` (any failure source): a failure a person posts with a credential that holds the `person` scope, the person's flag of a deploy.
  The person is its actor, and its `source_ref` is the server's, `person:<deploy id>:<login>`, so a second flag of one deploy by the same person writes nothing.

Every signal counts when it arrives, and no step waits for a person.
A failure leaves the numbers only when it is rejected.
A person or the project's controller rejects one with `PUT /api/v1/projects/{key}/production-events/{id}/state` and `{"state": "rejected", "reason": "…"}`.
The row stays, with the reason, who rejected it and when (`state_reason`, `state_by`, `state_at`), and `{"state": "counted", "reason": "…"}` puts it back with its own reason.
A person is a credential that holds the `person` scope.
The project's controller is the session that the project's Legion daemon names as its registered controller at the moment of the write, calling with that session's own broker-issued token.
Dispatch asks the daemon that the project's Legion setting names, at `GET /legion/v1/controller`, before it writes, and waits at most 2 seconds for the answer.
A person marks a failure restored with `{"state": "restored", "restored_by_deploy": "<deploy id>"}`, which sets `restored_at` to the time of the write and names the deploy that restored it where the body names one.
Anyone else is refused `403 PRODUCTION_EVENT_STATE_FORBIDDEN`, and so is a controller's write where the project names no Legion daemon or where its daemon names no registered controller or another session.
Where Dispatch cannot learn the controller, a controller's write is refused `503 LEGION_CONTROLLER_UNREAD`, and the error says why: the daemon answered `404`, because its build is older than that route, it did not answer in time, or Dispatch could not read its answer.
So the controller can reject or re-count only once its daemon runs a build that serves `GET /legion/v1/controller`, and a person can reject or re-count at any time.

## The failure measures

Three evals of each project count from the record: change failure rate (`change-fail-rate`), failed deployment recovery time (`failed-deploy-recovery`) and deployment rework rate (`deployment-rework-rate`).
They are on for a project with delivery on and at least one failure source, and they hold no row before its `failures_measured_since`.

A failure is a fact about its deploy.
A project's deploys of a UTC day are the deploy rows that finished that day and that count for the project (Which project a row counts for, below).
A posted deploy counts here only for a project that holds an `events` source, and a `dropped` deploy counts for no project.
The failures counted for the project are the failure rows on those deploys, whichever project's record holds them, that are not rejected and whose signal maps to a failure source the project has on: `check_fail` to `check_results`, `revert` to `reverts`, `incident_issue` to `incident_label`, and `api` to `events`.
A person's flag counts under any failure source.
So a failure counts for every project its deploy counts for whose failure sources count its signal.

- **Change failure rate**: the deploys with at least one counted failure, over every deploy.
  A deploy counts once, whatever number of failures blame it.
  The delivery measures route answers it as `{failed, total, rate}`.

- **Failed deployment recovery time**: the median of the minutes from each counted failure's deploy finishing to the failure's restore, over the failures restored in the window, beside `not_restored`, the counted failures of the window's deploys that no restore has closed yet.
  A window with no restore has no median: "no data", never 0.
  The delivery measures route answers it as `failed_deployment_recovery`, `{n, median_minutes, not_restored}`.

- **Deployment rework rate**: the deploys that restored a counted failure, over every deploy.
  A deploy restores a failure when it is that failure's `restored_by_deploy`: a revert's own deploy, or the deploy a person names when marking the failure restored.
  The delivery measures route answers it as `deployment_rework_rate`, `{restoring, total, rate}`.

A window's value folds its days: the two rates divide the summed numerators by the summed denominators, and the recovery time takes the median of every restore's minutes, never a mean of the daily medians.
From `failures_measured_since` on, every day holds a row of each of the three, a day with no deploy a row of zeros, so the two rates of a window from that day with no failure read 0 percent.
A window that starts before that day reads "no data": every statistic of the series is null, and the series' `data_since` names that day.
A `failures_measured_since` in the past counts the record's rows from that day.
A write to the record, including a reject, re-count or restore, makes the eval refresher rewrite the days it affects and update their stored figures together.
A save of the failure sources or measurement start rewrites the project's failure rows; an open Evals page receives the new figures together.
If both the previous and new measurement start are in the future, the page still updates the date it shows, even though there are no failure day rows yet.
A default-repository move changes which projects count a deploy only after the refresher publishes the move.
In the same pass it updates the affected projects' failure rows and figures.
A source move, a repository's source moved to another project, or delivery turned off or on is published the same way: from that commit, the Evals page answers the periods the move touched from the day rows until the refresher has stored their figures again, so the numbers move once, to the new sources' values.

`GET /api/v1/projects/{key}/evals/{slug}/items` of each of the three lists the rows of the record behind its numbers, `deploy` and `failure` rows, each with its `relation` to the numbers: `failed`, `restoring`, `deployed` or `blamed` for a deploy, and `counted`, `restored` or `not_restored` for a failure.

The Delivery page and `GET /api/v1/delivery/measures` show the three for the UTC days the window touches, from the stored rows of the one project that has delivery on.
Where two or more projects have delivery on, or that project's failure measures are off, the three read "no data" there, since a deploy that counts for two projects would count twice in a sum of theirs; each project's own evals still answer.
A window that starts before that project's `failures_measured_since` reads "no data" there too, as its evals read it.

## Which project a row counts for

Each delivery and CI eval counts its rows for projects by one rule.
The failure measures and their items use the same deploy attribution, including the published defaults and source selection:

- A pull request counts for the project of the issue it names.
  One that names no issue, or an issue Dispatch does not hold, counts for each project whose default repository is its repository, and for no project where no project's default repository is.

- A pull-request checks run, and each of its jobs, counts where its pull request counts.
  A run of no pull request, or of a pull request the population holds no row of, counts as a pull request with no issue does.

- A deploy run counts once for each project that the pull requests it ships count for through their issues, and so does each job of it that a deploy card follows.
  Where it ships a pull request that no issue attributes, it also counts for each project whose default repository is its repository, unless that project already counts it through an issue.

- A deploy run that ships no pull request counts for the project whose deploy source reads its repository, and for no other project, whatever the default repositories say.
  A run that did not reach production ships nothing, so it counts there too.
  Such runs follow the source: once the refresher's next pass has recorded a source moved to another repository, or a repository's source moved to another project, they count for the project whose source reads their repository then, and while no delivery-on source reads it they count for no project.

- A posted deploy counts for the project of the route it was posted to, and for no other project.
  While that project has delivery on and an `events` source, the deploy adds one to its `deploy-frequency` on the UTC day the deploy finished, and each population pull request it names adds its time from merge to the deploy's finish to `merge-to-production`, where no earlier deploy shipped it.
  That pull request's row in the eval's items carries the posted deploy's finish as `deployed_at` and no `deploy_run_id`, since a posted deploy is no GitHub run.
  A deploy posted to any other project stays in the record and counts in no eval.

Each series of an eval read carries `via_repository`: how many of the rows its window counts reached the project through a repository rather than an issue, such as a pull request with no issue and a deploy that ships no pull request.

## For agents

`GET /api/v1/delivery/measures?from=<RFC 3339>&to=<RFC 3339>` answers the same figures, the targets, whether each is met, and the unowned P0 issues.
Each target is `{"value", "inclusive", "direction", "judges"}`, or null when the eval's target is off: `inclusive` says whether the value itself is met, `direction` is the eval's, `higher_better` or `lower_better`, and `judges` lists the statistics the verdict judges, each `<series>.<stat>`, in the order they are judged (the eval's `target_judges`, or its primary series' headline statistic when it names none).
Each verdict under `status` is true, false, or null when there is no target or nothing to judge.
The verdicts are the eval's own, judged by the same code as `GET /api/v1/projects/{key}/evals`.
One of them is the verdict of one statistic the eval's target judges rather than of all of them: `merge_to_production_median` is the median's (`minutes.median` of `merge-to-production`), null where the eval's `judges` do not name that statistic, and with the catalogue's judges it has a verdict.
The route takes the timeline's parameters: `from` and `to` (the last seven days when both are left out, at most 84 days apart), `q` for the search, and any filter repeated for several values, such as `&repo=acme/widgets&priority=P0`.
`from`, `to` and `q`, and the Pipeline route's `at`, take one value each, and a query naming one of them twice is `400 INVALID_QUERY`, as it is on the eval routes.
Any signed-in person or agent token reads it.
`GET /api/v1/delivery/timeline` carries the same answer as its `measures` for its own window and filters, so an agent reading both needs only the timeline.
The routes and the page's live updates compute from the same stored facts with the same code, so they agree.
The three failure measures are `change_failure_rate` (`{failed, total, rate}`), `failed_deployment_recovery` (`{n, median_minutes, not_restored}`) and `deployment_rework_rate` (`{restoring, total, rate}`).
Where no stored day row of a measure's eval holds a part of the window, it reads "no data" with no verdict (The failure measures, above): `failed_deployment_recovery` and `deployment_rework_rate` are null, and `change_failure_rate` is still an object, with `failed`, `total` and `rate` null.
While a release rolls, the Delivery page of the release before this one is still served, and it reads three members of `measures` with no null guard.
So the answer keeps `change_failure_rate.per_pr` and `change_failure_rate.per_deploy`, and `time_to_restore`, as objects whose every leaf is null in every window: no release counts what they held, and nothing computes them.
`time_to_restore` is not the recovery time, which is `failed_deployment_recovery`.
No reader reads a figure from those members, and they are removed in the next release, with `GET` and `PUT /api/v1/settings/delivery`.
A project's own figures, its failure measures among them, are its evals: `GET /api/v1/projects/{key}/evals`.

The record has three routes, each of a project:

- `GET /api/v1/projects/{key}/production-events` reads one page of the project's rows, `{items, total, limit, offset}`, newest first: a deploy by its `finished_at` and a failure by its `detected_at`, in `[from, to)` (the last seven days by default, at most 84).
  `kind`, `state` and `deploy_id` keep the deploys or the failures, the counted or the rejected rows, and the failures that blame one deploy.
  Every reader of the project reads it, a read-only token included.

- `POST /api/v1/projects/{key}/production-events` writes one deploy or failure, `{kind, source_ref, environment?, status?, url?, started_at?, finished_at?, detected_at?, repo?, commits?, pull_requests?, deploy_id?, commit_sha?, note?, actor?}`, and takes OpenTelemetry's `deployment.environment.name`, `deployment.status` and `cicd.pipeline.run.url.full` for `environment`, `status` and `url` (one name of a field, or both alike) and `cicd.pipeline.run.id` for a failure's GitHub run.
  It answers `201` with the new row, or `200` with the row the project already holds under that `source_ref`, writing nothing.
  `note` is what the writer says of the event, kept as sent, at most 4,096 bytes.
  A deploy needs `finished_at`, and only an agent or service token posts one: a credential that holds the `person` scope is refused `403 PRODUCTION_EVENT_DEPLOY_FORBIDDEN`.
  A failure names the deploy it blames, a deploy row of any project, one way, by `deploy_id`, by `cicd.pipeline.run.id` with `repo`, or by `commit_sha`, and one no deploy row matches is `409 PRODUCTION_EVENT_NO_DEPLOY`.
  A system's post needs `source_ref`, its own key for the event, and one that starts with a prefix an automatic source keys its own rows under (`github:`, `person:`, `check:`, `revert:` or `incident:`) is refused, since that source's own write under the key would then write nothing.
  A person's flag takes the server's `source_ref`, and a person's post that names another is refused.
  Each of these refusals, and a field no row can hold, is `400 PRODUCTION_EVENT_INPUT`, naming the field.

- `PUT /api/v1/projects/{key}/production-events/{id}/state` rejects, counts again or restores one failure, `{state, reason, restored_by_deploy?}` (The record of deploys and failed changes, above), and answers the row.
  A row the project does not hold is `404 PRODUCTION_EVENT_NOT_FOUND`; a deploy's row, a state that is none of the three, a reject or a re-count without a reason, and a `restored_by_deploy` beside another state are `400 PRODUCTION_EVENT_STATE_INPUT`; and a `restored_by_deploy` that is no deploy row is `409 PRODUCTION_EVENT_NO_DEPLOY`.

Each of the three answers `404 PROJECT_NOT_FOUND` for no such project, and a credential the broker scoped to another project is refused either write with `403 PROJECT_SCOPE`.

`GET /ws/live` is the one websocket each browser tab keeps open for all of its live views.
A signed-in person or agent token may open it; only a person may subscribe to an agent conversation.
Every client message is one JSON object:

```json
{"type":"subscribe","sub":"<client id>","view":{"kind":"timeline","window":{"preset":28},"query":""}}
```

`sub` identifies that subscription in every response.
`unsubscribe` with the same `sub` ends it.
A delivery view is `timeline` or `measures` with its window and GET-route query; `pipeline` with an optional `at`; `legion` with the `project` whose Legion board it shows (the body of `GET /api/v1/projects/{key}/legion/board`) and nothing else; or `evals` with the `project` whose evals it shows, its window, and the query `GET /api/v1/projects/{key}/evals` would be sent without `from` and `to` (`compare_from` and `compare_to`, `repo`, `kind`, `limit`, `offset`).
A delivery view may resume with `{"seq":"…","revs":{…}}`.
An event-feed view is `{"kind":"events"}` and resumes with `{"since":"<event id>"}`.
An agent view is `{"kind":"agent","session_id":"<uuid>"}` and never has a resume.
A resume of the wrong shape, or a resume that carries a member its view does not take, is an `INVALID_FRAME` error.
Any other member a frame does not declare is ignored, so a page of a newer build can reach this build.

Dispatch answers a delivery view with:

- a `snapshot`: the matching GET route's body for the window resolved now, the change it reflects (`seq`), when that change was recorded (`changed_at`), and a revision for every key it holds;

- then only `delta` frames: the list items that changed (`items`, each list's `upsert` and the ids it `remove`s: the timeline's pull requests, runs and session groups, and an evals view's evals, by eval id), the map entries and counts that changed (`maps` and `counts`, a legion view's issues by key among the maps, null for a key that left), and the fields replaced whole (`set`, which carries `window` whenever it moved, never alone).
  A legion view sends an issue's entry only when its stored row moved, not each time an open issue's hours grow: an entry with a value that grows carries `as_of` and `growth`, so a page gives each growing value at its own clock as value plus rate times the hours since `as_of`, as the GET route does at its read, and an entry none of whose values grows carries null for both;

- `freshness`, the whole freshness verdict - the `freshness` object a timeline, measures or pipeline snapshot carries (the last event, the last reconcile, its error and how many pull requests cannot be fetched) and the `stale` source list - sent right after a subscription's first answer when Dispatch holds one, so a socket opened or resumed after the verdict changed still gets it, and to every subscriber whenever it changes;

- `error`, with the code the matching GET route would answer (`INVALID_QUERY`, `DELIVERY_NOT_CONFIGURED`; `LEGION_NOT_CONFIGURED` for a legion view of a project with no Legion daemon, `PROJECT_NOT_FOUND` for an evals view of a project that does not exist) or `INVALID_FRAME`; and

- `heartbeat`, every 15 seconds.
  While nothing the view shows changes, the heartbeat is the only frame sent.

An events subscription first receives `events_start` with the decimal id it starts after and `replay_complete`, then `event` frames with their id and event.
A page that has no cursor adopts `events_start.since`, so the next subscription resumes without a gap.
`replay_complete` is true when the subscription resumed from an id at or below the log's head: Dispatch replays every event after it, so the page refreshes only what those events change.
It is false with no resume, and for a resume above the head (a log that was reset), whose replay cannot cover the gap: then the page refreshes everything it holds.
An agent subscription receives `agent` frames whose `event` is `responder`, `replay`, or `frame`; their `data` is the session's JSON.
Either kind may receive `ended` with `behind` or `dropped`; subscribe that view again at once.
An events view that ends with no `events_start` since it was sent is one Dispatch could not start, which it ends a second after each subscription while its database does not answer: subscribe it again after a backoff from 1 s, doubling up to 30 s, as after an events `error`, and refresh what the page holds once, at the next `events_start`, unless that start says its replay is complete.
An agent refusal is terminal for that view (`OAUTH_SCOPE_REQUIRED`, `SESSION_ID_INPUT`, or `STREAM_UNAVAILABLE`).

A client that reconnects subscribes a delivery view again with `"resume": {"seq", "revs"}`, the last change it applied and the revisions it holds, and is sent a delta of what changed since; a resume from before the day of changes Dispatch keeps is answered with a snapshot.

Reconnect every subscription when the socket closes.
A 4001 `max age` close is routine: Dispatch chooses an age in [30, 60) minutes, and the page reconnects at once without refreshing.
Dispatch closes every socket with code 1012 when it restarts, and the page reconnects after a random wait of under two seconds, so the pages one restart closed do not all reach the next server at the same moment, and resumes; the `events_start` that follows decides what the page refreshes, as above.
A 1008 close means the caller no longer resolves; re-check sign-in before retrying.
A 1013 full-queue close, 1009 oversized frame, 1011 internal error, or an ordinary connection loss backs off and resumes.
The page may close a silent socket with 4000 before it opens its replacement.

`packages/dispatch/e2e/delivery-live-probe.ts` is the way to watch one delivery subscription from a checkout:

```sh
cd packages/dispatch
bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> \
  --view '{"kind":"timeline","window":{"preset":28},"query":""}' --minutes 10 --compare
```

It subscribes under the token in the file, writes one JSON line per frame (`at`, `type`, `bytes`, and the pull requests, runs, map keys, counts and `set` fields the frame carries), applies each frame as the page does, and reconnects and resumes as the page does.
It writes an `open` line each time a connection it holds opens and a `close` line each time one it holds closes, whether Dispatch closed it or the probe replaced it as silent; the one it holds when the watch ends, open or not, is let go and closed with no close line.
A close line carries `wait_ms`: the wait in milliseconds the probe computed before it would open again, or `null` where it answers that close with no timer, which is the rotation close it answers at once, the silent socket it replaces at once, and a first connection Dispatch refused.
`--minutes` counts from the moment its socket first opens.
Before that, the first connection has `--open-within <seconds>` to open; without that flag it has the page's silence rule, `LIVE_SOCKET_SILENT_MS` (45 s).
If that first connection does not open by then, or is refused, the probe ends with exit 1; a later connection that is refused or does not open is dialled again, as the page does.
When `--minutes` is up, the probe writes an `end` line, `{ at, type: "end" }`; a signal, a refusal or a failure writes none.
When the time is up, `--compare` reads the GET routes for the last window it applied and writes, for each, whether the pull request ids, run ids and every measure equal what it holds; it exits 1 when one does not, when no snapshot arrived to compare, or when a read receives nothing for `--read-within <seconds>`, a time that starts over as each piece of the answer arrives; without that flag a read has the page's own request deadline, `REQUEST_DEADLINE_MS` (30 s).
`--body-file <file>` writes the body it holds.
`--reconnect-spread-ms <n>` sets the window the probe's first reconnect attempt's wait is drawn from, uniformly in [0, n), in place of the page's own `RECONNECT_SPREAD_MS` of two seconds; a later attempt keeps the page's backoff whatever it is set to.
A value above a timer's longest wait, 2147483647 ms, is refused, because the runtime clamps such a delay to 1 ms and the probe would reopen at once: the opposite of what the option says.
That reason rests on a measurement of the runtime rather than on a row or a code reading: on Bun 1.4.4, `setTimeout` at 2147483648 ms warned `Timeout duration was set to 1` and fired after 118 ms, measured by the tester of dispatch://INFRA-1571 under six busy loops on two CPUs.
That delay is the scheduler's, not the clamp's: the same call fired after 71-110 ms in five more runs under that load and after 28-42 ms in three idle runs, against the 24.9 days the delay asked for.
A negative value is refused, and so is one that is not a number, and a blank or whitespace one, which `Number` would otherwise read as a spread of zero.
Each of these is a usage error: the probe writes `--reconnect-spread-ms is a number of milliseconds from 0 to 2147483647, not` followed by the value it was given, then the usage line, and exits 2 without connecting to Dispatch.
Of what this section says of the probe's lines and of `--reconnect-spread-ms`, these rest on a reading of the probe's code alone, because no row of its test drives or reads them: that a frame's line carries `at`; what it says of the silent socket; that an `open` line comes only for a connection the probe still holds; that the connection held when the watch ends writes no close line when it had not yet opened; that a negative value and one that is not a number are refused; and the usage error's text after `--reconnect-spread-ms`.

A session group's Parent agent value is `group:<id>`, so a rename keeps every filter and link that names it; the timeline's `session_groups` lists every group with its name and sessions, beside `session_titles` and `session_pr_counts` for the window's sessions.
`POST /api/v1/delivery/session-groups` (`{name, sessions}`) makes a group, `PATCH /api/v1/delivery/session-groups/{id}` (`{name?, add?, remove?}`) changes one, and `DELETE` removes it.
Any signed-in person or agent token may write them.
A session another group holds is `409 SESSION_IN_GROUP`, a name another group has `409 SESSION_GROUP_NAME_TAKEN`, and a name that is the title of a session outside the group `409 SESSION_GROUP_NAME_IS_SESSION_TITLE`, each naming what it refers to.
The [HTTP API reference](/legion/dispatch/reference/api/) lists them with every other route.

## Pipeline

The **Pipeline** page (`/pipeline`, **Pipeline** in the sidebar under **Delivery**) shows how long each step between a pull request and production takes, and how often it succeeds, over the last seven UTC days.
Today is the last day, drawn faded because it is not over.
The page follows neither the Delivery page's window nor its filters.
It updates itself live, as the Delivery page does, and a deploy card moves as soon as the deploy job it times finishes.
A pull request's CI run reaches the **PR CI** card and its two tables only when the five-minute reconcile records it, since Envoy relays no event for a workflow run of a pull request.

![The Pipeline page: six timing cards over the slowest-jobs and failing-jobs tables](/legion/media/dispatch/pipeline.png)

Each run counts on the day it started, and each job on its run's day.
Only a result that ended in success or failure is timed; a timed-out result is a failure.
A cancelled run or job is counted beside the timed ones and kept out of the success rate, and anything else (skipped, still running) is not counted at all.
Success is success over success plus failure.
Times are wall clock; a step's median and 90th percentile are rounded to a tenth of a minute.

- **PR CI**: runs of each source's pull-request checks workflow on a pull request (GitHub's `pull_request` event; merge-queue runs are not counted), from the run's start to its completion.

- **Staging end-to-end gate**, **Deploy to staging** and **Deploy to production**: the jobs each deploy source names in its deploy workflow, in that workflow's push runs on `main`.
  The production card follows every listed production job name.
  A run started by hand, or on another branch, is not counted.
  The first two are optional fields of a source; a pipeline whose sources name neither has no such card.

- **Merge to production**: the delivery lead time, by the day each pull request merged.
  A pull request still waiting has none yet.
  It is met or missed as the `merge-to-production` eval judges it, on the seven days' longest change and their median until someone changes that: under 45 minutes for every change.

- **PR open to merge**: every merged pull request, deployed or not, by the day it merged.
  It is met or missed as the `pr-open-to-merge` eval judges it, on the seven days' median until someone changes that: a median under 60 minutes.

The verdict on these two cards is the eval's own, judged by the same code as `GET /api/v1/projects/{key}/evals`.
Each card names the statistics its eval's target judges and shows today's and the seven days' value of each, leaving out one that another implies (merge to production shows its longest change, which under the target puts the median under it), so a card whose eval someone set to judge the 90th percentile reads "P90" where it read "Median".
Each figure on these cards, and each day's median bar and 90th-percentile bar, is green or red only when the eval's target judges the statistic it shows, by its own value, the target's own value included where the eval counts it as met; today's figures and the bars are a day's, so under a target of the count of pull requests only the seven days' count takes a colour.
The dashed line marks the target exactly where a bar can take a colour.
A coloured figure, and the value a coloured bar writes in its hover title, takes more decimals where its usual ones would put it on the other side of the target from its own value (on the target where the value is not, or past it), so it always reads on the side its colour says (59.9m under 60 minutes, never 60m, while a median of exactly 60 minutes keeps 60m), and the card writes its target exactly as it was set.

**PR CI: slowest jobs** ranks the checks workflow's jobs by their seven-day median, slowest first, and shows five.
A job needs a number of timed results in the seven days to be ranked, so a rarely run job cannot top the table on a handful of results: the `min_n` of the `job:*` series of the `pr-ci` eval of the project whose deploy sources read every deploy repository, which is 50 until someone changes it, and 50 where two or more projects own the deploy repositories, where no repository is read, or where that project has no `pr-ci` eval yet.
The page says the number it ranked by.
The `pr-ci` eval's items sorted by median list every job ranked the same way.
Only jobs of runs that ended in success or failure are counted.

**PR CI: jobs that failed in the last 24 hours** lists every job that failed in a run completed in the last day, most failures first, with how many of its results there failed and a link to the run of its latest failure.
A cancelled run's jobs are not counted, and neither is the roll-up job a deploy source names, which only reports the other jobs' result and fails whenever any of them does, nor a notification or guard step.

For agents, `GET /api/v1/delivery/pipeline` answers the same numbers.
Its optional `at` (RFC 3339) names the day the seven days end on and the moment the failing jobs look back from; left out, it is now.
`pr_checks.slowest_min_results` is the number of timed results a job needed to be ranked among the slowest, `targets` holds the same targets as the measures route, and `status` the verdicts of the two lead-time cards.
Any signed-in person or agent token reads it.
