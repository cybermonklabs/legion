package admit

import (
	"context"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// keepSignalFacts keeps what fact carries of an input of a signal of admission by signals, in the
// fact's own transaction and before the fact's admission pass (Apply), so the pass the fact runs
// reads what it kept, and the fact, the rows it kept and the pass commit together or not at all.
// The fact's processed mark (intake.ApplyFactWith) makes a fact delivered twice a duplicate that
// reaches no handler, but for a report of model requests and a turn edge, which keep none, and
// each row is keyed by its source's own identity as well, so a fact is kept once however it
// arrives.
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
//     one read by the boot are kept once. A timeline GitHub answered is gone (Gone) forgets the
//     events kept of the pull request under its repository or the name a configured rename pairs
//     with it (record.Store.ForgetQueuePull).
//   - intake.QueueBootStatus: why the boot's read of GitHub's merge queue failed, or that it
//     succeeded (record.Store.SetSignalInputReadError), so the queue signal reads as not recorded
//     with that reason where a read failed, and never as an empty queue.
//   - each fact of an ask event (intake.AskTurnOf: an ask opened, restored, answered or resolved,
//     handed back, edited, handed to a person, or replied to): the ask as signals.NextAsk turns it
//     by the event, kept at the event's sequence in its issue's Dispatch event log
//     (record.Store.KeepAskEvent), so an older event delivered after a newer one, or one event
//     read from two paths (the stream and the boot's catch-up), changes nothing.
//   - intake.DispatchIssue that shows the issue's assignee: that person, at the observation's
//     sequence (record.Store.KeepIssueAssignee).
//   - intake.DispatchDefaultAssignee: the project's default assignee as Dispatch's settings event
//     moved it, at its sequence in the project's event log (record.Store.KeepDefaultAssignee).
//   - intake.ApprovalsBootRead: the boot's read of Dispatch, its asks, assignees and default
//     assignee, each kept where no newer event of it was (record.Store.KeepApprovalsRead), and the
//     read's outcome (record.Store.SetSignalInputReadError), so the approvals signal reads as not
//     recorded with the reason where the read failed.
//   - intake.DaemonBooted: each boot records that this daemon keeps the kills, the queue, the
//     model requests and the approvals from now on, where no earlier boot recorded it
//     (record.Store.BeginSignalInput), so each signal that counts over a span is recorded from the
//     first boot of the release that keeps its input plus that span (signals.KillSpan,
//     signals.QueueExitSpan), and not before, where nothing was kept. The model requests are the
//     input of the provider's headroom, under signals.Provider. The boot records the configured
//     token limit too (record.Store.KeepTokenLimit), which each minute from then on is read
//     against.
//   - intake.ModelRequests: one report of a host's model requests, each kept once by the claim,
//     its launch generation and the request's own id, marked counted where the configured model
//     selector takes its model (record.Store.KeepModelRequests), with no mark of a write of its
//     own: the series-moved pass rule decides whether the report runs the pass (signalFactMoved),
//     and, while admission by signals is on, the deadline is set again from it either way. The
//     fact keeps no processed mark (intake's keepsMark): the request's own key keeps it once.
//   - intake.TurnEdge: the start or end of a turn of a claim, which the claim itself records, so
//     nothing is kept, nor any processed mark; where the claim's issue holds a slot, the
//     series-moved pass rule decides whether it runs the pass, as for a report. A claim of an
//     issue that holds none moves no series.
//   - intake.SignalSettingChanged: a person's change of the switch or of a limit, which Apply keeps
//     itself (keepSignalSetting) since the fact answers whether it moved the setting.
//
// Every other keep marks its write where its statement changed a row, so the fact runs the pass
// by the pass rule (Apply), and a duplicate, which changes none, runs none.
func (a *Admission) keepSignalFacts(ctx context.Context, tx pgx.Tx, fact intake.Fact) error {
	if turn, seq, ok := intake.AskTurnOf(fact); ok {
		return a.store.KeepAskEvent(ctx, tx, a.project, turn, seq)
	}
	switch fact := fact.(type) {
	case intake.MemoryGuardKill:
		return a.store.KeepMemoryKill(ctx, tx, a.project, fact.Report, a.now())
	case intake.PullRequestEnqueued:
		return a.queueTimelineRead(ctx, tx, fact.Repo, fact.Number, fact.Branch, record.QueueEnqueued)
	case intake.PullRequestDequeued:
		return a.queueTimelineOf(ctx, tx, fact.Repo, fact.Number, record.QueueDequeued)
	case intake.PullRequestMerged:
		return a.queueTimelineOf(ctx, tx, fact.Repo, fact.Number, record.QueueMerged)
	case intake.QueueTimeline:
		if fact.Gone {
			return a.store.ForgetQueuePull(ctx, tx, a.project, a.queueRepos(fact.Repo), fact.Number)
		}
		return a.store.KeepQueueEvents(ctx, tx, a.project, fact.Events, a.now())
	case intake.QueueBootStatus:
		return a.store.SetSignalInputReadError(ctx, tx, a.project, signals.Queue, fact.ReadError, a.now())
	case intake.DispatchIssue:
		// A view of another project's issue (a child release_children reads) carries an assignee
		// no ask of this project waits on.
		if !fact.AssigneeKnown || fact.Seq < 1 || !claim.KeyOfProject(a.project, fact.Key) {
			return nil
		}
		return a.store.KeepIssueAssignee(ctx, tx, a.project, record.IssueAssignee{Issue: fact.Key, Assignee: fact.Assignee, Seq: fact.Seq})
	case intake.DispatchDefaultAssignee:
		return a.store.KeepDefaultAssignee(ctx, tx, a.project, fact.Assignee, fact.Seq, fact.At)
	case intake.ApprovalsBootRead:
		if fact.ReadError == "" {
			if err := a.store.KeepApprovalsRead(ctx, tx, a.project, fact.Read); err != nil {
				return err
			}
		}
		return a.store.SetSignalInputReadError(ctx, tx, a.project, signals.Approvals, fact.ReadError, a.now())
	case intake.ModelRequests:
		report := record.ModelRequestsReport{Token: fact.Claim, Generation: fact.Generation, Requests: fact.Requests}
		if err := a.store.KeepModelRequests(ctx, tx, a.project, report, a.countsTokens, a.now()); err != nil {
			return err
		}
		return a.signalFactMoved(ctx, tx, signals.Provider)
	case intake.TurnEdge:
		slots, err := a.store.Slots(ctx, tx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == fact.Issue }) {
			return nil
		}
		return a.signalFactMoved(ctx, tx, signals.Idle)
	case intake.DaemonBooted:
		for _, input := range []string{signals.Kills, signals.Queue, signals.Provider} {
			if err := a.store.BeginSignalInput(ctx, tx, a.project, input, a.now()); err != nil {
				return err
			}
		}
		if err := a.store.KeepTokenLimit(ctx, tx, a.project, a.tokenLimit, a.now()); err != nil {
			return err
		}
		return a.beginApprovals(ctx, tx)
	}
	return nil
}

// UseModelRequests registers the project's model provider as the daemon's configuration gives it
// (config.Project.Provider): limit, the uncached input tokens a minute the project may spend (0
// for none), which each boot records for the minutes from then on, and models, the models whose
// tokens count against it, nil for every model, which decides whether each request a report
// carries is counted as it is kept. It is a setter rather than a parameter of New for the reason
// UseExperiments is.
func (a *Admission) UseModelRequests(limit int64, models *regexp.Regexp) {
	a.tokenLimit, a.tokenModels = limit, models
}

// countsTokens is whether a request of model counts its tokens against the project's limit: every
// model where no selector is configured.
func (a *Admission) countsTokens(model string) bool {
	return a.tokenModels == nil || a.tokenModels.MatchString(model)
}

// beginApprovals records, at the first boot of the release that keeps the approvals' inputs, that
// the daemon keeps them from now on, and that the boot's read of Dispatch has not succeeded yet
// (record.ApprovalsNotRead): no ask, issue assignee or default assignee was kept before it, so a
// count made before that read would leave out every ask open since before the boot. The read's
// own fact clears the reason (intake.ApprovalsBootRead). A later boot reads over what the earlier
// run kept, which the stream kept current, so it records nothing here.
func (a *Admission) beginApprovals(ctx context.Context, tx pgx.Tx) error {
	_, began, _, err := a.store.SignalInputStatus(ctx, tx, a.project, signals.Approvals)
	if err != nil || began {
		return err
	}
	return a.store.SetSignalInputReadError(ctx, tx, a.project, signals.Approvals, record.ApprovalsNotRead, a.now())
}

// queueTimelineOf queues the timeline read of a dequeued or merged pull request, which names no
// branch, for the pull request the record holds under repo or the name a configured rename pairs
// with it (ghrepo.Under): the record knows the pull request's branch, and the read is queued only
// where that branch is an issue's of this project. A pull request the record does not hold is
// none of this project's to keep, since the project's own is recorded when it opens.
func (a *Admission) queueTimelineOf(ctx context.Context, tx pgx.Tx, repo string, number int, move record.QueueMove) error {
	pr, err := ghrepo.Under(a.renames(), repo, func(name string) (*record.PullRequest, error) {
		return a.store.PullRequestRowByNumber(ctx, tx, name, number)
	})
	if err != nil || pr == nil {
		return err
	}
	return a.queueTimelineRead(ctx, tx, repo, number, pr.Branch, move)
}

// queueTimelineRead queues a record.QueueTimelineRead of pull request number of repo for the move
// notified when branch names an issue of this project (intake.ProjectBranchIssue), keyed by that
// issue so the project's daemon alone runs it. MinCount is how many events of the move's direction
// GitHub's timeline must hold before the read accepts that it shows the move
// (record.QueueProgress.Needs), which is what makes a read made before GitHub's timeline caught up
// a read made again. It counts the moves the store has kept, those whose events the 6-hour delete
// removed among them (record.Store.QueueProgressOf), so a re-entry long after the pull request's
// last exit, and the exit after it, each need one more than the timeline showed before. A pull
// request the store has kept no move of, such as one whose moves came before the first boot of the
// release that keeps them, has no count to need one more than, so its read is Unkept: it accepts
// only a timeline whose newest move is of the notified direction.
func (a *Admission) queueTimelineRead(ctx context.Context, tx pgx.Tx, repo string, number int, branch string, move record.QueueMove) error {
	issue, ok := intake.ProjectBranchIssue(branch, a.project)
	if !ok || repo == "" || number < 1 {
		return nil
	}
	progress, err := a.store.QueueProgressOf(ctx, tx, a.project, a.queueRepos(repo), number)
	if err != nil {
		return err
	}
	row, err := record.NewOutboxRow(issue, record.QueueTimelineRead{
		Repo: repo, Number: number, Move: move, MinCount: progress.Needs(move == record.QueueEnqueued), Unkept: !progress.Kept,
	}, a.now())
	if err != nil {
		return err
	}
	return a.store.Enqueue(ctx, tx, row)
}

// queueRepos is repo and the name a configured rename pairs with it, the names the store's queue
// events and counts of a pull request are read under: a notification and a read name the
// repository as the record spells it, and a kept event names the one GitHub's event url does.
func (a *Admission) queueRepos(repo string) []string {
	if partner, paired := a.renames().Partner(repo); paired {
		return []string{repo, partner.String()}
	}
	return []string{repo}
}

// renames is the deployment's repository equivalence, which the engine holds; none where an
// admission is built with no engine.
func (a *Admission) renames() ghrepo.Renames {
	if a.engine == nil {
		return ghrepo.Renames{}
	}
	return a.engine.RepositoryRenames()
}
