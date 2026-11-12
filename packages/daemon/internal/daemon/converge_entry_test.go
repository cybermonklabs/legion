package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// sweptStart is one supervise start row the entry sweep left: its id, which a second run writing
// the row again would change (record.Store.EnqueueStart writes a replacement as a row of its
// own), with the role it starts, the task it carries and the entry that task is of.
type sweptStart struct {
	id    int64
	role  claim.Role
	task  string
	entry int64
}

// sweptStarts is every supervise start row of issue, oldest first.
func sweptStarts(t *testing.T, pool *pgxpool.Pool, issue string) []sweptStart {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
		coalesce((payload->>'entry')::bigint, 0) from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
	if err != nil {
		t.Fatalf("list the start rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var starts []sweptStart
	for rows.Next() {
		var start sweptStart
		var role string
		if err := rows.Scan(&start.id, &role, &start.task, &start.entry); err != nil {
			t.Fatalf("read a start row of %s: %v", issue, err)
		}
		start.role = claim.Role(role)
		starts = append(starts, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the start rows of %s: %v", issue, err)
	}
	return starts
}

// The runtime's entry sweep is what converges a role behind its entry when nothing else does: on
// its own period it applies an intake.EntrySweep, and admission sends the role the work the entry
// its phases row names stands for. Nothing else happens here — no Dispatch event, no completion,
// no boot — which is the whole point: an admission pass runs on a fact of the project, and this
// project raises none.
//
// The fixture is the difference the sweep exists to close: a tree whose member stands mid-phase
// holding one of the cap's slots, its tester's phases row at the entry a transition raised with
// the work of it, and no claim of that tester at all — a role given nothing, so it is behind that
// entry.
//
// The sweep then runs again and again on its period, and the row it wrote the first time is the
// row the issue still holds: the start carries that entry, so every later run reads the entry as
// already carried and writes nothing (workflow.OwedEntry).
func TestTheEntrySweepTickConvergesARoleBehindItsEntry(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	log := quietLogger()
	records := record.NewStore()
	const sweepProject = "CAPTURE"
	root := record.Issue{Key: "CAPTURE-800", Tree: "CAPTURE-800", Project: sweepProject, Title: "the root",
		Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
		Rank: "CAPTURE-800", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	parent := root.Key
	member := record.Issue{Key: "CAPTURE-801", Tree: root.Key, Parent: &parent, Project: sweepProject,
		Title: "the member", Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "CAPTURE-801", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	project, err := claim.ProjectToken(sweepProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, member.Key, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	const entry = 2
	task := "Continue the member. Issue: " + member.Key + ". Phase: testing."
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.PutIssue(ctx, tx, member); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: member.Key, Index: 0, AdmittedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: member.Key, Role: claim.RoleTester, Claim: token,
			Entry: entry, EntryTask: task, EntryRaisedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("seed the member behind its entry: %v", err)
	}
	if starts := sweptStarts(t, pool, member.Key); len(starts) != 0 {
		t.Fatalf("the start rows of %s before the sweep = %+v, want none: the row's own premise", member.Key, starts)
	}

	engine := workflow.New(records, workflow.Config{Project: sweepProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 1, sweepProject, log)
	w := &workflowRuntime{
		pool: pool, records: tickRecords(), admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: sweepProject, bootID: "test-boot", log: log, entrySweep: 10 * time.Millisecond,
	}
	swept := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) from processed_events
			where event_id like 'entry-sweep:CAPTURE:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the sweeps applied: %v", err)
		}
		return n
	}

	sweeping, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.tickEntrySweep(sweeping); close(done) }()
	testwait.Eventually(t, "the sweep to send the entry's work", func() bool { return len(sweptStarts(t, pool, member.Key)) == 1 })
	first := sweptStarts(t, pool, member.Key)[0]
	testwait.Eventually(t, "three sweeps applied", func() bool { return swept() >= 3 })
	stop()
	<-done

	want := sweptStart{id: first.id, role: claim.RoleTester, task: task, entry: entry}
	if first != want {
		t.Fatalf("the start the sweep wrote for %s = %+v, want %+v: the sweep sends the work the role's phases row names, at that entry",
			member.Key, first, want)
	}
	if starts := sweptStarts(t, pool, member.Key); len(starts) != 1 || starts[0] != first {
		t.Fatalf("the start rows of %s after %d sweeps = %+v, want the one row %+v the first sweep wrote: a later sweep reads the entry that row carries and writes nothing",
			member.Key, swept(), starts, first)
	}
}
