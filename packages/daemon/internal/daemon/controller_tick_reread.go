package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// applyControllerTick is one turn: it re-reads from Dispatch the issues anything is currently
// waiting on and applies the tick carrying what it saw. The read runs here, before the fact,
// because every handler decides inside the fact's transaction and performs no I/O; the mirror
// handler writes the rows there, ahead of the two handlers that judge from them. So a blocker
// whose closing event the daemon never consumed is caught within one wake interval instead of
// standing until the next boot's listing.
func (w *workflowRuntime) applyControllerTick(ctx context.Context, at time.Time) {
	eventID := fmt.Sprintf("controller-tick:%s:%s:%d", w.dispatchProject, w.bootID, at.UnixNano())
	fact := intake.ControllerTick{Observed: w.observeWaitedOn(ctx)}
	if _, err := intake.ApplyFact(ctx, w.pool, "controller", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply the controller wake tick failed", "error", err)
	}
}

// observeWaitedOn is what Dispatch says now about the issues this daemon is holding something
// back for, and about the children of every issue whose phases have started. It is empty on a
// turn where nothing waits and no issue has started a phase — nothing read from Dispatch at all —
// and empty is the tick exactly as it has always been.
//
// A failed read never costs the turn: it is logged and the tick is applied with whatever was
// read, since the tick is also the controller's daily-report wake and the only wake a tree
// waiting on a refused root claim gets.
func (w *workflowRuntime) observeWaitedOn(ctx context.Context) []intake.IssueStatus {
	keys, parents, err := w.waitedOn(ctx)
	if err != nil {
		w.log.Warn("the controller tick's read of what this project waits on failed", "error", err)
		return nil
	}
	if len(keys) == 0 && len(parents) == 0 {
		return nil
	}
	return w.readObserved(ctx, keys, parents)
}

// waitedOn is the distinct keys of everything this daemon holds something back for, in one short
// read-only transaction: blockers.WaitedOn's selection of the keys and of the parents whose
// Dispatch children the turn reads, which a replay of a recorded window calls too, so the two read
// the same keys and children. It judges under the daemon's own rule, and the clock it hands over
// decides nothing there; it is the runtime's own (clockNow), the one every other judgement of this
// runtime reads.
func (w *workflowRuntime) waitedOn(ctx context.Context) ([]string, []string, error) {
	var keys, parents []string
	err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		keys, parents, err = blockers.WaitedOn(ctx, tx, w.records, w.dispatchProject, w.gatedSteps, record.MainRule, w.clockNow())
		return err
	})
	return keys, parents, err
}

// readObserved is Dispatch's answer for keys and for the children of parents, read by the plan a
// replay of a recorded window reads its ticks by too (blockers.ReadPlan). Where no issue has
// started a phase, it is one GetIssue for each key while they number blockers.ReadFanout or fewer;
// above that it is one whole-project listing, whose every summary is taken — it is one request
// for all of them, and a summary of a key not asked for is a reading of Dispatch like any other,
// which changes a value only where the copy holds it from an older reading or from none
// (record.Store.PutDispatchIssue). A key of another project is read one at a time in every
// branch, since the listing is of this daemon's project alone and names no issue of another.
//
// The children of parents are read from the listing, which is the one read of Dispatch the daemon
// makes that names an issue's children (dispatch.IssueSummary.Parent). Above the bound the listing
// takes them with every other summary. At or below it, where some parent stands, the turn lists
// the project once and takes the summary of each key of this project it waits on and of each
// issue whose parent is one of parents (blockers.Taken). The listing already holds every key of
// this project, with the fields a single read gives (blockers.Observed), so the turn reads none of
// them singly as well: that would repeat up to blockers.ReadFanout requests on nearly every turn of
// a project at work. A child filed and not released reaches the daemon so within one wake
// interval, and so does its close, its move to icebox and its move to another parent, since every
// child the mirror already holds under a started parent is among keys (waitedOn). That branch
// takes the keys and the children alone, not the whole listing, so a turn writes the mirror rows
// of what it waits on and of those children and no others.
//
// This turn is the only reader of a foreign key: an event whose subject key is another project's
// is dropped undecoded (intake.decodeDispatchFact), so a foreign blocker's close reaches the
// daemon only at its next turn: the first turn comes about a minute after the runtime starts
// (firstControllerTick), then one every controller_wake_interval_seconds, so the close arrives at
// most one interval after it, and a link to a foreign issue already done counts as unfinished
// until that turn too (blockers.Unfinished). The cost is one request per
// foreign key, sequential, on the wake path and outside the tick's transaction: it is zero for a
// project waiting on its own issues alone, and begins with the first foreign link.
//
// A key whose read fails is logged and left out, and so is one Dispatch no longer serves, which
// is no error at all: the rest of the turn goes on with what was read, and the row of a key left
// out keeps the last thing Dispatch said about it. A listing that fails is logged and leaves out
// what it would have read: above the bound the turn goes on with nothing read, and where a parent
// stands it leaves out the children of parents, and the turn reads this project's keys one at a
// time instead, as it reads them where no parent stands.
func (w *workflowRuntime) readObserved(ctx context.Context, keys, parents []string) []intake.IssueStatus {
	listProject, all, single := blockers.ReadPlan(w.dispatchProject, keys, parents)
	if all {
		summaries, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
		if err != nil {
			w.log.Warn("the controller tick's re-read of the project's issues failed", "blockers", len(keys), "error", err)
			return nil
		}
		observed := make([]intake.IssueStatus, 0, len(summaries)+len(single))
		for _, summary := range summaries {
			observed = append(observed, blockers.Observed(w.dispatchProject, summary))
		}
		return w.readSingly(ctx, observed, single)
	}
	observed := w.readSingly(ctx, make([]intake.IssueStatus, 0, len(keys)), single)
	if !listProject {
		return observed
	}
	summaries, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
	if err != nil {
		w.log.Warn("the controller tick's read of the children of the issues whose phases have started failed", "parents", len(parents), "error", err)
		var own []string
		for _, key := range keys {
			if blockers.OwnKey(w.dispatchProject, key) {
				own = append(own, key)
			}
		}
		return w.readSingly(ctx, observed, own)
	}
	for _, summary := range summaries {
		if blockers.Taken(w.dispatchProject, summary, keys, parents) {
			observed = append(observed, blockers.Observed(w.dispatchProject, summary))
		}
	}
	return observed
}

// readSingly is observed with the answer of each of keys read one at a time (readBlocker) after
// it, a key whose read fails or that Dispatch no longer serves left out.
func (w *workflowRuntime) readSingly(ctx context.Context, observed []intake.IssueStatus, keys []string) []intake.IssueStatus {
	for _, key := range keys {
		if status, read := w.readBlocker(ctx, key); read {
			observed = append(observed, status)
		}
	}
	return observed
}

// readBlocker is one key read singly, and whether Dispatch answered for it. The observation
// carries all five values the copy holds of an issue — its status, its parent, its blockers, its
// labels and its rank — and the project Dispatch names on the issue, the one read that answers
// it, so the row of a blocker outside this project is that project's rather than this daemon's
// (blockers.Mirror.put).
//
// An answer that names no project at all is left out as a failed read is, and logged the same
// way: its row would be written under the empty project, which drops it out of every project's
// read, and leaving it out keeps the row as the last answer that named one left it. The key's
// own prefix decides which keys the turn reads singly (blockers.ReadPlan, blockers.OwnKey); the
// project a mirror row carries is the one this read saw on the issue.
func (w *workflowRuntime) readBlocker(ctx context.Context, key string) (intake.IssueStatus, bool) {
	issue, err := w.dispatch.GetIssue(ctx, key)
	if err != nil {
		if dispatch.Missing(err) {
			w.log.Info("the controller tick re-read a blocker Dispatch no longer serves", "issue", key)
			return intake.IssueStatus{}, false
		}
		w.log.Warn("the controller tick's re-read of a blocker failed", "issue", key, "error", err)
		return intake.IssueStatus{}, false
	}
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	if issue.Project == "" {
		w.log.Warn("the controller tick re-read a blocker whose answer names no project", "issue", key)
		return intake.IssueStatus{}, false
	}
	return intake.IssueStatus{Key: key, Project: issue.Project, Status: issue.Status, Blockers: issue.BlockedBy,
		Seq: issue.LastSeq, ClaimedBy: issue.Claim.Holder(), ClaimKnown: !issue.ClaimUnread, Parent: parent,
		Labels: issue.Labels, Rank: issue.Rank}, true
}
