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
// among them, with no process told the merge. So the ready of launch 2 sends the ask again, once,
// its row carrying that launch (record.MergeForwardMessage.SentLaunch); the record's ask still
// names launch 1 until that row's delivered end records launch 2 (MergeForwardAskEnded). Once that
// row has been delivered, a second ready of launch 2, and a ready of launch 1, send nothing: each
// launch was told the ask. The landing then ends the hold.
//
// The negative control is no ask sent again at a ready (6ed3e46f): the outbox holds no second ask,
// and the record keeps no launch. With the launch comparison cut from askAgain, the second ready of
// launch 2 queues a second ask.
func TestTakeHoldOfAnAskOwedToARelaunchedRoleIsKeptByTheAskSentAgainAtItsReady(t *testing.T) {
	pool := askedInTesting(t)
	askDeliveredTo(t, pool, 1)

	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), implementerReadyAt(2))

	resent := sentAgain(implementerAsked(phase.Testing, record.ConflictCheckNotGreen), 2)
	if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, []record.MergeForwardMessage{resent}) {
		t.Fatalf("after the ready of launch 2 the asks are %+v, want the implementer's ask once, sent at launch 2", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 1 {
		t.Fatalf("after the ready of launch 2 the record's ask is %+v, want it still told launch 1: no row reached launch 2 yet", mf)
	}
	wantOwed(t, pool, "after the ask was sent again,", claim.RoleImplementer)

	// The re-send's row is delivered to launch 2, and its end records that launch, so no queued row
	// stands between the readies below and the launch the record's ask names.
	askRowsEnded(t, pool)
	endsAskRow(t, pool, resent, 2, 2)
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 2 {
		t.Fatalf("after the re-send's delivery the record's ask is %+v, want it told launch 2", mf)
	}
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), implementerReadyAt(2), implementerReadyAt(1))
	if got := asksOf(t, pool, record.MergeForwardAsk); len(got) != 0 {
		t.Fatalf("after a second ready of launch 2 and a ready of launch 1 the asks are %+v, want none: each launch was told", got)
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

// endsAskRow is the daemon's end of ask, a row of the implementer's ask of conflictHead, in a
// transaction of its own as the executor makes it (MergeForwardAskEnded): told is the launch the
// row was delivered to, 0 for an end undelivered, so a row told a launch is a delivered one, and
// launch the claim's launch as the executor reads it there. It answers whether the ask was sent
// again.
func endsAskRow(t *testing.T, pool *pgxpool.Pool, ask record.MergeForwardMessage, told, launch uint64) bool {
	t.Helper()
	var sent bool
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		sent, err = MergeForwardAskEnded(ctx, record.NewStore(), tx, "LEGION-208", ask, told != 0, told, launch, firstConflictRead.Add(5*time.Minute))
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
// before the row ended (and so left the ask to the row), sends the ask to launch 2, in a row sent at
// that launch, whose own delivered end records it; and where the claim holds no launch (failed,
// retired), or the ask is owed to nobody, nothing is sent. The record's ask names only a launch a
// row was delivered to.
func TestTakeHoldOfAnAskIsKeptByTheEndOfItsRowTellingALaterLaunch(t *testing.T) {
	act := implementerAsked(phase.Testing, record.ConflictCheckNotGreen)
	for _, tc := range []struct {
		name       string
		told, live uint64
		sent       bool
		sentLaunch uint64
		landed     bool
	}{
		{name: "delivered to the live launch: recorded, told once", told: 2, live: 2, sentLaunch: 2},
		{name: "delivered to launch 1 while launch 2 is live: launch 2 is told", told: 1, live: 2, sent: true, sentLaunch: 1},
		{name: "ended undelivered while launch 2 is live: launch 2 is told", told: 0, live: 2, sent: true},
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

			sent := endsAskRow(t, pool, act, tc.told, tc.live)

			if sent != tc.sent {
				t.Fatalf("the end of the row sent the ask again = %t, want %t", sent, tc.sent)
			}
			want := []record.MergeForwardMessage{}
			if tc.sent {
				want = append(want, sentAgain(act, tc.live))
			}
			if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, want) {
				t.Fatalf("the end queued the asks %+v, want %+v", got, want)
			}
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != tc.sentLaunch {
				t.Fatalf("after the end the record's ask is %+v, want it told launch %d", mf, tc.sentLaunch)
			}
		})
	}
}

// closesAt is GitHub's close of #42 at head, d after the first conflicting read, and reopensAt its
// reopen at head then: the close and the reopen a person makes on GitHub, whose clocks order them.
func closesAt(head string, d time.Duration) intake.PullRequestClosed {
	return intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, UpdatedAt: firstConflictRead.Add(d)}
}

func reopensAt(head string, d time.Duration) intake.PullRequestOpened {
	return intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Base: "main", HeadSHA: head,
		UpdatedAt: firstConflictRead.Add(d), Reopened: true}
}

// queuedAsk is the ask row of the record's ask of conflictHead the outbox holds last, failing the
// test where it holds none.
func queuedAsk(t *testing.T, pool *pgxpool.Pool) record.MergeForwardMessage {
	t.Helper()
	asks := asksOf(t, pool, record.MergeForwardAsk)
	if len(asks) == 0 {
		t.Fatal("the outbox holds no ask")
	}
	return asks[len(asks)-1]
}

// askSentAt is the implementer's ask of conflictHead in testing as a row carries it when it was
// sent at launch (record.MergeForwardMessage.SentLaunch): the launch a ready sent it again at, the
// launch after the last told for the reopen's, and 0 for the act's. It names the conflicting read.
func askSentAt(launch uint64) record.MergeForwardMessage {
	return sentAgain(implementerAsked(phase.Testing, record.ConflictCheckNotGreen), launch)
}

// wantToldThrough fails the test unless the record's ask names launch as the one a row of it was
// last delivered to (record.MergeForward.SentLaunch): the launch at and below which the daemon's
// delivery of an ask tells nothing (its mergeForwardMessage), at the point of the test named by
// when.
func wantToldThrough(t *testing.T, pool *pgxpool.Pool, when string, launch uint64) {
	t.Helper()
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != launch {
		t.Fatalf("%s the record's ask is %+v, want it told launch %d", when, mf, launch)
	}
}

// readUnknown is ask as a row queued after a reopen carries it: the reopen builds the record's row
// of the pull request afresh, with no read of GitHub's merge state (pullRequestOpened), so the row
// names none, and the ask says the state is unknown.
func readUnknown(ask record.MergeForwardMessage) record.MergeForwardMessage {
	ask.MergeState = ""
	return ask
}

// Row 47 of plan v162: a close at the same head does not stop a merge already running in the
// workspace, so the record keeps the ask and the take hold reads it owed through the close, and
// no end of the hold is told; the architect is sent pr-closed-unmerged, whose decision ends the
// wait.
func TestTakeHoldOfAnOwedMergeForwardStandsThroughASameHeadClose(t *testing.T) {
	pool := askedInTesting(t)
	ended := []string{}

	applyRefusingNothing(t, pool, endingEngineAt(10*time.Minute, &ended), closesAt(conflictHead, 10*time.Minute))

	if pr := recordedPR(t, pool, "LEGION-208"); pr.State != record.PullRequestClosed || pr.MergeForward == nil || pr.MergeForward.Landed != "" {
		t.Fatalf("after the close the record holds the pull request %s with the merge forward %+v, want it closed with the ask kept", pr.State, pr.MergeForward)
	}
	if len(ended) != 0 {
		t.Fatalf("the close told the end of the hold %v, want nothing told: the merge may still be running", ended)
	}
	wantOwed(t, pool, "after the close,", claim.RoleImplementer)
	if got := noticesOf(t, pool, "LEGION-208", "pr-closed-unmerged"); len(got) != 1 {
		t.Fatalf("the architect was sent %d pr-closed-unmerged notices, want one", len(got))
	}
}

// Row 48 of plan v162, the record's side: a reopen at the same head is the same pull request, and
// the record keeps its ask whole - its Earlier, which the completion fence reads, and its
// SentLaunch, which the delivery of a later row reads - so the hold stands through the close and
// the reopen alike, and neither tells its end.
//
// The negative control is the reopen building its row without the ask (pullRequestOpened at
// 88ed2ae7): the record holds no merge forward after it, and the reopen tells the end of the hold.
func TestTakeHoldOfAnOwedMergeForwardIsKeptByASameHeadReopen(t *testing.T) {
	pool := askedInTesting(t)
	askDeliveredTo(t, pool, 1)
	pr := recordedPR(t, pool, "LEGION-208")
	kept := *pr.MergeForward
	kept.Earlier = []record.LandedMergeForward{{Head: headThree, Landed: headEarlier}}
	pr.MergeForward = &kept
	seedPR(t, pool, pr)
	ended := []string{}

	applyRefusingNothing(t, pool, endingEngineAt(10*time.Minute, &ended), closesAt(conflictHead, 10*time.Minute))
	applyRefusingNothing(t, pool, endingEngineAt(20*time.Minute, &ended), reopensAt(conflictHead, 20*time.Minute))

	got := recordedPR(t, pool, "LEGION-208")
	if got.State != record.PullRequestOpen || got.MergeForward == nil || !reflect.DeepEqual(*got.MergeForward, kept) {
		t.Fatalf("after the reopen the record holds the pull request %s with the merge forward %+v, want it open with the ask kept whole %+v",
			got.State, got.MergeForward, kept)
	}
	if len(ended) != 0 {
		t.Fatalf("the close and the reopen told the end of the hold %v, want nothing told", ended)
	}
	wantOwed(t, pool, "after the reopen,", claim.RoleImplementer)
}

// Row 48 of plan v162, the ask at the reopen: the re-send at a launch's ready sends nothing while
// the pull request is not open, and the delivery of a row of the ask refuses one, so an ask whose
// row ended undelivered during the close, or whose asked role was relaunched during it, reached no
// live launch. The reopen queues the ask once. The workflow knows no launch of the claim, so the
// row is sent at the launch after the one the record's ask was last delivered to
// (record.MergeForward.SentLaunch), and the daemon's delivery, which reads the launch - the one
// place the asked role's launch is known - tells no launch below it. A row still queued at the
// reopen is not doubled, and nor is the reopen's row by a ready that comes while it is queued.
// Where the live launch was told before the close the workflow still queues the row, and its
// delivery tells nothing (the daemon's side). The reopen also holds again the tester, the worker
// the merge keeps waiting, since no hold reached a worker while the pull request was closed
// (cybermonklabs/legion#5312's hold, carried onto the reopen). The landing then ends the hold.
//
// The negative control is the reopen queuing nothing (88ed2ae7): no ask follows the reopen.
func TestTakeHoldOfAnAskTheCloseKeptFromItsRoleIsSentOnceAtTheReopen(t *testing.T) {
	act := implementerAsked(phase.Testing, record.ConflictCheckNotGreen)
	for _, tc := range []struct {
		name string
		// before runs before the close, and during between the close and the reopen.
		before, during func(t *testing.T, pool *pgxpool.Pool)
		want           []record.MergeForwardMessage
		// toldThrough is the launch the record's ask names as told when the reopen's row is queued.
		toldThrough uint64
	}{
		{name: "the ask's row ended undelivered during the close", during: func(t *testing.T, pool *pgxpool.Pool) {
			askRowsEnded(t, pool)
			if endsAskRow(t, pool, act, 0, 1) {
				t.Fatal("the end of the ask's row during the close sent the ask again, want nothing sent to a closed pull request")
			}
		}, want: []record.MergeForwardMessage{readUnknown(askSentAt(1))}},
		{name: "the asked role relaunched during the close", before: func(t *testing.T, pool *pgxpool.Pool) {
			askDeliveredTo(t, pool, 1)
		}, during: func(t *testing.T, pool *pgxpool.Pool) {
			applyRefusingNothing(t, pool, readsAfter(15*time.Minute), implementerReadyAt(2))
			if got := asksOf(t, pool, record.MergeForwardAsk); len(got) != 0 {
				t.Fatalf("the ready of launch 2 during the close queued %+v, want nothing sent to a closed pull request", got)
			}
		}, want: []record.MergeForwardMessage{readUnknown(askSentAt(2))}, toldThrough: 1},
		{name: "the act's row still queued at the reopen", want: []record.MergeForwardMessage{act}},
		{name: "the live launch told before the close", before: func(t *testing.T, pool *pgxpool.Pool) {
			askDeliveredTo(t, pool, 1)
		}, want: []record.MergeForwardMessage{readUnknown(askSentAt(2))}, toldThrough: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := askedInTesting(t)
			if tc.before != nil {
				tc.before(t, pool)
			}
			applyRefusingNothing(t, pool, readsAfter(10*time.Minute), closesAt(conflictHead, 10*time.Minute))
			if tc.during != nil {
				tc.during(t, pool)
			}
			holds := len(asksOf(t, pool, record.MergeForwardHold))

			applyRefusingNothing(t, pool, readsAfter(20*time.Minute), reopensAt(conflictHead, 20*time.Minute))

			if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("after the reopen the asks are %+v, want %+v", got, tc.want)
			}
			wantToldThrough(t, pool, "after the reopen", tc.toldThrough)
			if got := asksOf(t, pool, record.MergeForwardHold); len(got) != holds+1 ||
				got[len(got)-1] != readUnknown(heldIn(claim.RoleTester, phase.Testing, record.ConflictCheckNotGreen)) {
				t.Fatalf("after the reopen the holds are %+v, want the tester held once more", got)
			}
			wantOwed(t, pool, "after the reopen,", claim.RoleImplementer)
			applyRefusingNothing(t, pool, readsAfter(25*time.Minute), implementerReadyAt(3))
			if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("after a ready while the reopen's row is queued the asks are %+v, want %+v as before", got, tc.want)
			}

			ended := []string{}
			landsThrough(t, pool, endingEngineAt(30*time.Minute, &ended))
			if want := []string{"LEGION-208"}; !reflect.DeepEqual(ended, want) {
				t.Fatalf("the landing told the end of the hold %v, want %v", ended, want)
			}
			wantOwed(t, pool, "after the landing,", "")
		})
	}
}

// Row 48a of plan v162: the ask of implementing covers implementing alone, so once the issue moved
// on into its retro the ask is owed to nobody and the hold has ended. A reopen at the same head
// keeps the ask on the record, queues no row of it and tells nothing more.
//
// The negative control is a reopen that queues the record's ask whatever is owed: an ask follows
// the reopen.
func TestTakeHoldOfTheAskOfImplementingReopenedInItsRetroQueuesNothing(t *testing.T) {
	pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), "implementer")
	confirmsConflict(t, pool, &record.BaseRules{Unread: "HTTP 502"})
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "implemented", Commit: conflictHead, HandoffRecord: "implement-1", HandoffVersion: 1})
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the implementer's completion the issue is in %s, want retro", got)
	}
	askRowsEnded(t, pool)
	kept := recordedPR(t, pool, "LEGION-208").MergeForward
	ended := []string{}

	applyRefusingNothing(t, pool, endingEngineAt(10*time.Minute, &ended), closesAt(conflictHead, 10*time.Minute))
	applyRefusingNothing(t, pool, endingEngineAt(20*time.Minute, &ended), reopensAt(conflictHead, 20*time.Minute))

	if got := recordedPR(t, pool, "LEGION-208").MergeForward; kept == nil || got == nil || !reflect.DeepEqual(*got, *kept) {
		t.Fatalf("after the reopen the record's merge forward is %+v, want the ask of implementing kept %+v", got, kept)
	}
	if got := asksOf(t, pool, record.MergeForwardAsk); len(got) != 0 {
		t.Fatalf("the reopen queued %+v, want no ask: the ask is owed to nobody in the retro", got)
	}
	if len(ended) != 0 {
		t.Fatalf("the close and the reopen told the end of the hold %v, want nothing: it ended at the move", ended)
	}
	wantOwed(t, pool, "after the reopen,", "")
}

// Row 48b of plan v162: the re-send at a launch's ready carries that launch on its row
// (record.MergeForwardMessage.SentLaunch), and the record's ask names it only once the row's
// delivered end records it (MergeForwardAskEnded). Delivered to that launch while the pull request
// is open, the record names the launch, and the delivery of the row a later reopen queues tells
// that launch nothing more. Refused by a close before it was delivered, its row ends undelivered
// and records nothing, so the record still names the launch last told, and the delivery of the row
// the reopen queues tells the launch the re-send never reached.
//
// The negative control is a record that names the re-send's launch from its queue on (askAgain
// before cybermonklabs/legion#5312's fold): after the refused re-send the record names launch 2,
// and the reopen's row skips the launch that was never told.
func TestTakeHoldOfAReSendAtANewLaunchIsToldOnceAcrossALaterClose(t *testing.T) {
	t.Run("the re-send delivered while open, then a later close and reopen", func(t *testing.T) {
		pool := askedInTesting(t)
		askDeliveredTo(t, pool, 1)
		applyRefusingNothing(t, pool, readsAfter(10*time.Minute), closesAt(conflictHead, 10*time.Minute))
		applyRefusingNothing(t, pool, readsAfter(20*time.Minute), reopensAt(conflictHead, 20*time.Minute))
		wantToldThrough(t, pool, "after the reopen", 1)
		reopened := queuedAsk(t, pool)
		askRowsEnded(t, pool)
		// Its delivery skipped launch 1, which was told before the close: the row ends undelivered.
		if endsAskRow(t, pool, reopened, 0, 1) {
			t.Fatal("the end of the reopen's row at the launch told before the close sent the ask again, want nothing")
		}

		applyRefusingNothing(t, pool, readsAfter(25*time.Minute), implementerReadyAt(2))
		if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, []record.MergeForwardMessage{readUnknown(askSentAt(2))}) {
			t.Fatalf("after the ready of launch 2 the asks are %+v, want the re-send sent at launch 2", got)
		}
		wantToldThrough(t, pool, "after the ready of launch 2", 1)
		resent := queuedAsk(t, pool)
		askRowsEnded(t, pool)
		endsAskRow(t, pool, resent, 2, 2)
		wantToldThrough(t, pool, "after the re-send's delivery", 2)

		applyRefusingNothing(t, pool, readsAfter(30*time.Minute), closesAt(conflictHead, 30*time.Minute))
		applyRefusingNothing(t, pool, readsAfter(40*time.Minute), reopensAt(conflictHead, 40*time.Minute))
		if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, []record.MergeForwardMessage{readUnknown(askSentAt(3))}) {
			t.Fatalf("after the later reopen the asks are %+v, want one row sent past launch 2, which its delivery skips at launch 2", got)
		}
		wantToldThrough(t, pool, "after the later reopen", 2)
	})
	t.Run("the re-send refused by a close before its delivery", func(t *testing.T) {
		pool := askedInTesting(t)
		askDeliveredTo(t, pool, 1)
		applyRefusingNothing(t, pool, readsAfter(5*time.Minute), implementerReadyAt(2))
		resent := queuedAsk(t, pool)
		if resent != askSentAt(2) {
			t.Fatalf("the re-send at the ready of launch 2 is %+v, want it sent at launch 2", resent)
		}
		wantToldThrough(t, pool, "after the re-send was queued", 1)

		applyRefusingNothing(t, pool, readsAfter(10*time.Minute), closesAt(conflictHead, 10*time.Minute))
		askRowsEnded(t, pool)
		if endsAskRow(t, pool, resent, 0, 2) {
			t.Fatal("the end of the refused re-send sent the ask again, want nothing sent to a closed pull request")
		}
		wantToldThrough(t, pool, "after the refused re-send, launch 1 is still the last told:", 1)

		applyRefusingNothing(t, pool, readsAfter(20*time.Minute), reopensAt(conflictHead, 20*time.Minute))
		if got := asksOf(t, pool, record.MergeForwardAsk); !reflect.DeepEqual(got, []record.MergeForwardMessage{readUnknown(askSentAt(2))}) {
			t.Fatalf("after the reopen the asks are %+v, want one row sent past launch 1, which its delivery tells launch 2", got)
		}
		wantToldThrough(t, pool, "after the reopen", 1)
	})
}

// A row's end records a launch on the record's ask only where the row was delivered and the ask is
// still the row's own - the same head and asked role, not landed - and it never takes a launch
// back: a row that ends undelivered, a row that names no later launch, a row of an old head ending
// after a new ask replaced the record's, an ask of another role, a landing and a launch recorded
// since are each left as they are (MergeForwardAskEnded's recordAskEnd). So the record names only a
// launch a row reached, and a close that refused a row leaves the launch it was sent at to be told.
func TestTakeHoldOfAnAskRowsEndRecordsItsLaunchOnlyWhereItWasDeliveredToTheAskItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  record.MergeForwardMessage
		told uint64
		// record is what the record's ask holds when the row ends, from the ask told launch 1.
		record func(mf *record.MergeForward)
		want   uint64
	}{
		{name: "control: the re-send's row ended undelivered", ask: askSentAt(2), want: 1},
		{name: "the re-send's row delivered", ask: askSentAt(2), told: 2, want: 2},
		{name: "the act's row delivered to the launch already told", ask: askSentAt(0), told: 1, want: 1},
		{name: "a launch recorded since", ask: askSentAt(2), told: 2, record: func(mf *record.MergeForward) { mf.SentLaunch = 3 }, want: 3},
		{name: "a new ask of another head replaced the row's", ask: askSentAt(2), told: 2, record: func(mf *record.MergeForward) { mf.Head = headThree }, want: 1},
		{name: "an ask of another role", ask: askSentAt(2), told: 2, record: func(mf *record.MergeForward) {
			mf.Role, mf.Phase = claim.RoleMerger, phase.Merging
		}, want: 1},
		{name: "the ask landed", ask: askSentAt(2), told: 2, record: func(mf *record.MergeForward) { mf.Landed = forwardHead }, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := askedInTesting(t)
			askRowsEnded(t, pool)
			pr := recordedPR(t, pool, "LEGION-208")
			mf := *pr.MergeForward
			mf.SentLaunch = 1
			if tc.record != nil {
				tc.record(&mf)
			}
			pr.MergeForward = &mf
			seedPR(t, pool, pr)

			endsAskRow(t, pool, tc.ask, tc.told, 0)

			if got := recordedPR(t, pool, "LEGION-208").MergeForward; got == nil || got.SentLaunch != tc.want {
				t.Fatalf("after the row's end the record's ask is %+v, want it told launch %d", got, tc.want)
			}
		})
	}
}

// Row 49 of plan v162: a reopen at a head pushed while the pull request was closed, which no
// synchronize reports, is the merge forward landing as a synchronize to that head would record it
// (mergeForwardLanded): the record keeps the ask with Landed set, the review asked of the reviewer,
// whose step already ran in merging, and Earlier kept for the completion fence; the merger, the
// worker of merging, is sent its landed message and the reviewer the review; and the hold ends.
//
// The negative control is the reopen at 88ed2ae7, which drops the ask with none of them.
func TestTakeHoldOfAnOwedMergeForwardEndsAtAReopenAtAHeadPushedWhileClosed(t *testing.T) {
	pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	wantOwed(t, pool, "after the merger's ask,", claim.RoleMerger)
	pr := recordedPR(t, pool, "LEGION-208")
	asked := *pr.MergeForward
	asked.Earlier = []record.LandedMergeForward{{Head: headThree, Landed: headEarlier}}
	pr.MergeForward = &asked
	seedPR(t, pool, pr)
	before := len(mergeForwardMessages(t, pool))
	ended := []string{}

	applyRefusingNothing(t, pool, endingEngineAt(10*time.Minute, &ended), closesAt(conflictHead, 10*time.Minute))
	applyRefusingNothing(t, pool, endingEngineAt(20*time.Minute, &ended), reopensAt(forwardHead, 20*time.Minute))

	got := recordedPR(t, pool, "LEGION-208").MergeForward
	if got == nil || got.Head != conflictHead || got.Landed != forwardHead || !got.ReviewAsked || !reflect.DeepEqual(got.Earlier, asked.Earlier) {
		t.Fatalf("after the reopen at %s the record's merge forward is %+v, want the ask of %s landed there with the review asked and Earlier %+v kept",
			forwardHead, got, conflictHead, asked.Earlier)
	}
	landed := func(role claim.Role, step record.MergeForwardStep) record.MergeForwardMessage {
		return record.MergeForwardMessage{Role: role, Step: step, Head: conflictHead, Landed: forwardHead, Base: "main",
			Phase: phase.Merging, PullRequest: 42, Why: asked.Why}
	}
	want := []record.MergeForwardMessage{landed(claim.RoleMerger, record.MergeForwardLanded), landed(claim.RoleReviewer, record.MergeForwardReview)}
	if messages := mergeForwardMessages(t, pool); len(messages) < before || !reflect.DeepEqual(messages[before:], want) {
		t.Fatalf("the reopen queued the merge-forward messages %+v, want %+v", messages[min(before, len(messages)):], want)
	}
	if want := []string{"LEGION-208"}; !reflect.DeepEqual(ended, want) {
		t.Fatalf("the reopen told the end of the hold %v, want %v", ended, want)
	}
	wantOwed(t, pool, "after the reopen,", "")
}
