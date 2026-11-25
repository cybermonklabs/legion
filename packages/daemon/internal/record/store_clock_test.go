package record

import (
	"cmp"
	"context"
	"go/ast"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// clockTime is the time a store built with a clock reads in this file's checks: a moment in 2001,
// so Postgres's now(), which reads the wall clock, gives another answer to every statement that
// asks it.
var clockTime = time.Date(2001, 3, 5, 12, 0, 0, 0, time.UTC)

// clockAt is the clock this file builds its stores with, which always reads clockTime.
func clockAt() time.Time { return clockTime }

// nowStatement is one Go function of internal/record, internal/store or internal/treelifecycle
// that holds a statement reading Postgres's clock: a string literal naming now(), in any case.
// file is relative to the daemon's module, and function is functionName's, or "a package-level
// declaration" for a literal outside every function.
type nowStatement struct {
	file, function string
}

// clockRow is the table's answer for one nowStatement. A row of kind (a) runs the statement
// through a store built with a clock, on data the wall clock answers otherwise, and asserts the
// clock's answer (check). A row of kind (b) says why the admission pass and the engine never run
// the statement and the replay does not call it (never). A row is one or the other.
type clockRow struct {
	statement nowStatement
	check     func(t *testing.T)
	never     string
}

// A replay of a recorded window runs the daemon's own admission pass and engine on a simulated
// clock, and a statement that reads Postgres's now() reads the moment the replay ran instead: it
// stamps a time twenty-five years from the window, or compares the window's times with that
// moment and answers as if every lease had lapsed and every row were years old. The replay's own
// test (internal/replay's TestReplayWritesNoWallClockTime) finds the stamps, by reading every time
// column the replay wrote; this test covers the half that writes nothing, the comparisons, and
// the writes of those statements on data the replay's fixture may not reach.
//
// It lists each string literal naming now() from the record, store, treelifecycle and replay
// packages and fails for a statement no row answers or a row that names no statement. A row
// checks the store's injected clock or explains why nothing a replay runs reaches the statement.
func TestEveryStatementThatComparesWithNowTakesTheClock(t *testing.T) {
	module := filepath.Join("..", "..")
	found := nowStatements(t, module, "internal/record", "internal/store", "internal/treelifecycle", "internal/replay")
	rows := clockRows()
	answered := make(map[nowStatement]bool, len(rows))
	for _, row := range rows {
		answered[row.statement] = true
		if (row.check == nil) == (row.never == "") {
			t.Errorf("the row of %s in %s runs a check and gives a reason, or does neither: a row is of kind (a), a check through a store with a clock, or of kind (b), why nothing a replay runs reaches it",
				row.statement.function, row.statement.file)
		}
		if !slices.Contains(found, row.statement) {
			t.Errorf("the table answers %s in %s, which holds no statement naming now(): take the row out, or name the function the statement moved to",
				row.statement.function, row.statement.file)
		}
	}
	for _, statement := range found {
		if !answered[statement] {
			t.Errorf("%s in %s holds a statement that reads Postgres's now(), and the table has no row of it: run it through a store built with a clock and assert the clock's answer, or say why the admission pass and the engine never run it and the replay does not call it",
				statement.function, statement.file)
		}
	}
	for _, row := range rows {
		if row.check == nil {
			continue
		}
		t.Run(row.statement.function, row.check)
	}
}

// nowStatements is every nowStatement in the non-test Go source under each of dirs of module,
// once each and sorted. It fails the test where a directory holds no Go file, so a moved package
// is not read as a package with no statement.
func nowStatements(t *testing.T, module string, dirs ...string) []nowStatement {
	t.Helper()
	var statements []nowStatement
	for _, dir := range dirs {
		files := eachDeclaration(t, module, dir, func(file, function string, decl ast.Decl) {
			ast.Inspect(decl, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote a string literal of %s: %v", file, err)
				}
				if strings.Contains(strings.ToLower(text), "now()") {
					statements = append(statements, nowStatement{file: file, function: function})
				}
				return true
			})
		})
		if files == 0 {
			t.Fatalf("read no non-test Go file under %s of %s", dir, module)
		}
	}
	slices.SortFunc(statements, func(a, b nowStatement) int {
		return cmp.Or(cmp.Compare(a.file, b.file), cmp.Compare(a.function, b.function))
	})
	return slices.Compact(statements)
}

// clockRows is the table: one row for every statement nowStatements finds.
func clockRows() []clockRow {
	return []clockRow{
		{statement: nowStatement{"internal/record/dispatch_issues.go", "(*Postgres).TakeDispatchRefill"}, check: checkRefillMark},
		{statement: nowStatement{"internal/record/store.go", "(*Postgres).EnqueueStart"}, check: checkOwedStartCreated},
		{statement: nowStatement{"internal/record/store.go", "(*Postgres).MarkProcessed"}, check: checkProcessedAt},
		{statement: nowStatement{"internal/record/store.go", "(*Postgres).EnqueueGatedSteps"}, check: checkGatedStepsCreated},
		{statement: nowStatement{"internal/record/store.go", "(*Postgres).EnqueueDesignGates"}, check: checkDesignGatesCreated},
		{statement: nowStatement{"internal/record/store.go", "(*Postgres).EnqueueID"}, check: checkEnqueuedIDCreated},
		{statement: nowStatement{"internal/record/store.go", "(*Postgres).OwedStarts"}, check: checkOwedLease},
		{statement: nowStatement{"internal/record/store.go", "(*Postgres).RoleRuns"}, check: checkRoleRunAge},
		{statement: nowStatement{"internal/store/claims.go", "(*Store).PutClaimActivity"}, check: checkClaimActivity},
		{statement: nowStatement{"internal/store/claims.go", "(*Store).PutClaimStep"}, check: checkClaimStep},
		{statement: nowStatement{"internal/store/claims.go", "upsertClaim"}, check: checkClaimWrite},
		{statement: nowStatement{"internal/treelifecycle/lifecycle.go", "open"}, check: checkTreeOpened},
		{statement: nowStatement{"internal/store/idle.go", "(*Store).ReleaseIdleIssue"}, check: checkReleaseMark},
		{statement: nowStatement{"internal/store/idle.go", "insertOutboxRow"}, check: checkPauseRows},
		{statement: nowStatement{"internal/store/tree_lifecycle.go", "(*Store).IssueSandboxHome"},
			never: "the Sandbox runtime's orphan sweep (internal/runtime/sandbox); a replay runs no runtime"},
		{statement: nowStatement{"internal/store/wake.go", "CloseOpenResumes"},
			never: "the close of a pod's open resumes inside the idle release (ReleaseIdleIssue); a replay releases no pod"},
		{statement: nowStatement{"internal/store/wake.go", "(*PodRecords).ClosePodGoneResume"},
			never: "reached only through workflow.Config.Pods, the engine's wake of a claim whose pod a release took (workflow's wakeReleased); a replay's engine runs with no Pods, since a replay releases no pod"},
		{statement: nowStatement{"internal/treelifecycle/lifecycle.go", "ReserveCleanup"},
			never: "the outbox's close of a tree after its fact commits (store.Store.ReserveWorkflowTreeCleanup, ReserveOperatorTreeCleanup); a replay finishes a close row without releasing a tree"},
		{statement: nowStatement{"internal/treelifecycle/lifecycle.go", "ConfirmCleanup"},
			never: "the outbox's confirmation of a tree's released resources (store.Store.CleanupReservedTree); a replay releases no tree"},
	}
}

// wantClock fails t where got, what a statement wrote or answered, is not clockTime.
func wantClock(t *testing.T, what string, got time.Time) {
	t.Helper()
	if !got.Equal(clockTime) {
		t.Errorf("%s = %s, want the store's clock %s: the statement read Postgres's now(), the moment the test ran",
			what, got.UTC().Format(time.RFC3339Nano), clockTime.Format(time.RFC3339Nano))
	}
}

// clockIssue is a root of project ACME in implementing, as the rows of the outbox checks name.
func clockIssue(key string) Issue {
	return Issue{Key: key, Tree: key, Project: "ACME", Title: "Count the widgets per shelf", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "a0", HandedOver: true, LastDispatchSeq: 1,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// clockStart is the implementer's start of issue that waits for its slot, as workflow.StartOrOwe
// writes it, due at clockTime.
func clockStart(t *testing.T, issue string) OutboxRow {
	t.Helper()
	row, err := NewOutboxRow(issue, SuperviseRequest{Op: "start", Tree: issue, Role: claim.RoleImplementer,
		Phase: phase.Implementing, Generation: 1, Task: "Implement the change.", NeedsSlot: true}, clockTime)
	if err != nil {
		t.Fatalf("build the start of %s: %v", issue, err)
	}
	return row
}

// The refill mark a boot takes is stamped with the store's clock.
func checkRefillMark(t *testing.T) {
	ctx := context.Background()
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `insert into dispatch_issue_refills (project) values ('ACME')`)
		must(t, err)
		_, refilled, err := NewStoreWithClock(clockAt).TakeDispatchRefill(ctx, tx, "ACME")
		must(t, err)
		if !refilled {
			t.Fatal("the refill took no mark, though ACME's stood unstamped")
		}
		var at time.Time
		must(t, tx.QueryRow(ctx, `select refilled_at from dispatch_issue_refills where project = 'ACME'`).Scan(&at))
		wantClock(t, "the refill mark's refilled_at", at)
	})
}

// The first start an issue owes takes the store's clock as its created_at, which is the place the
// issue takes in the waiting line's returned part.
func checkOwedStartCreated(t *testing.T) {
	ctx := context.Background()
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		records := NewStoreWithClock(clockAt)
		must(t, records.PutIssue(ctx, tx, clockIssue("ACME-7")))
		must(t, records.EnqueueStart(ctx, tx, clockStart(t, "ACME-7")))
		var created time.Time
		must(t, tx.QueryRow(ctx, `select created_at from outbox where issue = 'ACME-7' and kind = 'supervise'`).Scan(&created))
		wantClock(t, "the owed start's created_at", created)
	})
}

// A fact marked processed takes the store's clock as its processed_at.
func checkProcessedAt(t *testing.T) {
	ctx := context.Background()
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		marked, err := NewStoreWithClock(clockAt).MarkProcessed(ctx, tx, "dispatch", "clock-event")
		must(t, err)
		if !marked {
			t.Fatal("the fact was not marked processed, though no row stood")
		}
		var at time.Time
		must(t, tx.QueryRow(ctx, `select processed_at from processed_events where source = 'dispatch' and event_id = 'clock-event'`).Scan(&at))
		wantClock(t, "the processed fact's processed_at", at)
	})
}

// outboxCreatedAt is the created_at of the one outbox row of kind for issue.
func outboxCreatedAt(t *testing.T, tx pgx.Tx, kind OutboxKind, issue string) time.Time {
	t.Helper()
	var created time.Time
	must(t, tx.QueryRow(context.Background(), `select created_at from outbox where kind = $1 and issue = $2`, string(kind), issue).Scan(&created))
	return created
}

// The project's gated-steps publish takes the store's clock as its created_at.
func checkGatedStepsCreated(t *testing.T) {
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		must(t, NewStoreWithClock(clockAt).EnqueueGatedSteps(context.Background(), tx,
			GatedStepsPublish{Project: "ACME", Steps: []DesignStep{DesignStepSpec}}, clockTime))
		wantClock(t, "the gated-steps publish's created_at", outboxCreatedAt(t, tx, OutboxKindGatedStepsPublish, "ACME"))
	})
}

// An issue's design-gates publish takes the store's clock as its created_at.
func checkDesignGatesCreated(t *testing.T) {
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		must(t, NewStoreWithClock(clockAt).EnqueueDesignGates(context.Background(), tx,
			DesignGatesPublish{Issue: "ACME-7", Gates: []DesignGateDocument{}}, clockTime))
		wantClock(t, "the design-gates publish's created_at", outboxCreatedAt(t, tx, OutboxKindDesignGatesPublish, "ACME-7"))
	})
}

// A row written for its id takes the store's clock as its created_at.
func checkEnqueuedIDCreated(t *testing.T) {
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		_, err := NewStoreWithClock(clockAt).EnqueueID(context.Background(), tx, clockStart(t, "ACME-7"))
		must(t, err)
		wantClock(t, "the row's created_at", outboxCreatedAt(t, tx, OutboxKindSupervise, "ACME-7"))
	})
}

// A lease that runs to an hour after the store's clock is live at that clock, so the start it
// holds is not owed: its lane has it.
func checkOwedLease(t *testing.T) {
	ctx := context.Background()
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		records := NewStoreWithClock(clockAt)
		must(t, records.PutIssue(ctx, tx, clockIssue("ACME-7")))
		must(t, records.Enqueue(ctx, tx, clockStart(t, "ACME-7")))
		_, err := tx.Exec(ctx, `update outbox set lease_token = 'lane', lease_until = $1 where issue = 'ACME-7'`, clockTime.Add(time.Hour))
		must(t, err)
		owed, err := records.OwedStarts(ctx, tx, "ACME")
		must(t, err)
		if len(owed) != 0 {
			t.Errorf("the starts owed = %#v, want none: a lane holds ACME-7's start until an hour after the store's clock, and only a lapse judged by Postgres's now() lets it go", owed)
		}
	})
}

// A queued start's age and its live lease are read at the store's clock.
func checkRoleRunAge(t *testing.T) {
	ctx := context.Background()
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		records := NewStoreWithClock(clockAt)
		must(t, records.PutIssue(ctx, tx, clockIssue("ACME-7")))
		row := clockStart(t, "ACME-7")
		lease := clockTime.Add(time.Hour)
		row.CreatedAt, row.LeaseToken, row.LeaseUntil = clockTime.Add(-10*time.Minute), "lane", &lease
		must(t, records.Enqueue(ctx, tx, row))
		key := RoleRunKey{Token: claim.Token("legion-acme-acme-7-implementer"), Issue: "ACME-7", Role: claim.RoleImplementer, Generation: 1}
		runs, err := records.RoleRuns(ctx, tx, []RoleRunKey{key})
		must(t, err)
		queued := runs[key.Token].Queued
		if len(queued) != 1 {
			t.Fatalf("the queued starts of %s = %#v, want the one row written", key.Token, queued)
		}
		if queued[0].Age != 10*time.Minute {
			t.Errorf("the start's age = %s, want 10m0s: created ten minutes before the store's clock", queued[0].Age)
		}
		if !queued[0].Running {
			t.Error("the start reads as no lane's, want running: its lease runs to an hour after the store's clock")
		}
	})
}

// An unowned release marks and queues its teardown at the same store time.
func checkReleaseMark(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	st.UseClock(clockAt)
	const issue = "ACME-7"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStoreWithClock(clockAt).PutIssue(ctx, tx, clockIssue(issue)))
	})
	row, err := NewOutboxRow(issue, IssueRelease{Tree: issue, Generation: 1, TreeGeneration: 1}, clockTime)
	must(t, err)
	marked, err := st.ReleaseIdleIssue(ctx, "acme", issue, func(pods.IdleView) (store.IdleReleaseMark, error) {
		return store.IdleReleaseMark{Generation: 1, SuspendsNoRole: true, Rows: []store.IdleReleaseRow{
			{Kind: string(row.Kind), Payload: row.Payload, Boundary: true},
		}}, nil
	})
	must(t, err)
	if !marked {
		t.Fatal("the release left no mark")
	}
	var at, created, due time.Time
	must(t, st.Pool().QueryRow(ctx, `select m.marked_at, o.created_at, o.next_at
		from issue_releases m join outbox o on o.id = m.boundary_row
		where m.project = 'acme' and m.issue = $1`, issue).Scan(&at, &created, &due))
	wantClock(t, "the release's marked_at", at)
	wantClock(t, "the teardown's created_at", created)
	wantClock(t, "the teardown's next_at", due)
}

// clockClaim is the implementer's claim of ACME-7, which the claim checks write.
func clockClaim() supervise.Claim {
	return supervise.Claim{Token: claim.Token("legion-acme-acme-7-implementer"), Project: "ACME", Tree: "ACME-7", Issue: "ACME-7",
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateRetired, Session: "ses_implementer",
		SessionFile: "/sessions/implementer.jsonl"}
}

// ageClaim sets the claim's updated_at back to 2001-01-01, so a write that does not stamp it
// leaves a value no clock gives.
func ageClaim(t *testing.T, st *store.Store, token claim.Token) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update claims set updated_at = '2001-01-01T00:00:00Z' where token = $1`, string(token))
		return err
	}); err != nil {
		t.Fatalf("age the claim %s: %v", token, err)
	}
}

// claimTimes reads the claim's updated_at and activity_at as the store holds them.
func claimTimes(t *testing.T, st *store.Store, token claim.Token) (updated time.Time, activity *time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select updated_at, activity_at from claims where token = $1`, string(token)).Scan(&updated, &activity)
	}); err != nil {
		t.Fatalf("read the claim %s: %v", token, err)
	}
	return updated, activity
}

// A claim write is stamped with the store's clock.
func checkClaimWrite(t *testing.T) {
	st := migratedStore(t)
	st.UseClock(clockAt)
	must(t, st.PutClaim(context.Background(), clockClaim()))
	updated, _ := claimTimes(t, st, clockClaim().Token)
	wantClock(t, "the claim's updated_at", updated)
}

// A session's activity report is bounded by the store's clock and stamps the claim with it: a
// report dated a minute ahead of the clock is taken at the clock.
func checkClaimActivity(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	st.UseClock(clockAt)
	must(t, st.PutClaim(ctx, clockClaim()))
	ageClaim(t, st, clockClaim().Token)
	known, err := st.PutClaimActivity(ctx, clockClaim().Token, 1, claim.SessionActivity{At: clockTime.Add(time.Minute)})
	must(t, err)
	if !known {
		t.Fatal("the store does not know the claim at generation 1, which the check wrote")
	}
	updated, activity := claimTimes(t, st, clockClaim().Token)
	wantClock(t, "the claim's updated_at after the activity report", updated)
	if activity == nil {
		t.Fatal("the report wrote no activity_at")
	}
	wantClock(t, "the report's activity_at, dated a minute ahead of the store's clock", *activity)
}

// A step record stamps the claim with the store's clock.
func checkClaimStep(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	st.UseClock(clockAt)
	must(t, st.PutClaim(ctx, clockClaim()))
	ageClaim(t, st, clockClaim().Token)
	known, err := st.PutClaimStep(ctx, clockClaim().Token, supervise.StepRecord{At: clockTime.Add(-time.Minute)})
	must(t, err)
	if !known {
		t.Fatal("the store does not know the claim, which the check wrote")
	}
	updated, _ := claimTimes(t, st, clockClaim().Token)
	wantClock(t, "the claim's updated_at after the step", updated)
}

// A tree's lifecycle opened through a store with a clock — its next epoch, after a confirmed
// cleanup, and its first, where no row stands — is stamped with that clock. It is the path the
// admission pass takes when it promotes a queued root (record.Store.OpenTreeLifecycle).
func checkTreeOpened(t *testing.T) {
	ctx := context.Background()
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		records := NewStoreWithClock(clockAt)
		_, err := tx.Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority, cleanup_started, cleanup_confirmed_at, updated_at)
			values ('acme', 'ACME-7', 1, 'workflow', true, $1, $1)`, clockTime.Add(-time.Hour))
		must(t, err)
		next, err := records.OpenTreeLifecycle(ctx, tx, "acme", "ACME-7", treelifecycle.AuthorityWorkflow)
		must(t, err)
		if next.Epoch != 2 {
			t.Fatalf("the open of ACME-7 answered epoch %d, want 2: its cleanup confirmed, so the open is its next epoch", next.Epoch)
		}
		first, err := records.OpenTreeLifecycle(ctx, tx, "acme", "ACME-8", treelifecycle.AuthorityWorkflow)
		must(t, err)
		if !first.Created {
			t.Fatal("the open of ACME-8 created no row, though none stood")
		}
		for _, tree := range []string{"ACME-7", "ACME-8"} {
			var updated time.Time
			must(t, tx.QueryRow(ctx, `select updated_at from tree_lifecycles where project = 'acme' and tree = $1`, tree).Scan(&updated))
			wantClock(t, "the lifecycle's updated_at of "+tree, updated)
		}
	})
}

// clockPauseClaim is the idle implementer's claim of ACME-7, the one live claim the pause check's
// pause stops.
func clockPauseClaim() supervise.Claim {
	return supervise.Claim{Token: claim.Token("legion-acme-acme-7-implementer"), Project: "acme", Tree: "ACME-7", Issue: "ACME-7",
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle}
}

// clockPauseStopRow is the pause service's stop row of stop (internal/pause's StopRow), keyed by
// the claim's issue.
func clockPauseStopRow(pause store.Pause, stop store.PauseStopClaim) (store.PauseRow, error) {
	row, err := NewOutboxRow(stop.Issue, SuperviseRequest{Op: "pause_stop", Tree: stop.Tree, Role: stop.Role,
		Generation: stop.Generation, Pause: pause.ID}, pause.Since)
	if err != nil {
		return store.PauseRow{}, err
	}
	return store.PauseRow{Issue: row.Issue, Kind: string(row.Kind), Payload: row.Payload}, nil
}

// clockPauseNotice is the controller's notice of a pause or, with resumed, of its resume, under
// the project's key.
func clockPauseNotice(resumed bool) func(store.Pause) (store.PauseRow, error) {
	return func(p store.Pause) (store.PauseRow, error) {
		notice := ControllerNotice{Kind: ProjectPausedNotice, Pause: &ProjectPause{ID: p.ID, By: PauseActor{Kind: string(p.By.Kind)}, At: p.Since,
			Reason: p.Reason}}
		if resumed {
			notice = ControllerNotice{Kind: ProjectResumedNotice, Pause: &ProjectPause{ID: p.ID, By: PauseActor{Kind: string(p.EndedBy.Kind)},
				At: p.EndedAt}}
		}
		row, err := NewOutboxRow(p.Project, notice, p.Since)
		if err != nil {
			return store.PauseRow{}, err
		}
		return store.PauseRow{Issue: row.Issue, Kind: string(row.Kind), Payload: row.Payload}, nil
	}
}

// A project's pause queues its stop rows and its notice, and its resume its notice, due and
// created at the store's clock, as a replay pauses the project its record shows paused
// (internal/replay's driver.pause): a stop row due at the moment the replay ran would never come
// due inside the window. The check pauses ACME, whose implementer of ACME-7 is live, reads the
// stop row and the pause's notice, then resumes ACME and reads the resume's notice.
func checkPauseRows(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	st.UseClock(clockAt)
	must(t, st.PutClaim(ctx, clockPauseClaim()))
	operator := store.PauseActor{Kind: store.PauseByOperator}
	_, stops, err := st.Pause(ctx, store.PauseRequest{Project: "ACME", By: operator, Reason: "the operator moves the database",
		At: clockTime, StopRow: clockPauseStopRow, Notice: clockPauseNotice(false)})
	must(t, err)
	if len(stops) != 1 {
		t.Fatalf("the pause asked to stop %+v, want ACME-7's live implementer", stops)
	}
	queued := func(when string, want int) {
		t.Helper()
		rows, err := st.Pool().Query(ctx, `select kind || ' ' || issue, next_at, created_at from outbox order by id`)
		must(t, err)
		defer rows.Close()
		read := 0
		for rows.Next() {
			var row string
			var next, created time.Time
			must(t, rows.Scan(&row, &next, &created))
			wantClock(t, when+": the next_at of "+row, next)
			wantClock(t, when+": the created_at of "+row, created)
			read++
		}
		must(t, rows.Err())
		if read != want {
			t.Fatalf("%s the outbox holds %d rows, want %d", when, read, want)
		}
	}
	queued("after the pause", 2)
	_, err = st.Resume(ctx, store.ResumeRequest{Project: "ACME", By: operator, At: clockTime, Notice: clockPauseNotice(true)})
	must(t, err)
	// The resume deleted the stop row no lane ran, and queued its own notice beside the pause's.
	queued("after the resume", 2)
}
