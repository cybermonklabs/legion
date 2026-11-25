package workflow

// An ask sent again at a launch (askAgain) records that launch on the record's ask as its row is
// queued, before anything tells it. A row that then ends undelivered must not leave the record
// naming a launch nothing told, or that launch is skipped by every later send.

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
	"github.com/sjawhar/legion/daemon/internal/record"
)

// askRowEnds is the daemon's executor ending the implementer's queued ask row: the row leaves the
// outbox under the fact lock, and the workflow settles its end (MergeForwardAskEnded) with told,
// the launch the row reached (0 for a row that ends undelivered), and launch, the claim's launch
// as the executor reads it at the end. It answers the row's payload.
func askRowEnds(t *testing.T, pool *pgxpool.Pool, engine *Engine, told, launch uint64) record.MergeForwardMessage {
	t.Helper()
	var ask record.MergeForwardMessage
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var payload []byte
		if err := tx.QueryRow(ctx, `delete from outbox where kind = $1 and issue = 'LEGION-208' and payload->>'step' = $2 returning payload`,
			string(record.OutboxKindMergeForward), string(record.MergeForwardAsk)).Scan(&payload); err != nil {
			return err
		}
		if err := json.Unmarshal(payload, &ask); err != nil {
			return err
		}
		_, err := MergeForwardAskEnded(ctx, engine.store, tx, "LEGION-208", ask.Role, ask.Head, told, launch, engine.now())
		return err
	})
	return ask
}

// sentAgainAtLaunch2 is LEGION-208 on the default steps in testing with the implementer asked and
// the tester held, the ask delivered to the implementer's launch 1, and the ready of its launch 2
// sending the ask again: one ask row queued for launch 2, and the record's ask naming launch 2.
func sentAgainAtLaunch2(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testerHeldWhileTheImplementerMerges(t)
	asksDelivered(t, pool, 1)
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), implementerReady(2))
	if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, []string{"tester hold", "implementer ask"}) {
		t.Fatalf("after the ready of launch 2 the merge-forward messages are %v, want [tester hold implementer ask]", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 2 {
		t.Fatalf("after the ready of launch 2 the record's ask is %+v, want it sent at launch 2", mf)
	}
	return pool
}

// The row sent again at launch 2 ends undelivered while the merge is still owed and the claim is
// still at launch 2: the executor read the row as ending (a merger's ask owed to nobody, a pull
// request closed), and by the end, under the fact lock, the ask is owed again. Nothing told launch
// 2, so the end sends it the ask, once, and the record names launch 2 again. A record that kept
// launch 2 as told at the queue would skip launch 2 at the end, and no ready of launch 2 comes
// again.
func TestAnAskRowEndedUndeliveredLeavesItsLaunchToBeToldAgain(t *testing.T) {
	pool := sentAgainAtLaunch2(t)
	askRowEnds(t, pool, readsAfter(3*time.Minute), 0, 2)
	if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, []string{"tester hold", "implementer ask"}) {
		t.Errorf("after the undelivered end the merge-forward messages are %v, want [tester hold implementer ask]: launch 2 is told the ask", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 2 {
		t.Errorf("after the undelivered end the record's ask is %+v, want it sent at launch 2 again", mf)
	}
}

// A newer ask replaced the one the row was sent for before the row ended undelivered: the head
// moved, and a merge forward of the new head was asked and told the implementer's launch 2, the
// launch the older row names too. The older row's end changes nothing of the newer ask, and sends
// nothing: the record's ask is not the row's own, since it is of another head.
func TestAnOlderAskRowEndedUndeliveredChangesNothingOfANewerAsk(t *testing.T) {
	pool := sentAgainAtLaunch2(t)
	pr := recordedPR(t, pool, "LEGION-208")
	newer := *pr.MergeForward
	newer.Head, newer.SentLaunch, newer.AskedAt = forwardHead, 2, newer.AskedAt.Add(time.Minute)
	pr.HeadSHA, pr.MergeForward = forwardHead, &newer
	seedPR(t, pool, pr)
	askRowEnds(t, pool, readsAfter(3*time.Minute), 0, 2)
	if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, []string{"tester hold"}) {
		t.Errorf("after the older row's end the merge-forward messages are %v, want [tester hold]: nothing sent", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Head != forwardHead || mf.SentLaunch != 2 {
		t.Errorf("after the older row's end the record's ask is %+v, want the newer ask of %s, sent at launch 2", mf, forwardHead)
	}
}

// After the row was queued for launch 2, a later launch, 3, was told the ask (the record names
// launch 3). The row then ends undelivered: the record keeps launch 3, which was told, and the end
// sends nothing to launch 3. A roll-back to the launch before the row, 1, would send launch 3 the
// ask a second time.
func TestAnAskRowEndedUndeliveredNeverRollsBackPastALaunchTold(t *testing.T) {
	pool := sentAgainAtLaunch2(t)
	pr := recordedPR(t, pool, "LEGION-208")
	told := *pr.MergeForward
	told.SentLaunch = 3
	pr.MergeForward = &told
	seedPR(t, pool, pr)
	askRowEnds(t, pool, readsAfter(3*time.Minute), 0, 3)
	if got := toldWho(mergeForwardMessages(t, pool)); !reflect.DeepEqual(got, []string{"tester hold"}) {
		t.Errorf("after the undelivered end the merge-forward messages are %v, want [tester hold]: launch 3 was told", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 3 {
		t.Errorf("after the undelivered end the record's ask is %+v, want it still sent at launch 3", mf)
	}
}

// The pull request closes while the row sent again at launch 2 is queued, and the row ends
// undelivered on the closed pull request. A reopen drops the record's ask (the reopened record
// carries none), and the conflict two reads confirm on the reopened head asks again afresh: the
// implementer is asked once more, with the tester held, and the ask reaches the launch through its
// own row. This held before the roll-back and holds after it.
func TestAnAskRowEndedOnAClosedPullRequestIsAskedAgainAfterTheReopen(t *testing.T) {
	pool := sentAgainAtLaunch2(t)
	closed := firstConflictRead.Add(4 * time.Minute)
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), closedAt(conflictHead, closed))
	askRowEnds(t, pool, readsAfter(5*time.Minute), 0, 2)
	applyRefusingNothing(t, pool, readsAfter(6*time.Minute), intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: conflictHead, UpdatedAt: closed.Add(2 * time.Minute), Reopened: true})
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf != nil {
		t.Fatalf("after the reopen the record's ask is %+v, want none: the reopen drops it", mf)
	}
	before := len(mergeForwardMessages(t, pool))
	rules := &record.BaseRules{Required: []string{"ci"}}
	applyRefusingNothing(t, pool, readsAfter(10*time.Minute), conflictingRead(rules))
	applyRefusingNothing(t, pool, readsAfter(10*time.Minute+conflictConfirmWait), conflictingRead(rules))
	got := toldWho(mergeForwardMessages(t, pool)[before:])
	if !reflect.DeepEqual(got, []string{"implementer ask", "tester hold"}) {
		t.Errorf("after the reopened head's conflict was confirmed the merge-forward messages queued are %v, want [implementer ask tester hold]", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleImplementer || mf.SentLaunch != 0 {
		t.Errorf("after the reopened head's conflict the record's ask is %+v, want the implementer's fresh ask, told no launch yet", mf)
	}
}
