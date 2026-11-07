package record

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// relaunchMidRead commits, behind the first statement a read sends, the pair a relaunch of a
// retired claim commits: the claim's new state, and then, in a transaction of its own, the
// delete of the queued start row that carried it. That is the order the outbox writes them in —
// the row runs through the claim's machine, which persists the state, and the row is finished
// after that (internal/daemon's outbox) — and the two commits land between the statements of the
// read whatever order the read sends them in.
//
// The writes go through a pool of their own, so they are another session's transactions and not
// the read's, and they are not themselves traced.
type relaunchMidRead struct {
	t     *testing.T
	pool  *pgxpool.Pool
	token claim.Token
	row   int64
	sql   string
	fired bool
}

func (r *relaunchMidRead) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.sql = data.SQL
	return ctx
}

// TraceQueryEnd fires behind the first of the read's two statements, whichever of them the read
// sends first: what this test holds is that neither order of the two can report the pair the stop
// rule reads as a worker nothing is starting again.
func (r *relaunchMidRead) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if r.fired || !strings.Contains(r.sql, "from outbox where kind") && !strings.Contains(r.sql, "from claims c") {
		return
	}
	r.fired = true
	if _, err := r.pool.Exec(ctx, `update claims set state = $2 where token = $1`,
		string(r.token), string(supervise.StateLaunching)); err != nil {
		r.t.Errorf("the relaunch's write of the claim state: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `delete from outbox where id = $1`, r.row); err != nil {
		r.t.Errorf("the relaunch's delete of the start row: %v", err)
	}
}

// RoleRuns reads a role's claim and its queued supervise rows as two statements of the caller's
// READ COMMITTED transaction, so a writer commits between them, and admission's stop rule reads
// three terms of that one answer: the claim is retired, no other start of it is queued, and it
// works no task of the row it is weighing (admit's stopOutlivedStart). The pair "retired beside
// no queued start" is what that rule reads as a worker an operator stopped and nothing is
// starting again, and it ends the row that carries the worker's task.
//
// A relaunch of that very claim commits the two halves of that pair one after the other: the
// claim's new state first, the delete of its start row second. A read that took the claim before
// the first commit and the rows after the second would report exactly that pair for a worker the
// daemon is at that moment starting again, and the task row would go. Reading the rows first is
// what rules it out: a read that takes the rows before the delete sees the start, and one that
// takes them after it takes the claim later still, so it sees the state the delete stands for.
//
// This test commits both halves behind the read's own first statement, which is the moment that
// made the pair reachable, and reads that the pair is not what comes back.
func TestRoleRunsNeverReportsARetiredClaimBesideAStartItsRelaunchHasTakenAway(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Clone(t, "legion_record_test")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the cloned database: %v", err)
	}
	t.Cleanup(st.Close)

	const issue = "LEGION-208"
	token := claim.Token("legion-legion-legion-208-implementer")
	stopped := supervise.Claim{Token: token, Project: "LEGION", Tree: issue, Issue: issue, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateRetired, Session: "ses_implementer", SessionFile: "/tmp/implementer.jsonl"}
	if err := st.PutClaim(ctx, stopped); err != nil {
		t.Fatalf("record the stopped claim: %v", err)
	}
	records := NewStore()
	relaunch := SuperviseRequest{Op: "start", Tree: issue, Role: claim.RoleImplementer, Generation: 1,
		Phase: phase.Implementing, Relaunch: true}
	var row int64
	inTx(t, st, func(tx pgx.Tx) {
		queued, err := NewOutboxRow(issue, relaunch, time.Now())
		must(t, err)
		must(t, records.Enqueue(ctx, tx, queued))
		must(t, tx.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'supervise'`, issue).Scan(&row))
	})

	// The writer's own pool, and the read's, which traces its statements.
	writer, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open the writer's pool: %v", err)
	}
	t.Cleanup(writer.Close)
	relaunching := &relaunchMidRead{t: t, pool: writer, token: token, row: row}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the read's pool configuration: %v", err)
	}
	config.ConnConfig.Tracer = relaunching
	reader, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the read's pool: %v", err)
	}
	t.Cleanup(reader.Close)

	key := RoleRunKey{Token: token, Issue: issue, Role: claim.RoleImplementer, Generation: 1}
	tx, err := reader.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	runs, err := records.RoleRuns(ctx, tx, []RoleRunKey{key})
	if err != nil {
		t.Fatalf("read the run of %s: %v", token, err)
	}
	if !relaunching.fired {
		t.Fatal("the relaunch never committed between the read's statements: the test's own premise")
	}

	run := runs[token]
	if run.Claim == nil {
		t.Fatalf("the run of %s carries no claim, want the one the fixture wrote", token)
	}
	var queuedStart bool
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" {
			queuedStart = true
		}
	}
	if run.Claim.State == supervise.StateRetired && !queuedStart {
		t.Fatalf("the run of %s reads retired with no queued start, while its relaunch was committing: admission's stop rule reads that pair as a worker an operator stopped and nothing is starting again, and ends the row that carries its task. The claim and the rows must be read in an order that cannot report it (record's RoleRuns)",
			token)
	}
}
