package daemon

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The three fault states that keptLongRegressionSeeds keeps seeds 41, 427 and 701 for, and that
// TestEachKeptFaultSeedReachesTheStateOfItsFault checks each seed reaches.
const (
	keptFaultToldAgainAfterAFailedWrite   = "a decision told again after a catch-up whose telling write failed"
	keptFaultDropBesideADeliveringTelling = "a drop of the root's catch-ups while a row of a kept telling is inside a publish that delivers and an unclaimed, untold copy of that telling stands"
	keptFaultSpentRehold                  = "a catch-up's re-holds spent while its architect holds its launch"
)

// keptFaultMarkers counts each time a run of the kept-decision model reaches one of the three
// fault states. Each counter is incremented at the moment the state occurs and logged once on the
// first firing, naming the seed and the step, so a control run with that fault's fix undone prints
// its marker line before the fault fails the test.
type keptFaultMarkers struct {
	toldAgainAfterAFailedWrite   atomic.Int64
	dropBesideADeliveringTelling atomic.Int64
	spentRehold                  atomic.Int64
}

// markToldAgainAfterAFailedWrite marks seed 41's state in keptRun.tell after earlier and excused
// are counted and before the switch decides, so it fires whether the oracle then accepts or fails.
func (r *keptRun) markToldAgainAfterAFailedWrite(ref, tree string) {
	if r.faults.toldAgainAfterAFailedWrite.Add(1) == 1 {
		r.t.Logf("seed %d: fault marker %q reached at step %d (ref=%s tree=%s)",
			r.seed, keptFaultToldAgainAfterAFailedWrite, r.step, ref, tree)
	}
}

// markDelivering wraps a held-open catch-up race whose answer is keptDelivers so r.delivering holds
// row for the time the race op runs. A receipt timeout tells nothing (keptPublisher.publish records
// no delivery for it, and workflow/parked_decision.go says the same of the daemon), so a publish
// answered with a receipt timeout leaves r.delivering at 0.
func (r *keptRun) markDelivering(race *keptRace, row int64) {
	if race.mode != keptDelivers {
		return
	}
	run := race.run
	race.run = func() error {
		r.delivering.Store(row)
		defer r.delivering.Store(0)
		return run()
	}
}

// markDropBesideADeliveringTelling marks seed 427's state when DropCatchUps runs while r.delivering
// is inside a delivering publish and an unclaimed, untold copy of that telling stands in the outbox.
func (r *keptRun) markDropBesideADeliveringTelling(issue string, delivering int64) {
	if r.faults.dropBesideADeliveringTelling.Add(1) == 1 {
		r.t.Logf("seed %d: fault marker %q reached at step %d (issue=%s delivering=%d)",
			r.seed, keptFaultDropBesideADeliveringTelling, r.step, issue, delivering)
	}
}

// markSpentRehold marks seed 701's state when a held-open catch-up publish in raceRow drew a
// receipt timeout on a copy whose re-holds are spent (Resends >= len(noticeReholdDelays)) and whose
// catch-up carries kept decisions, and reholdNotice took its spent branch rather than stopping with
// its finished tree or taking its superseded branch. In reholdNotice (notice_exceptions.go),
// stoppedWithTree runs before the Resends check, and the Resends check runs before
// catchUpSuperseded, so a non-stopped row at Resends >= len(noticeReholdDelays) takes the spent
// branch alone.
func (r *keptRun) markSpentRehold(row record.OutboxRow, payload record.OutboxPayload) {
	notice, ok := payload.(record.Notice)
	if !ok || notice.CatchUp == nil || notice.CatchUp.Kept == nil || notice.Resends < len(noticeReholdDelays) {
		return
	}
	tree := r.treeRow(row.Issue)
	if tree == nil {
		return
	}
	held := r.runner.supervisedClaim(tree.token)
	if !holdsRole(held) {
		_, lingers, err := r.runner.issueTree(r.ctx, row.Issue)
		if err != nil {
			r.fatalf("read %s's tree for the spent-rehold marker: %v", row.Issue, err)
		}
		if stoppedWithTree(lingers, held) {
			return
		}
	}
	if r.faults.spentRehold.Add(1) == 1 {
		r.t.Logf("seed %d: fault marker %q reached at step %d (row=%d issue=%s resends=%d)",
			r.seed, keptFaultSpentRehold, r.step, row.ID, row.Issue, notice.Resends)
	}
}

// DropCatchUps checks, in the drop's transaction under record.LockCatchUps(issue), whether an
// unclaimed, untold copy of the telling of r.delivering stands before calling the embedded drop.
// An advisory transaction lock is reentrant in one session, so the embedded drop's own LockCatchUps
// call succeeds without waiting. Every row of one telling shares the row it copies, or its own id
// where it copies none, and the telling's told mark is keyed by that row (record.KeptToldSource
// under record.OutboxKey).
func (s *keptForgetStore) DropCatchUps(ctx context.Context, tx pgx.Tx, issue string) ([]int64, error) {
	if s.run != nil {
		if delivering := s.run.delivering.Load(); delivering != 0 {
			if err := record.LockCatchUps(ctx, tx, issue); err != nil {
				return nil, err
			}
			var telling int64
			err := tx.QueryRow(ctx, `select coalesce((payload->>'resend_of')::bigint, id) from outbox
				where id = $1 and lease_token <> ''`, delivering).Scan(&telling)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			var stood bool
			if err == nil {
				if err := tx.QueryRow(ctx, `select exists (select 1 from outbox copy
					where copy.kind = $1 and copy.issue = $2 and copy.payload->>'kind' = 'catch-up'
					and copy.payload->'catch_up'->'kept' is not null and copy.lease_token = '' and copy.id <> $3
					and coalesce((copy.payload->>'resend_of')::bigint, copy.id) = $4)
					and not exists (select 1 from processed_events where source = $5 and event_id = $6)`,
					string(record.OutboxKindNotice), issue, delivering, telling, record.KeptToldSource,
					record.OutboxKey(telling)).Scan(&stood); err != nil {
					return nil, err
				}
			}
			if stood {
				s.run.markDropBesideADeliveringTelling(issue, delivering)
			}
		}
	}
	return s.Store.DropCatchUps(ctx, tx, issue)
}
