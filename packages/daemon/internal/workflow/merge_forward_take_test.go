package workflow

// While the record holds a merge forward owed to a role and not landed, the daemon holds every take
// of the issue that would adopt the working copy its roles share (supervise's Deps.MergeForwardOwed,
// answered by MergeForwardOwed): the ask is a role message and no take, so between two runs of the
// merge no role of the issue is in a turn, and an adoption then lands an empty commit on the merge
// in progress (dispatch://INFRA-4172). These tests are the workflow's side of rows 40 to 46 of plan
// v154: the record answers what the hold reads, the end of the hold - the landing, the withdrawal,
// the clear, and a move that leaves the ask owed to nobody - tells the daemon (Config.MergeForwardEnded)
// so each take the hold refused is written again at once, and the ask is sent again to a relaunched
// asked role, once for each launch, so the hold never waits on a process that was never told the
// merge (askAgainAtLaunch, MergeForwardAskEnded).
//
// Where a test names a negative control it is 6ed3e46f98896fb4f010554d9be7d9bd4e93a81f, which has
// none of these: no MergeForwardOwed, no end told, no launch on the ask.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// endingEngineAt is the engine of these tests at the first conflicting read plus d, which appends
// the issue of each end of the take hold to ended once the fact's transaction commits
// (Config.MergeForwardEnded).
func endingEngineAt(d time.Duration, ended *[]string) *Engine {
	engine := readsAfter(d)
	engine.cfg.MergeForwardEnded = func(issue string) { *ended = append(*ended, issue) }
	return engine
}

// mergeForwardOwedOf is what the take hold reads of LEGION-208 as the daemon reads it: the role the
// merge forward is owed to and whether it is owed (MergeForwardOwed), in a read of its own.
func mergeForwardOwedOf(t *testing.T, pool *pgxpool.Pool) (claim.Role, bool) {
	t.Helper()
	var role claim.Role
	var owed bool
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		role, owed, err = MergeForwardOwed(t.Context(), record.NewStore(), tx, "LEGION-208")
		return err
	})
	return role, owed
}

// wantOwed fails the test unless the take hold reads the merge forward owed to want (owed) or owed
// to nobody (the zero role), at the point of the test named by when.
func wantOwed(t *testing.T, pool *pgxpool.Pool, when string, want claim.Role) {
	t.Helper()
	role, owed := mergeForwardOwedOf(t, pool)
	if role != want || owed != (want != "") {
		t.Fatalf("%s the take hold reads the merge forward owed to %q (owed %t), want %q (owed %t)", when, role, owed, want, want != "")
	}
}

// landsThrough is landsMergeForward through engine: GitHub's synchronize of #42 from conflictHead
// to forwardHead, the merge forward landing.
func landsThrough(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	applyRefusingNothing(t, pool, engine, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Base: "main", HeadSHA: forwardHead, Before: conflictHead, Sender: "legion-implementer[bot]"})
}

// askedInTesting is LEGION-208 in testing with the implementer's ask of the merge forward of
// conflictHead made and not landed, the tester held for it.
func askedInTesting(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	wantOwed(t, pool, "after the ask,", claim.RoleImplementer)
	return pool
}

// Rows 40 and 41, the record's side: the implementer's ask is owed from the moment it is made, and
// the landing ends the hold: the daemon is told once, and the record owes the merge to nobody. A
// write that leaves the ask open ends nothing.
//
// The negative control is no end told at the landing, and no reader at all: the test does not
// compile at 6ed3e46f, and with putPullRequest's end removed the told list stays empty.
func TestTakeHoldOfAnOwedMergeForwardEndsAtTheLanding(t *testing.T) {
	pool := askedInTesting(t)
	ended := []string{}

	applyRefusingNothing(t, pool, endingEngineAt(2*time.Minute+30*time.Second, &ended), conflictingRead(&record.BaseRules{Required: []string{"ci"}}))
	if len(ended) != 0 {
		t.Fatalf("a read that leaves the ask open told the end of the hold %v, want nothing told", ended)
	}
	wantOwed(t, pool, "after a read that leaves it open,", claim.RoleImplementer)

	landsThrough(t, pool, endingEngineAt(3*time.Minute, &ended))

	if want := []string{"LEGION-208"}; !reflect.DeepEqual(ended, want) {
		t.Fatalf("the landing told the end of the hold %v, want %v: once, for the issue", ended, want)
	}
	wantOwed(t, pool, "after the landing,", "")
}

// Row 42, cleared: the tester moves the issue back into implementing, which clears the merge forward
// in the same transaction (mergeForwardOnEntry, clearMergeForward): the hold ends and the daemon is
// told once. That move is no order the asked role's own take is held by: the ask is gone.
//
// The negative control is as the landing's.
func TestTakeHoldOfAnOwedMergeForwardEndsWhenAMoveBackIntoImplementingClearsIt(t *testing.T) {
	pool := askedInTesting(t)
	ended := []string{}

	applyRefusingNothing(t, pool, endingEngineAt(2*time.Minute, &ended), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleTester,
		To: phase.Implementing, Reason: "the test shows the work is not done"})

	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the move back the issue is in %s, want implementing", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf != nil {
		t.Fatalf("after the move back the record holds the merge forward %+v, want it cleared", mf)
	}
	if want := []string{"LEGION-208"}; !reflect.DeepEqual(ended, want) {
		t.Fatalf("the clear told the end of the hold %v, want %v", ended, want)
	}
	wantOwed(t, pool, "after the clear,", "")
}

// Row 42, withdrawn: a write that takes the ask from open to withdrawn (record.MergeForward.Withdrawn)
// is the end of the hold, as the landing is, wherever it is written, so a site that withdraws an ask
// need not remember to tell the daemon (putPullRequest). A withdrawn ask is owed to nobody, a landed
// one and one of another head too, where the control is owed to the role asked.
//
// The negative control is as the landing's.
func TestTakeHoldOfAnOwedMergeForwardEndsWhenAWriteWithdrawsIt(t *testing.T) {
	pool := askedInTesting(t)
	ended := []string{}
	engine := endingEngineAt(2*time.Minute, &ended)

	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		pr, err := engine.store.PullRequest(ctx, tx, "LEGION-208")
		if err != nil || pr == nil || pr.MergeForward == nil {
			t.Fatalf("read the pull request: %+v, %v", pr, err)
		}
		withdrawn := *pr.MergeForward
		withdrawn.Withdrawn = true
		pr.MergeForward = &withdrawn
		return engine.putPullRequest(ctx, tx, *pr)
	}); err != nil {
		t.Fatalf("withdraw the ask: %v", err)
	}

	if want := []string{"LEGION-208"}; !reflect.DeepEqual(ended, want) {
		t.Fatalf("the withdrawal told the end of the hold %v, want %v", ended, want)
	}
	wantOwed(t, pool, "after the withdrawal,", "")
}

// Row 42, a move that leaves the ask owed to nobody: the implementer's ask of implementing covers
// implementing alone, and a plan with no step before the merge moves the issue out of implementing
// into its retro, where nothing asks again (mergeForwardOnEntry). The record keeps the ask, and it
// is owed to nobody there: the implementer's retro task, which adopts, is not held for a merge no
// role is at work on, and the move tells the daemon, so a take the ask held is written again.
//
// The negative control is no end told at the move: the told list stays empty.
func TestTakeHoldOfTheAskOfImplementingEndsWhenAMoveLeavesItOwedToNobody(t *testing.T) {
	pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), "implementer")
	confirmsConflict(t, pool, &record.BaseRules{Unread: "HTTP 502"})
	wantOwed(t, pool, "in implementing,", claim.RoleImplementer)
	ended := []string{}

	applyRefusingNothing(t, pool, endingEngineAt(2*time.Minute, &ended), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "implemented", Commit: conflictHead, HandoffRecord: "implement-1", HandoffVersion: 1})

	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the implementer's completion the issue is in %s, want retro", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Phase != phase.Implementing || mf.Landed != "" {
		t.Fatalf("the record's merge forward = %+v, want the ask of implementing, not landed", mf)
	}
	if want := []string{"LEGION-208"}; !reflect.DeepEqual(ended, want) {
		t.Fatalf("the move told the end of the hold %v, want %v", ended, want)
	}
	wantOwed(t, pool, "in the retro,", "")
}

// The hold reads the record's ask as owed only where the merge is owed to a role that stands to do
// it: the ask of the implementer made in testing is owed in every phase after, the merger's only in
// merging, and none that has landed, been withdrawn, or names another head is owed at all. An ask
// the issue does not hold, a pull request it does not hold, and an issue the record does not hold
// owe nothing.
func TestTakeHoldReadsTheRecordsAskAsOwedOnlyWhereTheMergeIsOwed(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   phase.Phase
		ask  *record.MergeForward
		want claim.Role
	}{
		{name: "the implementer's ask made in testing", at: phase.Testing,
			ask: &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing, Why: record.ConflictCheckNotGreen}, want: claim.RoleImplementer},
		{name: "an ask that names no role, which a daemon before the merger's asks wrote, is the implementer's", at: phase.Testing,
			ask: &record.MergeForward{Head: conflictHead, Base: "main", Phase: phase.Testing, Why: record.ConflictCheckNotGreen}, want: claim.RoleImplementer},
		{name: "the implementer's ask made in testing, owed in merging", at: phase.Merging,
			ask: &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing, Why: record.ConflictCheckNotGreen}, want: claim.RoleImplementer},
		{name: "the merger's ask in merging", at: phase.Merging,
			ask: &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleMerger, Phase: phase.Merging, Why: record.ConflictMergeNext}, want: claim.RoleMerger},
		{name: "the merger's ask outside merging", at: phase.Testing,
			ask: &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleMerger, Phase: phase.Merging, Why: record.ConflictMergeNext}},
		{name: "an ask that landed", at: phase.Testing,
			ask: &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing, Why: record.ConflictCheckNotGreen, Landed: forwardHead}},
		{name: "an ask that was withdrawn", at: phase.Testing,
			ask: &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing, Why: record.ConflictCheckNotGreen, Withdrawn: true}},
		{name: "an ask of another head", at: phase.Testing,
			ask: &record.MergeForward{Head: forwardHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing, Why: record.ConflictCheckNotGreen}},
		{name: "no ask", at: phase.Testing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := settledGreen("lint")
			pr.MergeForward = tc.ask
			pool := seedConflictIssue(t, tc.at, PhaseStatus(tc.at), pr)
			wantOwed(t, pool, tc.name+":", tc.want)
		})
	}
	t.Run("an issue and a pull request the record does not hold", func(t *testing.T) {
		pool := migratedPool(t)
		if role, owed := mergeForwardOwedOf(t, pool); owed || role != "" {
			t.Fatalf("an issue the record does not hold owes %q (owed %t), want nothing", role, owed)
		}
		standsIn(t, pool, phase.Testing, "testing")
		if role, owed := mergeForwardOwedOf(t, pool); owed || role != "" {
			t.Fatalf("an issue with no pull request owes %q (owed %t), want nothing", role, owed)
		}
	})
}

// implementerReadyAt is the ready of the implementer's claim at launch, which the daemon applies once
// for each launch of the claim (intake.ClaimReady).
func implementerReadyAt(launch uint64) intake.ClaimReady {
	return intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleImplementer, Launch: launch}
}

// askRowsEnded is the daemon's executor finishing the ask's rows: they leave the outbox as a
// delivered row does, before its end settles the launch it reached (MergeForwardAskEnded).
func askRowsEnded(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from outbox where kind = $1 and issue = 'LEGION-208' and payload->>'step' = $2`,
		string(record.OutboxKindMergeForward), string(record.MergeForwardAsk)); err != nil {
		t.Fatalf("finish the ask's rows: %v", err)
	}
}

// askDeliveredTo is the daemon's executor delivering the record's ask of conflictHead to the asked
// role's launch: the ask's rows leave the outbox (askRowsEnded), and the record's ask keeps that
// launch (record.MergeForward.SentLaunch), as the row's end records it (MergeForwardAskEnded).
func askDeliveredTo(t *testing.T, pool *pgxpool.Pool, launch uint64) {
	t.Helper()
	askRowsEnded(t, pool)
	pr := recordedPR(t, pool, "LEGION-208")
	if pr.MergeForward == nil {
		t.Fatal("the record holds no ask to deliver")
	}
	delivered := *pr.MergeForward
	delivered.SentLaunch = launch
	pr.MergeForward = &delivered
	seedPR(t, pool, pr)
}

// asksOf is the merge-forward messages of step the outbox holds, in the order written.
func asksOf(t *testing.T, pool *pgxpool.Pool, step record.MergeForwardStep) []record.MergeForwardMessage {
	t.Helper()
	asks := []record.MergeForwardMessage{}
	for _, message := range mergeForwardMessages(t, pool) {
		if message.Step == step {
			asks = append(asks, message)
		}
	}
	return asks
}

// Row 46, the workflow's side: the implementer is asked in testing and the ask's row is delivered to
// its launch 1, then its process is relaunched by a death (a launch 2 the row did not reach). The
// record owes the merge still, and the hold stands over every adopting take, the asked role's own
// among them, with no process told the merge. So the ready of launch 2 sends the ask again, once, and
// the record keeps that launch; a second ready of launch 2, and a ready of launch 1, send nothing. The
// landing then ends the hold.
//
// The negative control is no ask sent again at a ready (6ed3e46f): the outbox holds no second ask,
// and the record keeps no launch.
func TestTakeHoldOfAnAskOwedToARelaunchedRoleIsKeptByTheAskSentAgainAtItsReady(t *testing.T) {
	pool := askedInTesting(t)
	askDeliveredTo(t, pool, 1)
	why := record.ConflictCheckNotGreen

	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), implementerReadyAt(2))

	if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, []record.MergeForwardMessage{implementerAsked(phase.Testing, why)}) {
		t.Fatalf("after the ready of launch 2 the asks are %+v, want the implementer's ask once", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 2 {
		t.Fatalf("after the ready of launch 2 the record's ask is %+v, want it sent at launch 2", mf)
	}
	wantOwed(t, pool, "after the ask was sent again,", claim.RoleImplementer)

	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), implementerReadyAt(2), implementerReadyAt(1))
	if got := asksOf(t, pool, record.MergeForwardAsk); len(got) != 1 {
		t.Fatalf("after a second ready of launch 2 and a ready of launch 1 the asks are %+v, want the one as before", got)
	}

	ended := []string{}
	landsThrough(t, pool, endingEngineAt(5*time.Minute, &ended))
	wantOwed(t, pool, "after the landing,", "")
}

// A ready sends nothing while a row of the ask is still queued for the role, and leaves the ask to
// that row: the row reaches the new launch by its own delivery, so a second row would tell it twice.
// The record keeps no launch the ask was sent at until the row's end records it
// (MergeForwardAskEnded). The ask of a launch of another role, and one the record owes to nobody
// (landed, withdrawn), is sent at no ready.
//
// The negative control is the ready sending the ask whatever is queued: the outbox holds two asks.
func TestTakeHoldOfAnAskIsKeptWithoutASecondAskWhileItsRowIsQueued(t *testing.T) {
	pool := askedInTesting(t)

	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), implementerReadyAt(2))

	if got := asksOf(t, pool, record.MergeForwardAsk); len(got) != 1 {
		t.Fatalf("after the ready of launch 2 the asks are %+v, want the one still queued, which reaches that launch", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 0 {
		t.Fatalf("after the ready the record's ask is %+v, want it sent at no launch: the queued row records its own", mf)
	}
}

// A ready of a later launch asks the role again only while the merge of the head is owed to it: a
// landed ask and a withdrawn one are sent nothing, a ready of another role is sent nothing, and the
// control's ask is owed to the implementer and is sent again.
func TestTakeHoldOfAnAskIsKeptByTheAskSentAgainOnlyWhileTheMergeIsOwed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T) *pgxpool.Pool
		ready intake.ClaimReady
		again int
	}{
		{name: "control: the implementer's ask made in testing", seed: askedInTesting, ready: implementerReadyAt(2), again: 1},
		{name: "a ready of another role than the one asked", seed: askedInTesting,
			ready: intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleTester, Launch: 2}},
		{name: "the ask landed", seed: func(t *testing.T) *pgxpool.Pool {
			pool := askedInTesting(t)
			landsMergeForward(t, pool)
			return pool
		}, ready: implementerReadyAt(2)},
		{name: "the ask withdrawn", seed: func(t *testing.T) *pgxpool.Pool {
			pool := askedInTesting(t)
			pr := recordedPR(t, pool, "LEGION-208")
			withdrawn := *pr.MergeForward
			withdrawn.Withdrawn = true
			pr.MergeForward = &withdrawn
			seedPR(t, pool, pr)
			return pool
		}, ready: implementerReadyAt(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := tc.seed(t)
			askDeliveredTo(t, pool, 1)
			before := len(asksOf(t, pool, record.MergeForwardAsk))

			applyRefusingNothing(t, pool, readsAfter(5*time.Minute), tc.ready)

			if got := len(asksOf(t, pool, record.MergeForwardAsk)) - before; got != tc.again {
				t.Fatalf("the ready of launch 2 queued %d asks, want %d", got, tc.again)
			}
		})
	}
}

// endsAskRow is the daemon's end of a row of the implementer's ask of conflictHead, in a transaction
// of its own as the executor makes it (MergeForwardAskEnded): told is the launch the row was
// delivered to (0 for an end undelivered) and launch the claim's launch as the executor reads it
// there. It answers whether the ask was sent again.
func endsAskRow(t *testing.T, pool *pgxpool.Pool, told, launch uint64) bool {
	t.Helper()
	var sent bool
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		sent, err = MergeForwardAskEnded(ctx, record.NewStore(), tx, "LEGION-208", claim.RoleImplementer, conflictHead, told, launch, firstConflictRead.Add(5*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("end the ask's row: %v", err)
	}
	return sent
}

// The end of a row of the ask records the launch it reached and sends the ask to a later launch it
// did not reach, so each launch is told the ask once whichever of the row's end and the launch's
// ready comes first (MergeForwardAskEnded). A row delivered to launch 2 while the claim is at launch
// 2 only records it; one delivered to launch 1, or ended undelivered, while a launch 2 came ready
// before the row ended (and so left the ask to the row), sends the ask to launch 2; and where the
// claim holds no launch (failed, retired), or the ask is owed to nobody, nothing is sent.
func TestTakeHoldOfAnAskIsKeptByTheEndOfItsRowTellingALaterLaunch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		told, live uint64
		sent       bool
		sentLaunch uint64
		landed     bool
	}{
		{name: "delivered to the live launch: recorded, told once", told: 2, live: 2, sentLaunch: 2},
		{name: "delivered to launch 1 while launch 2 is live: launch 2 is told", told: 1, live: 2, sent: true, sentLaunch: 2},
		{name: "ended undelivered while launch 2 is live: launch 2 is told", told: 0, live: 2, sent: true, sentLaunch: 2},
		{name: "ended undelivered with no live launch: the next launch's ready tells it", told: 0, live: 0},
		{name: "delivered to launch 1 with no live launch: recorded only", told: 1, live: 0, sentLaunch: 1},
		{name: "an ask that landed is told to nobody", told: 1, live: 2, landed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := askedInTesting(t)
			if tc.landed {
				landsMergeForward(t, pool)
			}
			// The executor deletes the row in the transaction that settles its end, so no row of the ask is queued.
			askRowsEnded(t, pool)

			sent := endsAskRow(t, pool, tc.told, tc.live)

			if sent != tc.sent {
				t.Fatalf("the end of the row sent the ask again = %t, want %t", sent, tc.sent)
			}
			if got, want := len(asksOf(t, pool, record.MergeForwardAsk)), map[bool]int{true: 1}[tc.sent]; got != want {
				t.Fatalf("the end queued %d asks, want %d", got, want)
			}
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != tc.sentLaunch {
				t.Fatalf("after the end the record's ask is %+v, want it told launch %d", mf, tc.sentLaunch)
			}
		})
	}
}
