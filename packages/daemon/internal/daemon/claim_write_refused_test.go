package daemon

// A claim write the store refused for good, as the daemon tells it (R4 of dispatch://INFRA-4099's
// spec, D10 and D11 of its plan): the machine logs it and calls its hook once for each refusal
// (supervise's refusedWrite), and the daemon applies the hook as a workflow fact keyed on the
// claim, the generation, the SQLSTATE and the constraint (workflowRuntime.claimWriteRefused), so
// the roles a run of provider errors tells are told once for each key, also across a restart.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// refusedWriteNotices is how many outbox rows of kind — the architect's notice or the controller's
// — carry a claim-write-refused notice.
func refusedWriteNotices(t *testing.T, pool *store.Pool, kind record.OutboxKind) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and payload->>'kind' = $2`,
		string(kind), string(record.ClaimWriteRefusedNotice)).Scan(&rows); err != nil {
		t.Fatalf("count the %s rows of claim-write-refused notices: %v", kind, err)
	}
	return rows
}

// refusalWorkflow is the daemon's workflow runtime as newOlderLauncherRig wires it, over pool: a
// real workflow engine its facts are applied to, which a test builds again to stand for the next
// daemon reading the same records.
func refusalWorkflow(pool *store.Pool, log *slog.Logger) *workflowRuntime {
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	return &workflowRuntime{pool: pool, records: records, log: log, handlers: []intake.Handler{engine}}
}

// toldNew hands w the refusal r while the test holds the lock every fact applies under
// (intake.SerializeFacts), and returns once the apply that call started has ended: the apply runs
// on a goroutine of its own (applySessionFact), so it waits on the held lock, the test lets the
// lock go, and then takes it itself, which it is granted only once that apply's transaction has
// ended. What the outbox holds after it is what the telling wrote.
func toldNew(t *testing.T, pool *store.Pool, w *workflowRuntime, r supervise.WriteRefusal) {
	t.Helper()
	held := holdFactLock(t, pool)
	w.claimWriteRefused(r)
	if !factLockWaited(pool) {
		t.Fatal("the workflow's apply of the refusal never waited on the fact lock the test held")
	}
	if err := held.Rollback(context.Background()); err != nil {
		t.Fatalf("let the fact lock go: %v", err)
	}
	if err := holdFactLock(t, pool).Rollback(context.Background()); err != nil {
		t.Fatalf("let the fact lock go after the apply: %v", err)
	}
}

// toldAgainWithin is how long toldAgain watches for a fact transaction of a refusal told again. An
// apply of a new key waits on the held lock within milliseconds (toldNew), so a second with no
// waiter is an apply that took no fact transaction.
const toldAgainWithin = time.Second

// toldAgain hands w the refusal r, which the records hold as applied already, while the test holds
// the lock every fact applies under: the workflow reads the fact as applied (intake.Applied) and
// opens no fact transaction, so nothing waits on the held lock. A claim whose every write is
// refused tells the same refusal at each probe interval, and an apply of each would hold the
// daemon's fact lock for nothing.
func toldAgain(t *testing.T, pool *store.Pool, w *workflowRuntime, r supervise.WriteRefusal) {
	t.Helper()
	held := holdFactLock(t, pool)
	w.claimWriteRefused(r)
	if factLockWaitedWithin(pool, toldAgainWithin) {
		t.Error("the refusal told again waited on the fact lock the test held: it opened a fact transaction for a fact already applied")
	}
	if err := held.Rollback(context.Background()); err != nil {
		t.Fatalf("let the fact lock go: %v", err)
	}
}

// A refused claim write is told once for each claim, generation, SQLSTATE and constraint
// (c-refusal-told of dispatch://INFRA-4099's verification), to the roles a run of provider errors
// tells (providerErrorRunReached): a phase worker's claim reaches the issue's architect and the
// controller, the tree root's architect's own claim the controller alone, and a child's own
// architect's claim the architect above that child and the controller. The hook is called once for
// each refusal (supervise's refusedWrite), so the same refusal made again — at the next decision
// after the window, or by the next daemon's machine over the same row — is told again with the
// same key: it adds no row, from the same runtime or from a new one reading the same records, as
// a restart's does, and it opens no fact transaction, since the workflow reads it as applied
// first. A refusal of the next generation is a new key, and is told. A refusal the
// store made in Go names no SQLSTATE, and its rule word stands where the constraint stands; so
// does a retirement the store refused over another delivery in the claim's row, whose reason says
// what the store found.
func TestARefusedClaimWriteIsToldOnce(t *testing.T) {
	t.Parallel()
	const checkConstraint = "claims_suspended_by_check"
	for _, tc := range []struct {
		name                 string
		issue                string
		role                 claim.Role
		store                string // the refused store call, "" for PutClaim
		sqlstate, constraint string
		err                  error  // the refusal, nil for the one the SQLSTATE and the constraint make
		architect            string // whose architect topic hears it; "" for none
		// says and saysNot are words the notice's reason holds, and words it does not hold, beyond
		// the words every row's reason holds.
		says, saysNot []string
	}{
		{name: "a phase worker's claim", issue: "LEGION-1", role: claim.RoleImplementer, sqlstate: "23514", constraint: checkConstraint, architect: "LEGION-1"},
		{name: "the tree root's architect's own claim", issue: "LEGION-1", role: claim.RoleArchitect, sqlstate: "23514", constraint: checkConstraint},
		{name: "a child's own architect's claim", issue: "LEGION-2", role: claim.RoleArchitect, sqlstate: "23514", constraint: checkConstraint, architect: "LEGION-1"},
		{name: "a phase worker's claim the store refused in Go", issue: "LEGION-2", role: claim.RoleTester, constraint: supervise.RuleInt4Fit, architect: "LEGION-2"},
		{name: "a phase worker's retirement the store refused over another delivery", issue: "LEGION-2", role: claim.RoleReviewer,
			store: "RetireDelivery", constraint: supervise.RuleOtherDelivery, architect: "LEGION-2",
			err:     fmt.Errorf("retire delivery delivery-1: the row holds delivery delivery-2: %w", supervise.ErrOtherDelivery),
			says:    []string{"The store call was RetireDelivery.", "the claim's row holds another delivery than the one the daemon retired"},
			saysNot: []string{"before it sent a statement"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			ctx := context.Background()
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			// A controller notice is published only to a registered controller session that holds
			// the controller's role live (the outbox's controllerHolds), as in
			// TestAProviderErrorsNoticeReachesTheIssuesArchitectAndTheController.
			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatalf("open the store: %v", err)
			}
			defer st.Close()
			token, err := claim.ProjectToken("LEGION")
			if err != nil {
				t.Fatal(err)
			}
			generation, err := st.MintController(ctx, token, []byte("capability"))
			if err != nil {
				t.Fatalf("mint the controller capability: %v", err)
			}
			if ok, err := st.RegisterController(ctx, token, generation, "ses-controller", []byte("secret"), time.Now(), 0); err != nil || !ok {
				t.Fatalf("register the controller = %t, %v", ok, err)
			}
			log := quietLogger()
			architects := 0
			if tc.architect != "" {
				architects = 1
			}
			wantRows := func(when string) {
				t.Helper()
				if got := refusedWriteNotices(t, pool, record.OutboxKindNotice); got != architects {
					t.Errorf("%s the outbox holds %d architect notices of the refusal, want %d", when, got, architects)
				}
				if got := refusedWriteNotices(t, pool, record.OutboxKindControllerNotice); got != 1 {
					t.Errorf("%s the outbox holds %d controller notices of the refusal, want 1", when, got)
				}
			}

			err = &pgconn.PgError{Code: tc.sqlstate, ConstraintName: tc.constraint, Severity: "ERROR",
				Message: `new row for relation "claims" violates check constraint "` + tc.constraint + `"`}
			switch {
			case tc.err != nil:
				err = tc.err
			case tc.sqlstate == "":
				err = &supervise.RowRefusedError{Rule: tc.constraint, Err: errors.New("launch_failures 2147483648 does not fit an integer column")}
			}
			call := tc.store
			if call == "" {
				call = "PutClaim"
			}
			refusal := supervise.WriteRefusal{
				Claim: supervise.Claim{Token: mustClaimToken(t, tc.issue, tc.role), Project: "legion", Tree: "LEGION-1", Issue: tc.issue,
					Role: tc.role, Generation: 3, State: supervise.StateSuspended},
				Store: call, Stored: supervise.StateIdle,
				SQLState: tc.sqlstate, Constraint: tc.constraint, Err: err,
			}

			first := refusalWorkflow(pool, log)
			first.claimWriteRefused(refusal)
			// The rows are one transaction, so once the controller's row is there every row is.
			testwait.Eventually(t, "the notices of the refusal to commit", func() bool {
				return refusedWriteNotices(t, pool, record.OutboxKindControllerNotice) == 1
			})
			wantRows("after the first refusal")

			toldAgain(t, pool, first, refusal)
			wantRows("after the same refusal again")

			toldAgain(t, pool, refusalWorkflow(pool, log), refusal)
			wantRows("after the same refusal told by a new runtime over the same records")

			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			if tc.issue != "LEGION-1" {
				architectClaim(t, sup, tc.issue, supervise.StateWorking)
			}
			publisher := &holderPublisher{}
			publisher.holds(legionController, "ses-controller")
			runner := &outbox{log: log, pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the notices: %v", err)
			}
			_, delivered := publisher.snapshot()
			byTopic := map[string]outboxPublish{}
			for _, published := range delivered {
				byTopic[published.topic] = published
			}
			controller := controllerRoleTopic
			topics := []string{controller}
			if tc.architect != "" {
				topics = append(topics, architectTopic(t, tc.architect))
			}
			if len(delivered) != len(topics) || len(byTopic) != len(topics) || outboxRows(t, pool) != 0 {
				t.Fatalf("delivered %+v with %d rows left, want one publish on each of %v, none on any other topic, and no row left",
					delivered, outboxRows(t, pool), topics)
			}
			told := byTopic[controller].payload
			for _, topic := range topics {
				published, ok := byTopic[topic]
				if !ok {
					t.Fatalf("delivered %+v, want a publish on %s", delivered, topic)
				}
				if message := "claim-write-refused on " + tc.issue; published.message != message || !reflect.DeepEqual(published.payload, told) {
					t.Fatalf("the publish on %s = %q %+v, want %s carrying the controller's notice %+v", topic, published.message, published.payload, message, told)
				}
			}
			notice, ok := told.(record.Notice)
			if !ok || notice.Kind != record.ClaimWriteRefusedNotice || notice.Role != tc.role {
				t.Fatalf("the controller was published %+v, want a claim-write-refused notice of the %s", told, tc.role)
			}
			says := []string{"the " + string(tc.role) + " claim of " + tc.issue + " at generation 3", call, "is idle", "was suspended",
				strconv.Quote(tc.constraint), strconv.Quote(supervise.RefusedWriteLine), strconv.Quote(supervise.RereadLine)}
			if tc.sqlstate != "" {
				says = append(says, "The SQLSTATE is "+tc.sqlstate)
			} else {
				says = append(says, "there is no SQLSTATE")
			}
			for _, words := range append(says, tc.says...) {
				if !strings.Contains(notice.Reason, words) {
					t.Errorf("the notice's reason does not say %q:\n%s", words, notice.Reason)
				}
			}
			for _, words := range tc.saysNot {
				if strings.Contains(notice.Reason, words) {
					t.Errorf("the notice's reason says %q, which is not true of this refusal:\n%s", words, notice.Reason)
				}
			}
			if addressed := strings.Contains(notice.Reason, "Architect:"); addressed != (tc.architect != "") {
				t.Errorf("the notice's reason addresses an architect: %t, want %t, as an architect is told:\n%s", addressed, tc.architect != "", notice.Reason)
			}

			// The next generation's refusal is a key of its own, which the new runtime tells.
			next := refusal
			next.Claim.Generation = 4
			toldNew(t, pool, refusalWorkflow(pool, log), next)
			wantRows("after a refusal of the next generation")
		})
	}
}

// A suspension whose claim write a real server refuses leaves the claim and its process as the
// store holds them (R5 of dispatch://INFRA-4099's spec over a real Postgres, the shape of INFRA-3577's
// N1): a CHECK constraint added to the test database's claims table refuses the state suspended
// for this one claim, and an operator's suspension of the idle claim is made through the
// supervisor. The SQLSTATE and the constraint's name come from the server, on the refusal's log
// line and in the notice the controller is published. Memory reads the claim as its row does,
// idle and recording its process, and the agent's capability stands: the broker revoked nothing,
// and memory and the row both hold the enrollment. The runtime's stop comes before the record
// (R3), so nothing here says the runtime was asked to stop nothing. Once the constraint is dropped
// and the clock has passed the window after the refusal, a second operator's suspension writes
// the row suspended, and only then is the capability revoked.
func TestARealConstraintRefusalOfASuspendLeavesTheClaimAndItsProcessAsStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newOwedWriteRig(t, unmeasuredClaimWrite, owedRole{claim.RoleImplementer, supervise.StateIdle})
	token := rig.token(claim.RoleImplementer)

	// The idle claim holds the enrollment of its process's pod, which a suspension revokes.
	seeded, found, err := rig.st.ReadClaim(ctx, token)
	if err != nil || !found {
		t.Fatalf("read the seeded claim = %v, %v", found, err)
	}
	enrollment := supervise.Enrollment{ID: "enr-refused-suspend", Incarnation: seeded.Locator.Incarnation}
	seeded.Enrollment = &enrollment
	if err := rig.st.PutClaim(ctx, seeded); err != nil {
		t.Fatalf("record the claim's enrollment: %v", err)
	}

	// The daemon's workflow over the rig's database, as newOlderLauncherRig wires it, told of each
	// refusal through the machine's hook; a broker that records its revocations; and a clock the
	// test moves past the window after the refusal.
	workflowLog := &syncBuffer{}
	w := refusalWorkflow(rig.pool, slog.New(slog.NewJSONHandler(workflowLog, nil)))
	broker := &recordingEnroller{}
	clock := &heldClock{}
	rig.extend = func(deps *supervise.Deps) {
		deps.ClaimWriteRefused, deps.Secrets, deps.Clock = w.claimWriteRefused, broker, clock
	}
	rig.sup.stop()
	rig.boot()
	machine := rig.machine(claim.RoleImplementer)
	if c := machine.Claim(); c.State != supervise.StateIdle || c.Locator == nil || c.Enrollment == nil {
		t.Fatalf("the booted claim is %s recording process %+v and enrollment %+v, want idle with both", c.State, c.Locator, c.Enrollment)
	}
	process := *machine.Claim().Locator

	const constraint = "claims_refuse_this_suspension"
	if _, err := rig.pool.Exec(ctx, fmt.Sprintf(`alter table claims add constraint %s check (token <> '%s' or state <> 'suspended')`,
		constraint, strings.ReplaceAll(string(token), "'", "''"))); err != nil {
		t.Fatalf("add the constraint that refuses the claim's suspension: %v", err)
	}

	err = rig.handle(claim.RoleImplementer, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator})
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != constraint {
		t.Fatalf("the suspension answered %v, want the server's 23514 naming %s", err, constraint)
	}
	machine.Wait()

	lines := loggedMessages(t, rig.logs.String(), supervise.RefusedWriteLine)
	if len(lines) != 1 {
		t.Fatalf("the refusal was logged %d times, want once:\n%s", len(lines), rig.logs.String())
	}
	for field, want := range map[string]any{"claim": string(token), "store": "PutClaim", "stored": "idle", "refused": "suspended",
		"generation": float64(1), "sqlstate": "23514", "constraint": constraint} {
		if got := lines[0][field]; got != want {
			t.Errorf("the refusal's line carries %s=%v, want %v: %v", field, got, want, lines[0])
		}
	}
	if read := loggedMessages(t, rig.logs.String(), supervise.RereadLine); len(read) != 1 || read[0]["state"] != "idle" {
		t.Errorf("the read after the refusal was logged %v, want one line reading the claim idle", read)
	}

	if held, stored := rig.held(claim.RoleImplementer), rig.stored(claim.RoleImplementer); held != supervise.StateIdle || stored != supervise.StateIdle {
		t.Fatalf("after the refused suspension memory reads %s and the row %s, want both idle", held, stored)
	}
	row, _, err := rig.st.ReadClaim(ctx, token)
	if err != nil {
		t.Fatalf("read the claim's row: %v", err)
	}
	held := machine.Claim()
	if !reflect.DeepEqual(held.Locator, &process) || !reflect.DeepEqual(row.Locator, &process) {
		t.Errorf("memory records the process %+v and the row %+v, want both %+v", held.Locator, row.Locator, process)
	}
	if held.Enrollment == nil || *held.Enrollment != enrollment || row.Enrollment == nil || *row.Enrollment != enrollment {
		t.Errorf("memory holds the enrollment %+v and the row %+v, want both %+v: the capability stands", held.Enrollment, row.Enrollment, enrollment)
	}
	if revoked := broker.revocations(); len(revoked) != 0 {
		t.Errorf("the broker revoked %v, want the agent's capability standing", revoked)
	}

	// The controller is told the server's code and constraint, as the issue's architect is.
	testwait.Eventually(t, "the notices of the refusal to commit, or the workflow's line that it could not write them", func() bool {
		return refusedWriteNotices(t, rig.pool, record.OutboxKindControllerNotice) == 1 || strings.Contains(workflowLog.String(), `"error":`)
	})
	if strings.Contains(workflowLog.String(), `"error":`) {
		t.Fatalf("the workflow could not tell the refusal:\n%s", workflowLog.String())
	}
	if architects := refusedWriteNotices(t, rig.pool, record.OutboxKindNotice); architects != 1 {
		t.Errorf("the outbox holds %d architect notices of the refusal, want 1", architects)
	}
	var reason string
	if err := rig.pool.QueryRow(ctx, `select payload->>'reason' from outbox where kind = $1 and payload->>'kind' = $2`,
		string(record.OutboxKindControllerNotice), string(record.ClaimWriteRefusedNotice)).Scan(&reason); err != nil {
		t.Fatalf("read the controller notice's reason: %v", err)
	}
	for _, words := range []string{"The SQLSTATE is 23514", strconv.Quote(constraint), "PutClaim", "is idle", "was suspended"} {
		if !strings.Contains(reason, words) {
			t.Errorf("the controller notice's reason does not say %q:\n%s", words, reason)
		}
	}
	// Each line the notice tells the controller to find is a line the refusal left in the log.
	for _, line := range []string{supervise.RefusedWriteLine, supervise.RereadLine} {
		if !strings.Contains(reason, strconv.Quote(line)) || len(loggedMessages(t, rig.logs.String(), line)) == 0 {
			t.Errorf("the notice names the line %q: %t, and the log holds it: %t", line, strings.Contains(reason, strconv.Quote(line)),
				len(loggedMessages(t, rig.logs.String(), line)) > 0)
		}
	}

	if _, err := rig.pool.Exec(ctx, "alter table claims drop constraint "+constraint); err != nil {
		t.Fatalf("drop the constraint: %v", err)
	}
	clock.advance(rig.sup.deps.Timeouts.Probe)
	if err := rig.handle(claim.RoleImplementer, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("the second suspension answered %v", err)
	}
	machine.Wait()
	if held, stored := rig.held(claim.RoleImplementer), rig.stored(claim.RoleImplementer); held != supervise.StateSuspended || stored != supervise.StateSuspended {
		t.Fatalf("after the second suspension memory reads %s and the row %s, want both suspended", held, stored)
	}
	testwait.Eventually(t, "the suspension written to revoke the agent's capability", func() bool {
		return reflect.DeepEqual(broker.revocations(), []string{enrollment.ID})
	})
}
