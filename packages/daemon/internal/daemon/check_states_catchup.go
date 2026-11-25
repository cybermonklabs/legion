package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The boot's catch-up of check states: the check.state_changed events Dispatch appended to an
// issue's event log while this daemon did not take them — down, or a release before this one,
// which took them as no fact — read from that log and applied through the live decoder and the
// workflow's handler, so the states, the heads' sets and the serials stand as the live events
// would have left them, and a tester's move waiting on them decides.
//
// Each issue keeps a cursor (issues.check_states_seq, record.Store.CheckStatesSeqs): the boot
// reads only the events after it, and raises it to the last sequence it read, or to just below the
// first state event whose fact failed to apply, so the next boot reads that event again. Every
// issue starts at 0, the migration's default, so the first catch-up of each issue after the
// release reads its log from the start, and an issue the workflow records only later is read from
// the start at the boot after that.

// checkStatesIssue is one issue the boot's catch-up of check states reads: its key and its cursor.
type checkStatesIssue struct {
	key    string
	cursor int64
}

// catchUpCheckStates catches up the check states of every issue of the project that the workflow
// records, is not done and does not linger, one after another, before the durable consumers start.
// An issue in any phase is read: a state held while the issue plans or implements is what its
// later testing move's fence counts, and the workflow's handler moves only an issue in testing
// whose tester completed (workflow's checkState). A done or lingering issue has no head left to
// test, and the merge that closed it deleted its states.
//
// It follows the retry rule of every boot catch-up (readWithRetries): an issue Dispatch refuses
// is read again for an error Dispatch may stop giving, then passed over, logged, with its cursor
// where it stood, so the next boot reads it again; Dispatch not answering at all, an issue's own
// deadline passing or a deadline of the listing makes the attempt transient, and the boot's
// readiness retry waits it out. An issue it finished, caught up or passed over, is remembered
// across those retries (checkStatesCaughtUp), so a retry resumes after it.
func (w *workflowRuntime) catchUpCheckStates(ctx context.Context) error {
	if w.dispatch == nil {
		return nil
	}
	var issues []checkStatesIssue
	listing, cancel := context.WithTimeout(ctx, bootTimeout)
	err := store.TransactWith(listing, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		recorded, err := w.records.Issues(ctx, tx)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(recorded))
		for _, issue := range recorded {
			if issue.Phase == phase.Done || issue.Lingers() {
				continue
			}
			keys = append(keys, issue.Key)
		}
		cursors, err := w.records.CheckStatesSeqs(ctx, tx, keys)
		if err != nil {
			return err
		}
		issues = make([]checkStatesIssue, 0, len(keys))
		for _, key := range keys {
			issues = append(issues, checkStatesIssue{key: key, cursor: cursors[key]})
		}
		return nil
	})
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return &dispatch.TransientError{Err: fmt.Errorf("list the issues whose check states to catch up: %w", err)}
		}
		return fmt.Errorf("list the issues whose check states to catch up: %w", err)
	}
	if w.checkStatesCaughtUp == nil {
		w.checkStatesCaughtUp = map[string]struct{}{}
	}
	for _, issue := range issues {
		if _, ok := w.checkStatesCaughtUp[issue.key]; ok {
			continue
		}
		if _, err := w.readWithRetries(ctx, issue.key, "the check states of an issue", func(attempt context.Context) error {
			return w.catchUpIssueStates(attempt, issue)
		}); err != nil {
			return err
		}
		w.checkStatesCaughtUp[issue.key] = struct{}{}
	}
	return nil
}

// catchUpIssueStates reads issue's event log after its cursor, applies every check.state_changed
// event in it, and raises the cursor to the last sequence it read, or to just below the lowest
// sequence of a state event whose fact failed to apply.
//
// It treats each event as the live Dispatch consumer does. An event of any other type is passed
// over before it is decoded: the issue's other events are the live consumer's and the asks'
// catch-up's. A state event the decoder refuses is logged and passed over for good, since the live
// consumer terminates it as poison. A state event whose fact fails to apply — the handler's error,
// or the database's — is logged, and the rest of the log is still applied, but the cursor stops
// just below it, so the next boot reads it again, as the live consumer retries it until it
// applies; that matters once an outage outlasts the notification stream's retention, when the
// event log is the only place the event is still held. Only the boot's own end (ctx) stops it.
func (w *workflowRuntime) catchUpIssueStates(ctx context.Context, issue checkStatesIssue) error {
	events, err := w.dispatch.EventsAfter(ctx, issue.key, issue.cursor)
	if err != nil {
		return fmt.Errorf("read the events of %s after %d: %w", issue.key, issue.cursor, err)
	}
	var read, failed int64
	for _, event := range events {
		var header struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event, &header); err != nil {
			return fmt.Errorf("read the events of %s: %w", issue.key, err)
		}
		read = max(read, header.Seq)
		if header.Type != "check.state_changed" {
			continue
		}
		fact, unread, err := intake.DecodeDispatchEvent(w.dispatchProject, issue.key, event)
		if err != nil {
			w.log.Warn("boot catch-up passed over a state event it could not read", "issue", issue.key, "seq", header.Seq, "error", err)
			continue
		}
		if len(unread) > 0 {
			w.log.Warn("boot catch-up read a state event with fields it took as unknown", "issue", issue.key, "seq", header.Seq, "unread", unread)
		}
		if err := w.applyCheckStateCatchUp(ctx, issue.key, header.Seq, fact); err != nil {
			if ctx.Err() != nil {
				return err
			}
			w.log.Error("boot catch-up failed to apply a state event, and reads it again at the next boot", "issue", issue.key, "seq", header.Seq, "error", err)
			if failed == 0 || header.Seq < failed {
				failed = header.Seq
			}
		}
	}
	through := read
	if failed > 0 {
		through = failed - 1
	}
	if through <= issue.cursor {
		return nil
	}
	return store.TransactWith(ctx, w.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return w.records.SetCheckStatesSeq(ctx, tx, issue.key, through)
	})
}

// applyCheckStateCatchUp applies fact, the state event at seq of issue's log, under a processed id
// that names this boot (bootID), so the catch-up applies every state event it reads, whether or not
// processed_events records the event from the live consumer (`dispatch-<event id>`) or from an
// earlier boot's catch-up. Skipping a recorded event instead, as the asks' catch-up skips one it
// applied before, would lose states for good after the way back of the check states' migration:
// that way back drops the tables and the cursor and leaves processed_events, so the roll-forward's
// first catch-up reads every log from the start and would find each state event this release
// applied before the way back recorded. Applying one again is safe: the store keeps a state and a
// head's set only over a lower sequence and holds a serial once (record.Store.PutCheckState,
// PutCheckSet, PutCheckStateSerial), so an event the store already holds changes nothing, and the
// tester's move it decides again is the one the held states already call for. Within one boot the
// id is the same, so a retry of the boot's attempt does not apply an event twice. What it costs is
// one fact transaction per state event a boot reads after the cursor, at a boot, before any
// consumer applies a fact.
func (w *workflowRuntime) applyCheckStateCatchUp(ctx context.Context, issue string, seq int64, fact intake.Fact) error {
	id := fmt.Sprintf("check-states-catch-up:%s:%d:%s", issue, seq, w.bootID)
	if _, err := intake.ApplyFact(ctx, w.pool, catchUpSource, id, fact, w.handlers...); err != nil {
		return fmt.Errorf("apply %s: %w", id, err)
	}
	return nil
}
