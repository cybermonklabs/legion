package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// checkRoot is a closed root whose tree lingers for another hour: the state a production check's
// look runs in, where a phase start would finish without acting.
func checkRoot() record.Issue {
	until := time.Now().Add(time.Hour)
	return record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until}
}

// checkLookID is the look id of look 1 of openCheck's check, which checkStart's row gives: the
// id workflow's startLook makes for a look (crypto/rand's Text, 26 base32 characters), stores
// with the look and the check instance, and writes on the look's start row.
const checkLookID = "M4XQ2RZK7TBV6NWJHC5DLP3YGA"

// openCheck is issue's production check on acme/widgets#42 at merge, worked by the implementer —
// the role every row open before the project could name one carries — opened an hour ago with
// look 1 open since, woken by the merge, holding checkLookID as that look's id, and bound two
// weeks out.
func openCheck(issue record.Issue, merge string) record.ProductionCheck {
	opened := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	return record.ProductionCheck{
		Issue: issue.Key, Project: issue.Project, Tree: issue.Tree, Repo: "acme/widgets", Number: 42, MergeSHA: merge,
		Role: claim.RoleImplementer, OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Looks: 1, LookOpen: true, LookWakeAt: &opened,
		LastWakeKind: "merge", LastWakeDetail: merge, LastWakeAt: &opened,
		LookID: checkLookID, LookIDLook: 1, LookIDOpenedAt: opened,
	}
}

func putProductionCheck(t *testing.T, pool *store.Pool, records record.Store, check record.ProductionCheck) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutProductionCheck(context.Background(), tx, check)
	}); err != nil {
		t.Fatalf("put production check: %v", err)
	}
}

func readProductionCheck(t *testing.T, pool *store.Pool, records record.Store, issue string) record.ProductionCheck {
	t.Helper()
	var check *record.ProductionCheck
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		check, err = records.ProductionCheck(context.Background(), tx, issue)
		return err
	}); err != nil || check == nil {
		t.Fatalf("read production check of %s = %v, %v", issue, check, err)
	}
	return *check
}

// suspendedImplementer is issue's implementer as the close or the linger left it: suspended by the
// rule, its session kept, in the issue's tree.
func suspendedImplementer(t *testing.T, sup *supervisor, issue record.Issue, by supervise.SuspendSource) (*supervise.Machine, claim.Token) {
	t.Helper()
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: by, Session: "ses_implementer", SessionFile: "/sessions/implementer.jsonl"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return machine, token
}

// checkStart is the start row of look of issue's production check: the implementer, no phase, the
// issue's generation, fenced on the check (record.SuperviseRequest.CheckLook), naming no instance
// (a row queued before starts carried one), and giving checkLookID, the id openCheck's look 1
// holds.
func checkStart(t *testing.T, issue record.Issue, look int) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Generation: issue.Generation,
		CheckLook: look, LookID: checkLookID, Task: "Production check of " + issue.Key}, time.Now())
	row.ID = 57
	return row
}

// sameCheckFact says whether got is want, a fact of check: a check fact's OpenedAt is check's as
// an instant (the executor reads it from Postgres, in a location of pgx's choosing, where want
// names none) and every other field want's.
func sameCheckFact(got, want intake.Fact, check record.ProductionCheck) bool {
	switch got := got.(type) {
	case intake.CheckLookEnded:
		want, ok := want.(intake.CheckLookEnded)
		if !ok || !got.OpenedAt.Equal(check.OpenedAt) {
			return false
		}
		got.OpenedAt = want.OpenedAt
		return got == want
	}
	return got == want
}

// A check start is the one start a lingering tree runs: it reaches the implementer the linger
// suspended, resumes it, and delivers the look's task, where a phase start on the same tree finishes
// without acting. The delivery names no phase, so nothing drops it for the phase the issue has left,
// the row's id is its delivery's, and the delivery keeps the look id the row gives.
func TestACheckStartResumesTheSuspendedImplementerUnderALingeringRoot(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	putProductionCheck(t, pool, records, openCheck(root, "a1b2c3d4"))
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}

	if err := runner.execute(ctx, checkStart(t, root, 1)); err != nil {
		t.Fatalf("check start under the lingering root = %v, want the implementer resumed", err)
	}
	got := machine.Claim()
	if got.LastStartRow != 57 || len(rt.CallsOf("Resume")) != 1 {
		t.Fatalf("implementer after the check start = %s with last start %d and %d resumes, want resumed by row 57", got.State, got.LastStartRow, len(rt.CallsOf("Resume")))
	}
	if got.Pending == nil || got.Pending.ID != "outbox:57" || got.Pending.Phase != "" || got.Pending.Generation != 1 || !strings.HasPrefix(got.Pending.Task, "Production check of ") ||
		got.Pending.LookID != checkLookID {
		t.Fatalf("implementer's pending delivery = %+v, want the look's task under outbox:57 with no phase at generation 1, keeping the row's look id", got.Pending)
	}
	// The same row again is the same delivery, not a second one.
	if err := runner.execute(ctx, checkStart(t, root, 1)); err != nil {
		t.Fatalf("the check start run again = %v, want its delivery accepted as the same", err)
	}
}

// noLookIDReason is the outbox's log reason for a check start row that gives no look id, which an
// older daemon queued (record.WhyLookDoesNotHold).
const noLookIDReason = "the row gives no look id: an older daemon queued it"

// A check start whose look id is not the one the check holds for its look finishes without acting,
// logged with why: a row with no look id, which an older daemon queued and whose task gives none;
// a row with another id; a row of a look for which the check holds no id; and a row that gives the
// id the check holds for the look before (CurrentLookID is "" for each of the last two). The
// implementer stays as it was, with nothing queued to it and no start recorded against it, the
// row is done, and the look stays open. No line of the log carries a look id or the row's task.
func TestACheckStartWhoseLookIDIsNotTheStoredOneFinishesWithoutActing(t *testing.T) {
	t.Parallel()
	const (
		anotherID = "Z7Y6X5W4V3U2TSRQPONMLKJIHG"
		notOurs   = "the row's look id is not the id the check holds for the look"
	)
	for _, tc := range []struct {
		name   string
		check  func(record.ProductionCheck) record.ProductionCheck
		look   int
		lookID string
		why    string
	}{
		{name: "no id", check: func(c record.ProductionCheck) record.ProductionCheck { return c }, look: 1, why: noLookIDReason},
		{name: "another id", check: func(c record.ProductionCheck) record.ProductionCheck { return c }, look: 1, lookID: anotherID, why: notOurs},
		{name: "no stored id", check: func(c record.ProductionCheck) record.ProductionCheck {
			c.LookID, c.LookIDLook, c.LookIDOpenedAt = "", 0, time.Time{}
			return c
		}, look: 1, lookID: checkLookID, why: notOurs},
		{name: "an id stored for the previous look", check: func(c record.ProductionCheck) record.ProductionCheck {
			c.Looks = 2
			return c
		}, look: 2, lookID: checkLookID, why: notOurs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			check := tc.check(openCheck(root, "a1b2c3d4"))
			putProductionCheck(t, pool, records, check)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
			var logged testlog.Buffer
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				log: slog.New(slog.NewTextHandler(&logged, nil)), now: time.Now}
			task := "Production check of " + root.Key + ": root.\nThis look has the look id `" + tc.lookID + "`."
			enqueueOutbox(t, pool, records, mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer,
				Generation: root.Generation, CheckLook: tc.look, CheckOpenedAt: check.OpenedAt, LookID: tc.lookID, Task: task}, time.Now()))

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run once: %v", err)
			}
			if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(rt.CallsOf("Resume")) != 0 {
				t.Fatalf("implementer = %s with last start %d, pending %+v and %d resumes, want left suspended with nothing queued to it", got.State, got.LastStartRow, got.Pending, len(rt.CallsOf("Resume")))
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows after the start = %d, want the start done", left)
			}
			if got := readProductionCheck(t, pool, records, root.Key); !got.Open() || !got.LookOpen || got.Looks != tc.look {
				t.Fatalf("the check after the start = look %d open %t, want look %d left open", got.Looks, got.LookOpen, tc.look)
			}
			log := logged.String()
			if !strings.Contains(log, "production check start finishes without acting") || !strings.Contains(log, tc.why) {
				t.Fatalf("the log = %q, want the start finished without acting because %s", log, tc.why)
			}
			for _, secret := range []string{checkLookID, anotherID, "Production check of"} {
				if strings.Contains(log, secret) {
					t.Fatalf("the log carries %q, a look id or the row's task: %q", secret, log)
				}
			}
		})
	}
}

// A check start finishes without acting once its look is no longer the open one: the look ended (its
// bound, a report), a later look started, or the check closed. The implementer stays as it was, and
// no start is recorded against it.
func TestACheckStartFinishesWithoutActingWhenItsLookIsNotOpen(t *testing.T) {
	t.Parallel()
	closed := time.Now()
	for _, tc := range []struct {
		name  string
		check func(record.ProductionCheck) record.ProductionCheck
	}{
		{name: "the look ended", check: func(c record.ProductionCheck) record.ProductionCheck { c.LookOpen = false; return c }},
		{name: "a later look is open", check: func(c record.ProductionCheck) record.ProductionCheck { c.Looks = 2; return c }},
		{name: "the check closed", check: func(c record.ProductionCheck) record.ProductionCheck {
			c.LookOpen, c.ClosedAt, c.Outcome = false, &closed, record.CheckRecorded
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			putProductionCheck(t, pool, records, tc.check(openCheck(root, "a1b2c3d4")))
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}

			if err := runner.execute(context.Background(), checkStart(t, root, 1)); err != nil {
				t.Fatalf("check start of a look that is not open = %v, want finished without acting", err)
			}
			if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(rt.CallsOf("Resume")) != 0 {
				t.Fatalf("implementer = %s with last start %d, pending %+v and %d resumes, want left suspended with nothing started", got.State, got.LastStartRow, got.Pending, len(rt.CallsOf("Resume")))
			}
		})
	}
}

// A check start of an issue that runs again finishes without acting: a root reopened inside its
// linger (admit's readmit) stands in `admitted` with the status of the reopen, and the look its
// row would start would resume the implementer and relaunch the Sandbox under the run. The row's
// check is still open at its look, so the check alone would let it start.
func TestACheckStartOfAnIssueRunningAgainFinishesWithoutActing(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	root.Phase, root.Status, root.LingerUntil = phase.Admitted, "todo", nil
	putOutboxIssue(t, pool, records, root)
	putProductionCheck(t, pool, records, openCheck(root, "a1b2c3d4"))
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}

	if err := runner.execute(context.Background(), checkStart(t, root, 1)); err != nil {
		t.Fatalf("check start of an issue running again = %v, want finished without acting", err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(rt.CallsOf("Resume")) != 0 {
		t.Fatalf("implementer = %s with last start %d, pending %+v and %d resumes, want left suspended with nothing started", got.State, got.LastStartRow, got.Pending, len(rt.CallsOf("Resume")))
	}
	if check := readProductionCheck(t, pool, records, root.Key); !check.Open() || !check.LookOpen {
		t.Fatalf("the check after the refused start = %+v, want untouched, its look ending at its own bound", check)
	}
}

// A suspension of the implementer that holds — the operator's or the controller's — holds a phase
// start on the row's backoff until a resume lifts it; a look cannot
// wait that long, so the check start finishes without acting and ends the look as one nobody
// reported, of the check the row's issue holds, with the hold as its cause named by its holder,
// so the look's retry says whose resume it waited on.
func TestACheckStartOnAHeldImplementerEndsTheLook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		by    supervise.SuspendSource
		cause string
	}{
		{supervise.SuspendedByOperator, "operator hold"},
		{supervise.SuspendedByController, "controller hold"},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			check := openCheck(root, "a1b2c3d4")
			putProductionCheck(t, pool, records, check)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, _ := suspendedImplementer(t, sup, root, tc.by)
			facts := &outboxFactHandler{}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}

			if err := runner.execute(context.Background(), checkStart(t, root, 1)); err != nil {
				t.Fatalf("check start on the %s's hold = %v, want finished without acting", tc.by, err)
			}
			if got := machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != tc.by || got.LastStartRow != 0 || len(rt.CallsOf("Resume")) != 0 {
				t.Fatalf("implementer = %s by %s with last start %d and %d resumes, want left to the %s", got.State, got.SuspendedBy, got.LastStartRow, len(rt.CallsOf("Resume")), tc.by)
			}
			want := intake.CheckLookEnded{Issue: root.Key, Look: 1, Cause: tc.cause}
			if got := facts.facts(); len(got) != 1 || !sameCheckFact(got[0], want, check) {
				t.Fatalf("facts applied = %+v, want %+v of the check opened %s", got, want, check.OpenedAt)
			}
		})
	}
}

// olderDaemonsCheck is openCheck's check of issue as a daemon before dispatch://INFRA-3518 left
// it: its look 1 open, with no look id at all.
func olderDaemonsCheck(issue record.Issue) record.ProductionCheck {
	check := openCheck(issue, "a1b2c3d4")
	check.LookID, check.LookIDLook, check.LookIDOpenedAt = "", 0, time.Time{}
	return check
}

// bootOpenChecks runs the daemon's boot step over the open production checks
// (workflowRuntime.repairOpenChecks) on pool, as the boot runs it before the outbox runs any row,
// with the workflow's clock at at, and answers the look id it gave issue's open look.
func bootOpenChecks(t *testing.T, pool *store.Pool, records record.Store, at time.Time, issue string) string {
	t.Helper()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Clock: func() time.Time { return at }}, quietLogger())
	w := &workflowRuntime{pool: pool, records: records, engine: engine, dispatchProject: "LEGION", log: quietLogger()}
	if err := w.repairOpenChecks(context.Background()); err != nil {
		t.Fatalf("the boot's repair of the open production checks: %v", err)
	}
	id := readProductionCheck(t, pool, records, issue).CurrentLookID()
	if id == "" {
		t.Fatalf("the boot gave %s's open look no look id", issue)
	}
	return id
}

// reissueRow is the id of the start row the boot queued for issue's open look, the one row marked
// the new task of an open look (record.SuperviseRequest.LookReissue).
func reissueRow(t *testing.T, pool *store.Pool, issue string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `select id from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and (payload->>'lookReissue')::boolean`, issue).Scan(&id); err != nil {
		t.Fatalf("read the boot's start row of %s's open look: %v", issue, err)
	}
	return id
}

// The boot's new task of a look an older daemon opened (record.SuperviseRequest.LookReissue,
// workflow's RepairOpenChecks) that meets a hold of the implementer, the operator's or the
// controller's, waits for the resume as a phase start does (R6 of dispatch://INFRA-3518): the row
// waits on its backoff, the implementer stays held with nothing queued to it and nothing resumed,
// no fact ends the look, and the look stays open at look 1 with the id the boot gave it. Once the
// holder resumes the implementer, the row's next run gives it the look's new task, which carries
// that id. A start of a new look ends its look on the same hold instead
// (TestACheckStartOnAHeldImplementerEndsTheLook).
func TestAReissuedLookStartWaitsOutAnOperatorHold(t *testing.T) {
	t.Parallel()
	for _, by := range []supervise.SuspendSource{supervise.SuspendedByOperator, supervise.SuspendedByController} {
		t.Run(string(by), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			ctx := context.Background()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			putProductionCheck(t, pool, records, olderDaemonsCheck(root))
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, token := suspendedImplementer(t, sup, root, by)
			facts := &outboxFactHandler{}
			clock := time.Now()
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				handlers: []intake.Handler{facts}, log: quietLogger(), now: func() time.Time { return clock }}
			id := bootOpenChecks(t, pool, records, clock, root.Key)
			row := reissueRow(t, pool, root.Key)

			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the boot's start row: %v", err)
			}
			machine.Wait()
			if got := machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != by || got.LastStartRow != 0 || got.Pending != nil ||
				len(resumesOf(rt, token)) != 0 || outboxRows(t, pool) != 1 {
				t.Fatalf("under the %s's hold the implementer is %s by %s with last start %d, pending %+v, %d resumes and %d rows; want it held, nothing queued, the row waiting",
					by, got.State, got.SuspendedBy, got.LastStartRow, got.Pending, len(resumesOf(rt, token)), outboxRows(t, pool))
			}
			if got := facts.facts(); len(got) != 0 {
				t.Fatalf("facts applied under the %s's hold = %+v, want none: the hold ends no look", by, got)
			}
			if check := readProductionCheck(t, pool, records, root.Key); !check.Open() || !check.LookOpen || check.Looks != 1 || check.CurrentLookID() != id {
				t.Fatalf("the check under the %s's hold = %+v, want look 1 open with the id the boot gave it", by, check)
			}

			if err := machine.Handle(ctx, supervise.RequestResume{Claim: token, By: by}); err != nil {
				t.Fatalf("the %s's resume: %v", by, err)
			}
			clock = clock.Add(2 * time.Minute)
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the boot's start row after the resume: %v", err)
			}
			machine.Wait()
			if got := machine.Claim(); outboxRows(t, pool) != 0 || got.LastStartRow != row || got.Pending == nil || got.Pending.LookID != id ||
				got.Pending.CheckLook.Look != 1 || !strings.Contains(got.Pending.Task, "This look has the look id `"+id+"`.") {
				t.Fatalf("after the %s's resume: %d rows, last start %d, pending %+v; want the boot's row %d run and look 1's new task held for the implementer",
					by, outboxRows(t, pool), got.LastStartRow, got.Pending, row)
			}
			if got := facts.facts(); len(got) != 0 {
				t.Fatalf("facts applied = %+v, want none", got)
			}
		})
	}
}

// A start row of a look that an older daemon queued gives no look id, so it never starts the look,
// whether it runs before the boot's start row of that look or after it: it finishes without acting,
// logged with why, and the boot's row (record.SuperviseRequest.LookReissue) resumes the implementer
// once and gives it the look's new task, the one task of the look that reaches the claim
// (R4 and R6 of dispatch://INFRA-3518).
func TestAnOlderDaemonsLookStartFinishesWithoutActingInEitherOrder(t *testing.T) {
	t.Parallel()
	const olderTask = "Production check of LEGION-208: root.\nLook 1, woken by the merge. Then report with the legion tool's handoff_complete and a verdict."
	for _, tc := range []struct {
		name string
		// after is how long after the boot's row the older row is due: below zero it runs first.
		after time.Duration
	}{
		{name: "the older row first", after: -time.Minute},
		{name: "the boot's row first", after: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			ctx := context.Background()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			putProductionCheck(t, pool, records, olderDaemonsCheck(root))
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, token := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
			booted := time.Now().UTC().Truncate(time.Microsecond)
			older := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer, Generation: root.Generation,
				CheckLook: 1, Task: olderTask}, booted.Add(tc.after))
			enqueueOutbox(t, pool, records, older)
			id := bootOpenChecks(t, pool, records, booted, root.Key)
			row := reissueRow(t, pool, root.Key)
			var logged testlog.Buffer
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				log: slog.New(slog.NewTextHandler(&logged, nil)), now: func() time.Time { return booted.Add(time.Minute) }}

			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the two start rows: %v", err)
			}
			machine.Wait()
			got := machine.Claim()
			if outboxRows(t, pool) != 0 || got.LastStartRow != row || len(resumesOf(rt, token)) != 1 || got.Pending == nil || got.Pending.LookID != id ||
				!strings.Contains(got.Pending.Task, "This look has the look id `"+id+"`.") || got.Pending.Task == olderTask {
				t.Fatalf("after both rows: %d rows, last start %d, %d resumes, pending %+v; want both run, the implementer resumed once by the boot's row %d, holding look 1's new task",
					outboxRows(t, pool), got.LastStartRow, len(resumesOf(rt, token)), got.Pending, row)
			}
			for _, delivered := range got.DeliveredRows {
				if delivered != row {
					t.Fatalf("the implementer was delivered rows %v, want the boot's row %d alone", got.DeliveredRows, row)
				}
			}
			if log := logged.String(); !strings.Contains(log, "production check start finishes without acting") || !strings.Contains(log, noLookIDReason) {
				t.Fatalf("the log = %q, want the older row finished without acting because it gives no look id", log)
			}
			if check := readProductionCheck(t, pool, records, root.Key); !check.Open() || !check.LookOpen || check.Looks != 1 || check.CurrentLookID() != id {
				t.Fatalf("the check after both rows = %+v, want look 1 open with the id the boot gave it", check)
			}
		})
	}
}

// The boot gives the open look of an issue that runs again an id and a start row too, and that row
// finishes at the look's fence (checkLookHolds) as every start of a look of an issue the run holds
// does: the implementer stays as the run left it, with nothing queued and nothing resumed, and the
// look stays open, its id kept, for the run's own end to decide.
func TestTheBootsNewTaskOfALookOfAnIssueThatRunsAgainFinishesAtItsFence(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	root.Phase, root.Status, root.DispatchStatus, root.Generation, root.LingerUntil = phase.Admitted, "todo", "todo", 2, nil
	putOutboxIssue(t, pool, records, root)
	check := olderDaemonsCheck(root)
	check.OpenedGeneration = 1
	putProductionCheck(t, pool, records, check)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, token := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	var logged testlog.Buffer
	clock := time.Now()
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		log: slog.New(slog.NewTextHandler(&logged, nil)), now: func() time.Time { return clock }}
	id := bootOpenChecks(t, pool, records, clock, root.Key)
	reissueRow(t, pool, root.Key)

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the boot's start row: %v", err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(resumesOf(rt, token)) != 0 || outboxRows(t, pool) != 0 {
		t.Fatalf("implementer = %s with last start %d, pending %+v, %d resumes and %d rows; want it left suspended with nothing started, the row done",
			got.State, got.LastStartRow, got.Pending, len(resumesOf(rt, token)), outboxRows(t, pool))
	}
	if log := logged.String(); !strings.Contains(log, "production check start finishes without acting") || !strings.Contains(log, "the issue runs again (admitted, todo)") {
		t.Fatalf("the log = %q, want the boot's row finished without acting because the issue runs again", log)
	}
	if got := readProductionCheck(t, pool, records, root.Key); !got.Open() || !got.LookOpen || got.Looks != 1 || got.CurrentLookID() != id {
		t.Fatalf("the check after the boot's row = %+v, want look 1 open with the id the boot gave it", got)
	}
}

// A check start of a check a later merge replaced — its row names the first check's OpenedAt, and
// the issue's check is the second, at a look 1 of its own — finishes without acting: the
// implementer stays as it was, with no start recorded against it, since the look the row would
// start is the first check's, ended long before, and the second's look 1 has a start row of its own.
func TestACheckStartOfAReplacedCheckFinishesWithoutActing(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	second := openCheck(root, "f00dfeed")
	putProductionCheck(t, pool, records, second)
	first := openCheck(root, "a1b2c3d4")
	first.OpenedAt = second.OpenedAt.Add(-24 * time.Hour)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}

	stale := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer, Generation: root.Generation,
		CheckLook: 1, CheckOpenedAt: first.OpenedAt, LookID: checkLookID, Task: "Production check of " + root.Key}, time.Now())
	stale.ID = 58
	if err := runner.execute(context.Background(), stale); err != nil {
		t.Fatalf("the first check's start against the second = %v, want finished without acting", err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(rt.CallsOf("Resume")) != 0 {
		t.Fatalf("implementer = %s with last start %d, pending %+v and %d resumes, want left suspended with nothing started", got.State, got.LastStartRow, got.Pending, len(rt.CallsOf("Resume")))
	}
	if got := facts.facts(); len(got) != 0 {
		t.Fatalf("facts applied = %+v, want none", got)
	}
	if got := readProductionCheck(t, pool, records, root.Key); !got.LookOpen || got.Looks != 1 {
		t.Fatalf("the second check after the first check's start = look %d open %t, want its look 1 left open", got.Looks, got.LookOpen)
	}

	// The second check's own start row, naming it, resumes the implementer.
	own := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer, Generation: root.Generation,
		CheckLook: 1, CheckOpenedAt: second.OpenedAt, LookID: checkLookID, Task: "Production check of " + root.Key}, time.Now())
	own.ID = 59
	if err := runner.execute(context.Background(), own); err != nil {
		t.Fatalf("the second check's own start = %v, want the implementer resumed", err)
	}
	if got := machine.Claim(); got.LastStartRow != 59 || len(rt.CallsOf("Resume")) != 1 {
		t.Fatalf("implementer after the second check's start = %s with last start %d and %d resumes, want resumed by row 59", got.State, got.LastStartRow, len(rt.CallsOf("Resume")))
	}
}

// A check_look row ends the look it names as its bound; a check_bound row brings the check's bound
// due. Each is applied through the workflow's handlers: the look's under the event of the check
// and the look, so the row run again is a duplicate, and the bound's under the row's, so a bound
// the workflow deferred as a new row applies afresh.
func TestCheckLookAndCheckBoundRowsApplyTheirFacts(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, "a1b2c3d4")
	putProductionCheck(t, pool, records, check)
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}

	look := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1, OpenedAt: check.OpenedAt}, time.Now())
	look.ID = 61
	for range 2 {
		if err := runner.execute(ctx, look); err != nil {
			t.Fatalf("check_look row = %v", err)
		}
	}
	first := mustOutboxRow(t, root.Key, record.CheckBound{}, time.Now())
	first.ID = 62
	deferred := mustOutboxRow(t, root.Key, record.CheckBound{}, time.Now())
	deferred.ID = 63
	for _, row := range []record.OutboxRow{first, deferred} {
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("check_bound row %d = %v", row.ID, err)
		}
	}
	want := []intake.Fact{
		intake.CheckLookEnded{Issue: root.Key, Look: 1, Cause: "look bound"},
		intake.CheckBound{Issue: root.Key},
		intake.CheckBound{Issue: root.Key},
	}
	got := facts.facts()
	if len(got) != len(want) {
		t.Fatalf("facts applied = %+v, want %+v", got, want)
	}
	for i := range want {
		if !sameCheckFact(got[i], want[i], check) {
			t.Fatalf("fact %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A look's bound counts no time a project's pause held the check's role, nor the time that role's
// launch then waited its turn in the wake pacing: a look that began before the end of the
// project's latest pause counts its two hours (workflow.CheckLookBound) from the later of that end
// and the role's last ready where that ready is after the end, and nothing while the role's claim
// is still coming up. The look began an hour into a three-hour pause, so the row of its bound fell
// due at the pause's end and runs at the resume. With the role launching, as behind the resume's
// queue, the row waits a whole bound out and the look stays open; a ready of the role before the
// pause counts for nothing. With the role ready eleven minutes after the end, the row waits until
// that ready and the bound, and then ends the look as today: the role is suspended, its pod is
// deleted and the next look is named a bound out.
func TestACheckLookBoundAfterAPauseCountsFromTheCheckRolesReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		// ready is the role's last ready after the pause's end, negative for one before the pause.
		ready time.Duration
		// at is when the row runs after the pause's end, and waits is when it runs again.
		at, waits time.Duration
	}{
		{name: "the role still launching", state: supervise.StateLaunching, ready: -4 * time.Hour,
			at: time.Minute, waits: time.Minute + workflow.CheckLookBound},
		{name: "the role ready since eleven minutes after the end", state: supervise.StateReady, ready: 11 * time.Minute,
			at: 12 * time.Minute, waits: 11*time.Minute + workflow.CheckLookBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			ctx := context.Background()
			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatalf("open the store: %v", err)
			}
			t.Cleanup(st.Close)
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			now := time.Now().UTC().Truncate(time.Microsecond)
			ended := now.Add(-tc.at)
			began := ended.Add(-3 * time.Hour)
			endLegionPause(t, pool, openLegionPause(t, pool, began), ended)
			lookAt := began.Add(time.Hour)
			check := openCheck(root, "a1b2c3d4")
			check.OpenedAt, check.BoundAt, check.LookWakeAt, check.LastWakeAt = lookAt, lookAt.Add(336*time.Hour), &lookAt, &lookAt
			putProductionCheck(t, pool, records, check)
			token, err := claim.NewToken("legion", root.Key, check.Role)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: root.Tree, Issue: root.Key, Role: check.Role,
				Generation: 1, State: tc.state, LastReady: ended.Add(tc.ready), Session: "ses_implementer",
				SessionFile: "/sessions/implementer.jsonl"}); err != nil {
				t.Fatalf("store the check's role %s: %v", tc.state, err)
			}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, root.Key, record.CheckLook{Look: 1, OpenedAt: check.OpenedAt}, lookAt.Add(workflow.CheckLookBound)))
			engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour}, quietLogger())
			clock := now
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{engine}, project: "legion",
				log: quietLogger(), now: func() time.Time { return clock }}
			rowsOf := func(kind record.OutboxKind) int {
				t.Helper()
				var n int
				if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = $2`, root.Key, string(kind)).Scan(&n); err != nil {
					t.Fatalf("count the %s rows: %v", kind, err)
				}
				return n
			}

			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run once: %v", err)
			}
			if got := readProductionCheck(t, pool, records, root.Key); !got.LookOpen {
				t.Fatalf("look 1 after its row ran %s after the pause's end is ended, want it open: its bound counts from %s after the end",
					tc.at, tc.waits-workflow.CheckLookBound)
			}
			if suspends, wakes := rowsOf(record.OutboxKindIssueSuspend), rowsOf(record.OutboxKindCheckWake); suspends != 0 || wakes != 0 {
				t.Fatalf("after the row waited: %d issue_suspend and %d check_wake rows, want none", suspends, wakes)
			}
			var attempts int
			var nextAt time.Time
			if err := pool.QueryRow(ctx, `select attempts, next_at from outbox where issue = $1 and kind = 'check_look'`, root.Key).Scan(&attempts, &nextAt); err != nil {
				t.Fatalf("read the waiting check_look row: %v", err)
			}
			if want := ended.Add(tc.waits); attempts != 1 || !nextAt.Equal(want) {
				t.Fatalf("the check_look row = attempts %d due %s, want one attempt due %s after the pause's end",
					attempts, nextAt.Sub(ended), tc.waits)
			}
			if tc.state == supervise.StateLaunching {
				return
			}

			clock = ended.Add(tc.waits)
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run once at the bound: %v", err)
			}
			if got := readProductionCheck(t, pool, records, root.Key); got.LookOpen {
				t.Fatal("look 1 is still open once its row ran at the bound counted from the role's ready, want it ended")
			}
			if suspends, wakes := rowsOf(record.OutboxKindIssueSuspend), rowsOf(record.OutboxKindCheckWake); suspends != 1 || wakes != 1 {
				t.Fatalf("after the look ended: %d issue_suspend and %d check_wake rows, want one of each", suspends, wakes)
			}
		})
	}
}

// sentStatements records every statement a pool's connections send (pgx.QueryTracer).
type sentStatements struct {
	mu  sync.Mutex
	sql []string
}

func (s *sentStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sql = append(s.sql, data.SQL)
	return ctx
}

func (*sentStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *sentStatements) statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sql)
}

// The look's read of the project's latest pause and of its role's claim (lookBoundAfterPause)
// writes nothing and takes no lock, so its transaction opens read-only and reads no transaction
// id (store.TransactWith, pgx.ReadOnly): the database refuses a write the read would make, and
// the commit costs no id read. The pause ended after the look began, so the read goes on to the
// role's claim and the look waits for its bound; the runner's pool records what it sends.
func TestACheckLooksReadOfTheLatestPauseOpensReadOnlyAndReadsNoID(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ended := now.Add(-time.Minute)
	began := ended.Add(-3 * time.Hour)
	endLegionPause(t, pool, openLegionPause(t, pool, began), ended)
	root := checkRoot()
	lookAt := began.Add(time.Hour)
	check := openCheck(root, "a1b2c3d4")
	check.LookWakeAt = &lookAt
	sent := &sentStatements{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = sent
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the recording pool: %v", err)
	}
	t.Cleanup(traced.Close)
	runner := &outbox{pool: store.NewPool(traced), dispatchProject: "LEGION", records: record.NewStore(), project: "legion",
		log: quietLogger(), now: func() time.Time { return now }}
	row := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1, OpenedAt: check.OpenedAt}, lookAt.Add(workflow.CheckLookBound))
	err = runner.lookBoundAfterPause(ctx, row, 1, check)
	if due, waits := wait.UntilAt(err); !waits || !due.Equal(ended.Add(workflow.CheckLookBound)) {
		t.Fatalf("the look after the pause = %v, want it to wait a bound from the pause's end: the read found no ended pause", err)
	}
	statements := sent.statements()
	if len(statements) == 0 {
		t.Fatal("UNKNOWN: the pool sent no statement, so nothing says how the read opened its transaction")
	}
	begins, readOnly, ids, claims := 0, 0, 0, 0
	for _, sql := range statements {
		lower := strings.ToLower(strings.TrimSpace(sql))
		if strings.HasPrefix(lower, "begin") {
			begins++
			if strings.Contains(lower, "read only") {
				readOnly++
			}
		}
		if strings.Contains(sql, "pg_current_xact_id_if_assigned") {
			ids++
		}
		if strings.Contains(sql, "pending_task_deliveries") {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("reads of the role's claim = %d, want 1: the read stopped at the pause; sent %q", claims, statements)
	}
	if begins != 1 || readOnly != 1 || ids != 0 {
		t.Errorf("the read began %d transactions, %d of them read-only, and read the transaction's id %d times; want one read-only transaction and no id read; sent %q",
			begins, readOnly, ids, statements)
	}
}

// A check_look row of no open check — none recorded, or one closed since — and one of a check a
// later merge replaced (its OpenedAt another than the issue's check's) each finish without acting:
// the look they would end is not the issue's check's.
func TestACheckLookRowOfNoCheckOrAReplacedOneAppliesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		recorded bool
		closed   bool
		replaced bool
	}{
		{name: "no check"},
		{name: "a closed check", recorded: true, closed: true},
		{name: "a replaced check", recorded: true, replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			check := openCheck(root, "a1b2c3d4")
			if tc.closed {
				closed := time.Now().UTC().Truncate(time.Microsecond)
				check.LookOpen, check.ClosedAt, check.Outcome = false, &closed, record.CheckRecorded
			}
			if tc.recorded {
				putProductionCheck(t, pool, records, check)
			}
			named := check.OpenedAt
			if tc.replaced {
				named = named.Add(-24 * time.Hour)
			}
			facts := &outboxFactHandler{}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}
			row := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1, OpenedAt: named}, time.Now())
			row.ID = 61
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("check_look row = %v, want finished without acting", err)
			}
			if got := facts.facts(); len(got) != 0 {
				t.Fatalf("facts applied = %+v, want none", got)
			}
		})
	}
}

// A second production check of the same issue — a rerun's, after the first closed and its merge
// opened another — counts its looks from one again, and its look 1 has a bound row of its own
// (workflow.startLook). That row must end that look: the first check's look 1 ended at its own
// bound long before, and nothing clears processed_events between the two checks, so the event the
// row applies under must tell the two checks apart. Otherwise a silent implementer keeps its pod
// for the whole of the rerun's check, the cost the look bound exists to remove.
func TestACheckLookRowOfALaterCheckEndsThatChecksLook(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	engine := workflow.New(records, workflow.Config{Project: "legion", Steps: config.DesignGateRootIssues.Steps(), Linger: 24 * time.Hour}, quietLogger())
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{engine}, log: quietLogger(), now: time.Now}

	first := openCheck(root, "a1b2c3d4")
	putProductionCheck(t, pool, records, first)
	look := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1}, time.Now())
	look.ID = 61
	if err := runner.execute(ctx, look); err != nil {
		t.Fatalf("the first check's look bound = %v", err)
	}
	if got := readProductionCheck(t, pool, records, root.Key); got.LookOpen || !got.Open() {
		t.Fatalf("the first check after its look bound = look open %t, closed %t; want the look ended and the check open", got.LookOpen, !got.Open())
	}

	// The first check closed at its bound as not run; the rerun's merge opened the second a day
	// later, with its own look 1 and the look's own bound row.
	closed := time.Now().UTC().Truncate(time.Microsecond)
	first.ClosedAt, first.Outcome = &closed, record.CheckNotRun
	putProductionCheck(t, pool, records, first)
	second := openCheck(root, "f00dfeed")
	reopened := closed.Add(24 * time.Hour)
	second.OpenedAt, second.BoundAt, second.LookWakeAt, second.LastWakeAt, second.LastWakeDetail = reopened, reopened.Add(336*time.Hour), &reopened, &reopened, "f00dfeed"
	putProductionCheck(t, pool, records, second)
	later := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1}, time.Now())
	later.ID = 64
	if err := runner.execute(ctx, later); err != nil {
		t.Fatalf("the second check's look bound = %v", err)
	}
	if got := readProductionCheck(t, pool, records, root.Key); got.LookOpen {
		t.Fatalf("the second check's look 1 is still open after its own bound row ran: the row's fact was taken as the first check's look 1 bound; want the look ended")
	}
}

// A row whose effect waits for a known moment (wait.Until: a lingering tree's close waiting for a
// member's production check, bounded at a time the record holds) runs again at that moment rather
// than on its backoff; a moment already past is the backoff's.
func TestSettleRunsARowAgainAtTheMomentItsWaitNames(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{name: "a moment ahead", at: now.Add(3 * time.Hour), want: now.Add(3 * time.Hour)},
		{name: "a moment past", at: now.Add(-time.Minute), want: now.Add(time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.LingerClose{Generation: 1}, now))
			waiting := &outboxFactHandler{err: wait.Until(tc.at, "tree LEGION-208 lingers past its deadline; member LEGION-209's production check is open")}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{waiting}, log: quietLogger(), now: func() time.Time { return now }}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run once: %v", err)
			}
			var attempts int
			var nextAt time.Time
			var lastError string
			if err := pool.QueryRow(context.Background(), "select attempts, next_at, last_error from outbox where "+notStateRows).Scan(&attempts, &nextAt, &lastError); err != nil {
				t.Fatalf("read the waiting row: %v", err)
			}
			if attempts != 1 || !nextAt.Equal(tc.want) || !strings.Contains(lastError, "production check is open") {
				t.Fatalf("retry = attempts=%d next_at=%s error=%q, want one attempt due at %s", attempts, nextAt, lastError, tc.want)
			}
		})
	}
}

// A check_wake row applies the wake it carries — the look it was named at, the check instance it
// names and its reason — through the workflow's handlers. The same row run again is a duplicate
// and applies nothing; the row the workflow queues again while the issue runs is a row of its
// own, so its apply is fresh rather than this one's duplicate, as a deferred bound's is
// (checkBound).
func TestACheckWakeRowAppliesItsWakeAndARequeuedRowAppliesAfresh(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, "a1b2c3d4")
	check.LookOpen = false
	putProductionCheck(t, pool, records, check)
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}

	due := record.CheckWakeDue{Look: 1, OpenedAt: check.OpenedAt, WakeAt: check.OpenedAt.Add(2 * time.Hour), Reason: "the deploy has not carried the merge"}
	row := mustOutboxRow(t, root.Key, due, time.Now())
	row.ID = 71
	for range 2 {
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("check_wake row = %v", err)
		}
	}
	again := mustOutboxRow(t, root.Key, due, time.Now())
	again.ID = 72
	if err := runner.execute(ctx, again); err != nil {
		t.Fatalf("the check_wake row queued again = %v", err)
	}
	want := intake.CheckWakeDue{Issue: root.Key, Look: 1, OpenedAt: check.OpenedAt, WakeAt: due.WakeAt, Reason: "the deploy has not carried the merge"}
	got := facts.facts()
	if len(got) != 2 {
		t.Fatalf("facts applied = %+v, want the row's once and the row queued again's", got)
	}
	for i, fact := range got {
		wake, ok := fact.(intake.CheckWakeDue)
		if !ok || wake.Issue != want.Issue || wake.Look != want.Look || !wake.OpenedAt.Equal(want.OpenedAt) || !wake.WakeAt.Equal(want.WakeAt) || wake.Reason != want.Reason {
			t.Fatalf("fact %d = %+v, want %+v", i, fact, want)
		}
	}
}

// A check_wake row of a check with a look to start starts it through the workflow: the look the
// row names is the check's, with no look open, so the next look opens with a start row of its
// own, woken by the row's reason. A row of a check a later merge replaced starts nothing.
func TestACheckWakeRowStartsTheNextLookAndAReplacedChecksStartsNone(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, "a1b2c3d4")
	check.LookOpen, check.Role = false, claim.RoleTester
	putProductionCheck(t, pool, records, check)
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour, ProductionCheckRole: claim.RoleTester}, quietLogger())
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{engine}, log: quietLogger(), now: time.Now}

	orphan := mustOutboxRow(t, root.Key, record.CheckWakeDue{Look: 1, OpenedAt: check.OpenedAt.Add(-24 * time.Hour), Reason: "an earlier check's wake"}, time.Now())
	orphan.ID = 73
	if err := runner.execute(ctx, orphan); err != nil {
		t.Fatalf("the replaced check's wake = %v, want it finished without acting", err)
	}
	if got := readProductionCheck(t, pool, records, root.Key); got.Looks != 1 || got.LookOpen {
		t.Fatalf("the check after a replaced check's wake = look %d (open %t), want look 1 still ended", got.Looks, got.LookOpen)
	}

	row := mustOutboxRow(t, root.Key, record.CheckWakeDue{Look: 1, OpenedAt: check.OpenedAt, Reason: "the deploy has not carried the merge"}, time.Now())
	row.ID = 74
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("the check's own wake = %v", err)
	}
	got := readProductionCheck(t, pool, records, root.Key)
	if got.Looks != 2 || !got.LookOpen || got.WakeAt != nil || got.LastWakeDetail != "the deploy has not carried the merge" {
		t.Fatalf("the check after its wake = %+v, want look 2 open, woken by the row's reason, with no wake standing", got)
	}
	var starts int
	if err := pool.QueryRow(ctx, "select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'", root.Key).Scan(&starts); err != nil {
		t.Fatalf("count the start rows of %s: %v", root.Key, err)
	}
	if starts != 1 {
		t.Fatalf("start rows = %d, want the woken look's one", starts)
	}
}

// The boot fact carries the boot's record id and nothing of the binary's build: no build is
// compared against an open production check, so the boot reads neither its own revision nor
// Dispatch's.
func TestTheBootFactCarriesTheBootAlone(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	facts := &outboxFactHandler{}
	w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", handlers: []intake.Handler{facts}, log: quietLogger()}
	if err := w.booted(context.Background(), 7); err != nil {
		t.Fatalf("booted = %v", err)
	}
	if got := facts.facts(); len(got) != 1 || got[0] != (intake.DaemonBooted{Boot: 7}) {
		t.Fatalf("the boot applied %+v, want the boot alone", got)
	}
}

// lookRetirement is the rig of a `retire_look` row: checkRoot's lingering root, its production
// check worked by the tester (openCheck at look 1, open, holding checkLookID), and the outbox over
// a supervisor whose look and phase fences are the daemon's own (workflowRuntime), as daemon.go
// wires them. The tester's claim is made by each row in the state it needs.
type lookRetirement struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	records record.Store
	root    record.Issue
	check   record.ProductionCheck
	sup     *supervisor
	rt      *fake.Runtime
	runner  *outbox
	token   claim.Token
	logged  *testlog.Buffer
}

func newLookRetirement(t *testing.T) *lookRetirement {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, "a1b2c3d4")
	check.Role = claim.RoleTester
	putProductionCheck(t, pool, records, check)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	answers := &workflowRuntime{pool: pool, records: records}
	sup.deps.LookHolds, sup.deps.PhaseHolds = answers.lookHolds, answers.phaseHolds
	logged := &testlog.Buffer{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion",
		stateDir: t.TempDir(), log: slog.New(slog.NewTextHandler(logged, nil)), now: time.Now}
	return &lookRetirement{t: t, ctx: context.Background(), pool: pool, records: records, root: root, check: check, sup: sup, rt: rt,
		runner: runner, token: mustClaimToken(t, root.Key, claim.RoleTester), logged: logged}
}

// look is look 1 of the rig's check.
func (r *lookRetirement) look() supervise.CheckLook {
	return supervise.CheckLook{Look: 1, OpenedAt: r.check.OpenedAt}
}

// lookTask is look 1's task as its start row delivers it: no phase, the issue's run, the look and
// the look's id, unconfirmed.
func (r *lookRetirement) lookTask() *supervise.Delivery {
	return &supervise.Delivery{ID: "outbox:57", Task: "Production check of " + r.root.Key + ": root.\nThis look has the look id `" + checkLookID + "`.",
		Generation: 1, CheckLook: r.look(), LookID: checkLookID, QueuedAt: time.Now()}
}

// claimIn makes the tester's claim in state, holding pending with budgets spent, as a claim a
// daemon restored would stand: a session recorded, a process for the states that have one, and
// who suspended it or why it failed.
func (r *lookRetirement) claimIn(state supervise.ClaimState, by supervise.SuspendSource, pending *supervise.Delivery, budgets supervise.Budgets) *supervise.Machine {
	r.t.Helper()
	c := supervise.Claim{Token: r.token, Project: "legion", Tree: r.root.Tree, Issue: r.root.Key, Role: claim.RoleTester, Generation: 1,
		State: state, Session: "ses_tester", SessionFile: "/sessions/tester.jsonl", Budgets: budgets, Pending: pending}
	switch state {
	case supervise.StateSuspended:
		c.SuspendedBy = by
	case supervise.StateFailed:
		c.Failure = "the launch budget ran out"
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: r.token, Incarnation: "4242:77", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	}
	machine, _, err := r.sup.Create(r.ctx, c, "")
	if err != nil {
		r.t.Fatalf("create the tester's claim %s: %v", state, err)
	}
	return machine
}

// row is the `retire_look` row of look 1, marked reported or not.
func (r *lookRetirement) row(reported bool) record.OutboxRow {
	r.t.Helper()
	return mustOutboxRow(r.t, r.root.Key, record.SuperviseRequest{Op: "retire_look", Tree: r.root.Tree, Role: claim.RoleTester,
		CheckLook: 1, CheckOpenedAt: r.check.OpenedAt, Reported: reported}, time.Now())
}

// retire runs the row of look 1, marked reported or not, as the outbox executes it.
func (r *lookRetirement) retire(reported bool) {
	r.t.Helper()
	if err := r.runner.execute(r.ctx, r.row(reported)); err != nil {
		r.t.Fatalf("run the retire_look row (reported %t) = %v, want it run", reported, err)
	}
}

// putCheck writes the rig's check as change leaves it.
func (r *lookRetirement) putCheck(change func(*record.ProductionCheck)) {
	r.t.Helper()
	change(&r.check)
	putProductionCheck(r.t, r.pool, r.records, r.check)
}

// stored is the tester's claim as the claim store last had it written.
func (r *lookRetirement) stored() supervise.Claim {
	claims := r.sup.deps.Store.(*outboxClaimStore)
	claims.mu.Lock()
	defer claims.mu.Unlock()
	return claims.claims[r.token]
}

// noSecretLogged fails the row where a line of the outbox's log carries the look id or the task.
func (r *lookRetirement) noSecretLogged() {
	r.t.Helper()
	log := r.logged.String()
	for _, secret := range []string{checkLookID, "Production check of"} {
		if strings.Contains(log, secret) {
			r.t.Fatalf("the log carries %q, the look id or the look's task: %q", secret, log)
		}
	}
}

// A `retire_look` row (workflow's retireLook) retires the task the claim of the check's role
// holds only when it is that look's task and the look fence the daemon wires (record.LookHolds)
// rejects it: the same task stays while its look holds, and the testing task of a new run and a
// task a turn has taken stay too. A send of the task in flight keeps the row, which retires the
// task once the send has settled. A reported row counts the look served and clears the prompt
// budgets whatever the claim holds, and ends in the same state whichever of it and the close's
// row runs first. The row runs on a claim suspended, failed or idle, and a role this daemon holds
// no claim of finishes it. No line the row logs carries the look id or the task.
func TestARetireLookRowRetiresOnlyThatLooksTask(t *testing.T) {
	t.Parallel()
	spent := supervise.Budgets{PromptFailures: 2, PromptRetires: 1}
	settled := func(t *testing.T, r *lookRetirement, c supervise.Claim) {
		t.Helper()
		if c.Budgets.PromptFailures != 0 || c.Budgets.PromptRetires != 0 || c.ServingCheckLook != r.look() {
			t.Fatalf("claim = budgets %+v, serving look %+v; want no prompt failures or retirements and look %+v served", c.Budgets, c.ServingCheckLook, r.look())
		}
	}

	t.Run("that look's unconfirmed task is retired once the fence rejects it", func(t *testing.T) {
		r := newLookRetirement(t)
		machine := r.claimIn(supervise.StateReady, "", r.lookTask(), supervise.Budgets{})
		r.putCheck(func(c *record.ProductionCheck) { c.LookOpen = false })
		r.retire(false)
		if got := machine.Claim(); got.Pending != nil || got.State != supervise.StateReady {
			t.Fatalf("tester = %s holding %+v, want still ready and holding nothing: the look ended", got.State, got.Pending)
		}
		if stored := r.stored(); stored.Pending != nil {
			t.Fatalf("stored tester holds %+v, want nothing", stored.Pending)
		}
		if !strings.Contains(r.logged.String(), "outbox ran the retirement of an ended production check look's task") {
			t.Fatalf("the log = %q, want the retirement's line", r.logged.String())
		}
		r.noSecretLogged()
	})

	t.Run("the same task is kept while the fence still holds it", func(t *testing.T) {
		r := newLookRetirement(t)
		machine := r.claimIn(supervise.StateReady, "", r.lookTask(), supervise.Budgets{})
		r.retire(false)
		if got := machine.Claim().Pending; got == nil || got.ID != "outbox:57" || got.LookID != checkLookID {
			t.Fatalf("tester holds %+v, want look 1's task kept: its look is still open", got)
		}
		r.noSecretLogged()
	})

	t.Run("the testing task of a new run is left in place", func(t *testing.T) {
		r := newLookRetirement(t)
		running := r.root
		running.Phase, running.Generation, running.Status, running.LingerUntil = phase.Testing, 2, "testing", nil
		putOutboxIssue(t, r.pool, r.records, running)
		testing := &supervise.Delivery{ID: "outbox:60", Task: "Test " + r.root.Key + ".", Phase: phase.Testing, Generation: 2, Entry: 1, QueuedAt: time.Now()}
		machine := r.claimIn(supervise.StateIdle, "", testing, spent)
		r.retire(true)
		got := machine.Claim()
		if got.Pending == nil || got.Pending.ID != testing.ID {
			t.Fatalf("tester holds %+v, want the new run's testing task left in place", got.Pending)
		}
		settled(t, r, got)
		settled(t, r, r.stored())
	})

	t.Run("a confirmed task is left in place", func(t *testing.T) {
		r := newLookRetirement(t)
		confirmed := r.lookTask()
		confirmed.DeliveredAt, confirmed.ConfirmedAt = time.Now(), time.Now()
		machine := r.claimIn(supervise.StateWorking, "", confirmed, supervise.Budgets{})
		r.putCheck(func(c *record.ProductionCheck) { c.LookOpen = false })
		r.retire(false)
		if got := machine.Claim(); got.Pending == nil || got.Pending.ID != confirmed.ID || got.State != supervise.StateWorking {
			t.Fatalf("tester = %s holding %+v, want still working on look 1's confirmed task: its turn's end retires it", got.State, got.Pending)
		}
	})

	t.Run("a send in flight waits", func(t *testing.T) {
		r := newLookRetirement(t)
		machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: r.token, Project: "legion", Tree: r.root.Tree, Issue: r.root.Key,
			Role: claim.RoleTester, State: supervise.StateQueued}, "")
		if err != nil {
			t.Fatalf("create the tester's claim: %v", err)
		}
		gated := newGatedPrompt()
		r.sup.deps.Conns.(*fake.Conns).Register(r.token, gated)
		if err := machine.Handle(r.ctx, supervise.RequestSpawn{Claim: r.token}); err != nil {
			t.Fatalf("spawn the tester: %v", err)
		}
		readyClaimAs(t, r.sup, r.token, claim.RoleTester, "ses-tester")
		task := r.lookTask()
		if err := machine.Handle(r.ctx, supervise.RequestDeliver{Claim: r.token, ID: task.ID, Task: task.Task, Generation: task.Generation,
			CheckLook: task.CheckLook, LookID: task.LookID}); err != nil {
			t.Fatalf("deliver look 1's task: %v", err)
		}
		if sent := testwait.Receive(t, "the send of look 1's task", gated.entered); sent != task.ID {
			t.Fatalf("the send in flight is of %s, want %s", sent, task.ID)
		}
		r.putCheck(func(c *record.ProductionCheck) { c.LookOpen = false })

		enqueueOutbox(t, r.pool, r.records, r.row(false))
		if err := r.runner.RunOnce(r.ctx); err != nil {
			t.Fatalf("run once: %v", err)
		}
		if left := outboxRows(t, r.pool); left != 1 {
			t.Fatalf("outbox rows while the send is in flight = %d, want the retire_look row kept", left)
		}
		if got := machine.Claim().Pending; got == nil || got.ID != task.ID {
			t.Fatalf("tester holds %+v while its send is in flight, want look 1's task", got)
		}
		if err := r.runner.execute(r.ctx, r.row(false)); !errors.Is(err, supervise.ErrTaskSending) {
			t.Fatalf("the row run while the send is in flight = %v, want %v", err, supervise.ErrTaskSending)
		}

		gated.release <- struct{}{}
		machine.Wait()
		r.retire(false)
		if got := machine.Claim().Pending; got != nil {
			t.Fatalf("tester holds %+v once the send settled, want nothing: the row run again retires look 1's task", got)
		}
	})

	for _, order := range []struct {
		name string
		rows []bool
	}{
		{name: "a reported row, then the close's", rows: []bool{true, false}},
		{name: "the close's row, then a reported one", rows: []bool{false, true}},
	} {
		t.Run("a reported row leaves the budgets at 0 and serving_check_look at the look: "+order.name, func(t *testing.T) {
			r := newLookRetirement(t)
			machine := r.claimIn(supervise.StateIdle, "", r.lookTask(), spent)
			closed := time.Now()
			r.putCheck(func(c *record.ProductionCheck) {
				c.LookOpen, c.ClosedAt, c.Outcome = false, &closed, record.CheckRecorded
			})
			for _, reported := range order.rows {
				r.retire(reported)
			}
			got := machine.Claim()
			if got.Pending != nil {
				t.Fatalf("tester holds %+v, want nothing: the look's task was reported", got.Pending)
			}
			settled(t, r, got)
			stored := r.stored()
			if stored.Pending != nil {
				t.Fatalf("stored tester holds %+v, want nothing", stored.Pending)
			}
			settled(t, r, stored)
		})
	}

	for _, state := range []struct {
		state supervise.ClaimState
		by    supervise.SuspendSource
	}{
		{state: supervise.StateSuspended, by: supervise.SuspendedByOperator},
		{state: supervise.StateFailed},
		{state: supervise.StateIdle},
	} {
		t.Run("a claim "+string(state.state), func(t *testing.T) {
			r := newLookRetirement(t)
			machine := r.claimIn(state.state, state.by, r.lookTask(), supervise.Budgets{})
			r.putCheck(func(c *record.ProductionCheck) { c.LookOpen = false })
			r.retire(false)
			got := machine.Claim()
			if got.Pending != nil || got.State != state.state || got.SuspendedBy != state.by {
				t.Fatalf("tester = %s by %q holding %+v, want still %s by %q holding nothing", got.State, got.SuspendedBy, got.Pending, state.state, state.by)
			}
			if spawns, resumes := len(r.rt.CallsOf("Spawn")), len(r.rt.CallsOf("Resume")); spawns != 0 || resumes != 0 {
				t.Fatalf("the row launched the tester %d time(s) and resumed it %d time(s), want neither: it starts nothing", spawns, resumes)
			}
		})
	}

	t.Run("no claim of the role on this daemon finishes the row", func(t *testing.T) {
		r := newLookRetirement(t)
		enqueueOutbox(t, r.pool, r.records, r.row(true))
		if err := r.runner.RunOnce(r.ctx); err != nil {
			t.Fatalf("run once: %v", err)
		}
		if left := outboxRows(t, r.pool); left != 0 {
			t.Fatalf("outbox rows after the retirement of a claim this daemon does not hold = %d, want the row finished", left)
		}
		if !strings.Contains(r.logged.String(), "this daemon supervises no claim of the check's role") {
			t.Fatalf("the log = %q, want the row finished because no claim is held", r.logged.String())
		}
		r.noSecretLogged()
	})
}
