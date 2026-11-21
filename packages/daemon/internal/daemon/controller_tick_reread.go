package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
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

// blockerReadFanout is how many blocker keys one turn reads one at a time: up to this many are
// read singly, and above it the whole project is listed once instead. The listing is a single
// unpaged request (dispatch.HTTPClient.ListIssues) and Dispatch caps an issue at 20 blockers, so
// the fan-out a waiting set can ask for is bounded on both sides of the choice.
const blockerReadFanout = 25

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
// read-only transaction: the judgement the deciders themselves make, read rather than remade
// (blockers.Blocked), over every root on the waiting line (record.Waiting), every recorded child
// a blocker holds at phase.Admitted, every open Dispatch child holding a parent's own first
// phase (record.ChildWaitsOf), and every open child a parent waiting in awaiting_children waits
// on (blockers.ChildRows). A candidate the rule exempts is read for nothing, as promotion admits
// it whatever its links say and nothing consults the answer (blockers.Exempt), and the keys come
// out in that judgement's own order, by the issue waiting on them. The open children of every
// other started parent follow (record.LineFacts.StartedParentWaits), which the admission pass
// moves into awaiting_children or leaves where it stands from the same judgement.
//
// An open child is in the set because the turn's re-read is what writes its row of the copy
// again: a reading of all five values, each of which replaces the one an older reading wrote. A
// child the architect filed and has not released has no event this daemon consumes — it reaches
// the copy from a reading of Dispatch alone — so without this its row would stand until the next
// boot, and its move to icebox, to another parent or out from under its parent would not end the
// wait it holds.
//
// parents is every issue whose phases have started (record.StartedParents): one in planning
// through accepting, held from one of them, owing its worker a start, or waiting in
// awaiting_children (dispatch://INFRA-3320 R13). A child filed under such an issue is in no
// mirror row until a listing shows it, and the admission pass holds the issue's next start, or
// stops the phase under way, the moment the mirror does, so the turn reads the children of each
// of them whatever the number of keys (readObserved).
func (w *workflowRuntime) waitedOn(ctx context.Context) ([]string, []string, error) {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issues, err := w.records.LiveIssues(ctx, tx, w.dispatchProject)
	if err != nil {
		return nil, nil, fmt.Errorf("list the issues of %s: %w", w.dispatchProject, err)
	}
	slots, err := w.records.Slots(ctx, tx)
	if err != nil {
		return nil, nil, fmt.Errorf("list the admission slots: %w", err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, w.records, w.dispatchProject, issues, w.gatedSteps)
	if err != nil {
		return nil, nil, err
	}
	// Judged through the one judgement admission itself promotes from (blockers.JudgeLine): the
	// rows of every issue a blocker can hold come before the line, which leaves such an issue out
	// and cannot judge that itself (record.LineFacts.Held); the issues the line names are read
	// after it, since only the line says which they are. This read wants those keys alone and
	// drops the held rows.
	rows, facts, line, _, err := blockers.JudgeLine(ctx, tx, w.records, w.dispatchProject, issues, slots, facts, nil)
	if err != nil {
		return nil, nil, err
	}
	var keys []string
	seen := make(map[string]struct{})
	add := func(key string) {
		if _, repeated := seen[key]; repeated {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, blocked := range blockers.Blocked(rows, issues, line, facts) {
		for _, blocker := range blocked.BlockedBy {
			add(blocker)
		}
	}
	parents := record.StartedParents(issues, facts.Owed)
	for _, parent := range parents {
		for _, child := range facts.StartedParentWaits[parent].Open {
			add(child.Key)
		}
	}
	return keys, parents, nil
}

// readObserved is Dispatch's answer for keys and for the children of parents. Where no issue has
// started a phase, it is one GetIssue for each key while they number blockerReadFanout or fewer;
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
// issue whose parent is one of parents. The listing already holds every key of this project, with
// the fields a single read gives (blockers.Observed), so the turn reads none of them singly as
// well: that would repeat up to blockerReadFanout requests on nearly every turn of a project at
// work. A child filed and not released reaches the daemon so within one wake interval, and so
// does its close, its move to icebox and its move to another parent, since every child the mirror
// already holds under a started parent is among keys (waitedOn). That branch takes the keys and
// the children alone, not the whole listing, so a turn writes the mirror rows of what it waits on
// and of those children and no others.
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
	if len(keys) > blockerReadFanout {
		summaries, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
		if err != nil {
			w.log.Warn("the controller tick's re-read of the project's issues failed", "blockers", len(keys), "error", err)
			return nil
		}
		observed := make([]intake.IssueStatus, 0, len(summaries))
		for _, summary := range summaries {
			observed = append(observed, blockers.Observed(w.dispatchProject, summary))
		}
		for _, key := range keys {
			if w.ownDispatchKey(key) {
				continue
			}
			if status, read := w.readBlocker(ctx, key); read {
				observed = append(observed, status)
			}
		}
		return observed
	}
	observed := make([]intake.IssueStatus, 0, len(keys))
	if len(parents) == 0 {
		for _, key := range keys {
			if status, read := w.readBlocker(ctx, key); read {
				observed = append(observed, status)
			}
		}
		return observed
	}
	own := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if w.ownDispatchKey(key) {
			own[key] = struct{}{}
			continue
		}
		if status, read := w.readBlocker(ctx, key); read {
			observed = append(observed, status)
		}
	}
	summaries, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
	if err != nil {
		w.log.Warn("the controller tick's read of the children of the issues whose phases have started failed", "parents", len(parents), "error", err)
		for _, key := range keys {
			if _, waited := own[key]; !waited {
				continue
			}
			if status, read := w.readBlocker(ctx, key); read {
				observed = append(observed, status)
			}
		}
		return observed
	}
	for _, summary := range summaries {
		_, waited := own[summary.Key]
		// parents comes sorted (record.StartedParents), so each summary's parent is one search.
		started := false
		if summary.Parent != nil {
			_, started = slices.BinarySearch(parents, *summary.Parent)
		}
		if waited || started {
			observed = append(observed, blockers.Observed(w.dispatchProject, summary))
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
// own prefix decides which keys the turn reads singly (ownDispatchKey); the project a mirror
// row carries is the one this read saw on the issue.
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

// ownDispatchKey says whether key is an issue of this daemon's own Dispatch project, by its key's
// project as intake's decodeDispatchFact reads it (`<PROJECT>-<number>`).
func (w *workflowRuntime) ownDispatchKey(key string) bool {
	project, _, _ := strings.Cut(key, "-")
	return project == w.dispatchProject
}
