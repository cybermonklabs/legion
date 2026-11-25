---
title: Concepts
description: The controller, the architect, the phase workers and the plan that picks which of them an issue runs, admission and the legion label, the design gate, handoffs, review, the architect's acceptance, the merge, the close when the issue's checks pass, and the production check your project's production_check setting opens after the merge.
sidebar:
  order: 2
---

Legion has a few moving parts. This page names each one and says what it does, so the rest of the
section can use the words without stopping to explain them.

## The pieces

- **The Legion daemon** is one process an operator runs with `legion start`. It reads Dispatch's
  issue events and GitHub's pull request, review and CI events, decides which phase each issue is
  in, and starts and stops the agents that do the work. It keeps its own record in Postgres, so a
  restart picks up where it left off. It never writes code; the merge is the merger agent's request
  under the repository's rules.
- **Agents** are [Oh My Pi](https://github.com/cybermonklabs/oh-my-pi) sessions. Under the Kubernetes
  runtime each issue runs in one pod of its own, from one published worker image: every agent of
  the issue runs in that pod, each in Linux namespaces of its own, and talks to the daemon over a
  connection the pod dials back. A phase worker's role may run on Claude Code instead, under either
  runtime (`worker_backends` in `legion.yaml`); it does the same work through the same `legion`
  tool and keeps its conversation the same way.
- **[Dispatch](/legion/dispatch/)** is where people and agents meet: the issue, its spec, the
  questions an agent asks you, the approvals, the messages and the status everyone reads. Legion
  never reads or writes a GitHub issue.
- **GitHub** holds the code: one branch, `legion/<KEY>`, and one pull request per issue, in the
  issue's repository. Each issue targets one repository, the one Dispatch resolves for it (its
  own, else its nearest ancestor's in its own project, else the project's default repository,
  which is set in Dispatch), which Legion records, with that repository's default branch as GitHub
  names it, when it first records the issue, and keeps. This release's one-time pass is the one
  exception: it writes a configured rename's new name (`repository_renames`) over the old one on
  the records.
  Legion acts there through two GitHub Apps, so the account that reviews a change is never the one
  that wrote it. The daemon creates a root's branch at the recorded default branch (`main`,
  `develop` or `release/v7.1` alike) when it admits the root, before its architect starts, and a
  child's when the child starts its first step in its tree, so no agent's push is the one that
  creates a branch: GitHub can refuse that push on a large repository. An issue's workspace with
  no branch to start from starts at that default branch too. A pull request or push names its
  repository, and counts for an issue only in the issue's own.

## Several repositories

One Legion deployment can work issues across several GitHub repositories, under several owners,
with exactly one repository per issue — never two.
A change that spans repositories uses child issues, each in one repository.
Several children may share a repository, each child's own `--repo` fixed when the daemon records it.
An issue may name any `owner/name`, and the account that installs a role's GitHub App decides what that role reaches.
Every worker holds a gh bundle of its role's GitHub App, with one token for each owner that App
is installed on.
Each token is minted on that owner's installation with the role's own permissions: the implement
App's write set for the implementer and the merger, the review App's review set for every other
role.
The bundle holds one directory per owner the App is installed on, one more per owner a
configured rename moved a repository away from while the project serves the name it moved to, and
one for the issue's recorded owner.
An issue pod shows its roles the directories that both Apps' bundles hold.
No directory holds a read-only token, so a role writes in every repository its App reaches,
whichever owner holds it.
The repository an issue names decides only which directory `GH_CONFIG_DIR` names: its owner's,
or, for a repository a configured rename moved, the directory of the owner it moved to.
The controller's bundle is the one bundle of read-only tokens.
Each token reaches every repository its own installation holds.
`legion gh`, the `gh` wrapper every pane and pod runs, reads the directory of the owner of the
repository a command names, and `git` reaches the same bundle through a credential helper that
picks a directory the same way, so a command naming an owner the App is not installed on is
refused there, not merely discouraged. The one exception is a `gh` read the wrapper knows (a
command such as `pr list` typed by its full name with each value its own word, a `gh api` GET,
or a `gh api graphql -f query=…` in the one form
[the deployment guide](https://github.com/cybermonklabs/legion/blob/main/docs/deployment.md#what-a-panes-gh-git-and-jj-are)
lists, whose query holds neither the word `mutation` nor `subscription`, in any case) that names
only such owners, names no repository a configured
rename moves, and reads no `github.com` remote of the working directory: it runs with
`GH_CONFIG_DIR`'s token, which GitHub lets read public data and that installation's own
repositories alone.

## The controller

The controller is the one Legion agent a person talks to directly. By default (`controller:
operator`) the operator starts it with `legion controller start` in a terminal on their own machine,
and it runs there as an interactive Oh My Pi session; closing that terminal leaves the project
without a controller until someone runs the command again. Under `controller: daemon` (Kubernetes
only) the daemon launches it as a pod in the cluster and relaunches it when it dies; a person
reaches it through Dispatch. The daemon wakes it when something needs a judgment no tree owns:

- a `triage` issue labelled `legion` by an agent session, to admit, park or leave to you (a
  person's label is queued by the daemon: the issue moves to `todo` without the controller);
- a tree its own architect escalated, or one whose architect ran out of launches.

The daemon also wakes it when an admission slot frees and on a timer, every
`controller_wake_interval_seconds` (an hour by default), and on its first turn of each UTC day the
controller posts a report on what Legion finished, closed and is running. That same timer runs the
daemon's status repair, which puts back the Dispatch status of any issue Legion works whose status
is not the status of the phase it stands in, so a longer interval slows that repair as well. A free
slot is never a reason for the controller to take an issue: it takes only what a person hands over
and the clear defects Legion's agents report.

No Legion agent files an issue for a problem it finds outside its own issue. A defect with a clear
fix in code the agent's own tree touches or depends on is fixed in that tree's pull request, while
the agent is there. Any other goes to the controller with its evidence, and the controller gives it
to the running tree whose code it is. Only when no tree owns that code does it file the issue with
the `legion` label, so Legion works it without asking you. A problem that needs a decision of
yours goes into a Dispatch ask on
Legion's daily report issue instead, naming the issue it came from and where the fix belongs (the
repository or layer, and saying so when it is not Legion), with its evidence and a recommendation,
and you decide whether it needs an issue. The controller files that issue, with the `legion` label
only when your answer asks Legion to work it. An architect also files the children of its own
issue.

The only statuses the controller sets are `todo`, `backlog` and `icebox` (`triage` is set by
whoever files an issue). It reads pull requests as the review App, and never touches a tree's work.

## Trees and the architect

Every issue Legion admits becomes the root of a **tree**, and an **architect** owns it from
admission to close. The architect:

1. claims the issue in Dispatch, so no other session works it alongside Legion;
2. extends your issue's spec in place, writing each open question as a decision block for you to
   answer;
3. asks you to approve the spec (the [design gate](#the-design-gate));
4. decides how many pull requests deliver the work best, files any child issues that takes, and releases them with their dependencies;
5. decides what to do when a phase gets stuck, a review round cap is reached, or CI keeps failing;
6. accepts the issue once the step before its acceptance passes (in the default steps, the reviewer's approval), reading the evidence of the steps that ran, or sends it back. After the merge, the issue closes when its checks pass ([the close](#the-close)). An issue whose [plan](#the-plan-of-an-issue) has no acceptance step goes on to retro without one.

The architect writes no code. A child issue runs the steps its own plan names, as its root does,
with a pull request of its own, inside the same tree.

Size pull requests to deliver the work most efficiently and reliably, with no preference on their number.
Each round tests a large pull request whole again; one failure holds all the work, and the request merges late.
It can also conflict with the default branch.
Many small pull requests each cost a workspace, an admission wait, review, CI, a queue wait and a deploy.
They can conflict with each other, and dependent slices run in sequence.
A child may wait on a sibling's merge in the same repository: release it with `--blocked-by` that sibling, and Legion holds it until the sibling is `done`.

**Each child gets an architect of its own.** When the daemon starts a child's first phase, it also
starts an architect on that child; no agent starts one. That architect owns the child to its close
as the root's architect owns the root: it files the child's own children under the child, releases
them, decides when a phase of the child gets stuck, and accepts the child. It registers no design
gate and asks you nothing. The architect above it reviews each version of the child's spec, and a
question only you can answer goes up to the root's architect, which asks it on the root. Until the
child's first phase starts, the architect above it keeps the child.

**The children do the work first.** An issue's first phase does not start while Dispatch shows an
open child under it, a child the architect has filed and not released yet included; the architect
is told so once, with a notice naming each open child and the Dispatch status that holds it. A
parent's own first phase can start once each of them has left the workflow or merged and stands in
`verifying`, and its pull request then builds on a base that already carries their merges. For the
same reason the daemon refuses a parent's acceptance while an issue beneath it is in the workflow
and not in `verifying`. An issue's close waits for every issue beneath it in the workflow, one in
`verifying` included ([the close](#the-close)). A child's own architect starts with the child's
first phase, so the children it files run beside the child's own work. Where its task has an
acceptance step, the child is accepted only after its last round, on a head that the step before
its acceptance passed and at which its checks pass.

## Phase workers

The daemon starts each phase's worker itself, one phase at a time, and runs only the phases of the
steps the issue's [plan](#the-plan-of-an-issue) names, in the order the plan names them; the table
below lists them in the order of the default steps. A role keeps
its session between its assignments until its issue leaves the workflow, so its next task reaches
the same session. Under the Kubernetes runtime the pod is shorter-lived than the session: the
daemon releases an issue's pod once no role of it has a turn in progress or a turn due for ten
minutes and no working copy or bookmark of its workspace holds a commit that origin does not hold,
nor the working copy a change, and the next wake for one of its roles — a
task, a notice, a message to the role or to its session, or a CI settlement on the pull request's
head — brings the pod back and resumes the same sessions where they stopped. A commit that no
working copy and no bookmark reaches, such as a side head, is not kept.

| Phase | Worker | What it does | GitHub identity |
| --- | --- | --- | --- |
| `awaiting_person` | none | The root keeps its admission slot while its live architect waits for and reads a person's answer to a question on the root. | none |
| `planning` | planner | Writes the implementation plan into the issue's `plan.md` document. | review App |
| `implementing` | implementer | Writes the change, proves it works, pushes `legion/<KEY>` and opens the pull request. | implement App |
| `testing` | tester | Runs the changed surface for real, tries to break it, judges whether the tests would catch what breaks, and reads CI for everything static; reports `pass` or `fail`. | review App |
| `reviewing` | reviewer | Reviews the pull request and submits an approval of the head, or changes requested. | review App |
| `accepting` | the architect | Where the issue's plan has the step `acceptance:owner`: reads each check of the issue's Verification document with its latest result, the tester's handoff and the reviewer's approval of the head where those steps come before the acceptance, and accepts the issue (on to the task's next step, or to retro where the acceptance is its last step) or sends it back to an earlier step of its task. The daemon refuses the acceptance while an issue beneath it is still in the workflow and not in `verifying`. No worker holds this phase; the issue's Dispatch status stays `needs_review`. | review App |
| `retro` | implementer | Writes down what the work taught, as one comment on the issue, and brings up to date whatever the repository's instructions require the pull request description to say about the paths it changes. It commits nothing. | implement App |
| `merging` | merger | Reads the pull request and merges it itself, at the head it read and under a message it writes, which GitHub completes under the base branch's own rules. The merge closes nothing by itself: once the daemon reads it, the issue moves to `verifying`. | implement App |
| `verifying` | none | The issue's pull request has merged, and its close waits ([the close](#the-close)): for each check of its Verification document to pass, for its production check, for each issue beneath it, for a defect or for a next change. It holds no admission slot, and its Dispatch status is `testing`. | none |

A failed test or a review that requests changes sends the issue back to `implementing` from
testing or review, and the change goes through the issue's steps after the implementer again: in
the default steps, the tester before the reviewer sees it. A head that starts conflicting with its
base moves no phase (below). Red CI sends nothing
back from anywhere: the review round and CI are two rules, each read on its own.
The round reads no check's conclusion, since the reviewer reads the code commit's checks on GitHub
before it approves ([review signalling](#review-signalling)), but an approved head that GitHub
holds blocked, on a check or a review the base branch requires, keeps the issue in `reviewing`
until that merge state comes clean. What a red check does is reach the architect that owns
the issue, the worker of the issue's current phase and the implementer, as one message naming each
check and its conclusion; and hold the merge, where GitHub applies the base branch's own required
checks.

### The plan of an issue

The table is the line of the **default steps**. Each architect of a tree, the root's or a
sub-architect's, is an owner of each issue that stands in that tree now, and can give an issue a
**plan** that picks which of these steps it runs and in what order: one task whose steps are
some of `planner implementer tester reviewer acceptance:owner`, in the order the owner names them,
with `implementer` among them and before each `tester`, `reviewer` and `acceptance:owner` step,
since each of those reads the pull request the implementer opens,
where `acceptance:owner` is the architect's own acceptance in `accepting`. It writes
the plan as the issue's `plan.md` document, with one `plan_task` block such as
`:::plan_task{#a steps="implementer reviewer"}` whose text says what the task must deliver, and
gives that version to Legion with the `legion` tool's `register_plan` before the issue's work
starts: for a child, before `release_children` releases it, since a released child can start its
first step in the same admission pass. Legion holds a plan given for a child it has not recorded
yet, and the child's first step takes the newest plan its owner gave. Legion runs only those steps,
in the order the plan names them, then the retro and the merge, so an issue with that plan runs no
planner and no tester. Legion refuses any other plan with `PLAN_REFUSED` and one
reason for each cause. An issue whose owner gives it no plan, or a task with no steps, runs the
project's default steps, `projects.<KEY>.default_steps` in `legion.yaml`, which are
`planner implementer tester reviewer acceptance:owner` unless the project sets others; so does an
issue whose work started before its owner gave it a plan, until the owner stops that task with
`stop_task` and gives a plan whose one task replaces it.
Set `default_steps` only once every daemon that reads the file runs a release that knows it: an
earlier daemon refuses the whole file (`Unknown key "projects.<KEY>.default_steps"`).
An issue that leaves its tree, as an orphan admitted again as a root of its own does, takes no plan
its earlier owner gave: at its first step Legion deletes that plan, logs
`workflow: a plan of an earlier owner is not taken`, and tells the issue's architect once, in a
`plan-not-taken` notice, which version it did not take and which plan the step runs instead, the
new owner's or the default steps.
An issue that a project's experiment drew into a group whose issues run the project's default
steps (an arm's `plan: default`) takes no plan from its owner: once the daemon records it, the
owner's `register_plan` and `stop_task` are answered `409 PLAN_NOT_TAKEN`, naming the group, and
a plan its owner or an earlier owner gave before is deleted at its first step, logged for each
version as `workflow: a plan is not taken: the issue's group runs the project's default steps`,
never as an earlier owner's line above, and told in the same `plan-not-taken` notice, which names
each version and the group and offers no `register_plan`.

A step a plan leaves out hands its duty on. With no tester step, the implementer's own results
against the issue's checks are the evidence, and a reviewer step refuses to approve while any of
them has no pass at the head. With no reviewer step, nothing approves the head, so on a base that
requires an approving review the merger waits and tells the architect which review is missing.
The implementer then writes the pull request body's `Look at first` and `Not proven / risk` lines
at each round, and before each completion, its retro's included, answers and resolves each bot
review thread still open, naming each one it declined to the architect. With no acceptance step,
the last step passing starts the retro; a parent that gets there while an issue beneath it is
still in the workflow is held before its retro until the last one leaves.
`dispatch steps --issue <KEY>` prints each run of each step of an issue.

## Admission and the `legion` label

Legion works only the issues handed to it with the Dispatch label **`legion`** (in any case), so it
can share a Dispatch project with people and other agents. A root issue is admitted when it carries
the label, is in `todo`, and a slot is free. A slot is one issue the daemon has started, a root or a
child, so a tree of four started issues holds four of them. The number of slots is `admission_cap`
in the daemon's configuration, four by default; an issue that finds no free slot waits in line, and
a labelled `todo` root takes its place in that line by its Dispatch priority and then in the
project board's order.

An issue takes a slot when the daemon starts it — a root at its admission, which starts its
architect, a child at the start of its first phase. An issue holds one of the cap's slots while it
stands in a phase that keeps one, from the moment the daemon starts it until it is done or Dispatch
takes it out of the workflow, except while it waits on another issue there.
`accepting`, `retro` and `merging` hold none (in the default steps, the phases after the review),
and a step after `accepting` whose phase keeps a slot waits for one again.
So an issue holds its slot through the other waits in a phase that keeps one: on a person, and on a
review. In `accepting`, `retro` and `merging` no phase worker writes the change, so the acceptance,
the retro and the merge run on no slot at all, and an issue there that waits on another issue has
no slot to give back.

A wait on another issue in a phase that keeps a slot is the exception: a started issue that Dispatch shows
blocked by an issue that is not `done`, or whose own first phase waits on an open child under
it, holds no admission slot.
The daemon takes its slot back and stands it in the
waiting line within its priority, among the issues whose work came back, oldest first.
Once the daemon has seen the wait end, the issue takes a free slot by itself, after every issue
of a higher priority and ahead of every issue of its own priority that has not started; under a
full cap it waits for a free slot.
The daemon sees a blocker in the same project close as a live event.
It sees a blocker in another project close only at its periodic turn's re-read:
the first turn comes about a minute after the daemon starts,
then one every `controller_wake_interval_seconds`, an hour by default.
Three started issues keep their slot although a wait
names them: a root in `awaiting_person`; a root at `admitted` whose design gate has never
opened, whose architect is writing the design; and a root that arrived from another tree
carrying the row it held there, which the line starts whatever its links say.
Such a wait is a wait by design, in any project, when nothing in the issue's own tree moves it:
a `blocked_by` link to an issue that is not `done`, in another project or in this one, run by a
Legion tree or by none, or an open child the daemon has released into the tree and runs.
A wait on an open child the architect filed and has not released is not one, because releasing
that child is the tree's own work.

An issue holding a slot starts its next phase whatever the cap says, and a start into `accepting`,
`retro` or `merging`, which hold none, runs at once. An issue that holds none and is owed a start
into a phase that keeps one, a child an architect has just released or a step after `accepting`
among them, waits in line while the project stands at the cap.
A slot stops
nothing either way: every worker of a waiting issue stays live in the issue's pod and answers
questions until the issue closes, and giving a slot back for a wait stops nothing either — no
claim is suspended, no session ends, no phase moves, and no Dispatch status is written.
The worker at work goes on, but every start of a phase worker of the issue into a phase that keeps
a slot, its next phase's or a restart of its current one, waits until the issue holds a slot again;
a start of `retro` or `merging` takes no slot and runs at once.
`legion state` names such an issue under `admission.waiting` and under `admission.blocked` and
never under `admission.active`, which is how you tell it from an issue a person parked, which is
in none of the three.

The line is ordered by Dispatch priority first, and `legion state` prints it in the order the
daemon promotes it: the issues at P0, then at P1, P2 and P3, and then the issues with no priority.
An issue waits at its own priority, else at its tree root's, and after P3 when neither has one.
Within one priority the line has two parts: the issues whose work came back, oldest first by the
moment it came back, and then the issues that have not started, by the Dispatch rank of each
issue's tree root and then its own. So an issue whose work came back goes ahead of every issue of
its priority that has not started, and never ahead of an issue of a higher priority. A re-rank on
the board moves an issue inside its priority's second part, and never ahead of an issue of its
priority whose work came back; a change of the issue's priority in Dispatch moves it to the
place of its new priority at the daemon's next admission pass. Priority orders the line and
nothing else: it takes no slot back from work that runs, an issue that still waits on another
issue is placed by its priority and passed over while that wait lasts, and an issue that already
holds its slot is placed by its priority and resumes whatever the cap says. The members of a
re-admitted tree stay out of the line until their root's promotion, whatever their own priority.
Every issue the workflow records carries `priority` in `legion state --json`, its own Dispatch
priority: 0 for P0 to 3 for P3, or null for none. Each waiting issue carries `waitingLine` there:
its `place` and the line's `length`, its `priority`, the one that placed it (its own, else its
tree root's, or null for an issue that waits after P3), its `part` (`returned` or `starting`) and
`kind`, its `partPlace` and `partLength` within its priority and part, `holdsSlot` where it
already holds its admission slot and so resumes whatever the cap says, the key that placed it
within its part — `since`, the moment the work came back, or `rootRank`, `ownRank` and `rootKey`
for an issue that has not started — and `reason`, the daemon's own sentence for that place.
Every issue that is not working now carries
`why` beside that: a `state`, one of `claim_stopped` (a claim nothing will wake), `waiting`,
`blocked` and `unlisted` (the record holds it in the workflow and no list places it), with a
`reason` sentence. An issue holding an admission slot carries no `why` unless it
also stands in `admission.waiting` or `admission.blocked`, or a stranded claim names it: an
answered root reads `waiting`, and an issue at work whose worker runs, one `held` and a root whose ask a person has
not answered yet, which `waitingOn` answers for, read nothing. Neither does an issue the record
does not hold in the workflow. So the daemon says why an issue stands where it does, and nobody has to
work it out from the order.

An issue gets the label in one of three ways:

- **A person hands it over**, from the issue header in the Dispatch dashboard.
- **A person asks the controller to**, in a message, a Slack request, or the answer to one of its
  asks, and the controller sets the label for them.
- **A Legion agent reports a clear defect** no running tree owns, one whose fix needs no choice of
  yours, and the controller files it with the label.

A free slot stays free until an issue gets the label one of these ways: the controller never takes
one on its own.

A child issue needs no label: it runs under its tree once the root is admitted. Taking the label
off a waiting root takes it out of line; taking it off a running tree does not stop the tree.

A root whose architect asked you a question and now waits only on your answer keeps its slot while
it waits: once nothing else of its tree runs and the architect has taken the task it was last
given, the root moves to `awaiting_person`, holding the row it took at its admission, so the
project admits its next root only as a slot frees elsewhere. Its architect keeps its pod, reading
your comments and replies as they arrive.
Its tree stays live for its children, and `waitingOn` in `legion state` names the question and when the
wait began. Your act on any ask of the root answers it, whichever ask the wait names: your answer,
resolution, or reply that hands an open ask back to the agent; so does your approval or changes
request of the root's spec, and so does the spec's design gate standing open. The wait also ends
when nothing is asked of you any more: Dispatch retracts the question because its decision block
left the spec, a session closes it, or a session hands it back to the agent's turn. The answered
root goes into the waiting line within its priority, among the issues whose work came back, oldest
first, and so behind every issue of a higher priority and every issue of its own priority whose
work came back before it, a root's own first phase
included, and its promotion there gives the same architect session a task naming what answered it
and what is still with you. That promotion takes no further slot, since the root has held one all
along, so it resumes however full the cap is, whether or not its design gate has opened. Until the
pass that reads your answer resumes it, the root stands in the admission line as well, where
`waitingLine` on the issue gives its place and its length, its `priority`, its `kind` `answered`,
its `holdsSlot` true, and a `reason` that says a person answered it, when its work came back, the
priority it waits at and that it holds its slot already. The daemon sends
its architect nothing about that wait and resumes the root by itself on that pass,
so neither the place nor the wait is anything for you to act on. An
agent's answer or resolution of another ask answers nothing, and an act of yours from before the
wait began is one the architect already read. Taking the label off, or a person or an outside session
claiming the root on Dispatch, takes it out of the workflow instead: Legion never resumes work
another owner took. A root whose architect requested approval of its spec and registered that version waits the same way, on the design gate, until the person reviews the spec or a newer version hands the request back to the architect. A root's
wait on you is no reason for the controller to label or admit anything: a root you
handed to Legion takes its place through the waiting line.

A parked root waits on nobody. When you approve its spec, the daemon sets it to `todo` itself,
whoever parked it, when the root keeps its `legion` label and has no parent. An approval on a root
that has a parent, and one that Dispatch carries in your name from an earlier one, do not queue it.
Such an approval, a request for changes, and an answer or a reply on one of its asks go to the
controller, which reads the decision. If the controller parked the root for a hold of its own, which
it marks with a comment that begins `Parked by Legion while it waits on:`, and the decision gives
the work a next step, the controller sets the root back to `todo`, and the architect starts by
reading that decision. The controller's walk also sets such a root back to `todo`, with a comment
that cites the mark, once the hold's cause is gone or the event it waits on has happened. Every
other park, a root you parked yourself, one another agent's session parked, or one an operator
parked with `legion issue status`, stays parked until a person sets the root to `todo`, approves its
spec as above, or asks the controller to start it. The controller says so in a comment, and the
architect then starts with the decision. When the approved version is no longer the spec's current
one, the architect asks for approval of the current one. A question alone leaves the root parked:
the controller answers it on its ask from the issue's record, or says that setting the root to
`todo` brings the architect back to answer. A reply an agent session writes on such an ask is no
decision: it stays on the ask for the person who asked, and nothing wakes.

A held root is not a parked one. Its work stopped because a worker's budget ran out
([Supervision](#supervision)), and it is still in the workflow. Setting it to `todo` releases the
hold: the issue returns to the phase it was held from and that phase's worker starts again, with
no new run of the tree. A root you parked, by contrast, starts a new run when you set it to
`todo`.

## The design gate

By default (`gates.design: [spec]`), no phase starts until a person approves the root issue's
spec. The architect settles the spec's decision blocks with you first, then asks for approval with a
summary of what the tree will do. Where the tree builds a page you open, or changes one a lot, the
architect shows you that page first — pictures, or a page you can click — and gets your answer
before it asks you to approve the spec; a small change, such as a label or a style fix, usually
carries no pictures, unless the architect is unsure what you want. You approve from the document's
header or from the approval question in your Dispatch Inbox; **Request changes** sends it back with
your reason.

Approval is pinned to a version: a later edit to the spec closes the gate again until someone
approves the new version. Your own answer to a decision block in the approved spec is the one
exception: when that answer is the whole change since the version you approved, Dispatch records
your approval at the new version and the gate reopens, so the architect asks you for nothing; when
the answer means the spec's text changes too, the architect rewrites it and asks you to approve
again, naming what changed since your approval. Work already in flight carries on while the gate
is closed, and of that work only the merge waits until you approve the new version. When you
approve it, each worker that holds an affected issue gets a task that names the sections and
checks that changed, and decides whether its work still matches. A worker whose work is stale
moves its issue back, and a planner revises its plan instead and runs the plan review again. The
daemon moves no issue and stops no turn itself, and an issue whose pull request has merged never
moves back. The daemon only reads approvals; nothing in Legion approves a spec on a person's
behalf, and the approval Dispatch records at your answer is yours, in your name.

`gates.design` is a list, so a deployment can ask for more than one document of the root issue:
`decisions` is the issue's decisions document, `criteria` its Verification document, and `spec`
its primary document. The daemon keeps one gate record for each step listed, and the tree starts
only once a person has approved every one of them; each closes on its own document's next
version. Under a list that names `spec` and not `criteria`, the default among them, the architect
can also register the root's Verification document as `criteria`: that tree then waits for both
documents, and one approval of a request made with `--include-verification` opens both. A
deployment that does not want the gate sets `gates.design: []`, and the tree starts as soon as the
architect has written its spec.

## Handoffs on the Dispatch issue

Each phase ends by writing a **handoff**, a small JSON record on its Dispatch issue, kept under the
phase's word: `plan`, `implement`, `test` and `review`. The worker writes it with
`dispatch handoff-write` under its own session, and every write adds a version; the next phase's
task names the version the daemon accepted, which it reads with
`dispatch handoff-read --version <n>`. A handoff is the copy that survives: when a worker
comes back after a restart, or a memory disagrees with a record, the record wins. The issue's
Conversation in Dispatch lists every version, and opens each to its fields. The merger writes
none, and nor does the architect.

A handoff's body is `head`, the commit the phase ended at, and, where the role has one, a `note`
of at most 1,000 characters that tells the next role how to run the issue's checks in the worker
pod or an order of work it must keep. The tester's body also keeps `implementerProof`, `failures`
and `failed`. A worker writes its handoff once, when it completes its phase. What other records
hold stays out of it: the diff, the pull request body, each check's result, the GitHub review,
and `plan.md` with its plan checks and its departures from the spec.

A worker's completion names its record, and the daemon reads that record back from Dispatch before
it moves the issue: it refuses a record another session wrote, one of another issue, phase or
version, and the record the role named for its previous phase. No handoff is a commit, so the
issue branch carries only work, every push gets its own CI, and the merge brings no handoff onto
the default branch. Retro commits nothing, so nothing of retro's sits above the head the issue's
last step passed — the head the reviewer approved, where its plan has a review step — and, outside
a forward merge of the base, nothing in Legion compares the head that merges with it: a repository
that wants a later push reviewed says so in its own rulesets, and GitHub holds the merge to them.

## Review signalling

Legion uses GitHub's own review mechanisms rather than labels:

- The **reviewer** submits a GitHub review: an approval of the head by its commit, or changes
  requested. A plain comment decides nothing, and the architect is told the round is stuck. The
  daemon hears of a review from GitHub's webhook, and it also reads the pull request's reviews
  with the implement App at its own start and after every handoff of the issue, so a review whose
  webhook never arrived still counts at the next of those reads. Either way the newest deciding
  review counts, and counts once: a review
  the daemon already applied, by webhook or by read, decides nothing again. A review submitted while
  no round is open (the work is being implemented or tested) decides no later round when the daemon
  reads it before the next round opens. One it first hears of after that round opened can still
  decide it, as a webhook delivered late can. The reviewer names the head it read in its review's
  Legion footer, and its review decides the round only when that head is the pull request's current
  head: GitHub rewrites a review's `commit_id` to the current head, so the footer is the review's
  own word for the code it read, and a person's review, which carries no footer, decides by its
  webhook alone. An approval counts for the head it names and no
  other, since any push may change code, and a push on its way from that head leaves the approval
  undecided until the new head arrives; anything else sets it aside, and only an approval of the
  head itself or a request for changes ends the round. An approval does not end it on its own:
  the round ends once the record reaches the approved head and GitHub reports that head one the
  issue may advance on. A head GitHub holds blocked, on a check or a review the repository
  requires, leaves the issue in `reviewing` until that merge state comes clean.
- **CI** on the pull request's head is information, not a verdict. When GitHub's checks settle, the
  daemon names every check GitHub reported, with the conclusion GitHub gave it, and judges none of
  them: it sends one message each to the architect that owns the issue, the worker of the issue's
  current phase and, before `merging`, the implementer whose code the checks ran on, and moves the
  issue nowhere; in `merging` the merge and what breaks it are the merger's. The
  message names the head's commit statuses too, each with its state: Dispatch reads them from
  GitHub for each open head where its GitHub App holds Commit statuses, since GitHub's status event
  names no pull request. A red is for those roles to decide on — re-run a check that failed on
  itself, fix code that is at fault, or wait — and for GitHub to hold the merge over, under the base
  branch's own required checks, which Legion reads only to decide whether a conflicting head needs
  its merge forward now (below), and never re-implements. The reviewer reads the
  head's checks on GitHub itself before it approves, and the architect before it accepts; those
  reads are the ones that decide anything. Two settlements of the same head that name the same
  checks tell nobody twice, and a lingering tree is told nothing at all.
- **A head that conflicts with its base** moves no phase. GitHub computes no merge ref for such a
  head and runs no check on it. The daemon reads the pull request's mergeability twice, at least a
  minute apart, since a read made while GitHub is still computing can be stale. It acts only on a
  conflict both reads report, and only when the next step needs a mergeable head. That is so in
  four cases. The issue stands in `merging`. A check the base requires has no green result on the
  head. The base requires a pull request to be current with it. The daemon could not read the
  base's rules, which it reads (rulesets and branch protection) only on a conflicting read.
  Otherwise it records the conflict, and the task's steps go on at that head; an approval of the
  head ends the review round. Where every step of the task has run, in `retro` or in `accepting`
  with no step of its task after the acceptance, the merge comes next, so the conflict waits there
  whatever the base requires, and the daemon acts once the issue enters `merging`.
  When it acts, the issue keeps its phase and takes no admission slot for the merge. Before
  `merging` the implementer is asked on its role topic to merge the base forward in the issue
  workspace and push, with no handoff and no completion for it, and the tester or the reviewer
  holding the phase is told to stop writing the workspace and not to complete its phase at the old
  head, and keeps its turn, its background jobs, its process and its session. In `merging` the
  merger is asked: the merge is its own, and it resolves the conflict itself. The architect is told
  once with a `checks-red` notice naming the phase the issue stays in and the role asked. In
  `implementing` only the implementer is told.
  When the merge lands, the phase's worker is told both heads and goes on at the new one, and a
  completion of the phase the issue stands in that names the replaced head, or that names no head,
  is refused (`HANDOFF_HEAD_MERGED_FORWARD`) and is made again at the new head; a completion of a
  phase the issue has already left is recorded as before. Where the task's reviewer step already
  ran, the daemon asks the reviewer to read the merge (`git show --remerge-diff`) and approve the
  new head by its SHA, or request changes. A reviewer step still to come reviews the new head when
  it runs. A merger that enters `merging` while that approval is owed, or that made the merge,
  merges the new head only once the approval is on the pull request. A task with no reviewer step
  has no review to ask, and its merger goes on at the new head.
- **A completion behind its phase entry.** A move the role's completion did not make — a
  reviewer's own request for changes, which ends the round before the reviewer completes — keeps that role's
  turn, its background jobs, its process and its session. Only the entry of the phase the issue
  moves to is raised, so a completion the role sends for the phase it lost
  still names the entry that phase gave it and is refused `HANDOFF_NOT_CURRENT_PHASE`;
  `HANDOFF_STALE_ENTRY` comes once the issue has handed that role the phase again, and only
  while the issue stands in that phase. A completion of another entry than the role's phases row
  names, from a role whose phase the issue is no longer in, is answered
  `HANDOFF_NOT_CURRENT_PHASE` as well, before anything is recorded: the task of that entry was
  queued for the phase the issue has left, so the daemon drops it unsent and owes the claim none.
  One completion behind its entry goes on all the same: a verdict on the head the record holds,
  of a phase that ends with the role's own handoff record. The phase was entered again at that
  head, so the work the new entry's task asks for is the work that completion reports; taking it
  moves the issue on, so the same completion sent again, or one naming a newer record, is
  answered `HANDOFF_NOT_CURRENT_PHASE`. A completion at another commit is refused; so is one that
  names no commit, and so is any while the record holds no pull request for the issue. A
  completion is refused whatever head it names when the entry the phase stands on was raised by a
  move back, which asks for that phase's work to be done again.
- **An implementer that completes with no pull request** is refused (`HANDOFF_NO_PULL_REQUEST`)
  until one is open on its branch, and the daemon's read of that issue tells its architect
  `pull-request-missing`, once per handoff commit, so an issue never waits in `implementing`
  unnoticed.
- **Review threads** close one by one, and only once the thread's opener (or, for a thread a bot
  opened, Legion's reviewer, or the implementer on a task with no reviewer step) accepts the reply.
- One limit flags a loop: after `review_round_cap` counted rounds (three by default) the daemon
  posts a message on the issue and tells the architect; it stops no worker.
  It says so once for each value the cap holds, not on every round past it: a cap raised in
  `legion.yaml` tells again at its new number, a lowered one tells once on the next counted round,
  and a new generation tells again from its own first crossing.
  At the round cap the architect decides, in the turn the notice wakes, whether to narrow the next
  rounds, split work
  off (into a sibling child, or, on a root, an ask whether the rest needs an issue of its own), or
  let converging rounds run, and posts that decision on the issue. Only a review that requests
  changes counts a round: a tester's fail and a move back by a worker or the architect count none.
  A request for changes made while the record holds the head conflicting with its base is a
  conflict round, which `review_round_cap` does not count. The implementer answers such a round
  with a forward merge, which no reviewer asked for. The cap's message names both numbers, the
  rounds counted and the conflict rounds left out.
- Each move back is counted by its kind, as the pair `<from>-<to>` of the phase it leaves
  and the phase it goes to — `testing-implementing`, `reviewing-implementing`, `merging-implementing`,
  or any other of the 28 moves back a task's line can make, such as `testing-reviewing` on a
  task whose reviewer step comes before its tester step —
  beside the round count, and a move made while the head conflicts with its base raises no pair.
  `GET /legion/v1/state` serves the counts as `workers.implementer.backwardMoves`, left out for an
  issue that has had no move back.
- **Hooks** put a deployment's own words in front of the workers on a move back. A hook is an
  entry of `hooks:` in [legion.yaml](/legion/legion/running-legion/#legionyaml): the moves it
  selects, how many of them fire it, the roles it writes to, and the words. A move back the hook
  selects fires it when the counts of the moves it selects, added up, are then a whole multiple
  of that number; a move back made while the head conflicts with its base fires none. The daemon
  writes the words to each role the hook names, between its own
  header — the hook and the move — and its own counts of the moves back, and posts
  `Legion hook <name> fired at <N> moves back: <from> to <to>. Prompted: <role>, <role>.` on the
  issue. It gates nothing: the phase the move opened starts its worker whether a hook fired or
  not, and a role that reads the words answers in its own turn. A deployment that writes no
  `hooks:` key runs one shipped hook, which prompts the implementer, the tester and the reviewer
  every fifth move back of any kind to say what is in their way.

## The merge

When the issue's steps have passed — in the default steps, the reviewer has approved and the
architect has accepted — and retro is done, the merger reads the
pull request and merges it itself, as the implement App and at the head it read, which it names as
the head GitHub must match (`gh pr merge <n> --squash --match-head-commit <head>`). On a base with
a merge queue it enqueues the pull request instead, with GraphQL `enqueuePullRequest` and the same
head as `expectedHeadOid`, and watches the pull request until GitHub merges it or takes it out of
the queue. Merging at a named head is how bors and Prow's Tide merge only what they tested: a push
that lands under the merger is refused instead of merged unseen.

The merge carries a message the merger writes itself,
`--subject '<pull request title> (#<n>)' --body ''`. GitHub's own default
(`squash_merge_commit_message` of `COMMIT_MESSAGES`) joins every commit message of the branch into
the squash commit, which would put the tree's Dispatch keys and its `Omp-Session:` and
`Legion-Role:` trailers on your default branch. `enqueuePullRequest` can carry no message, so on a
merge-queue base whose repository setting still joins them the merger enqueues nothing and tells
the architect: only a person can change that setting, to `PR_BODY` or `BLANK`.

The merger never turns auto-merge on, and the daemon submits no merge and changes no merge setting
of any pull request. The repository's own required reviews and checks decide when it lands; Legion
reads none of them at the merge, bypasses none, and waits for a review only a person can give. On
an issue whose plan has no reviewer step and a base that requires an approving review, the merger
also tells the architect which review is missing. Nothing holds the head that merges to the head a
reviewer approved: a repository that wants its newest push reviewed says so in its own rulesets.
The one exception is the merger's own merge of the base, below, whose new head it merges only once
the reviewer approved it, where the task has a reviewer step.

When a merge fails, the decision is the merger's, on GitHub's own answer: wait for what GitHub
waits for, merge again at the head it re-reads, or tell the architect what no worker can do — a
repository setting or an App permission only a person can change.

A conflict with the base, and a check the base's newer code breaks, are the merger's to resolve,
and the issue stays in `merging`: the work passed every step of its task, and what is left is its
merge. The merger merges the base forward in the issue workspace, resolves the conflict so both
sides keep their intent, adapts the change in that merge commit where the base changed code it
uses, runs again what the merge can reach, and pushes. The daemon asks it when two reads find the
head conflicting, and, where the task has a reviewer step, asks the reviewer to read the merge
(`git show --remerge-diff`) and approve the new head; the merger merges that head once the
approval is on the pull request. On a task with no reviewer step, the merger goes on at the new
head. It may ask the implementer, the tester or the reviewer what they know over Envoy, and it
sends the issue back with `request_backward_move` only when the fix is a change to the change's
own logic that no merge of the base resolves, saying why. A merge queue that takes the pull
request out because its checks failed comes to the merger the same way, as a check that failed.

A merge queue that takes the pull request out for a conflict says which conflict it was in the
pull request's merge state, which GitHub computes once the entry is no longer queued. A head
GitHub still merges conflicted with an entry queued ahead of it rather than with the base, so the
merger enqueues that head once more, says so on the issue, and the issue stays in `merging`. A
head GitHub reports dirty conflicts with the base itself, which the merger merges forward; a merge
state GitHub answers unknown is asked again on the merger's own backoff for up to five minutes and
then read the same way.

The daemon's own reading of the merged pull request, by GitHub's webhook or by its own read,
records the merge, whether the merger's completion arrived before that or after it.
A merge no longer closes the issue by itself: the issue moves to `verifying`, and
[the close](#the-close) says what it waits for.

## The close

After its merge, the issue stands in `verifying`, with the Dispatch status `testing`.
It holds no admission slot, no worker starts for the phase, and the move suspends no role.
It stays there until all of these hold:

- each check of its Verification document passes, as Dispatch computes it at the newest merge;
- its [production check](#the-production-check) of that merge passes, where the project runs one;
- no issue beneath it is in the workflow, and no open child beneath it;
- no defect waits, and no next change is open.

The daemon then closes it and writes `done`.
Where all of that already holds when the merge lands, the issue closes at the first read of its
checks after it.
A merged issue that waits only for an issue beneath it stands in `verifying` too, so a root whose
tree still runs waits there for the rest of its tree.
A child held at `admitted` under its root starts at the root's merge, and the root's close waits
for it.
Each read of the checks also reads the issue's children from Dispatch, so a child that the
architect files holds the close as soon as Dispatch holds it.
A role's word that the issue is done closes nothing: the close reads the checks.

What the close waits for is in the issue's document `close-wait.md`, and in `legion state`'s
`closeWait`.
Legion writes the document again whole at each change of the wait, and posts no message for such
a change.
The document's versions are the history, and its last version says that the issue closed and when.
Once a day, Legion tells the owning architect each term that still holds the close, with its cause,
so that it acts on it.
For example, it has an eligible agent run a check, or a person approve a reworded check.

**The next change.**
A check that Dispatch computes as failed after a merge, and a production check's `fail`, start the
next change of the same issue by themselves.
Legion tells the owning architect at once, and no role reports them.
A defect that a role finds after the task's last step passed starts one too: the role reports it
with the `legion` tool's `report_defect`.
A gap found before that goes back as any gap does, and in `accepting` by the owner's return.
The retro's check gate does not change: a `before_merge` check with no pass at the head is a failed
check, and the work goes back for it.
The next change is a new pull request of the same issue, on the same branch `legion/<KEY>`, which
Legion first moves to the head of the base that the last pull request merged into.
It runs the default steps, or the steps of a plan that its owning architect gives for it before its
first step, and it asks no person to approve the design again.
Its own pull request needs its own pass of each `before_merge` check.
It starts only after the merge of the reviewed work, and after the merger's completion.
A next change never starts while another one is open.
Legion stops no next change for a count: at each multiple of `review_round_cap` next changes, it
tells the owning architect and the controller, and goes on.
The tree's architect keeps its run through a next change of its root: its pending task and a
request for approval in flight stand, and the notice `next-change` tells it of the change.
The next change is no deferral, because the issue stays open and Legion starts the work by itself.

**What changes a check, and who else closes an issue.**
Only a new version of the Verification document that a person approves removes or rewords a check,
and Legion closes nothing over a fail.
Work that is owed after the merge and is no defect is a check of the Verification document whose
`when` is after the merge, which the owning architect adds before its acceptance.
Legion keeps no second list.
The owning architect's `done` on a child whose pull request merged is set back: such a child closes
by its checks.
`close_root` is refused for a root whose pull request merged, which closes by its checks.
Where its architect judges the rest moot, it withdraws each defect with its reason and asks a
person, whose `done` closes the root.
After the close, Legion opens nothing for the issue.
A `fail` recorded later on an issue that Legion closed by its checks sends the controller one
notice, `closed-check-failed`.

## The production check

Where your project asks for it, the close also waits for a production check:
`projects.<KEY>.production_check` in `legion.yaml` takes `off` or `tester` and is `tester` by
default.
The check opens at the first read of the issue's checks after its merge where no issue beneath it
is in the workflow.
The tester is handed it in its own session or, where the issue's plan had no tester step, a fresh
one.
A project that sets `off` opens no check, and its close waits for none.
In such a project no role runs a check after the merge: at the merge, the owning architect is told
each such check, and that it must have an eligible agent run it.
The loader refuses any other value, naming the key and the value it read. The check's record
carries the role, written when the check opened, so changing the setting leaves an open check with
the role it started under, and only that role may report on it.
The look's task names each check of the issue whose `when` is `after_merge` or `default_branch`
and that names no `ci_check`, and the checking role runs each of them at each look.
A check whose `subject` is `code` is recorded at a commit of the merge's branch that holds the
newest merge.
A check whose `subject` is `production` or `graph` is recorded at the commit of the build that the
live system runs, which holds the merge.
A pass of a check after the merge comes from CI, another program, or an agent that made no commit
of the change, never from the implementer: the tester qualifies.
Each look of the check reaches the deploy as your deployment instructions say (the merge commit
published to your deploy role, with no reply awaited), reads whether production carries the merge,
and ends one of three ways:
`pass`, once the agent drove the change in production through a user's own access path, drove each
requirement of the issue only the deploy can show as the requirement names it, and recorded a
result of each check the look's task names, naming what woke the look, with each result's line on
the pull request;
`pending`, when production does not carry the merge yet, or an observation a requirement names has
not happened yet;
or `fail`, a requirement observed as not met, or a defect of the change, naming the issue, the pull
request, the merge commit, what it drove and what failed.
A `fail` starts the next change of the issue ([the close](#the-close)) and goes to no controller.
A defect the look finds outside the issue goes to the controller, which gives it to the tree that
owns that code and files an issue only when none does.
One that needs a decision is a Dispatch ask on Legion's daily report issue, so you decide whether it
needs an issue.
The record is those results and the pull request.
Each look's report names `check_opened_at`, the RFC 3339 opening time its task gives, with the
stored microseconds unchanged. That field alone selects a report on that check: without it,
`handoff_complete` completes a step, never a look, whatever phase the issue is in.

Nothing waits for the deploy, and the agent names its own next look. A `pending` on any look but
the check's last carries `wake_at`, an RFC 3339 time: when the agent wants to look again. The
look ends and the checking role's claim is suspended. The pod goes once no role of the issue has
had a turn for ten minutes, as [Phase workers](#phase-workers) says — and its disk with it, since
an issue's disk belongs to its pod — while the conversation is kept in the session database. At
the time the agent named, a pod comes back and the same session takes the
next look. A time less than 60 seconds ahead becomes 60 seconds ahead, and a time at or after the
check's own bound starts no look of its own, because the look at that bound already stands.
Nothing else wakes an open check: no deploy run, no Dispatch `issue.deployed`, no Dispatch build and
no daemon boot, and the daemon reads no deployment's production workflows at all.
A look is bounded at two hours, and a look nobody reported — its two hours or an operator's
hold — starts the next look two hours later. An agent whose claim fails, in a look or after it
ended, backs the check off: the next look comes two hours later after the first such look, and
twice as long after each failed look in a row after it (four hours, eight, and so on, never past
the check's bound), until a look the agent reports sets it back to two hours. That look tries the
agent once more, with one attempt left of the budget its failure spent, so an agent that cannot
start is launched once a look rather than a whole budget's worth of times; a failure that spent no
budget, a provider error's, starts a fresh run. The architect is told of each such failure, with
its cause, how the agent's last pod ended and when the next look tries it. An operator's
`legion claims retry` or the controller's fresh session gives the agent its whole budget again.
A look that began before
a pause of the project ended counts its two hours from the resume, or from its agent's last ready
after the resume where that is later, and counts nothing while that agent is still starting.
The whole check is bounded at `production_check_hours` (336, fourteen days, by default), counted
from its open: the look at that bound is the last.
On the last look, a requirement still unobserved is `pending`, which closes the check as not run,
and the agent writes so on the pull request.
A requirement observed as not met is a `fail`.
A check closed as not run is no pass, so the issue stays open, and its owning architect is told
once.
After that, each result recorded for a check without a pass opens one more look.
`legion state` prints how many checks
are open and the oldest of them, with when its next look is due.

A child's close deletes its pod, and the disk goes with it; a root's close releases any admission
slot its issues still held, which happens once no other issue of its tree is still in the
workflow, and the finished tree **lingers** for
`linger_hours` (72 by default) in case work resumes, each member's conversation kept in the session
database; then its Sandboxes are deleted, once no member's check is open. Closing a root yourself
from the Dispatch dashboard lingers the tree at once, children and all: your decision outranks
that order. What a stop keeps is the conversation and whatever was pushed to the issue's branch: a
workspace lives as long as the pod it was provisioned in, and the next pod provisions it again
from that branch. Setting a done root back to `todo` runs it again as a new generation, starting
from its architect.

## Supervision

The daemon counts what goes wrong with each agent: launches that fail, deaths while it had work
outstanding, prompts it acknowledged without taking a turn, and the times it was relaunched for
that. When a budget runs out (`launch_failure_limit`, `prompt_failure_limit` and
`prompt_retire_limit` in the configuration) the daemon stops relaunching that agent and **holds**
the issue. A held phase worker is its architect's to retry or escalate to the controller. A root's
architect that runs out goes to the controller, which restarts a running tree when it finds no
cause or the cause is gone, and otherwise asks a person whether to park it. A child's architect
that runs out holds nothing: the architect above it is told and takes the child's notices, and
the daemon starts the child's architect again, with fresh budgets, at the child's next phase
start.
[Troubleshooting](/legion/legion/troubleshooting/) shows how to see each of these.

The daemon says why the claim failed rather than leaving that in its log alone. It keeps one word
for it — the runtime refused the launch, the process died, the agent never registered, its plugin
was stale, its prompt retirements ran out, or a provider error ended its turn — with the sentence
written beside that word, the moment, and the four counts against the limits above. The
architect that owns the issue reads it on the claim, the controller reads it with the limits
beside it, and the notice of the hold carries it as well, so the first reader of a hold already
knows what ran out. The held issue names the phase the hold took it out of.

Four moves release a held issue. The architect's own retry puts it back in that phase. Setting a
held root to `todo` releases the hold the same way, whether you move it in the Dispatch
dashboard, the controller sets it, or an operator runs `legion issue status`: the issue returns to
that phase and its worker starts again at the same generation, in the same pod and at the same place
in the admission line. Where the pull request's head is red, the issue goes on to implementing
instead, and a held review whose round is already decided goes where that decision sends it; the
board then shows the phase the issue ends in. Starting the worker's claim in a fresh session
does the same at the cost of that session. A restart, the root set to `backlog` and then to
`todo`, runs the whole tree over as a new generation, which is the move for a tree that must start
from its architect.

A launch failure that is charged is spaced from the next try by a backoff: 2 s after the first,
doubling after each further one, each wait drawn between half its step and the whole of it. A
launch the runtime refused is tried again after it, and so is a process that died or was retired at
its registration deadline, so the three tries of `launch_failure_limit`'s default span three
seconds at least, never the moment one fault lasts. The wait logs `supervise: waiting before the
next launch` with its length. The try it waits for is stored, with the charge, before the wait
begins, so a daemon stop during it forgives nothing; the next boot launches the claim again.

**What the daemon records of every worker.** Each agent's claim carries a step record: when the
daemon last saw that agent step, when the turn that step belongs to began, and the tool call it is
in — the tool, the call id the stream's start and end frames pair by, when the call started and,
once it has, when it ended, with the arguments' length and a fingerprint of them, never their
text, since a shell command routinely holds a credential. It is what says a worker mid-turn is
still moving, and a call that carries a start and no end is the shape a turn that stopped moving
leaves behind. The daemon's own backstop reads that record and nothing else before it ends such a
turn (`worker_stall_backstop_hours` in
[legion.yaml](/legion/legion/running-legion/#legionyaml)), and a person reads the same record
three ways: `legion state`'s `agents in a tool call` line, which names each working claim in an
open call by issue and role ([Observe](/legion/legion/running-legion/#observe)); `legion claims
list`, whose last column is that call for each claim, the project controller's — which is on no
issue, and so on no part of the `legion state` line — included; and `GET /legion/v1/state`, the
bytes `legion state --json` prints, under `issues.<KEY>.architect.step` and
`issues.<KEY>.workers.<role>.claim.step`.
