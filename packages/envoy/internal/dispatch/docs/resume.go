package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SettlementResumeInterval is how often the server arms the settlements documents still owe that no
// settlement committed. A test process shortens it (Deps.SettlementResume); every deployment runs
// at this period.
const SettlementResumeInterval = time.Minute

// settlementResumeAge is how long a document's pending-settlement row stands before the resumption
// arms its settlement, whether or not the document's issue is closed. Every update refreshes the
// row and arms a settlement in the process that wrote it, which runs two seconds later, so a row
// this old was left by one that did not commit: a shutdown cut it short, a failure dropped it,
// perhaps in another process, or its issue's close stopped it (closeRecordLocked) - or one that
// is still running, which holds the row under its lease (leaseSettlementPending) and which the
// pass leaves alone. On a rolling deploy that process stops after the new one has started, which
// is why this runs on an interval and not only at start.
const settlementResumeAge = time.Minute

// settlementResumeBatch is the most documents one resumption pass arms, closed and open issues'
// together; the rest wait for the passes after it. Each armed document loads its room and renders
// it, and its settlement waits for one of the settlementWorkers slots and loads a cold room on the
// settlements pool's four connections (store/pools.go), which take them in turn. The last reading
// of the backlog was 21 documents, so one pass clears the known backlog, and a larger one drains at
// 50 a SettlementResumeInterval rather than all at once. That is reasoning, not a measurement:
// nobody has timed 50 settlements on the serving process.
const settlementResumeBatch = 50

// settlementResumptionLease names the lease each resumption pass runs under (duty.Exclusive). A
// settlement reads what it settles from the store under the document's lock, whichever process
// runs it, so one process arming the owed settlements is the whole of the work; every process
// arming them each period would run each owed document's settlement once per process. The name
// itself is duty's, since the lease pool is sized from duty's list and Exclusive refuses a name
// that is not on it.
const settlementResumptionLease = duty.SettlementResumptionLease

// RunSettlementResumption arms the settlement of the documents owing one past settlementResumeAge,
// at most settlementResumeBatch of them in a pass, at start and every settlementResume period,
// until ctx ends (resumeOwedSettlements). Each pass runs under settlementResumptionLease for that
// period (duty.RunLeased), so one of the envoy-dispatch processes serving the database runs it per
// period, in that process, and the next pass may run in another. A document nobody opens therefore
// settles too, whether or not its issue is closed: its ask blocks reach the open asks, or are
// saved resolved on a closed issue, without anyone loading it.
func (s *Service) RunSettlementResumption(ctx context.Context, leases duty.Leases) {
	duty.RunLeased(ctx, leases, settlementResumptionLease, s.settlementResume, duty.TickAtStart, func(ctx context.Context) {
		if err := s.resumeOwedSettlements(ctx, settlementResumeAge); err != nil && ctx.Err() == nil {
			slog.Error("dispatch: resume owed document settlements", "error", err)
		}
	})
}

// resumeOwedSettlements arms the settlement of at most settlementResumeBatch documents whose
// pending-settlement row is at least age old, whether or not their issue is closed: a closed
// issue's document settles as an open one's does (settleRoomWithin). The statement runs on the
// background pool, which no request waits on; the settlements it arms each wait for a slot
// (settlementWorkers).
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
// It skips a row whose lease is still ahead (leased_until) too: a settlement under way holds it,
// in this process or another, from before it does anything to the document until it ends, and
// holds no lock on it meanwhile. Arming that document again would move its room's generation under
// the running settlement, which then ends writing nothing (applyStamp); a document whose every
// settlement outlasted a period was armed again under each of them and never committed. A
// settlement that commits deletes the row, and one that ends any other way gives the lease back
// (releaseSettlementLease), so the next pass arms the document at once; only a settlement whose
// process died, or whose give-back failed, leaves the lease to run out (settlementLease).
//
// The rows come back in no order, and none is needed: every document of the pass is armed with
// the same settle delay.
func (s *Service) resumeOwedSettlements(ctx context.Context, age time.Duration) error {
	ctx = store.WithTransactionTracking(ctx)
	background, err := s.backgroundPool()
	if err != nil {
		return err
	}
	rows, err := background.Query(ctx, `
		with picked as (
			select artifact_id from doc_settlements_pending
			where marked_at <= now() - make_interval(secs => $1)
				and (leased_until is null or leased_until <= now())
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

// leaseSettlementPendingStatement is leaseSettlementPending's one statement: it marks the document
// $1's pending-settlement row held by a settlement under way until the database's now() plus $2
// seconds, answers that end, and writes nothing where the row has gone.
const leaseSettlementPendingStatement = `
	update doc_settlements_pending set leased_until = now() + make_interval(secs => $2)
	where artifact_id = $1
	returning leased_until`

// leaseSettlementPending takes the lease of a settlement beginning its work on room: it marks the
// document's pending-settlement row held for lease (settlementLease,
// doc_settlements_pending.leased_until), which keeps every resumption pass from arming the
// document again while the settlement runs (resumeOwedSettlements), and answers the lease's end,
// nil where the document owes no settlement. The settlement takes it once it holds its slot,
// before it warms a cold room, whichever way it was armed and in whichever process
// (settleRoomWithin), and gives it back as it ends: committing deletes the row, and any other end
// clears the lease where it is still this settlement's (releaseSettlementLease). Only a settlement
// whose process dies, or whose give-back fails, leaves the lease to run out. The next settlement
// of the document takes the lease again from its own start.
//
// It is one statement of its own on the settlements pool, through the connection the settlement's
// slot holds for it there (settlementWorkers), outside any transaction: the row lock it takes ends
// with the statement, so it holds no lock across the document's work, the copy lock a pass takes
// later included. The statement waits for the row's lock only behind another writer of the row
// until that writer's transaction ends: an append's upsert of it (settlementPendingStatement) and
// a closing room's write of its latest edit source (persistLastActor), each in a short transaction
// under the document's lock; a pass's pick, one statement; another process's settlement of the
// document committing its delete, within settlementFinishBound; a removal of the document deleting
// the row with its artifact (api.removeArtifact), after which it writes nothing; and a settlement
// of another document that marked this one an owed copy (owedCopiesOf), within that settlement's
// own deadline and finish. The settlement's deadline (ctx, settleTimeout) bounds the wait whatever
// holds the row.
//
// The write is the settlement's bookkeeping, not a request's: a request that runs a settlement - an
// issue's close settling its documents first (settleOwedNow) - writes it outside the transaction
// its Idempotency-Key's commit step marks, and a repeat of that request after a crash takes it
// again, which only moves the lease's end.
func leaseSettlementPending(ctx context.Context, settlements *store.Pool, room string, lease time.Duration) (*time.Time, error) {
	var until time.Time
	err := settlements.QueryRow(ctx, leaseSettlementPendingStatement, room, lease.Seconds()).Scan(&until)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("lease the document's pending settlement: %w", err)
	}
	return &until, nil
}

// settlementLeaseReleaseBound bounds a settlement's give-back of its lease
// (releaseSettlementLease), which runs apart from the settlement's own deadline, since a settlement
// that ran past that deadline gives its lease back too.
const settlementLeaseReleaseBound = 5 * time.Second

// releaseSettlementLeaseStatement is releaseSettlementLease's one statement: it clears the lease
// on the document $1's pending-settlement row where the row still holds the lease that ends at $2,
// and writes nothing where the row has gone or another settlement has leased it since.
const releaseSettlementLeaseStatement = `
	update doc_settlements_pending set leased_until = null
	where artifact_id = $1 and leased_until = $2`

// releaseSettlementLease gives back the lease a settlement of room took (leaseSettlementPending),
// which ended at until, as the settlement ends without having committed: it ran past its deadline,
// the database did not serve it, a newer generation of the room superseded it, the issue's close
// refused an update it held, or it failed. The row then reads as no settlement's, and the next
// resumption pass arms the document at once rather than once the lease has run out. The statement
// clears only a lease that is still this settlement's, so a newer settlement of the document, in
// this process or another, keeps the one it took since. It runs on the settlements pool through
// the connection the settlement's slot still holds, outside any transaction, on ctx's values
// without its deadline and under settlementLeaseReleaseBound; a failure is logged, and the lease
// then runs out by itself. Like the lease, it is the settlement's bookkeeping, not a request's.
func releaseSettlementLease(ctx context.Context, settlements *store.Pool, room string, until time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settlementLeaseReleaseBound)
	defer cancel()
	if _, err := settlements.Exec(ctx, releaseSettlementLeaseStatement, room, until); err != nil {
		slog.Warn("dispatch: give back a settlement's lease on its document's pending settlement; the lease runs out by itself",
			"room", room, "error", err)
	}
}
