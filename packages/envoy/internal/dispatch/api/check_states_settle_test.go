// check_states_settle_test.go holds how a settle starts and runs (check_states.go): the trigger of
// each table a state reads marks the issue owed, whoever wrote the row; the writer's commit wakes
// the loop and the background pass settles a mark no wake started; two writers and two settlers
// end with the state Dispatch computes; a settle asks GitHub nothing about a check it publishes no
// state of; the lease pool counts the loop; and a held hook ends with its test. The rig is
// check_states_test.go's.
package api

//ci:reads packages/envoy/cmd/dispatch/main.go TestTheLeasePoolCountsTheCheckStatesLoop parses ../../../cmd/dispatch/main.go for the lease pool's reservations it counts

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
)

// TestTwoWritersAndTheSettlerEndWithThePublishedStateDispatchComputes: two settlers, A and B, as
// two processes, each on a lease connection of its own. Writer 1 records a pass of k1 at H1 as the
// implementer, writer 2 retracts it, and the settles run in a fixed order, held at the settler's
// hooks. After each step the published state of k1 at H1 is the one a fresh computation gives and
// no mark of ACME-1 is left, and the events of k1 at H1 are yes, no, yes, no and no for fail.
func TestTwoWritersAndTheSettlerEndWithThePublishedStateDispatchComputes(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	a, b := rig.settler, rig.newSettler(t, "check states of a second process")
	settleOwedBy(t, rig, b)
	ctx := store.WithTransactionTracking(t.Context())
	var k1 []checkStatesEvent

	// 1. B publishes the first state of k1 at H1.
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	wantEvents(t, "step 1", eventsOfCheck(settleBy(t, rig, b), "k1", eachHeadH1), stateWant{"k1", eachHeadH1, "no", "no_result"})
	wantPublishedIsComputed(t, "step 1", rig, "k1", eachHeadH1)

	// 2. Writer 1 records; A computes yes and is held; writer 2 retracts; B skips, since A holds
	// the lease; A publishes yes, and its next round computes no and publishes it.
	pass := eachHeadResult(t, rig.handler, rig.issue, "k1", "implementer", "pass", eachHeadH1)
	hold := newHoldOnce(t)
	a.hooks.afterCompute = hold.hook
	after := lastEventID(t, rig.database)
	settled := make(chan [3]bool, 1)
	go func() {
		ran, failed, owed := a.settle(ctx, rig.issue)
		settled <- [3]bool{ran, failed, owed}
	}()
	hold.await(t, "A's settle")
	if retracted := retractAsPerson(t, rig.handler, pass, "Writer 2 retracts the pass."); retracted.Code != http.StatusCreated {
		t.Fatalf("writer 2's retraction: status=%d body=%s", retracted.Code, retracted.Body.String())
	}
	if ran, _, _ := b.settle(ctx, rig.issue); ran {
		t.Fatal("B settled ACME-1 while A held its lease")
	}
	hold.release()
	if outcome := <-settled; outcome != [3]bool{true, false, false} {
		t.Fatalf("A's settle answered ran, failed, owed = %v, want a settle that ran and left nothing", outcome)
	}
	a.hooks.afterCompute = nil
	step := eventsOfCheck(checkStateEventsAfter(t, rig.database, after), "k1", eachHeadH1)
	wantEvents(t, "step 2", step, stateWant{"k1", eachHeadH1, "yes", ""}, stateWant{"k1", eachHeadH1, "no", "no_result"})
	k1 = append(k1, step...)
	wantPublishedIsComputed(t, "step 2", rig, "k1", eachHeadH1)

	// 3. Writer 1 records again and A settles it; writer 2 retracts and B settles it.
	pass = eachHeadResult(t, rig.handler, rig.issue, "k1", "implementer", "pass", eachHeadH1)
	step = eventsOfCheck(settleBy(t, rig, a), "k1", eachHeadH1)
	wantEvents(t, "step 3, A", step, stateWant{"k1", eachHeadH1, "yes", ""})
	k1 = append(k1, step...)
	if retracted := retractAsPerson(t, rig.handler, pass, "Writer 2 retracts the pass again."); retracted.Code != http.StatusCreated {
		t.Fatalf("writer 2's second retraction: status=%d body=%s", retracted.Code, retracted.Body.String())
	}
	step = eventsOfCheck(settleBy(t, rig, b), "k1", eachHeadH1)
	wantEvents(t, "step 3, B", step, stateWant{"k1", eachHeadH1, "no", "no_result"})
	k1 = append(k1, step...)
	wantPublishedIsComputed(t, "step 3", rig, "k1", eachHeadH1)

	// 4. Writers 1 and 2 record and retract before any settle: one compute, no event.
	pass = eachHeadResult(t, rig.handler, rig.issue, "k1", "implementer", "pass", eachHeadH1)
	if retracted := retractAsPerson(t, rig.handler, pass, "Writer 2 retracts before a settle."); retracted.Code != http.StatusCreated {
		t.Fatalf("writer 2's third retraction: status=%d body=%s", retracted.Code, retracted.Body.String())
	}
	if events := settleBy(t, rig, a); len(events) != 0 {
		t.Fatalf("step 4: a record and its retraction settled together published %v, want nothing", eventLines(events))
	}
	wantPublishedIsComputed(t, "step 4", rig, "k1", eachHeadH1)

	// 5. Writer 3's fail of k1, inserted in a transaction held open, draws the lower mark id; the
	// implementer's pass of k2 commits. A reads the mark of k2 alone, computes and is held while
	// writer 3 commits, publishes k2, and its next round publishes k1, no for fail.
	writer3, err := rig.database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin writer 3: %v", err)
	}
	t.Cleanup(func() { _ = writer3.Rollback(context.Background()) })
	writerFail(t, rig, func(ctx context.Context, sql string, args ...any) error {
		_, err := writer3.Exec(ctx, sql, args...)
		return err
	}, "k1", eachHeadH1)
	eachHeadResult(t, rig.handler, rig.issue, "k2", "implementer", "pass", eachHeadH1)
	hold = newHoldOnce(t)
	a.hooks.afterCompute = hold.hook
	after = lastEventID(t, rig.database)
	go func() {
		ran, failed, owed := a.settle(ctx, rig.issue)
		settled <- [3]bool{ran, failed, owed}
	}()
	hold.await(t, "A's settle of k2")
	if err := writer3.Commit(context.Background()); err != nil {
		t.Fatalf("commit writer 3: %v", err)
	}
	hold.release()
	if outcome := <-settled; outcome != [3]bool{true, false, false} {
		t.Fatalf("A's settle in step 5 answered ran, failed, owed = %v, want a settle that ran and left nothing", outcome)
	}
	a.hooks.afterCompute = nil
	events := checkStateEventsAfter(t, rig.database, after)
	wantEvents(t, "step 5", events, stateWant{"k2", eachHeadH1, "yes", ""}, stateWant{"k1", eachHeadH1, "no", "fail"})
	k1 = append(k1, eventsOfCheck(events, "k1", eachHeadH1)...)
	wantPublishedIsComputed(t, "step 5", rig, "k1", eachHeadH1)

	wantEvents(t, "the events of k1 at H1 after step 1", k1,
		stateWant{"k1", eachHeadH1, "yes", ""}, stateWant{"k1", eachHeadH1, "no", "no_result"},
		stateWant{"k1", eachHeadH1, "yes", ""}, stateWant{"k1", eachHeadH1, "no", "no_result"},
		stateWant{"k1", eachHeadH1, "no", "fail"})
}

// TestAMarkThatNoStartSettledIsSettledByTheBackgroundPass: the loop runs with a wake that nothing
// signals, as in a process whose listener lost the notification of a write, and with a sweep of
// 200 ms. A fail the tester records after the loop's start is published by the pass at its due
// time within 2 s, and no mark is left. A loop started after the write, as a process starts after
// a crash, publishes it in its first pass.
func TestAMarkThatNoStartSettledIsSettledByTheBackgroundPass(t *testing.T) {
	t.Run("a write after the loop's start", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		settleOwed(t, rig)
		rig.settler.schedule.sweep = 200 * time.Millisecond
		runCheckStatesLoop(t, rig.settler, make(chan struct{}))
		// The loop's first pass finds nothing due; the write comes after it.
		time.Sleep(2 * rig.settler.schedule.minGap)
		after := lastEventID(t, rig.database)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
		events := awaitEvents(t, rig, after, 2*time.Second, func(events []checkStatesEvent) bool { return len(events) > 0 })
		wantEvents(t, "the background pass", events, stateWant{"k1", eachHeadH1, "no", "fail"})
		if left := marksOf(t, rig.database, rig.issue); left != 0 {
			t.Fatalf("the background pass left %d marks of %s", left, rig.issue)
		}
	})
	t.Run("a write before the loop's start", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		settleOwed(t, rig)
		after := lastEventID(t, rig.database)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
		// A sweep far past the wait, so only the loop's first pass can publish within it.
		rig.settler.schedule.sweep = 10 * time.Minute
		runCheckStatesLoop(t, rig.settler, make(chan struct{}))
		events := awaitEvents(t, rig, after, 2*time.Second, func(events []checkStatesEvent) bool { return len(events) > 0 })
		wantEvents(t, "the first pass", events, stateWant{"k1", eachHeadH1, "no", "fail"})
		if left := marksOf(t, rig.database, rig.issue); left != 0 {
			t.Fatalf("the first pass left %d marks of %s", left, rig.issue)
		}
	})
}

// TestTheWritersCommitStartsTheSettle: the loop runs on a real listener hub on the test database,
// with a sweep of 10 minutes, so only a notification can start a pass. A recorded result publishes
// its event within 2 s. A result inserted in a transaction the test rolls back leaves no mark and
// publishes nothing.
func TestTheWritersCommitStartsTheSettle(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	settleOwed(t, rig)
	wake := listenForCheckStates(t, rig)
	rig.settler.schedule.sweep = 10 * time.Minute
	runCheckStatesLoop(t, rig.settler, wake)
	// The loop's first pass and the pass the hub's signal after its LISTEN earned find nothing.
	time.Sleep(4 * rig.settler.schedule.minGap)

	after := lastEventID(t, rig.database)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
	events := awaitEvents(t, rig, after, 2*time.Second, func(events []checkStatesEvent) bool { return len(events) > 0 })
	wantEvents(t, "the commit's notification", events, stateWant{"k1", eachHeadH1, "no", "fail"})

	after = lastEventID(t, rig.database)
	rolled, err := rig.database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the rolled-back writer: %v", err)
	}
	writerFail(t, rig, func(ctx context.Context, sql string, args ...any) error {
		_, err := rolled.Exec(ctx, sql, args...)
		return err
	}, "k2", eachHeadH1)
	if err := rolled.Rollback(context.Background()); err != nil {
		t.Fatalf("roll the writer back: %v", err)
	}
	if left := marksOf(t, rig.database, rig.issue); left != 0 {
		t.Fatalf("a rolled-back result left %d marks of %s", left, rig.issue)
	}
	if events := awaitEvents(t, rig, after, 2*time.Second, func(events []checkStatesEvent) bool { return len(events) > 0 }); len(events) != 0 {
		t.Fatalf("a rolled-back result published %v", eventLines(events))
	}
}

// TestAWriteOfTheReleaseBeforeIsPublished: a pull_request_heads row and a check_results row
// inserted with plain SQL, as the release before this one writes them with no code of this build,
// each publish their states: the triggers mark the issue whoever wrote the row.
func TestAWriteOfTheReleaseBeforeIsPublished(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	settleOwed(t, rig)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, event_at, checked_at)
		values ($1, 70, $2, 'legion/work', 'main', 'open', $3, now(), now())`, ciJobRepository, eachHeadH1, rig.issue); err != nil {
		t.Fatalf("insert the head of #70: %v", err)
	}
	wantEventSet(t, "a head row of the release before", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"})

	writerFail(t, rig, func(ctx context.Context, sql string, args ...any) error {
		_, err := rig.database.Pool.Exec(ctx, sql, args...)
		return err
	}, "k1", eachHeadH1)
	wantEvents(t, "a result row of the release before", settleOwed(t, rig), stateWant{"k1", eachHeadH1, "no", "fail"})
}

// TestASettleAsksNoBranchAboutACheckItPublishesNoStateOf: ACME-1's Verification document holds k1,
// a `before_merge` check, beside k8, a `default_branch` check, and k9, an `after_merge` check, and
// #70 is open at H1. The tester records a pass of k1 at H1, and a pass of k8 and of k9 at H2, a
// commit no branch of the stand-in for GitHub carries. The settles publish k1's state at H1 and ask
// GitHub nothing about H2 and the default branch, and store no answer of where H2 stands: the
// settler publishes the states of `before_merge` checks alone, and a "not on the branch" it stored
// would decide what the results route answers once the repository cannot be read. The results route
// then compares k8 and k9 as it always did, one compare read of main...H2 whose answer it stores,
// so this count sees a compare read wherever one is made.
func TestASettleAsksNoBranchAboutACheckItPublishesNoStateOf(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesBlock("k1", ciCheckOrdinary, "The server records a result against a check.")+"\n"+
		checkStatesBlock("k8", `when="default_branch" `+ciCheckOrdinary, "Production answers on the trunk.")+"\n"+
		checkStatesBlock("k9", `when="after_merge" `+ciCheckOrdinary, "Production answers at the merged commit."))
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	eachHeadResult(t, rig.handler, rig.issue, "k8", "tester", "pass", eachHeadH2)
	eachHeadResult(t, rig.handler, rig.issue, "k9", "tester", "pass", eachHeadH2)
	stored := func() int {
		t.Helper()
		var count int
		if err := rig.database.Pool.QueryRow(context.Background(),
			`select count(*) from branch_commits where sha = $1`, eachHeadH2).Scan(&count); err != nil {
			t.Fatalf("count the stored branch answers of H2: %v", err)
		}
		return count
	}

	wantEvents(t, "the settles of the three results", settleOwed(t, rig), stateWant{"k1", eachHeadH1, "yes", ""})
	if reads, answers := rig.github.compareReadsOf("main", eachHeadH2), stored(); reads != 0 || answers != 0 {
		t.Fatalf("the settles read the comparison main...H2 %d times and stored %d answers of where H2 stands, want none: they publish no state of k8 or k9", reads, answers)
	}

	readResults(t, rig.handler, rig.issue)
	if reads, answers := rig.github.compareReadsOf("main", eachHeadH2), stored(); reads != 1 || answers != 1 {
		t.Fatalf("the results route read the comparison main...H2 %d times and stored %d answers of where H2 stands, want one of each", reads, answers)
	}
}

// TestEveryTableAStateReadsHasItsTrigger is the inventory of the writes that mark an issue owed:
// each table the state of a check at a head is computed from holds an enabled trigger whose
// function marks the issue (check_states_owe_*). A table that a later change adds to the state's
// computation joins this list.
func TestEveryTableAStateReadsHasItsTrigger(t *testing.T) {
	database := storetest.Open(t)
	tables := []string{"check_results", "ci_check_results", "check_result_retractions", "ci_check_result_retractions", "document_blocks", "pull_request_heads"}
	rows, err := database.Pool.Query(context.Background(), `
		select distinct c.relname from pg_trigger g
		join pg_class c on c.oid = g.tgrelid
		join pg_proc p on p.oid = g.tgfoid
		where not g.tgisinternal and g.tgenabled <> 'D' and p.proname like 'check\_states\_owe\_%' and c.relname = any($1)`, tables)
	if err != nil {
		t.Fatalf("read the triggers: %v", err)
	}
	var marked []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("read a trigger's table: %v", err)
		}
		marked = append(marked, table)
	}
	rows.Close()
	for _, table := range tables {
		if !slices.Contains(marked, table) {
			t.Errorf("%s holds no enabled trigger of a check_states_owe_ function: a write to it changes a state and marks no issue owed", table)
		}
	}
}

// TestTheLeasePoolCountsTheCheckStatesLoop: the lease pool's callers are complete with the
// reservations cmd/dispatch's main makes, read from its source, beside the intakes' own and the
// keyword fill's own, and incomplete with the check states settler's taken out.
func TestTheLeasePoolCountsTheCheckStatesLoop(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "..", "cmd", "dispatch", "main.go"), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse cmd/dispatch/main.go: %v", err)
	}
	var names []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if name, ok := call.Fun.(*ast.Ident); !ok || name.Name != "reserve" {
			return true
		}
		if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
			if value, err := strconv.Unquote(literal.Value); err == nil {
				names = append(names, value)
			}
		}
		return true
	})
	if len(names) == 0 {
		t.Fatal("cmd/dispatch/main.go makes no reserve(\"...\") call the test can read")
	}
	if !slices.Contains(names, "check states") {
		t.Fatalf("cmd/dispatch/main.go reserves %v, and no connection for the check states settler", names)
	}
	reserveAll := func(skip string) error {
		callers, err := storetest.Open(t).Pool.Leases()
		if err != nil {
			t.Fatalf("open the lease pool: %v", err)
		}
		for _, name := range names {
			if name == skip {
				continue
			}
			if _, err := callers.For(name); err != nil {
				t.Fatalf("reserve %q: %v", name, err)
			}
		}
		if _, err := delivery.ReserveIntakeLeases(callers); err != nil {
			t.Fatalf("reserve the intakes' callers: %v", err)
		}
		if _, err := textversions.ReserveLeases(callers); err != nil {
			t.Fatalf("reserve the keyword fill's callers: %v", err)
		}
		return callers.Complete()
	}
	if err := reserveAll(""); err != nil {
		t.Fatalf("the build's reservations do not complete the lease pool: %v", err)
	}
	if err := reserveAll("check states"); err == nil {
		t.Fatal("the reservations without the check states settler's complete the lease pool: its connection is counted for nothing")
	}
}

// TestAHeldHookIsReleasedWhenItsTestEnds: a subtest that ends while its settle is held at a hook,
// as one whose assertion fails does, still ends: the hook returns once the subtest's context is
// cancelled, which comes before every cleanup, so the cleanup that stops the loop waits for no
// held settle. A watchdog releases the hook where it did not return, and the test then fails.
func TestAHeldHookIsReleasedWhenItsTestEnds(t *testing.T) {
	watchdog := make(chan bool, 1)
	t.Run("a subtest that ends with its settle held", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		hold := newHoldOnce(t)
		rig.settler.hooks.afterMarks = hold.hook
		runCheckStatesLoop(t, rig.settler, make(chan struct{}))
		hold.await(t, "the loop's first settle")
		go func() {
			select {
			case <-hold.returned:
				watchdog <- false
			case <-time.After(10 * time.Second):
				watchdog <- true
				hold.release()
			}
		}()
	})
	if <-watchdog {
		t.Fatal("the subtest's cleanup waited for its held settle until a watchdog released the hook")
	}
}
