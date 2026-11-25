package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// MaintenanceSweepInterval is how often one process deletes the Idempotency-Keys and spent DPoP
// proofs past their windows and sends the deliveries a process that died left owed. Nothing here
// is in a request's latency budget: a key is read by a repeat long before it is swept, a proof
// record only refuses a replay until its window ends, and an owed delivery is one whose sender
// already died, so a minute of delay costs nothing a caller can see, and a minutes-scale interval
// keeps these statements off any tight loop in production.
const MaintenanceSweepInterval = time.Minute

// maintenanceLease names the lease one pass of this sweep runs under. The name is the one the
// suggestion outcome sweep carried this work under before it moved here, and it stays so: during
// a rolling deploy the earlier build's pass, which swept the keys and the owed deliveries inside
// its own suggestion pass, and this build's timer still exclude each other on that work, which a
// new name would not.
const maintenanceLease = "suggestion-outcomes"

// RunMaintenanceSweep deletes every Idempotency-Key past its window (sweepRequestKeys) and every
// spent DPoP proof past its acceptance window (store.DPoPProofs.Sweep), ends every answer run
// past its window that stored no terminal event and deletes every run past its TTL
// (sweepGoingRequests), settles every Agent root a restore of the release before left owed
// (sweepAgentDismissalsOwed), and then sends every delivery a process that died left owed
// (resumeDeliveries, the server's resumeOwedDeliveries). resumeDeliveries is nil where nothing
// delivers. It stops when ctx is done.
//
// It is a timer and not a woken loop (duty.RunWoken), the shape every other sweep of this server
// took, because no commit announces any of its work: a key and a proof record pass their windows
// by the clock alone, an answer run passes its ends_at by the clock too, an owed root is a row a
// trigger appends with no notice, and a delivery is owed because the process that claimed it
// died, which commits nothing for a notification to carry. They share one loop rather than taking
// one each: a loop of its own would need a lease connection of its own (store.LeasePoolSize, whose
// headroom is zero) for statements that cost milliseconds.
//
// Each pass runs under maintenanceLease for one interval, so one process sweeps per interval
// however many serve the database and a second process's tick, which falls between the first's
// rather than on it, finds that interval served; leases is required, as it is of every leased
// loop (cmd/dispatch exits at boot if the lease pool cannot be opened). The period is interval
// itself and not MaintenanceSweepInterval: this loop ticks on what its caller asked for. The
// ticks are duty.RunLeased's, a ticker's: the first one interval after the call, then one each
// interval; the first tick a pass overran comes at once with the moment it was due, and only the
// ones after it are dropped. The sweeps fail apart: one's failure is logged and the others still
// run.
func RunMaintenanceSweep(
	ctx context.Context, pool *store.Pool, interval time.Duration, leases duty.Leases,
	resumeDeliveries func(context.Context) error,
) {
	duty.RunLeased(ctx, leases, maintenanceLease, interval, duty.TickAfterInterval, func(ctx context.Context) {
		if err := sweepRequestKeys(ctx, pool); err != nil {
			slog.Warn("dispatch: idempotency key sweep failed", "error", err)
		}
		if _, err := store.NewDPoPProofs(pool).Sweep(ctx); err != nil {
			slog.Warn("dispatch: DPoP proof sweep failed", "error", err)
		}
		if err := sweepGoingRequests(ctx, pool); err != nil {
			slog.Warn("dispatch: going request sweep failed", "error", err)
		}
		if err := sweepAgentDismissalsOwed(ctx, pool); err != nil {
			slog.Warn("dispatch: owed agent root sweep failed", "error", err)
		}
		if resumeDeliveries == nil {
			return
		}
		if err := resumeDeliveries(ctx); err != nil {
			slog.Warn("dispatch: delivery sweep failed", "error", err)
		}
	})
}

// agentDismissalsOwedBatch is how many owed roots one transaction of sweepAgentDismissalsOwed
// takes.
const agentDismissalsOwedBatch = 200

// The statements of one batch of sweepAgentDismissalsOwed: the next owed roots past the cursor
// ($1, null for the first), each once, in ascending id; the settle of each of those roots that
// exists and that no other transaction holds, taken in that order `for no key update skip locked`
// so the sweep waits at no root; and the delete of the owed rows of those that no longer exist.
const (
	agentDismissalsOwedNext = `
	select root_id::text from user_agent_thread_dismissed_owed
	where $1::uuid is null or root_id > $1::uuid
	group by root_id
	order by root_id
	limit $2`
	agentDismissalsOwedSettle = `
	with taken as materialized (
		select m.id from messages m
		where m.id = any($1::uuid[])
		order by m.id
		for no key update skip locked
	)
	select user_agent_thread_dismissed_settle(taken.id) from taken`
	agentDismissalsOwedGone = `
	delete from user_agent_thread_dismissed_owed owed
	where owed.root_id = any($1::uuid[])
	  and not exists (select 1 from messages m where m.id = owed.root_id)`
)

// sweepAgentDismissalsOwed settles the roots user_agent_thread_dismissed_owed holds (migration
// 20261125053125): a root whose column a delete of its author's mark left standing because another
// transaction held the root, which happens where a restore of the release before, which takes the
// mark and no root, meets a writer that holds it. That writer settles the root after its own
// delete or write of the marks, so the table is empty but for a moment, and this pass is what keeps
// it so where no writer comes.
//
// It walks the owed roots in batches, one transaction each, in ascending id. Each batch takes the
// roots `for no key update skip locked` and settles each one it got
// (user_agent_thread_dismissed_settle): the column from its author's mark as it now stands, and
// its owed rows deleted. A root another transaction holds is skipped and stays owed for that
// transaction, or for the next pass, so the sweep never waits at a root, and holds nothing a
// writer of the lock order written on api/inbox_read.go's markInboxReadThreads waits for while it
// waits itself. The owed rows of a root that no longer exists, which no writer will settle, are
// deleted with the batch.
func sweepAgentDismissalsOwed(ctx context.Context, pool *store.Pool) error {
	var after *string
	for {
		roots, err := settleAgentDismissalsOwed(ctx, pool, after)
		if err != nil {
			return err
		}
		if len(roots) < agentDismissalsOwedBatch {
			return nil
		}
		after = &roots[len(roots)-1]
	}
}

// settleAgentDismissalsOwed is one batch of sweepAgentDismissalsOwed: the owed roots past after,
// read on the pool, then settled where taken and deleted where gone, in one transaction, which a
// batch that read no root never opens. It answers the roots the batch read, which a batch shorter
// than agentDismissalsOwedBatch says were the last.
func settleAgentDismissalsOwed(ctx context.Context, pool *store.Pool, after *string) ([]string, error) {
	rows, err := pool.Query(ctx, agentDismissalsOwedNext, after, agentDismissalsOwedBatch)
	if err != nil {
		return nil, fmt.Errorf("read the owed agent roots: %w", err)
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the owed agent roots: %w", err)
	}
	if len(roots) == 0 {
		return nil, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, agentDismissalsOwedSettle, roots); err != nil {
		return nil, fmt.Errorf("settle the owed agent roots: %w", err)
	}
	if _, err := tx.Exec(ctx, agentDismissalsOwedGone, roots); err != nil {
		return nil, fmt.Errorf("delete the owed rows of agent roots that are gone: %w", err)
	}
	return roots, tx.Commit(ctx)
}
