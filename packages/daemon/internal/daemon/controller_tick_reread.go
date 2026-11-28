package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
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
//
// Once the tick is applied, or its apply failed, the turn writes one line for each read of a key
// of another project (logForeignRead). That read is the one way the daemon learns of such a key's
// close (readObserved), and what the copy did with the reading is settled only by the apply, so
// the line comes after it.
func (w *workflowRuntime) applyControllerTick(ctx context.Context, at time.Time) {
	eventID := fmt.Sprintf("controller-tick:%s:%s:%d", w.dispatchProject, w.bootID, at.UnixNano())
	observed, foreign := w.observeWaitedOn(ctx)
	fact := intake.ControllerTick{Observed: observed}
	_, err := intake.ApplyFact(ctx, w.pool, "controller", eventID, fact, w.handlers...)
	if err != nil {
		w.log.Warn("apply the controller wake tick failed", "error", err)
	}
	for _, read := range foreign {
		w.logForeignRead(read, err == nil)
	}
}

// foreignRead is one turn's read of a key of another project and what the turn knew of the key
// before it, for the one line the turn writes of that read (logForeignRead).
type foreignRead struct {
	key string
	// waiting is the issues of this project that wait on key, sorted, as the judgement waitedOn
	// reads names them (blockers.Blocked); empty for a key the turn reads for another reason.
	waiting []string
	// copied is the copy's row of key before the turn, nil where the copy held none.
	copied *record.DispatchIssueRow
	// reading is what Dispatch served, where err is nil; err is why the read failed (readBlocker).
	reading intake.IssueStatus
	err     error
}

// logForeignRead writes, at info, the one line of a turn's read of a key of another project, once
// the tick carrying the reading is applied (written) or its apply failed: the key, the issues
// waiting on it, what Dispatch served (the reading's status and sequence, the issue's own
// last_seq) or why the read failed, and copy, what the copy did with the reading. That is applied
// where the copy held no row of the key or one at a lower sequence before the turn, the fence of
// record.Store.PutDispatchIssue; kept where the copy's sequence was not lower, since the fence
// keeps the copy's value at an equal sequence; read failed; or not written where the apply
// failed. Every reading the copy takes carries all five values (blockers.readingOf, the one
// production path into PutDispatchIssue), so the row's sequence, dispatch_seq, is each value's and
// decides the fence for all of them. With kept and read failed the line names the row the copy
// held, left out where it held no row.
func (w *workflowRuntime) logForeignRead(read foreignRead, written bool) {
	attrs := []any{"blocker", read.key, "waiting", read.waiting}
	if read.err != nil {
		attrs = append(attrs, "error", read.err, "copy", "read failed")
		if read.copied != nil {
			attrs = append(attrs, "copy_status", read.copied.Status, "copy_seq", read.copied.Seq)
		}
	} else {
		attrs = append(attrs, "served_status", read.reading.Status, "served_seq", read.reading.Seq)
		switch {
		case !written:
			attrs = append(attrs, "copy", "not written")
		case read.copied != nil && read.copied.Seq >= read.reading.Seq:
			attrs = append(attrs, "copy", "kept", "copy_status", read.copied.Status, "copy_seq", read.copied.Seq)
		default:
			attrs = append(attrs, "copy", "applied")
		}
	}
	w.log.Info("the controller tick re-read a blocker of another project", attrs...)
}

// blockerReadFanout is how many blocker keys one turn reads one at a time: up to this many are
// read singly, and above it the whole project is listed once instead. The listing is a single
// unpaged request (dispatch.HTTPClient.ListIssues) and Dispatch caps an issue at 20 blockers, so
// the fan-out a waiting set can ask for is bounded on both sides of the choice.
const blockerReadFanout = 25

// observeWaitedOn is what Dispatch says now about the issues this daemon is holding something
// back for, and about the children of every issue whose phases have started, with each read of a
// key of another project among them, for its line (foreignRead). It is empty on a turn where
// nothing waits and no issue has started a phase — nothing read from Dispatch at all — and empty
// is the tick exactly as it has always been.
//
// A failed read never costs the turn: it is logged and the tick is applied with whatever was
// read, since the tick is also the controller's daily-report wake and the only wake a tree
// waiting on a refused root claim gets.
func (w *workflowRuntime) observeWaitedOn(ctx context.Context) ([]intake.IssueStatus, []foreignRead) {
	waiting, err := w.waitedOn(ctx)
	if err != nil {
		w.log.Warn("the controller tick's read of what this project waits on failed", "error", err)
		return nil, nil
	}
	if len(waiting.keys) == 0 && len(waiting.parents) == 0 {
		return nil, nil
	}
	return w.readObserved(ctx, waiting)
}

// waitingSet is what the turn reads from Dispatch, as waitedOn reads it: keys, what this daemon
// holds something back for; parents, the issues whose children the turn reads; and foreign, the
// turn's entry for each key of another project among keys.
type waitingSet struct {
	keys, parents []string
	foreign       map[string]foreignRead
}

// waitedOn's keys are the distinct keys of everything this daemon holds something back for, in one
// short read-only transaction: the judgement the deciders themselves make, read rather than remade
// (blockers.Blocked), over every root on the waiting line (record.Waiting), every recorded child
// a blocker holds at phase.Admitted, every open Dispatch child holding a parent's own first
// phase (record.ChildWaitsOf), and every open child a parent waiting in awaiting_children waits
// on (blockers.ChildRows). A candidate the rule exempts is read for nothing, as promotion admits
// it whatever its links say and nothing consults the answer (blockers.Exempt), and the keys come
// out in that judgement's own order, by the issue waiting on them. Then come the open children of
// every other started parent (record.LineFacts.StartedParentWaits), which the admission pass moves
// into awaiting_children or leaves where it stands from the same judgement, and of every parent
// held before its retro (record.RetroHolds.Open, in record.LineFacts.RetroHolds), whose retro the
// admission pass starts once none of them is open (admit's childrenWaits). Both kinds of parent
// are sorted together by key, and each parent's open children are added in that order, a key
// already added keeping the place it took first.
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
// awaiting_children (dispatch://INFRA-3320 R13), and every parent held before its retro (R8), which
// an open Dispatch child holds as well: the keys of record.LineFacts.RetroHolds, which holds an
// entry for each such parent (record.RetroHeldParents) whatever holds it. A child filed under
// such an issue is in no mirror row until a listing shows it, and the admission pass holds the
// issue's next start, stops the phase under way, or holds its retro, the moment the mirror does,
// so the turn reads the children of each of them whatever the number of keys (readObserved).
//
// foreign is an entry for each key of another project among the keys (foreignReads), read in this
// same transaction, so the line the turn writes of its read of such a key names the issues
// waiting on it and the copy's row of it as they stood before the tick wrote anything.
func (w *workflowRuntime) waitedOn(ctx context.Context) (waitingSet, error) {
	var waiting waitingSet
	err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issues, err := w.records.LiveIssues(ctx, tx, w.dispatchProject)
		if err != nil {
			return fmt.Errorf("list the issues of %s: %w", w.dispatchProject, err)
		}
		slots, err := w.records.Slots(ctx, tx)
		if err != nil {
			return fmt.Errorf("list the admission slots: %w", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, w.records, w.dispatchProject, issues, w.gatedSteps)
		if err != nil {
			return err
		}
		// Judged through the one judgement admission itself promotes from (blockers.JudgeLine):
		// the rows of every issue a blocker can hold come before the line, which leaves such an
		// issue out and cannot judge that itself (record.LineFacts.Held); the issues the line
		// names are read after it, since only the line says which they are. This read wants those
		// keys alone and drops the held rows.
		rows, facts, line, _, err := blockers.JudgeLine(ctx, tx, w.records, w.dispatchProject, issues, slots, facts, nil)
		if err != nil {
			return err
		}
		seen := make(map[string]struct{})
		add := func(key string) {
			if _, repeated := seen[key]; repeated {
				return
			}
			seen[key] = struct{}{}
			waiting.keys = append(waiting.keys, key)
		}
		blocked := blockers.Blocked(rows, issues, line, facts)
		for _, wait := range blocked {
			for _, blocker := range wait.BlockedBy {
				add(blocker)
			}
		}
		waiting.parents = slices.AppendSeq(record.StartedParents(issues, facts.Owed), maps.Keys(facts.RetroHolds))
		slices.Sort(waiting.parents)
		waiting.parents = slices.Compact(waiting.parents)
		for _, parent := range waiting.parents {
			for _, child := range facts.StartedParentWaits[parent].Open {
				add(child.Key)
			}
			for _, child := range facts.RetroHolds[parent].Open {
				add(child)
			}
		}
		// Every chain the copy cannot read to its top is read again as well (R16 of
		// dispatch://INFRA-3320): the issue and each link of its chain as the copy holds it, up to
		// record.MaxChainLinks, present rows included, so a row a stale reading left is replaced
		// by Dispatch's own answer. The daemon's own re-read (rereadChains) reads such a chain
		// within seconds of a pass finding it; the turn reads every one the copy holds once more.
		for _, key := range facts.Chains.Unknown() {
			add(key)
			chain := facts.Chains.Chain(key)
			for _, link := range chain.Links {
				add(link)
			}
			if chain.At != "" {
				add(chain.At)
			}
		}
		waiting.foreign, err = w.foreignReads(ctx, tx, waiting.keys, blocked)
		return err
	})
	return waiting, err
}

// foreignReads is an entry for each key of another project among keys, read in tx: the issues
// blocked names as waiting on it, which come sorted since blocked comes by issue
// (blockers.Blocked), and the copy's row of it, which DispatchIssuesOf reads whatever its
// project. It is nil where every key is this project's, and then reads nothing.
func (w *workflowRuntime) foreignReads(ctx context.Context, tx pgx.Tx, keys []string, blocked []record.BlockedIssue) (map[string]foreignRead, error) {
	var foreignKeys []string
	for _, key := range keys {
		if !w.ownDispatchKey(key) {
			foreignKeys = append(foreignKeys, key)
		}
	}
	if len(foreignKeys) == 0 {
		return nil, nil
	}
	reads := make(map[string]foreignRead, len(foreignKeys))
	for _, key := range foreignKeys {
		reads[key] = foreignRead{key: key, waiting: []string{}}
	}
	for _, wait := range blocked {
		for _, blocker := range wait.BlockedBy {
			if read, named := reads[blocker]; named {
				read.waiting = append(read.waiting, wait.Issue)
				reads[blocker] = read
			}
		}
	}
	rows, err := w.records.DispatchIssuesOf(ctx, tx, foreignKeys)
	if err != nil {
		return nil, fmt.Errorf("read the copy's rows of the keys of other projects: %w", err)
	}
	// The read answers the rows those rows wait on as well; only a key asked for is taken.
	for _, row := range rows {
		if read, asked := reads[row.Key]; asked {
			read.copied = &row
			reads[row.Key] = read
		}
	}
	return reads, nil
}

// readObserved is Dispatch's answer for waiting's keys and parents' children. Where no issue has
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
// project waiting on its own issues alone, and begins with the first foreign link. So each read
// of a foreign key leaves one line at info, failed or not (logForeignRead): no event of such a key
// reaches the daemon, so that line is the one record of the read, of what Dispatch served and of
// what the copy did with it. A key of this project has its events besides, and its read logs only
// where it fails.
//
// A key whose read fails is left out, and so is one Dispatch no longer serves, which is no error
// at all: the rest of the turn goes on with what was read, and the row of a key left out keeps
// the last thing Dispatch said about it. Such a read of a key of this project is logged here
// (logBlockerFailure); every read of a key of another project, one of foreign (waitedOn), is
// returned instead, failed or not, in the order the turn made them, for its line. A listing that
// fails is logged and leaves out what it would have read: above the bound the turn goes on with
// nothing read, and where a parent stands it leaves out the children of parents, and the turn
// reads this project's keys one at a time instead, as it reads them where no parent stands.
func (w *workflowRuntime) readObserved(ctx context.Context, waiting waitingSet) ([]intake.IssueStatus, []foreignRead) {
	var observed []intake.IssueStatus
	var reads []foreignRead
	// readSingly is one key read alone (readBlocker): Dispatch's answer joins observed, the read of
	// a key of another project joins reads with the answer or the failure, and a failed read of a
	// key of this project is logged.
	readSingly := func(key string) {
		status, err := w.readBlocker(ctx, key)
		if read, other := waiting.foreign[key]; other {
			read.reading, read.err = status, err
			reads = append(reads, read)
		} else if err != nil {
			w.logBlockerFailure(key, err)
		}
		if err == nil {
			observed = append(observed, status)
		}
	}
	if len(waiting.keys) > blockerReadFanout {
		summaries, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
		if err != nil {
			w.log.Warn("the controller tick's re-read of the project's issues failed", "blockers", len(waiting.keys), "error", err)
			return nil, nil
		}
		observed = make([]intake.IssueStatus, 0, len(summaries))
		for _, summary := range summaries {
			observed = append(observed, blockers.Observed(w.dispatchProject, summary))
		}
		for _, key := range waiting.keys {
			if w.ownDispatchKey(key) {
				continue
			}
			readSingly(key)
		}
		return observed, reads
	}
	observed = make([]intake.IssueStatus, 0, len(waiting.keys))
	if len(waiting.parents) == 0 {
		for _, key := range waiting.keys {
			readSingly(key)
		}
		return observed, reads
	}
	own := make(map[string]struct{}, len(waiting.keys))
	for _, key := range waiting.keys {
		if w.ownDispatchKey(key) {
			own[key] = struct{}{}
			continue
		}
		readSingly(key)
	}
	summaries, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
	if err != nil {
		w.log.Warn("the controller tick's read of the children of the issues whose phases have started failed", "parents", len(waiting.parents), "error", err)
		for _, key := range waiting.keys {
			if _, waited := own[key]; !waited {
				continue
			}
			readSingly(key)
		}
		return observed, reads
	}
	for _, summary := range summaries {
		_, waited := own[summary.Key]
		// parents comes sorted (record.StartedParents), so each summary's parent is one search.
		started := false
		if summary.Parent != nil {
			_, started = slices.BinarySearch(waiting.parents, *summary.Parent)
		}
		if waited || started {
			observed = append(observed, blockers.Observed(w.dispatchProject, summary))
		}
	}
	return observed, reads
}

// readBlocker is one key read singly: Dispatch's answer for it, or why there is none. The
// observation carries all five values the copy holds of an issue — its status, its parent, its
// blockers, its labels and its rank — and the project Dispatch names on the issue, the one read
// that answers it, so the row of a blocker outside this project is that project's rather than this
// daemon's (blockers.Mirror.put).
//
// Why there is none is the request's error, which for a key Dispatch no longer serves says so and
// still answers dispatch.Missing, or errNoProject for an answer that names no project at all: its
// row would be written under the empty project, which drops it out of every project's read, and
// leaving it out as a failed read is keeps the row as the last answer that named one left it. The
// caller logs the reason (logBlockerFailure, logForeignRead). The key's own prefix decides which
// keys the turn reads singly (ownDispatchKey); the project a mirror row carries is the one this
// read saw on the issue.
func (w *workflowRuntime) readBlocker(ctx context.Context, key string) (intake.IssueStatus, error) {
	issue, err := w.dispatch.GetIssue(ctx, key)
	if dispatch.Missing(err) {
		return intake.IssueStatus{}, fmt.Errorf("Dispatch no longer serves it: %w", err)
	}
	if err != nil {
		return intake.IssueStatus{}, err
	}
	reading, ok := intake.ReadingOf(key, issue)
	if !ok {
		return intake.IssueStatus{}, errNoProject
	}
	return reading, nil
}

// errNoProject is readBlocker's reason for an answer that names no project.
var errNoProject = errors.New("Dispatch's answer names no project")

// logBlockerFailure logs a failed read of a key of this project: a key Dispatch no longer serves
// at info, since that is no error at all, and a failed request or an answer that names no project
// as a warning.
func (w *workflowRuntime) logBlockerFailure(key string, err error) {
	switch {
	case dispatch.Missing(err):
		w.log.Info("the controller tick re-read a blocker Dispatch no longer serves", "issue", key)
	case errors.Is(err, errNoProject):
		w.log.Warn("the controller tick re-read a blocker whose answer names no project", "issue", key)
	default:
		w.log.Warn("the controller tick's re-read of a blocker failed", "issue", key, "error", err)
	}
}

// ownDispatchKey says whether key is an issue of this daemon's own Dispatch project, by its key's
// project as intake's decodeDispatchFact reads it (claim.KeyProject).
func (w *workflowRuntime) ownDispatchKey(key string) bool {
	return claim.KeyProject(key) == w.dispatchProject
}
