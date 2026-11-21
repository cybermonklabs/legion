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
— a live Dispatch event (`Engine.dispatchIssue`), the boot listing (admission's reconcile), admission's hand-over of a recorded child as a root of its own, and `internal/daemon`'s `followRecorded`, which runs over every recorded issue once a boot is over, for the changes that boot's listing found and read GitHub for none of.
Each correction is written from what the fact's own preparation read of GitHub for the repository it moves onto (`intake.DispatchIssue.RepositoryRead`: both role Apps' installations, GitHub's spelling and the default branch), never from a read the daemon kept, and a fact no preparation read GitHub for writes nothing.
It applies the correction with one statement of the caller's transaction (`record.(*Postgres).CorrectIssueRepository`), which writes the new repository, its default branch and GitHub's spelling of it, keeps the repository the record held and the moment of the correction, and clears whatever change was waiting.
A repository a role App is not installed on is not written onto the record:
the record keeps what it has, the issue is told once, and the change waits on the record (`record.Store.NoteIssueRepositoryChange`) for the issue's next event or the next boot, whose preparation reads GitHub again.
Since dispatch://INFRA-4401 (#4493) the daemon reads no repository mapping, so nothing else reads such a repository again, and every waiting change waits for one of those two.
No deploy run is matched any more: the production check a merge opens is woken only by the `wake_at` its own agent
— the role the project's `production_check` names
— gave on a `pending` report, by the retry of a look nobody reported, or by the check's own bound, so `projects.<KEY>.deploys` is read for nothing and only warned about.
The reads keyed on the `pull_requests` table's `repo` column
— the engine's `pullRequest` and `pullRequestByBranch`, `internal/daemon`'s `reviewedRepository` and intake's `recordedPullRequest` — ask under each name `Renames.Lookups` gives, the fact's own first and the pair's other name only when the first finds nothing, which is what lets a pull request already open at the transfer go on taking its CI, its review and its merge.
The equivalence writes no record but for one pass of this release, `internal/daemon`'s rename pass (`record.RenameIssueRepository`), which writes each pair's `to` over its `from` on the stored rows of each issue with no live pod, a saved workspace bundle's repository among them, at boot and when the issue's pod ends;
until it writes a record, the record keeps the repository it was written under, so the pair must stay configured until no row names the old repository.
The pass writes beside `CorrectIssueRepository` and is no correction: the pair names one repository, so it records no repository the issue left, and its statement matches the old name rather than a copy read earlier.
It serves no repository of its own either
— a name is served when GitHub answers that both role Apps are installed on it, and until GitHub has answered for a pair's names its `to` counts as served and its `from` does not
— so a pair may be configured before the transfer and simply answers nothing until the events arrive under the new name.
What moves a record onto another repository is Dispatch's own resolution of the issue, which `ApplyRepositoryChange` applies, and nothing else moves it.
