package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// TaskPullRequest names the pull request of one task of an issue, as the engine asks the daemon to
// read it (Config.ReadBaseMergeStates): the issue's key and the task (record.PullRequest.Task), ""
// for an issue with no plan rows. Each task of a plan has a pull request of its own, so the issue
// alone does not say which pull request a read is of.
type TaskPullRequest struct {
	Issue, Task string
}

// basePush records that a pull request's base branch moved. Two facts are such a push: the push
// Envoy publishes for a branch that is no issue's, which is the shape of a branch pull requests sit
// on (intake.BasePush), and a push on an issue's or a task's branch (intake.Push), on which the pull
// request of a task that needs that task is based, walked here after the push's own handling.
// GitHub recomputes whether each head on that base still merges into it, and sends no event for the
// pull request itself while it does, so a head the record holds as clean can already be behind or
// dirty with nothing in the record saying so. Recording the commit the push left the base at says
// which base the recorded mergeability and merge state were read against, so the next read of the
// pull request is known to be needed rather than waited for; it decides nothing else, and the phase
// of every issue is left exactly where it is. The pull request's own facts - a head, a
// settlement, a review - are what move an issue, and a base that moved is none of them. So the
// write below is one of the two in this package that write a pull request row through the store
// rather than through the helper that marks the issue moved (putPullRequest): the one field it
// writes is no news of the pull request (pullRequestNews), the record of when the issue last
// moved is left where it stands, and the walk holds each row already, which a read apiece would
// only repeat. It writes that field alone (record.Store.SetPullRequestBaseMoved): the walk's
// records come from a list read, which loads none of what each pull request remembers of its
// settled heads, and a write of the whole record would delete that memory, which PutPullRequest
// refuses.
//
// One fact serves every pull request on the base, so the fact lock is taken once per (repo, base)
// push however many pull requests sit on it, rather than once per pull request. The first read is
// one indexed lookup of the open pull requests recorded on that base
// (record.Store.OpenPullRequestsOnBase), so a push to a branch Legion records no pull request on -
// every push of the other automation a heard repository carries - ends the transaction there,
// having written nothing. A pull request whose recorded base is still the empty one no read has
// filled matches no push: the daemon's next read of that pull request - at a handoff, at its
// start, or for a round waiting on the merge state - fills the base, and from then on its base's
// pushes find it.
//
// The one walk both records the move and decides the read a moved base calls for (mergeStateOwed,
// under the gate's base-push reason, classify.MergeStateFactBasePush), which is why a base push is
// the one fact Apply's askMergeState leaves alone: what tells a redelivered push from a first
// delivery is the commit the record held when the push arrived, and this walk is the last reader
// of it. The reads it owes are asked for together, once the transaction commits
// (Config.ReadBaseMergeStates): one batch of reads of the base for every pull request the push
// owes one, where a read of each was asked for apiece before, so the daemon reads the owed pull
// requests of a base a few to a GraphQL query rather than one REST read each.
func (e *Engine) basePush(ctx context.Context, tx pgx.Tx, fact intake.BasePush) (intake.Result, error) {
	open, err := e.store.OpenPullRequestsOnBase(ctx, tx, fact.Repo, fact.Branch)
	if err != nil {
		return intake.Result{}, err
	}
	var owed []TaskPullRequest
	for _, pr := range open {
		if pr.BaseMoved == fact.After {
			// The same push redelivered, or a push that returned the base to the commit the
			// pending read is already of: the record says what it would say, and the read it
			// calls for was asked for by the delivery that recorded it.
			continue
		}
		// A GitHub fact applies to an issue only when it names the issue's repository
		// (ownPullRequest), which a record written in a clone's spelling can differ from.
		own, err := e.ownPullRequest(ctx, tx, &pr, fact.Repo)
		if err != nil {
			return intake.Result{}, err
		}
		if own == nil {
			continue
		}
		if err := e.store.SetPullRequestBaseMoved(ctx, tx, own.Issue, own.Branch, fact.After); err != nil {
			return intake.Result{}, err
		}
		// A base that moved is what takes a head from clean to behind, which holds an approved
		// issue in reviewing, or to conflicting, which needs a merge forward once the issue's
		// review is over (dispatch://INFRA-4374 R0) and leaves a record in testing or reviewing
		// reading a state GitHub no longer answers (R4). So every pull request this push moved is
		// read for where the push owes a read (mergeStateOwed): an issue in testing through
		// merging, by the gate (classify.MergeStateReadOwed), or a round that waits on the merge
		// state. The read is decided here rather than from Apply's askMergeState because the line
		// above has just overwritten what tells a redelivery from a first delivery: a second walk
		// after the handler would find every pull request on the base recorded at this push's
		// commit and ask for all of their reads again, however many times the push was delivered.
		if e.cfg.ReadBaseMergeStates == nil {
			continue
		}
		read, err := e.mergeStateOwed(ctx, tx, own, classify.MergeStateFactBasePush)
		if err != nil {
			return intake.Result{}, err
		}
		if read {
			owed = append(owed, TaskPullRequest{Issue: own.Issue, Task: own.Task})
		}
	}
	if len(owed) > 0 {
		repo, base := fact.Repo, fact.Branch
		store.OnCommit(ctx, func() { e.cfg.ReadBaseMergeStates(repo, base, owed) })
	}
	return intake.Result{}, nil
}
