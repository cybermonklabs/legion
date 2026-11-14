package docs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SettlementResumeInterval is how often the server arms the settlements documents still owe that no
// settlement committed.
const SettlementResumeInterval = time.Minute

// settlementResumeAge is how long a document's pending-settlement row stands before the resumption
// arms its settlement, whether or not the document's issue is closed. Every update refreshes the
// row and arms a settlement in the process that wrote it, which runs two seconds later, so a row
// this old was left by one that did not commit: a shutdown cut it short, a failure dropped it,
// perhaps in another process, or its issue's close stopped it (closeRecordLocked). On a rolling
// deploy that process stops after the new one has started, which is why this runs on an interval
// and not only at start.
const settlementResumeAge = time.Minute

// settlementResumeBatch is the most documents one resumption pass arms, closed and open issues'
// together; the rest wait for the passes after it. Each armed document loads its room and renders
// it, and cold room loads share the rooms pool's four connections (store/pool.go), which take them
// in turn. The last reading of the backlog was 21 documents, so one pass clears the known backlog,
// and a larger one drains at 50 a SettlementResumeInterval rather than all at once. That is
// reasoning, not a measurement: nobody has timed 50 settlements on the serving process.
const settlementResumeBatch = 50

// settlementResumptionLease names the lease each resumption pass runs under (duty.Exclusive). A
// settlement reads what it settles from the store under the document's lock, whichever process
// runs it, so one process arming the owed settlements is the whole of the work; every process
// arming them each period would run each owed document's settlement once per process.
const settlementResumptionLease = "settlement-resumption"

// RunSettlementResumption arms the settlement of the documents owing one past settlementResumeAge,
// at most settlementResumeBatch of them in a pass, at start and every SettlementResumeInterval,
// until ctx ends (resumeOwedSettlements). Each pass runs under settlementResumptionLease for the
// period SettlementResumeInterval (duty.RunLeased), so one of the envoy-dispatch processes serving
// the database runs it per period, in that process, and the next pass may run in another. A
// document nobody opens therefore settles too, whether or not its issue is closed: its ask blocks
// reach the open asks, or are saved resolved on a closed issue, without anyone loading it.
func (s *Service) RunSettlementResumption(ctx context.Context, leases duty.Leases) {
	duty.RunLeased(ctx, leases, settlementResumptionLease, SettlementResumeInterval, duty.TickAtStart, func(ctx context.Context) {
		if err := s.resumeOwedSettlements(ctx, settlementResumeAge); err != nil && ctx.Err() == nil {
			slog.Error("dispatch: resume owed document settlements", "error", err)
		}
	})
}

// resumeOwedSettlements arms the settlement of at most settlementResumeBatch documents whose
// pending-settlement row is at least age old, whether or not their issue is closed: a closed
// issue's document settles as an open one's does (settleRoomWithin).
//
// It picks the rows no pass has armed yet first, oldest marked_at first, and then the rows a pass
// has armed, oldest tried_at first, and stamps tried_at = now() on each row it picks, in the one
// statement that picks them. A row a pass armed is still here only when its settlement did not
// commit: it failed, its document is outside the Proof schema (ErrDocSchema), or it ended in any
// other way that leaves the row, since a committed settlement deletes the row and the stamp with
// it (clearSettlementPending), and an append's upsert of the row leaves the stamp alone
// (settlementPendingStatement). Such a document waits behind every document not yet tried, so a
// set of documents whose settlement fails every time never holds the bound. The stamp is in the
// database, not in this process's memory, because the pass runs under a lease
// (RunSettlementResumption): the next pass may run in another process, and it reads the same
// stamps.
//
// The pick skips a row another transaction holds (for update skip locked): that transaction is a
// settlement or an append working on the document, so the row is left to a later pass. Waiting
// for it while this statement holds the rows it already picked could deadlock with a settlement
// that marks a copy owed (owedCopiesOf): that settlement holds the copies' rows while it works and
// takes its own document's row as it commits, so a pass holding its row and waiting on a copy's
// would wait on a settlement that waits on the pass.
//
// The rows come back in no order, and none is needed: every document of the pass is armed with
// the same settle delay.
func (s *Service) resumeOwedSettlements(ctx context.Context, age time.Duration) error {
	ctx = store.WithTransactionTracking(ctx)
	rows, err := s.store.Pool.Query(ctx, `
		with picked as (
			select artifact_id from doc_settlements_pending
			where marked_at <= now() - make_interval(secs => $1)
			order by tried_at is not null, tried_at, marked_at
			limit $2
			for update skip locked
		)
		update doc_settlements_pending p set tried_at = now()
		from picked where p.artifact_id = picked.artifact_id
		returning p.artifact_id::text
	`, age.Seconds(), settlementResumeBatch)
	if err != nil {
		return fmt.Errorf("pick owed document settlements: %w", err)
	}
	// Drained before any is armed: an open cursor holds its connection, and a settlement takes
	// one. The statement's row locks last until it completes, and a settlement that commits
	// deletes its row, so it would wait for them too.
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			rows.Close()
			return fmt.Errorf("scan owed document settlement: %w", err)
		}
		rooms = append(rooms, room)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pick owed document settlements: %w", err)
	}
	for _, room := range rooms {
		s.scheduleSettle(room)
	}
	if len(rooms) > 0 {
		s.hooks.countSettlementResumption(rooms, time.Now())
		slog.Info("dispatch: resumed owed document settlements", "documents", len(rooms))
	}
	return nil
}
