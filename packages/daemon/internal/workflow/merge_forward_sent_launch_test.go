package workflow

// The record's ask names the launch a row of it was last delivered to
// (record.MergeForward.SentLaunch), which every send of the ask again reads: a launch it names is
// told already. So it names a launch only once a row reached it, at the row's delivered end
// (MergeForwardAskEnded), never at the queue: a row that then ends undelivered leaves nothing to
// take back, and the launch it was sent at is told again.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// askRowEnds is the daemon's executor ending the implementer's queued ask row: under the fact lock
// (intake.SerializeFacts) the row leaves the outbox, and the workflow settles its end
// (MergeForwardAskEnded) with delivered and told, the launch the delivery reached (0 for an
// undelivered row, and for a delivery the listener named no holder of), and launch, the claim's
// launch as the executor reads it at the end.
func askRowEnds(t *testing.T, pool *pgxpool.Pool, engine *Engine, delivered bool, told, launch uint64) {
	t.Helper()
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return err
		}
		var payload []byte
		if err := tx.QueryRow(ctx, `delete from outbox where kind = $1 and issue = 'LEGION-208' and payload->>'step' = $2 returning payload`,
			string(record.OutboxKindMergeForward), string(record.MergeForwardAsk)).Scan(&payload); err != nil {
			return err
		}
		var ask record.MergeForwardMessage
		if err := json.Unmarshal(payload, &ask); err != nil {
			return err
		}
		_, err := MergeForwardAskEnded(ctx, engine.store, tx, "LEGION-208", ask, delivered, told, launch, engine.now())
		return err
	})
}

// sentAgainAtLaunch2 is LEGION-208 on the default steps in testing with the implementer asked and
// the tester held, the ask delivered to the implementer's launch 1, and the ready of its launch 2
// sending the ask again: one ask row queued for launch 2, and the record's ask still told launch
// 1, since no row has reached launch 2 yet.
func sentAgainAtLaunch2(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testerHeldWhileTheImplementerMerges(t)
	asksDelivered(t, pool, 1)
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), implementerReady(2))
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{heldIn(claim.RoleTester, phase.Testing, why), sentAgain(implementerAsked(phase.Testing, why), 2)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the ready of launch 2 the merge-forward messages are %+v, want %+v", got, want)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 1 {
		t.Fatalf("after the ready of launch 2 the record's ask is %+v, want it still told launch 1", mf)
	}
	return pool
}

// The row sent again at launch 2 ends undelivered while the merge is still owed and the claim is
// still at launch 2: the executor read the row as ending, and by the end, under the fact lock, the
// ask is owed again. Nothing told launch 2, and the record does not name it, so the end sends it
// the ask once more, as a row sent at launch 2. A record that named launch 2 from the queue on
// would skip launch 2 at the end, and no ready of launch 2 comes again.
func TestAnAskRowEndedUndeliveredLeavesItsLaunchToBeToldAgain(t *testing.T) {
	t.Parallel()
	pool := sentAgainAtLaunch2(t)
	askRowEnds(t, pool, readsAfter(3*time.Minute), false, 0, 2)
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{heldIn(claim.RoleTester, phase.Testing, why), sentAgain(implementerAsked(phase.Testing, why), 2)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("after the undelivered end the merge-forward messages are %+v, want %+v: launch 2 is told the ask", got, want)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 1 {
		t.Errorf("after the undelivered end the record's ask is %+v, want it still told launch 1", mf)
	}
}

// The row sent again at launch 2 is delivered while the listener names no holder, so the delivery
// names no launch told. It is still taken as told to the launch it was sent at: the record's ask
// names launch 2, and the end sends launch 2 nothing more. Recording nothing for it would send the
// ask to launch 2 again at this end, and at the end of every such row after it.
func TestASentAgainRowDeliveredWithNoHolderIsToldItsLaunch(t *testing.T) {
	t.Parallel()
	pool := sentAgainAtLaunch2(t)
	askRowEnds(t, pool, readsAfter(3*time.Minute), true, 0, 2)
	if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, []string{"tester hold"}) {
		t.Errorf("after the delivery the merge-forward messages are %v, want [tester hold]: launch 2 was told", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 2 {
		t.Errorf("after the delivery the record's ask is %+v, want it told launch 2", mf)
	}
}

// A newer ask replaced the one the row was sent for before the row was delivered: the head moved,
// and a merge forward of the new head was asked and told the implementer's launch 1. The older
// row's delivery to launch 2 told launch 2 the old head's merge, not the new one's, so it records
// nothing on the newer ask, and the end sends the newer ask to launch 2.
func TestAnOlderAskRowDeliveredRecordsNothingOnANewerAsk(t *testing.T) {
	t.Parallel()
	pool := sentAgainAtLaunch2(t)
	pr := recordedPR(t, pool, "LEGION-208")
	newer := *pr.MergeForward
	newer.Head, newer.SentLaunch, newer.AskedAt = forwardHead, 1, newer.AskedAt.Add(time.Minute)
	pr.HeadSHA, pr.MergeForward = forwardHead, &newer
	seedPR(t, pool, pr)
	askRowEnds(t, pool, readsAfter(3*time.Minute), true, 2, 2)
	why := record.ConflictCheckNotGreen
	toNewer := sentAgain(implementerAsked(phase.Testing, why), 2)
	toNewer.Head = forwardHead
	want := []record.MergeForwardMessage{heldIn(claim.RoleTester, phase.Testing, why), toNewer}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("after the older row's delivery the merge-forward messages are %+v, want %+v: the newer ask is sent to launch 2", got, want)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Head != forwardHead || mf.SentLaunch != 1 {
		t.Errorf("after the older row's delivery the record's ask is %+v, want the newer ask of %s, still told launch 1", mf, forwardHead)
	}
}

// A newer ask of the same head and role was made after the row was queued for launch 2 - a move
// into a phase where the ask the record held is owed to nobody asks the implementer again for that
// head - and the newer ask's own row told launch 2. The older row then ends undelivered while the
// claim is still at launch 2: launch 2 was told, by the newer ask's row, so the end sends it
// nothing and the record keeps launch 2. An end that read the newer ask as the row's own and took
// its launch back would send launch 2 the ask a second time.
func TestAnOlderAskRowEndedUndeliveredLeavesANewerAskOfItsHeadAsItIs(t *testing.T) {
	t.Parallel()
	pool := sentAgainAtLaunch2(t)
	pr := recordedPR(t, pool, "LEGION-208")
	newer := *pr.MergeForward
	newer.SentLaunch, newer.AskedAt = 2, newer.AskedAt.Add(time.Minute)
	pr.MergeForward = &newer
	seedPR(t, pool, pr)
	askRowEnds(t, pool, readsAfter(3*time.Minute), false, 0, 2)
	if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, []string{"tester hold"}) {
		t.Errorf("after the older row's end the merge-forward messages are %v, want [tester hold]: the newer ask's row told launch 2", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || !mf.AskedAt.Equal(newer.AskedAt) || mf.SentLaunch != 2 {
		t.Errorf("after the older row's end the record's ask is %+v, want the newer ask, told launch 2", mf)
	}
}

// The pull request closes while the row sent again at launch 2 is queued, and the row ends
// undelivered on the closed pull request, so it records nothing: the record's ask still names
// launch 1, the last told. A reopen keeps the record's ask and asks the implementer once more,
// with the tester held (askAtReopen): the ask reaches launch 2 through its own row, whose delivery
// tells no launch at or below launch 1. The conflict two reads confirm on the reopened head then
// asks nothing more, since the merge of that head is still owed to the implementer.
func TestAnAskRowEndedOnAClosedPullRequestIsAskedAgainAfterTheReopen(t *testing.T) {
	t.Parallel()
	pool := sentAgainAtLaunch2(t)
	closed := firstConflictRead.Add(4 * time.Minute)
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), closedAt(conflictHead, closed))
	askRowEnds(t, pool, readsAfter(5*time.Minute), false, 0, 2)
	before := len(mergeForwardMessages(t, pool))
	applyRefusingNothing(t, pool, readsAfter(6*time.Minute), intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: conflictHead, UpdatedAt: closed.Add(2 * time.Minute), Reopened: true})
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleImplementer || mf.SentLaunch != 1 {
		t.Fatalf("after the reopen the record's ask is %+v, want the implementer's ask kept, still told launch 1", mf)
	}
	if got := toldWho(mergeForwardMessages(t, pool)[before:]); !reflect.DeepEqual(got, []string{"implementer ask", "tester hold"}) {
		t.Errorf("after the reopen the merge-forward messages queued are %v, want [implementer ask tester hold]", got)
	}
	after := len(mergeForwardMessages(t, pool))
	rules := &record.BaseRules{Required: []string{"ci"}}
	applyRefusingNothing(t, pool, readsAfter(10*time.Minute), conflictingRead(rules))
	applyRefusingNothing(t, pool, readsAfter(10*time.Minute+conflictConfirmWait), conflictingRead(rules))
	if got := toldWho(mergeForwardMessages(t, pool)[after:]); len(got) != 0 {
		t.Errorf("after the reopened head's conflict was confirmed the merge-forward messages queued are %v, want none: the merge is still owed to the implementer", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleImplementer || mf.SentLaunch != 1 {
		t.Errorf("after the reopened head's conflict the record's ask is %+v, want the implementer's ask kept, told launch 1", mf)
	}
}
