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
in `workflow`, one `Engine.sameRepository` stands where `strings.EqualFold` stood, at `issueForBranch`, `ownPullRequest`, the opened handler's redelivery test, `check.go`'s comparison of a recorded check, and `Engine.ApplyRepositoryChange`, which therefore corrects nothing when Dispatch re-points an issue at the repository the one it is recorded on moved to.
`ApplyRepositoryChange` makes a recorded issue's repository follow the one Dispatch resolves the issue to, in each pass that reads that resolution
— a live Dispatch event (`Engine.dispatchIssue`) and the boot and recovery listing (admission's reconcile).
It applies the correction with one statement of the fact's own transaction (`record.(*Postgres).CorrectIssueRepository`), which writes the new repository, its default branch and GitHub's spelling of it, keeps the repository the record held and the moment of the correction, and clears whatever change was waiting.
A repository Legion serves no installation of is not written onto the record:
the record keeps what it has, the issue is told once, and the change waits on the record (`record.Store.NoteIssueRepositoryChange`) for the pass that finds that repository served.
No deploy run is matched any more: the production check a merge opens is woken only by the `wake_at` its own agent
— the role the project's `production_check` names
— gave on a `pending` report, by the retry of a look nobody reported, or by the check's own bound, so `projects.<KEY>.deploys` is read for nothing and only warned about.
The reads keyed on the `pull_requests` table's `repo` column
— the engine's `pullRequest` and `pullRequestByBranch`, `internal/daemon`'s `reviewedRepository` and intake's `recordedPullRequest` — ask under each name `Renames.Lookups` gives, the fact's own first and the pair's other name only when the first finds nothing, which is what lets a pull request already open at the transfer go on taking its CI, its review and its merge.
The equivalence is read-side only: a rename rewrites no record and moves no record onto the other name, and so the pair must stay configured for as long as any record names the old repository.
It serves no repository of its own either
— the served set is Dispatch's mapping alone, and no rename is read from GitHub
— so a pair may be configured before the transfer and simply answers nothing until the events arrive under the new name.
What does move a record's repository is Dispatch's own resolution of the issue, which `ApplyRepositoryChange` applies, and nothing else moves it.
