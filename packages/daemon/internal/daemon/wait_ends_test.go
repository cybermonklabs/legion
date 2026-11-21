package daemon

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// waitDeclaredAt is when every wait below is declared, and waitEndedAt the moment the workflow
// reads as now, which an end is written at.
var (
	waitDeclaredAt = time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)
	waitEndedAt    = waitDeclaredAt.Add(30 * time.Minute)
)

// waitEndsRig is the daemon's workflow over a database of its own, every fact applied through the
// production handler chain (factHandlers): the mirror, the engine, admission and, last, the wait
// reconciler. The engine logs to logs.
type waitEndsRig struct {
	t         *testing.T
	pool      *store.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	handlers  []intake.Handler
	logs      *testlog.Buffer
}

func newWaitEndsRig(t *testing.T) *waitEndsRig {
	t.Helper()
	logs := &testlog.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return waitEndedAt }}, log)
	admission := admit.New(records, engine, 2, "LEGION", log)
	return &waitEndsRig{t: t, pool: isolatedOutboxPool(t), records: records, engine: engine, admission: admission,
		handlers: factHandlers(records, "LEGION", engine, admission), logs: logs}
}

// apply applies fact as source's event id through the production chain, failing the test on an
// error or a refusal.
func (r *waitEndsRig) apply(source, id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(context.Background(), r.pool, source, id, fact, r.handlers...)
	if err != nil {
		r.t.Fatalf("apply %s/%s: %v", source, id, err)
	}
	if result.Refusal != nil {
		r.t.Fatalf("apply %s/%s was refused: %+v", source, id, *result.Refusal)
	}
}

// issue records issue, failing the test on an error.
func (r *waitEndsRig) issue(issue record.Issue) {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, r.records, issue)
}

// waiting declares, through the production chain, the wait of role's worker on issue at
// generation: its claim live in session ses-<role>, and its phases row on entry 2. It answers the
// wait's id and the claim's token.
func (r *waitEndsRig) waiting(issue, tree string, role claim.Role, generation uint64) (string, claim.Token) {
	r.t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session,
		session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $3, $4, 1, $5, '', 'working', 0, 0, 0, 0)`,
		string(token), tree, issue, string(role), "ses-"+string(role)); err != nil {
		r.t.Fatalf("seed the claim of %s's %s: %v", issue, role, err)
	}
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		return r.records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: issue, Role: role, Claim: token, Entry: 2})
	}); err != nil {
		r.t.Fatalf("seed the phases row of %s's %s: %v", issue, role, err)
	}
	id := "wait-" + strings.ToLower(issue) + "-" + string(role)
	r.apply("api", "wait-declared:"+id, intake.WaitDeclared{Token: token, Issue: issue, Role: role, Session: "ses-" + string(role),
		Generation: generation, WaitID: id, WaitingOn: "the run on the branch's head", WakeAt: waitDeclaredAt.Add(2 * time.Hour),
		TakenEntry: 2, At: waitDeclaredAt})
	return id, token
}

// assertEnded asserts wait id of token, on issue for role, ended with cause, ended by endedBy at
// the workflow's now, and that the one `wait: ended` line of it names the claim, the issue, the
// role, the wait, the cause and endedBy. Once ended the wait no longer stands (WaitStands), so
// every reader of it acts on the claim as before it was declared.
func (r *waitEndsRig) assertEnded(id string, token claim.Token, issue string, role claim.Role, cause, endedBy string) {
	r.t.Helper()
	var endedAt *time.Time
	var endCause, by string
	if err := r.pool.QueryRow(context.Background(), `select ended_at, coalesce(end_cause, ''), coalesce(ended_by, '')
		from claim_waits where id = $1`, id).Scan(&endedAt, &endCause, &by); err != nil {
		r.t.Fatalf("read wait %s: %v", id, err)
	}
	if endedAt == nil || !endedAt.Equal(waitEndedAt) || endCause != cause || by != endedBy {
		r.t.Fatalf("wait %s ended at %v with cause %q by %q, want at %s with cause %q by %q", id, endedAt, endCause, by,
			waitEndedAt, cause, endedBy)
	}
	var lines []string
	for line := range strings.SplitSeq(r.logs.String(), "\n") {
		if strings.Contains(line, `msg="wait: ended"`) && strings.Contains(line+" ", " wait="+id+" ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		r.t.Fatalf("the end of wait %s was logged %d times, want once; the log is:\n%s", id, len(lines), r.logs.String())
	}
	for _, field := range [][2]string{{"claim", string(token)}, {"issue", issue}, {"role", string(role)},
		{"cause", cause}, {"ended_by", endedBy}} {
		value := field[1]
		if strings.ContainsAny(value, " =\"") {
			value = strconv.Quote(value)
		}
		if want := field[0] + "=" + value; !strings.Contains(lines[0]+" ", " "+want+" ") {
			r.t.Fatalf("the line %q does not carry %s", lines[0], want)
		}
	}
	if r.stands(token, issue, role) {
		r.t.Fatalf("wait %s still stands after its end", id)
	}
}

// stands is what WaitStands answers for the wait of token on issue as the record stands now.
func (r *waitEndsRig) stands(token claim.Token, key string, role claim.Role) bool {
	r.t.Helper()
	var stands bool
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		issue, err := r.records.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		root, err := r.records.Issue(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		rows, err := r.records.Phases(ctx, tx, key)
		if err != nil {
			return err
		}
		gates, err := r.records.GatesOf(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		claims, err := r.records.RoleClaims(ctx, tx, []claim.Token{token})
		if err != nil {
			return err
		}
		_, stands = workflow.WaitStands(*issue, *root, record.PhaseRowOf(rows, role), record.RoleRun{Claim: claims[token]},
			record.TreeGate{Steps: config.DesignGateRootIssues.Steps(), Rows: gates})
		return nil
	}); err != nil {
		r.t.Fatalf("read whether the wait of %s stands: %v", token, err)
	}
	return stands
}

// A re-admission ends the wait it stops in the fact that writes it. A child left at implementing
// by a root that closed before any hand-over of it existed still runs its phase, and its
// implementer declares a wait. A person's todo with the label then makes the child an orphan
// (admit's orphan), which admission re-admits as a root of its own carrying its run: the phase,
// the phases row and the claim stay, and the generation is new. Admission writes that inside the
// same fact as the engine before it, and the wait reconciler runs after both (factHandlers), so
// the wait ends in that fact with the cause that the issue runs a new generation, ended by the
// Dispatch event.
func TestAReadmissionsNewGenerationEndsTheWaitInItsOwnFact(t *testing.T) {
	r := newWaitEndsRig(t)
	const root, child = "LEGION-208", "LEGION-209"
	r.issue(record.Issue{Key: root, Tree: root, Project: "LEGION", Title: root, Repo: "sjawhar/legion", DefaultBranch: "main",
		GitHubRepo: "sjawhar/legion", Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A",
		HandedOver: true, LastDispatchSeq: 1})
	r.issue(record.Issue{Key: child, Tree: root, Parent: new(root), Project: "LEGION", Title: child, Repo: "sjawhar/legion",
		DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
		DispatchStatus: "in_progress", Rank: "B", LastDispatchSeq: 1})
	id, token := r.waiting(child, root, claim.RoleImplementer, 1)
	if !r.stands(token, child, claim.RoleImplementer) {
		t.Fatalf("the implementer's wait does not stand before the re-admission")
	}

	r.apply("dispatch", "child-handed-over", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo",
		Title: child, Parent: root, Rank: "B", HandedOver: true, ResolvedRepo: "sjawhar/legion"})

	readmitted := r.recorded(child)
	if readmitted.Tree != child || readmitted.Generation != 2 || readmitted.Phase != phase.Implementing {
		t.Fatalf("%s = tree %s, generation %d, phase %s; want it re-admitted as its own root at generation 2, still implementing",
			child, readmitted.Tree, readmitted.Generation, readmitted.Phase)
	}
	r.assertEnded(id, token, child, claim.RoleImplementer, workflow.WaitEndNewGeneration, "dispatch/child-handed-over")
}

// recorded is key's record, failing the test when there is none.
func (r *waitEndsRig) recorded(key string) record.Issue {
	r.t.Helper()
	var issue *record.Issue
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(context.Background(), tx, key)
		return err
	}); err != nil || issue == nil {
		r.t.Fatalf("read %s: %v (found %v)", key, err, issue != nil)
	}
	return *issue
}

// The boot's reconcile is the one admission pass that runs outside a fact: it applies Dispatch's
// listing in a transaction of its own. A move the listing shows that the daemon missed while it
// was down — a person's backlog of a child whose tester waits — is applied there as its event
// would have been (the engine's leave), and the wait it stops ends in that same transaction, ended
// by the reconcile, which has no event id of its own: the pass and the boot.
func TestTheBootReconcileEndsTheWaitsItsListingStops(t *testing.T) {
	r := newWaitEndsRig(t)
	const root, child = "LEGION-208", "LEGION-209"
	r.issue(record.Issue{Key: root, Tree: root, Project: "LEGION", Title: root, Repo: "sjawhar/legion", DefaultBranch: "main",
		GitHubRepo: "sjawhar/legion", Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "A", HandedOver: true, LastDispatchSeq: 1})
	r.issue(record.Issue{Key: child, Tree: root, Parent: new(root), Project: "LEGION", Title: child, Repo: "sjawhar/legion",
		DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Phase: phase.Testing, Generation: 1, Status: "testing",
		DispatchStatus: "testing", Rank: "B", LastDispatchSeq: 1})
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		if err := r.records.PutGate(context.Background(), tx, record.DesignGate{Issue: root, ArtifactID: "artifact-208",
			LatestVersion: 3, ApprovedVersion: new(3)}); err != nil {
			return err
		}
		if err := r.records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: waitDeclaredAt}); err != nil {
			return err
		}
		return openTreeTx(context.Background(), tx, "LEGION", root)
	}); err != nil {
		t.Fatalf("open the tree of %s: %v", root, err)
	}
	id, token := r.waiting(child, root, claim.RoleTester, 1)

	conn := testnats.Connect(t, workflowNATS(t))
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	consumers, err := intake.OpenConsumers(context.Background(), js, r.pool, intake.ConsumerSpec{
		Project: "LEGION", Repositories: []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")},
		AckWait: time.Second, NakDelay: time.Millisecond, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	parent := root
	w := &workflowRuntime{pool: r.pool, records: r.records, engine: r.engine, admission: r.admission, handlers: r.handlers,
		repositories: newRepositories("LEGION", ghrepo.Renames{}), dispatchProject: "LEGION", bootID: "wait-boot",
		log: quietLogger(), consumers: consumers, dispatch: &filteringDispatch{summaries: []dispatch.IssueSummary{
			{Key: root, ResolvedRepo: "sjawhar/legion", Title: root, Status: "in_progress", Rank: "A", HandedOver: true, LastSeq: 1},
			{Key: child, ResolvedRepo: "sjawhar/legion", Title: child, Status: "backlog", Parent: &parent, Rank: "B", HandedOver: true, LastSeq: 2},
		}}}
	if err := w.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := r.recorded(child).Phase; got != phase.Done {
		t.Fatalf("%s stands in %s after the boot read its backlog, want done", child, got)
	}
	r.assertEnded(id, token, child, claim.RoleTester, workflow.WaitEndPhaseMoved(phase.Done), "reconcile/boot:wait-boot")
}
