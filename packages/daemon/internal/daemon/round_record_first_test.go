package daemon

// R7 of #5285, GPT-6.1 Sol's point 1 on #5285 at fa9deb04 (the Legion reviewer's
// minor point 2), which the spec's architect ruled a fix inside R7: a round's replacement writes
// its record before its last word, so the last word is the last decision before the suspension. A
// turn that starts while the store writes the record is a turn the last word reads: the last word
// refuses the replacement, and the record goes. The control is the same replacement with no such
// turn. The Legion reviewer's minor point 3: where the runtime finds the pod gone at the call, the
// round ends as the replacement it made, and the issue's architect is told what the read before it
// found, as the head before the record was.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// holdRecordWrites has the store hold every write of a replacement's record (pod_replacements)
// until the func it answers lets them go: a transaction of the test's own holds a lock on the table
// that each such write waits for, while reads of the table go on. The transaction runs on a context
// of its own, outside the rig's hold mark, as a second client of the database would. The test's
// cleanup lets the writes go too, so a test that fails while they wait leaves no write held.
func (r *podUpdateRig) holdRecordWrites() (letGo func()) {
	r.t.Helper()
	tx, err := r.pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		r.t.Fatalf("begin the transaction that holds the record writes: %v", err)
	}
	var once sync.Once
	letGo = func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	r.t.Cleanup(letGo)
	if _, err := tx.Exec(context.Background(), `lock table pod_replacements in share mode`); err != nil {
		r.t.Fatalf("lock the records of pod replacements: %v", err)
	}
	return letGo
}

// awaitRecordWriteHeld waits until a write of a replacement's record waits for the lock that
// holdRecordWrites holds.
func (r *podUpdateRig) awaitRecordWriteHeld() {
	r.t.Helper()
	testwait.Eventually(r.t, "a write of the replacement's record to wait for the lock", func() bool {
		var waiting int
		err := r.pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query ilike '%insert into pod_replacements%'`).Scan(&waiting)
		return err == nil && waiting > 0
	})
}

// replaceAsks is every ReplacePod call the runtime answered, made or refused by the last word.
func (r *podUpdateRig) replaceAsks() int {
	return len(r.rt.CallsOf("ReplacePod")) + len(r.rt.CallsOf("ReplacePodNotDue"))
}

func TestATurnThatStartsWhileTheRecordIsWrittenRefusesTheReplacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		turn bool
	}{
		{name: "the_implementer_starts_a_turn_while_the_record_is_written", turn: true},
		{name: "control:_no_turn_starts_while_the_record_is_written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
			rig.answerRound(podUpdateDrift, claim.RoleImplementer, claim.RoleTester)

			letGo := rig.holdRecordWrites()
			// The sweep runs on a hold mark of its own, as the orphan sweep's goroutine does, so the
			// connection its held write keeps is no connection of the test's own calls below.
			sweepCtx := store.ForConcurrentWorker(context.Background())
			swept := make(chan struct{})
			go func() {
				defer close(swept)
				for deadline := time.Now().Add(time.Minute); rig.replaceAsks() == 0 && time.Now().Before(deadline); {
					claims, err := rig.sup.Claims(sweepCtx)
					if err != nil {
						return
					}
					rig.s.reconcilePodUpdates(sweepCtx, claims)
				}
			}()
			t.Cleanup(func() { letGo(); <-swept })
			rig.awaitRecordWriteHeld()
			if tc.turn {
				rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: rig.tokens[claim.RoleImplementer]})
			}
			letGo()
			<-swept

			if !tc.turn {
				if made := rig.replacements(); len(made) != 1 {
					t.Fatalf("the pod was replaced %d times with no turn while its record was written, want once", len(made))
				}
				rig.awaitClaim(claim.RoleTester, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
				return
			}
			if made := rig.replacements(); len(made) != 0 {
				t.Fatalf("the pod was replaced %d times although the implementer started a turn while the record was written, want none: the last word comes after the record", len(made))
			}
			if refused := rig.rt.CallsOf("ReplacePodNotDue"); len(refused) != 1 {
				t.Fatalf("the last word refused %d replacements, want the one asked after the implementer's turn started", len(refused))
			}
			if records := rig.replacementRecords(); records != 0 {
				t.Fatalf("the store holds %d records of a replacement the last word refused, want none", records)
			}
			for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
				if c := rig.claimOf(role); c.UpdateRound != podUpdateRound {
					t.Fatalf("the %s records round %q, want the round still open", role, c.UpdateRound)
				}
			}
		})
	}
}

// The Legion reviewer's minor point 3: the runtime finds no pod of the round to end at the call
// (FindNoPodToReplace), as the Sandbox runtime answers a Sandbox suspended or a pod no longer its
// own, and asks no last word. The round ends as the replacement it made, and the architect is told
// what the read before it found, once.
func TestAPodTheRuntimeFindsGoneAtTheReplacementIsTold(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.rt.FindNoPodToReplace(true)
	rig.answerRound(roundBreakingDrift, claim.RoleImplementer, claim.RoleTester)
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.awaitSwept(role, "its round to end as the replacement it made", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	rig.wantToldOnce("once the runtime found no pod to replace")
	if asked := rig.rt.CallsOf("ReplacePodNoPod"); len(asked) != 1 {
		t.Fatalf("the runtime was asked %d times for a replacement it found no pod for, want once", len(asked))
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records of the replacement once its notice is queued, want none", records)
	}
}
