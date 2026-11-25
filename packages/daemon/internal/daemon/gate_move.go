package daemon

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A design gate's move to a phase holder whose pod an idle release took (R4 of
// dispatch://INFRA-4480): workflow's tellGateMoved writes the close and the opening for every phase
// holder that is no merger, each marked with the move (record.RoleMessage.Gate). Two rules decide
// such a row, both read at each attempt before roleMessage publishes:
//
//   - gateMoveStale, first: only the newest move its holder was told of is published, and only
//     while the gate still stands as that move left it. Every other row ends unpublished.
//   - gateMoveEnds, then, in roleMessageWay: the row ends where its holder can no longer be the
//     one the move was for. Otherwise it waits while the holder's claim reads released and goes on
//     to roleMessageWay's shared tail once the claim has left released for any cause.

// gateMoveStale is whether a design gate's move (record.RoleMessage.Gate) is to end unpublished at
// this attempt, read against the record as it stands now, before anything else of the row: the
// rule that decides at publish time which move its holder reads. The row is published only while
// both of these hold, and ends unpublished with one line naming why where either does not:
//
//   - It is the newest move of the gate its holder was told of: the holder's told row still holds
//     the opening number the move's settlement gave it (record.Store.OpeningSeq against
//     record.GateMove.Seq). Every move of the gate settles every told row of the tree, a close
//     through CloseOpenings and an opening through SettleOpening, and nothing else moves that
//     number, so a later move of the gate, whether its row is still queued or has already run and
//     gone, moves it past the older move's. A new version of a step while the gate stays where it
//     stood is no move (workflow's putGateTold writes none), so it outruns nothing.
//   - The gate still stands as the move left it: open for an opening, closed for a close
//     (classify.StandingGate over the root's rows, as designGateClosed reads it).
//
// So the holder is told the state the gate stands in, once, and never an older move after a newer
// one: it extends to every gate row the rule cybermonklabs/legion#4661's closeOutrun gives a close
// that the opening outran. Both reads come from one REPEATABLE READ snapshot. A holder's row that
// is gone, its issue's generation cleared, ends the row too. An outbox with no store, a test's that
// drives one row by hand, reads nothing and ends nothing here.
func (r *outbox) gateMoveStale(ctx context.Context, row record.OutboxRow, payload record.RoleMessage) (bool, error) {
	if r.pool == nil || r.records == nil {
		return false, nil
	}
	move := *payload.Gate
	var seq int
	var found bool
	var gates map[record.DesignStep]record.DesignGate
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if seq, found, err = r.records.OpeningSeq(ctx, tx, row.Issue, record.AudienceHolder); err != nil {
			return err
		}
		gates, err = r.records.GatesOf(ctx, tx, move.Tree)
		return err
	}); err != nil {
		return false, fmt.Errorf("%s's design gate message for %s: read its holder's opening number and the design gate of %s: %w",
			row.Issue, payload.Role, move.Tree, err)
	}
	_, closed := classify.StandingGate(r.gatedSteps, gates)
	var why string
	switch {
	case !found:
		why = "its holder's told row is gone"
	case seq != move.Seq:
		why = "a later move of the gate was told to its holder"
	case move.Open == closed:
		why = "the gate no longer stands as the move left it"
	default:
		return false, nil
	}
	r.log.Info("outbox role message finished unpublished: it is not the newest move of the design gate as the gate stands now",
		"row", row.ID, "issue", row.Issue, "role", payload.Role, "tree", move.Tree, "step", move.Step, "version", move.Version,
		"open", move.Open, "seq", move.Seq, "holder_seq", seq, "why", why)
	return true, nil
}

// gateMoveEnds is R4's end rule for a design gate's move to a phase holder (record.RoleMessage.Gate),
// read from held, this daemon's view of the holder's claim. Under `gates.hold: merge` the holder's
// phase goes on under the gate, so neither the close nor the opening is worth a wake, and the row
// wakes nothing. The row ends undelivered, with one line (messageEnded), where:
//
//   - the claim was taken over (suspended by any source but the idle release, and a pause, whose
//     stop suspends with the release's source too), failed or retired;
//   - the issue has left the phase the holder works: workflow.RoleFor of the issue's phase is no
//     longer the holder's role. At admitted and accepting RoleFor answers the architect, whom
//     tellGateMoved never marks, so a row whose issue stands there ends;
//   - the issue has left the workflow, or its tree lingers (wakeableIssue).
//
// Otherwise it answers messagePublished, and roleMessageWay's shared tail waits while the claim
// reads released, a routine wait that the phase's backstop bounds, and once the claim has left
// released for any other cause — the backstop, a CI settlement, a start, a message — publishes with
// keep. On a deployment with no backstop (outbox.backstop zero), under a gate that never moves
// again, the row waits for the life of the phase.
func (r *outbox) gateMoveEnds(ctx context.Context, row record.OutboxRow, payload record.RoleMessage, held supervise.ClaimView) (messageWay, error) {
	if held.Gone() && !held.Released() {
		r.log.Info("outbox role message finished undelivered: the holder its design gate move was for was taken over, failed or retired",
			"row", row.ID, "issue", row.Issue, "role", payload.Role, "state", held.State, "suspended_by", held.SuspendedBy)
		return messageEnded, nil
	}
	issue, wakeable, err := r.wakeableIssue(ctx, held.Issue)
	if err != nil {
		return messagePublished, err
	}
	if !wakeable || workflow.RoleFor(issue.Phase) != held.Role {
		r.log.Info("outbox role message finished undelivered: its design gate move's issue left the phase its holder works, or the workflow",
			"row", row.ID, "issue", row.Issue, "role", payload.Role, "phase", issue.Phase)
		return messageEnded, nil
	}
	return messagePublished, nil
}
