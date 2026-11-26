package admit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// keepSignalFacts keeps what fact carries of an input of a signal of admission by signals, in the
// fact's own transaction and before the fact's admission pass (Apply), so the pass the fact runs
// reads what it kept, and the fact, the rows it kept and the pass commit together or not at all.
// The fact's processed mark (intake.ApplyFactWith) makes a fact delivered twice a duplicate that
// reaches no handler, and each row is keyed by its source's own identity as well, so a fact is
// kept once however it arrives.
//
//   - intake.MemoryGuardKill: the memory kill of an issue pod of the project, of a role's process or
//     of one of no role, by the guard or by the pod's kernel, kept at the report's own moment
//     (record.Store.KeepMemoryKill), whatever the workflow does with it: the engine runs first and
//     tells the role, or tells no role where the process was of none or the daemon records no such
//     issue, but the kill counts either way, as the issue pod's launcher line of it does.
//     intake.ControllerMemoryGuardKill keeps nothing: a kill in the controller's pod does not
//     count.
//   - intake.PullRequestEnqueued, intake.PullRequestDequeued and intake.PullRequestMerged: a pull
//     request of an issue of the project, on that issue's branch, moved in the merge queue. The
//     notification carries no time of the move, so each queues a record.QueueTimelineRead, which
//     the outbox runs outside the fact lock to read GitHub's timeline of the pull request, whose
//     events carry GitHub's own id and time. A pull request of another project and one on a branch
//     that names no issue queues none (queueTimelineOf).
//   - intake.QueueTimeline: GitHub's merge-queue events of one pull request, kept once each by
//     GitHub's event id (record.Store.KeepQueueEvents), so a timeline read by a webhook's row and
//     one read by the boot are kept once.
//   - intake.QueueBootStatus: why the boot's read of GitHub's merge queue failed, or that it
//     succeeded (record.Store.SetSignalInputReadError), so the queue signal reads as not recorded
//     with that reason where a read failed, and never as an empty queue.
//   - intake.DaemonBooted: each boot records that this daemon keeps the kills and the queue from
//     now on, where no earlier boot recorded it (record.Store.BeginSignalInput), so each signal
//     that counts over a span is recorded from the first boot of the release that keeps its input
//     plus that span (signals.KillSpan, signals.QueueExitSpan), and not before, where nothing was
//     kept.
func (a *Admission) keepSignalFacts(ctx context.Context, tx pgx.Tx, fact intake.Fact) error {
	switch fact := fact.(type) {
	case intake.MemoryGuardKill:
		return a.store.KeepMemoryKill(ctx, tx, a.project, fact.Report, a.now())
	case intake.PullRequestEnqueued:
		return a.queueTimelineRead(ctx, tx, fact.Repo, fact.Number, fact.Branch, "enqueued")
	case intake.PullRequestDequeued:
		return a.queueTimelineOf(ctx, tx, fact.Repo, fact.Number, "dequeued")
	case intake.PullRequestMerged:
		return a.queueTimelineOf(ctx, tx, fact.Repo, fact.Number, "merged")
	case intake.QueueTimeline:
		return a.store.KeepQueueEvents(ctx, tx, a.project, fact.Events, a.now())
	case intake.QueueBootStatus:
		return a.store.SetSignalInputReadError(ctx, tx, a.project, signals.Queue, fact.ReadError, a.now())
	case intake.DaemonBooted:
		for _, input := range []string{signals.Kills, signals.Queue} {
			if err := a.store.BeginSignalInput(ctx, tx, a.project, input, a.now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// queueTimelineOf queues the timeline read of a dequeued or merged pull request, which names no
// branch, for the pull request the record holds under repo or the name a configured rename pairs
// with it (ghrepo.Under): the record knows the pull request's branch, and the read is queued only
// where that branch is an issue's of this project. A pull request the record does not hold is
// none of this project's to keep, since the project's own is recorded when it opens.
func (a *Admission) queueTimelineOf(ctx context.Context, tx pgx.Tx, repo string, number int, move string) error {
	pr, err := ghrepo.Under(a.renames(), repo, func(name string) (*record.PullRequest, error) {
		return a.store.PullRequestRowByNumber(ctx, tx, name, number)
	})
	if err != nil || pr == nil {
		return err
	}
	return a.queueTimelineRead(ctx, tx, repo, number, pr.Branch, move)
}

// queueTimelineRead queues a record.QueueTimelineRead of pull request number of repo for the move
// notified, one of "enqueued", "dequeued" and "merged", when branch names an issue of this
// project (intake.ProjectBranchIssue), keyed by that issue so the project's daemon alone runs it.
// MinCount is how many events of the move's direction GitHub's timeline must hold before the read
// accepts that it shows the move (record.QueueProgress.Needs), which is what makes a read made
// before GitHub's timeline caught up a read made again.
func (a *Admission) queueTimelineRead(ctx context.Context, tx pgx.Tx, repo string, number int, branch, move string) error {
	issue, ok := intake.ProjectBranchIssue(branch, a.project)
	if !ok || repo == "" || number < 1 {
		return nil
	}
	names := []string{repo}
	if partner, paired := a.renames().Partner(repo); paired {
		names = append(names, partner.String())
	}
	progress, err := a.store.QueueProgressOf(ctx, tx, a.project, names, number)
	if err != nil {
		return err
	}
	row, err := record.NewOutboxRow(issue, record.QueueTimelineRead{
		Repo: repo, Number: number, Move: move, MinCount: progress.Needs(move == "enqueued"),
	}, a.now())
	if err != nil {
		return err
	}
	return a.store.Enqueue(ctx, tx, row)
}

// renames is the deployment's repository equivalence, which the engine holds; none where an
// admission is built with no engine.
func (a *Admission) renames() ghrepo.Renames {
	if a.engine == nil {
		return ghrepo.Renames{}
	}
	return a.engine.RepositoryRenames()
}
