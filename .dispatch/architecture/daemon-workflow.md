---
title: Admission and workflow
parent: daemon
---
Planned: the issue-cap admission line, durable Dispatch and GitHub event intake, classification, the transition table, status and notice effects, and the Go Dispatch client
— `internal/admit`, `internal/intake`, `internal/workflow`, `internal/classify`, `internal/dispatch`, `internal/notify`, `internal/phase`.
LEGION-219 (Stage 3) carries them; none exists on `main` yet, so this component has no paths.
The planned workflow advances phases from facts the coordinator observes; agents do not choose the next phase.

Admission and the workflow read the deployment's repository equivalence (`repository_renames`, `config.Config.RepositoryRenames`, a `ghrepo.Renames`) wherever they compare a repository's name, so a transfer that renames a repository leaves the trees already recorded under the old name running:
in intake, `ConsumerSpec.Renames` carries the pair into the consumer's `recordedPullRequest` read;
in `workflow`, one `Engine.sameRepository` stands where `strings.EqualFold` stood, at `issueForBranch`, `ownPullRequest`, the opened handler's redelivery test, `check.go`'s comparison of a recorded check, and `Engine.ApplyRepositoryChange`, which therefore corrects nothing when Dispatch re-points an issue at the repository the one it is recorded on moved to, or still names the old name after the rename pass wrote the new one.
`ApplyRepositoryChange` makes a recorded issue's repository follow the one Dispatch resolves the issue to, in each pass that reads that resolution
— a live Dispatch event (`Engine.dispatchIssue`), the boot listing (admission's reconcile), admission's hand-over of a recorded child as a root of its own, and `internal/daemon`'s `followRecorded`, which runs over every recorded issue once a boot is over, for the changes that boot's listing found and read GitHub for none of, over each recorded issue whose event's or hand-over's reads GitHub did not answer, at once unless a follow already owed covers it and otherwise on the refresher's backoff, until GitHub answers, over each record the refresher's backfill filled, once the fill has committed (`backfillLater`), and over each issue whose correction the engine skipped because its record moved after the fact was prepared.
Each correction is written from what the fact's own preparation read of GitHub for the repository it moves onto (`intake.DispatchIssue.RepositoryRead`: both role Apps' installations, GitHub's spelling and the default branch), never from a read the daemon kept, and a fact no preparation read GitHub for, or whose reads GitHub did not answer, writes nothing.
It applies the correction with one statement of the caller's transaction (`record.(*Postgres).CorrectIssueRepository`), which writes the new repository, its default branch and GitHub's spelling of it, keeps the repository the record held and the moment of the correction, and clears whatever change was waiting.
A fact also carries the repository its record held at its preparation (`intake.DispatchIssue.PreparedAgainst`, `intake.Handover.PreparedAgainst`), and `ApplyRepositoryChange` writes nothing from one whose record holds another repository now: two reads of Dispatch at one sequence cannot be ordered and a correction moves no sequence, so such a fact would put the record back on an older resolution.
The engine asks for a follow of that issue on its transaction's commit instead (`workflow.Config.FollowIssue`, the daemon's `requestFollowOf`), and the next boot's listing covers a process that stopped between the commit and the ask.
A repository a role App is not installed on is not written onto the record:
the record keeps what it has, the issue is told once, and the change waits on the record (`record.Store.NoteIssueRepositoryChange`) for the issue's next event or the next boot, whose preparation reads GitHub again.
Since dispatch://INFRA-4401 (#4493) the daemon reads no repository mapping, so nothing else reads such a repository again, and every waiting change waits for one of those two.
No deploy run is matched any more: the production check a merge opens is woken only by the `wake_at` its own agent
— the role the project's `production_check` names
— gave on a `pending` report, by the retry of a look nobody reported, or by the check's own bound, so `projects.<KEY>.deploys` is read for nothing and only warned about.
The reads keyed on the `pull_requests` table's `repo` column
— the engine's `pullRequest` and `pullRequestByBranch`, `internal/daemon`'s `reviewedRepository` and intake's `recordedPullRequest` — ask under each name `Renames.Lookups` gives, the fact's own first and the pair's other name only when the first finds nothing, which is what lets a pull request already open at the transfer go on taking its CI, its review and its merge.
The equivalence writes no record but for one pass of this release, `internal/daemon`'s rename pass (`record.RenameIssueRepository`), which writes each pair's `to` over its `from` on the stored rows of each issue with no live pod, at boot and when the issue's pod ends;
until it writes a record, the record keeps the repository it was written under, so the pair must stay configured until no row names the old repository.
The pass writes beside `CorrectIssueRepository` and is no correction: the pair names one repository, so it records no repository the issue left, and its statement matches the old name rather than a copy read earlier.
It serves no repository of its own either
— a name is served when GitHub answers that both role Apps are installed on it, and until GitHub has answered for a pair's names its `to` counts as served and its `from` does not
— so a pair may be configured before the transfer and simply answers nothing until the events arrive under the new name.
What moves a record onto another repository is Dispatch's own resolution of the issue, which `ApplyRepositoryChange` applies, and nothing else moves it.

News of an approved new version reaches the holders of a tree's work from the record, never from a message composed when the gate moved.
Migration `20261119203047_design_news` adds the told record, `design_news_told`, one row per issue and audience: the `holder` row is what the session holding that issue's phase took, the `architect` row of a tree root what that tree's root architect took, `versions` maps each gated step to the approved version taken, and `opening_owed` says the gate's opening is owed to that audience.
A confirmed delivery's copy of the news its prompt carried (`pending_task_deliveries.carried_news`) is a credit, not a write to the row: every reader reads the told rows through `design_news_told_in`, which adds each standing credit, and a credit counts only in the run it was rendered in (`design_news_current`).
The write that records `carried_ran`, once the turn's result says the turn ran, folds the credit into the row (`store.settleDesignNews`).
A take-back or a retirement writes nothing to the told rows: the delivery written without its copy, or deleted, ends its credit.
`Engine.enter` writes a member's baseline at its first working entry (`record.Store.BaselineTold`), so a delivery retired before its turn ran leaves its news owed.
`record.OwedNews` is the one rule over it: news is owed while the standing gate is open and a step's approved version is above the told one, `spec` and `decisions` concerning every member and `criteria` the tree root alone, and never to a member whose pull request merged, which `Engine.backward` also refuses to move back (`BACKWARD_REFUSED`).
The change set is Dispatch's, read from `GET /api/v1/artifacts/{id}/changes` and kept in `design_changes` once per document and version pair, `read` or `unavailable` with its reason, since two stored versions never change.
The outbox kind `design_changes` (`record.DesignNewsReconcile`, one unrun row per tree) runs on a lane of its own, `record.LaneDocuments`, so its Dispatch read holds back no start, stop or suspend of the lifecycle lane and waits behind no architect notice of the board lane;
its executor, the daemon's `designChanges`, records each pair, raises the told versions of a change that changed nothing of the work (decision blocks alone, or a version Dispatch answers `unchanged`: `record.DesignChanges.WorkUnchanged`), and queues a news-only start (`record.SuperviseRequest.News`) for each holder owed news.
`putGateTold` queues that row when an approved version rises or the standing gate opens, `Engine.queueMovedNews` when a move leaves its holder owed news, and the entry sweep's tick (`queueOwedDesignNews`) is the backstop.
Every phase start and every news-only start carries the news, which `workflow.RenderNews` renders at send from the record as it stands then, and a merger's start waits while its change set is not recorded (`mergerWaitsForTheDesignGate`).
The opening is decided for each holder in one function, `openingDelivery`, which `tellGateMoved` calls: a holder owed news is sent no role message, its `opening_owed` is set, and the news task leads with the opened sentence;
every other holder is sent the opened role message, with the clause that nothing its issue holds changed where its told row shows it.
The closed message goes to every holder as before.
