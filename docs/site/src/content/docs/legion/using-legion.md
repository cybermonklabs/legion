---
title: Using Legion
description: Hand an issue to Legion, answer its questions and approve its spec, review and merge its pull request, and read what each Dispatch status means.
sidebar:
  order: 3
---

This page is for the person whose issue Legion works: you write the issue, answer the architect's
questions, approve the spec, and review the pull request. Everything happens in
[Dispatch](/legion/dispatch/) and on GitHub; you never need the `legion` command.

## Before you start

- **The issue must be in a Dispatch project a Legion daemon runs.** Each daemon runs the projects its
  operator configured; ask your operator which ones.
- **Assign the issue to the person who will answer for it.** The assignee's Inbox gets the
  architect's questions and the spec approval for the whole tree. An issue an agent files without
  one is not left unassigned: it takes its parent's assignee when the parent is in the same
  project, else the project's default assignee, which every project names. An issue whose assignee
  someone has cleared still runs, and its questions go to the project's default assignee until
  someone takes the issue from its header.
- **Write the outcome, not the steps.** A title that says what someone can do afterwards, and a spec
  with the problem, the acceptance criteria and how you would check them, give the architect what it
  needs. It extends your spec rather than replacing it, and it asks about anything it cannot decide.
  Child issues you have already created are adopted as part of the work.

## Hand an issue to Legion

1. Open the issue in Dispatch and add the label **`legion`** from the issue header.
2. For a new issue (status `triage`), that is the whole handover: your label is queued by the
   daemon, and the issue moves to `todo` itself. For a parked issue (`backlog` or `icebox`), set
   its status to **`todo`**, or approve its spec: your approval queues it the same way when the
   issue has no parent. An approval that Dispatch carries in your name from an earlier one does
   not queue it. An approval that Legion applies after the issue's status changed again (it reached
   Legion late, or the operator replayed it) does not queue the issue either: set its status to
   `todo` instead.

When a slot is free the daemon admits the issue and the status changes to `in_progress`; when
every slot is taken, the issue waits in `todo` for a free slot, in the order the line gives it ([Admission and the `legion` label](/legion/legion/concepts/#admission-and-the-legion-label)).
A slot is one issue the daemon has started, a root or a child, so a tree of four issues at work
holds four of them. An issue keeps its slot through a wait on you or on a review, and its
agents stay live beside it. A started issue that waits on another issue gives its slot back, and
nothing of its work stops, except a parent that an open child of its own holds: its phase stops,
and it waits in `awaiting_children` until no child under it is open; in `accepting`, `retro` and
`merging` an issue holds no slot at all,
and a step after `accepting` whose phase keeps a slot waits for one again
([Admission and the `legion` label](/legion/legion/concepts/#admission-and-the-legion-label)).
A label an agent session set on an issue in `triage` goes to the controller instead: the controller
decides whether to admit it or leave it for you, and parks it only when a person asked for that or
its work is done, folded into another issue or no longer needed.

The controller never takes an issue on its own, whatever slots are free: Legion works what a person
hands it, and the clear defects its own agents report that no tree owns. You can also ask the
controller to hand an issue over for you, in a message or in Slack. An agent that finds a defect
outside its own issue whose fix needs no choice of yours fixes it itself, in its own pull request
or a separate pull request of its tree, in whichever repository the defect is. It writes the
defect to the architect of the tree that owns the code, as a Dispatch comment that mentions that
architect's role, only when its fix would collide with that tree's open pull request or its token
does not reach the repository. The controller files the defect labelled `legion` only when no tree
owns the code, and it passes no defect on to a tree. When the problem needs a decision, a deploy
window or work much larger than the agent's own, the agent asks you in a Dispatch ask whether to
file one, rather than filing it. Answer with `File it for Legion` and the controller files it
labelled `legion`.

Do not label child issues: a child runs under its tree once the root is admitted. The children do
the work first: an issue starts no phase of its own while Dispatch shows an open child under it,
so a parent's own pull request is built and merged after its children's. A child filed under an
issue already at work stops that issue's phase. The issue waits, shown `in_progress`, and goes on
by itself once its children are done ([Concepts](/legion/legion/concepts/#trees-and-the-architect)).

## Ask Legion in Slack

In a Slack channel your operator connected to the project, mention the Legion app (`@Legion`) and
the project's controller answers in that thread:

- A question about what Legion is doing ("what are you working on?", "where is ACME-12?") is
  answered from the daemon's state and Dispatch.
- A request for work is filed as a Dispatch issue in the project, and the reply names it with its
  link. The issue carries the `legion` label, so Legion takes it, only when you ask Legion to do the
  work; otherwise it waits for a person to hand it over.
- A question an issue's architect owns is passed to that architect, and the reply says so; its
  answer comes on the issue.

The controller answers only the channels the operator listed, and not while it is stopped: a
mention made then gets no reply, so ask again once it runs.

## Answer the questions and approve the spec

Shortly after admission the architect claims the issue (the issue shows **Claimed by** Legion's
session), then grows your spec into a design: how the work splits, how each outcome is proven, and
the decisions it needs from you. Each open question is a **decision block** in the spec, with
options and a recommendation, and it reaches your Inbox.

1. Answer each decision block. The architect reads every answer and revises the spec.
2. When nothing is open, the architect asks you to approve the spec, with a short summary of what
   the work will do that you have not already agreed to. Approve it from the approval question in
   your Inbox or with **Approve** in the document's header.
3. If it is not right, choose **Request changes** and say why. The architect revises the spec and
   asks again.

No code is written until you approve. The approval is for that version of the spec: if the spec
changes afterwards, you are asked again. (`gates.design` says which documents of the root issue a
person approves: by default the spec, and also the issue's Verification document when the
architect asks you to approve both in one request, and a deployment can add the issue's decisions
document or its Verification document, each approved the same way. A deployment that sets
`gates.design: []` turns the design gate off; then the work starts as soon as the spec is written,
and nobody is asked.)

While your approval stands, the model requests of the root's architect carry a message that says
you approved that version of the spec. It tells the architect that its top responsibility is now to
deliver that spec and no other, and to bring you a change to what the spec delivers as one decision,
before any work on it. If the spec changes after the work started, the deployment's `gates.hold`
says what waits until you approve the new version: the merge and the start of new issues
(`merge`, the default), or all the tree's work (`work`), whose workers the daemon stops with their
sessions kept. See [The design gate](/legion/legion/concepts/#the-design-gate).

If a phase gets stuck, or the architect hits something only a person can decide, it asks you the
same way: a question in your Inbox that says what is blocked, the options, and what it recommends.

## Follow the work

The issue's status tells you which phase is running (see [What each status means](#what-each-status-means)).
The issue's events show each status change, and the architect's messages there say what changed
and why. The pull request appears under the issue's external links, and its body carries the line
`Dispatch: <KEY>` pointing back to the issue.

## Review the pull request

You can read the pull request at any time. Legion's implementer writes its description from a fixed
template, and the later phases fill in their own lines:

- **Outcome** and **Why**: what changes for a user, and the issue it answers.
- **Proven by** and **Checks**: one line per check of the issue's Verification document, each the
  line Dispatch printed when the result was recorded — the check, when it runs, the verdict, the
  commit it ran at and the role that ran it.
- **Look at first** and **Not proven / risk**: written by Legion's reviewer, or by its implementer
  on a task whose plan has no reviewer step, naming where a wrong decision would hurt and, where
  one stands, a risk.
- The `after_merge` check, recorded after the merge closes the issue by the role your project's
  `production_check` names — the tester, unless the setting is `off`, where no production check
  opens — once your deploy carries the change; its line reads `not yet run` until then, or a `fail`
  at the check's last look for a requirement only the deploy can show that is still unobserved,
  whether or not production carries the merge.
- Any line your repository's own instructions derive from the paths the change touches, such as a
  checklist named for each kind of path: after the task's last step, the retro recomputes it for
  the whole change, the way those instructions say.

Legion's reviewer posts its review as the review App. A review from anyone with write access to the
repository, submitted on GitHub while the issue is in `needs_review`, counts the same way:
**Request changes** sends the work back to the implementer with your comments, and an approval of
the head can end the round. A review from an account without write access decides nothing, since
any GitHub account can review a public repository's pull request, and a plain comment moves
nothing. Legion's handoffs are records on the Dispatch issue, never commits, so the pull request's
file list shows only the change and the merge brings no handoff onto the default branch.

## Merge

When the task's steps and the retro have passed, the merger merges the pull request itself. It
merges as the implement App, at the head it read, which it names as the head GitHub must match. It
writes the squash commit's message itself — the pull request's title and number, and an empty
body — so GitHub's own default, which joins every commit message of the branch, never puts
Legion's internal trailers on your default branch. On a base with a merge queue it enqueues the
pull request instead.

The repository's branch protection and code-owner rules decide when it lands: a review
only you can give is waited for. Legion turns auto-merge on nowhere, approves no pull request on
your behalf and changes nobody's right to merge.

The merge closes the issue: it moves to `done` the moment GitHub reports the pull request merged.
A root with work still running beneath it is the exception: its merge is recorded and the issue
waits until the last other issue of its tree is done or parked, so no child's finished work is cut
off, and then it closes. Where the issue's task has the architect's acceptance step, the architect
accepted the change before that (on the default steps, once the reviewer approved it), with a comment on the issue summarizing the evidence, the issue's goal and each requirement of
its spec among it; the daemon refuses that acceptance while an issue beneath it is still being
worked, so a parent is accepted after its children. After the close the role your project's
`production_check` names — the tester, unless the setting is `off` — checks the change in
production and records what it saw
as the result of the issue's `after_merge` check, with that result's line on the pull request; a
closed issue takes a result for such a check or a `default_branch` one, a comment and a message,
so look in both places.
Each look that finds the deploy missing says when it wants to look again; besides that, only the
check's bound and the retry of a look nobody reported bring it back.
The tree lingers for three days (`linger_hours`, 72 by default) in case the work resumes: its
sessions are kept, and under Kubernetes its pods and their disks go at the close, and a re-admission
makes them afresh.

## Stop, park or rerun

- **To stop Legion working an issue**, move it to `backlog` (or `icebox`) from the Dispatch
  dashboard. A person's move takes the tree out at once and the slots its issues held go to the next issues in line; its
  agents stop, each once the turn it is in ends, however long that takes, so a turn already under
  way finishes what it was doing. To keep Legion off it for good, also take the `legion` label off;
  taking the label off alone does not stop a tree that has started.
- **To run it again**, set it back to `todo` with the label on, or approve its spec: a person's
  approval of a parked labelled issue's spec is queued by the daemon when the issue has no
  parent, and the issue moves to `todo` itself; an approval that Dispatch carries in your name from
  an earlier one does not queue it. An approval that Legion applies after the issue's status
  changed again (it reached Legion late, or the operator replayed it) does not queue the issue
  either: set it to `todo`. The tree starts over as a new generation, from its architect.
- **To close it without a change**, tell the architect at the design gate that no change is needed;
  it closes the issue with that reason. You can also close the issue yourself from the dashboard.

The daemon sets back a status that an agent session of the tree writes on a running issue, with
one exception. The architect that owns a child can write `done`, `backlog`, `icebox` or `triage`
on that child, and the daemon takes that write as it takes a person's move: the child leaves the
workflow. The root's architect owns every issue of its tree, and a child's architect owns the issues
under its own issue. Every other status that a session of the tree writes is set back: a status
that moves the workflow, `todo`, any status on the root, any status that a phase worker writes, and
an architect's status on an issue that it does not own. A root keeps its own rule. Only a person's
move in the dashboard, the controller's `set_status`, or an operator's `legion issue status` takes
a running tree out, and the controller moves a root whose work is folded into another issue or no
longer needed to `backlog` or `icebox`, because it cannot close a running root `done`. When a
session outside the tree writes a status on a running root, the daemon sets it back and tells the
root's architect who wrote it. A move of your own between the workflow's own statuses —
`in_progress`, `testing`, `needs_review`, `retro`, and `todo` on a root (on a child, `todo` reruns
it) — on a running issue does not stay either: the daemon's status repair writes back the status
of the phase the issue stands in, at each boot and then every `controller_wake_interval_seconds`
(an hour by default). `backlog` or `icebox` is how you stop the work.

## What each status means

| Status | Set by | What it means for a Legion issue |
| --- | --- | --- |
| `triage` | whoever files the issue | New. A person's `legion` label is queued by the daemon: the issue moves to `todo` itself. A label a session set goes to the controller, which decides whether to admit it now or leave it to you, and parks it only when a person asked for that or its work is done, folded into another issue or no longer needed. |
| `icebox` | a person or the controller | Parked for the long term. Legion does not run it. |
| `backlog` | a person, the controller, or the architect that owns a child | Should not run now: a person asked for that, for example while it waits on a decision or a deploy. A root also goes here when its work is done, folded into another issue or no longer needed. The architect that owns a child writes `backlog` on it only when a person asked for that, or as the first step of a restart (`backlog`, then `rerun_child`), and it closes a child whose work is done, folded or no longer needed with `done`. Legion does not run it. |
| `todo` | a person, the controller, or the daemon for a person's handover | Ready. A labelled root is admitted when a slot is free and waits in line until then. A child in `todo` waits in the same line for a slot of its own, and shows `todo` only until its first phase: the move into its first step sets it to `in_progress`. |
| `in_progress` | the daemon | Admitted: the architect is writing the spec or waiting for your approval, or the planner or implementer is working. Work sent back by a failed test or a review returns here. A head that conflicts with its base keeps the status it has: until every step of the task has run the implementer merges the base forward, and then the merger does when it merges, while the issue stays in its phase. |
| `testing` | the daemon | The implementer opened the pull request and finished; the tester is checking the change. |
| `needs_review` | the daemon | A step that reviews the change runs: Legion's reviewer, or the architect's acceptance, where the task's plan places it. Your GitHub review counts now. |
| `retro` | the daemon | The task's last step passed: on the default steps, the architect accepted. The issue stays here through retro and the merge — and, for a root whose tree still runs, through the wait between its merge and its close. |
| `done` | the daemon at the merge, a person, or the architect that owns a child | Finished: the pull request merged, or, for a child, the architect that owns it closed it because its work is done, folded into another issue or no longer needed. A root whose tree is still running waits in `retro` until the last of it is done or out of the workflow, then closes. The production check that your project's `production_check` opens after a merge — the tester's, unless the setting is `off` — follows on the pull request; the tree lingers, then closes. |
