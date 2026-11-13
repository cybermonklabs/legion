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
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
// The sweep then runs again, and the row it wrote the first time is the row the issue still
// holds: the start carries that entry, so the role is owed nothing (workflow.OwedEntry), the
// gate reads that and the later sweeps write no row and apply no fact of their own.
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
	stop()
	<-done
	// The ticker is stopped and the sweeps that follow are driven by hand, since the gate answers
	// that nothing is owed once the tick above sent the work: what a later sweep does is read
	// from these three calls rather than from a period that now applies nothing.
	applied := swept()
	for i := range 3 {
		w.applyEntrySweep(ctx, time.Now().Add(time.Duration(i+1)*time.Second))
	}

	want := sweptStart{id: first.id, role: claim.RoleTester, task: task, entry: entry}
	if first != want {
		t.Fatalf("the start the sweep wrote for %s = %+v, want %+v: the sweep sends the work the role's phases row names, at that entry",
			member.Key, first, want)
	}
	if starts := sweptStarts(t, pool, member.Key); len(starts) != 1 || starts[0] != first {
		t.Fatalf("the start rows of %s after three more sweeps = %+v, want the one row %+v the first sweep wrote: a later sweep reads the entry that row carries and writes nothing",
			member.Key, starts, first)
	}
	if n := swept(); n != applied {
		t.Fatalf("three sweeps after the work was sent applied %d facts, want none beyond the %d the tick applied: the role carries the entry's work, so nothing is owed and no fact is taken",
			n-applied, applied)
	}
}

// The sweep is what sends a role the work its entry names when no fact of the project arrives,
// so its period is the longest such a role waits, and a daemon with no override must sweep at
// least once a minute. Every other test of the sweep shortens the period, so this one reads the
// choice tickEntrySweep makes (entrySweepPeriod) with no override, as the shipped binary runs
// it, rather than the constant behind it; and with an override, which is how every other test
// and the live proof reach the sweep at all.
func TestTheEntrySweepConvergesAtLeastEveryMinuteWithNoOverride(t *testing.T) {
	if got := entrySweepPeriod(0); got <= 0 || got > time.Minute {
		t.Fatalf("the entry sweep's period with no override = %s, want more than zero and at most %s: a role behind its entry would wait longer than a minute for its work",
			got, time.Minute)
	}
	const override = 10 * time.Millisecond
	if got := entrySweepPeriod(override); got != override {
		t.Fatalf("the entry sweep's period with an override of %s = %s, want the override as given", override, got)
	}
}

// The sweep asks one read-only question before it applies anything, and it is the pass's own
// selection: which roles of this project are owed the work of the entry their phases rows name
// (admit's OwesEntryWork). A project with nothing owed takes neither the fact's lock nor a row
// of processed_events, which nothing prunes — one row a minute per project, for the life of a
// daemon, where no role is waiting for work. The same sweep applies its fact as soon as a role
// is owed that work.
//
// What is driven is the one call each tick of tickEntrySweep makes (applyEntrySweep), so the
// count below is of sweeps, not of elapsed periods.
func TestTheEntrySweepAppliesNoFactWhileNoRoleIsBehindItsEntry(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	log := quietLogger()
	records := record.NewStore()
	const sweepProject = "CAPTURE"
	root := record.Issue{Key: "CAPTURE-810", Tree: "CAPTURE-810", Project: sweepProject, Title: "the root",
		Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
		Rank: "CAPTURE-810", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	parent := root.Key
	member := record.Issue{Key: "CAPTURE-811", Tree: root.Key, Parent: &parent, Project: sweepProject,
		Title: "the member", Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "CAPTURE-811", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
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
	phaseRow := func(at int64) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return records.PutPhase(ctx, tx, record.PhaseRow{Issue: member.Key, Role: claim.RoleTester, Claim: token,
				Entry: at, EntryTask: task, EntryRaisedAt: time.Now().UTC()})
		}); err != nil {
			t.Fatalf("record the tester's phases row at entry %d: %v", at, err)
		}
	}
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
		// The tester's claim holds the entry its row names: nothing of this project is behind.
		_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, given_entry)
			values ($1, $2, $3, $4, 'tester', 1, 'ses', '/sessions/ses.jsonl', 'idle', 1, 0, 0, 0, 0, $5)`,
			string(token), project, root.Key, member.Key, int64(entry))
		return err
	}); err != nil {
		t.Fatalf("seed the member level with its entry: %v", err)
	}
	phaseRow(entry)

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

	for i := range 3 {
		w.applyEntrySweep(ctx, time.Now().Add(time.Duration(i)*time.Second))
	}

	if n := swept(); n != 0 {
		t.Fatalf("three sweeps over a project with nothing behind applied %d facts, want none: each one takes the fact's lock and leaves a row nothing prunes", n)
	}
	if starts := sweptStarts(t, pool, member.Key); len(starts) != 0 {
		t.Fatalf("the start rows of %s = %+v, want none: no role is behind", member.Key, starts)
	}

	phaseRow(entry + 1)
	w.applyEntrySweep(ctx, time.Now().Add(4*time.Second))

	if n := swept(); n != 1 {
		t.Fatalf("the sweep over a role behind its entry applied %d facts, want the one that closes the difference", n)
	}
	starts := sweptStarts(t, pool, member.Key)
	if len(starts) != 1 || starts[0].entry != entry+1 || starts[0].task != task {
		t.Fatalf("the start rows after the sweep = %+v, want the one of entry %d carrying the row's own task", starts, entry+1)
	}
}

// A role whose phase its issue has left is no part of the sweep's gate, because it is no part of
// the pass the sweep's fact makes: that pass reads the role of each slotted issue's current
// phase and nothing else (phase.RoleFor, admit's owedEntryRoles), so it would never send such
// a role anything. The past role's phases row stands where the last transition that handed it
// work left it, above the entry its claim was given — here a claim retired with its delivery
// unconfirmed — and a gate reading the phases rows of the issue rather than asking the pass
// would read true for the life of the slot: a fact and a row of processed_events a minute over
// a project where no role waits for work.
//
// The role of the issue's own phase is level while that is so, and the sweep applies nothing,
// pass after pass. The moment that role goes behind, the same sweep applies its fact and sends
// its work.
func TestTheEntrySweepAppliesNoFactForARolePastThePhaseItsIssueStandsIn(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	log := quietLogger()
	records := record.NewStore()
	const sweepProject = "CAPTURE"
	root := record.Issue{Key: "CAPTURE-820", Tree: "CAPTURE-820", Project: sweepProject, Title: "the root",
		Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
		Rank: "CAPTURE-820", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	parent := root.Key
	member := record.Issue{Key: "CAPTURE-821", Tree: root.Key, Parent: &parent, Project: sweepProject,
		Title: "the member", Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "CAPTURE-821", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	project, err := claim.ProjectToken(sweepProject)
	if err != nil {
		t.Fatal(err)
	}
	tester, err := claim.NewToken(project, member.Key, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	implementer, err := claim.NewToken(project, member.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	const entry = 2
	task := "Continue the member. Issue: " + member.Key + ". Phase: testing."
	testerRow := func(at int64) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return records.PutPhase(ctx, tx, record.PhaseRow{Issue: member.Key, Role: claim.RoleTester, Claim: tester,
				Entry: at, EntryTask: task, EntryRaisedAt: time.Now().UTC()})
		}); err != nil {
			t.Fatalf("record the tester's phases row at entry %d: %v", at, err)
		}
	}
	held := func(token claim.Token, role claim.Role, state supervise.ClaimState, given int64) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
				serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, given_entry)
				values ($1, $2, $3, $4, $5, 1, 'ses', '/sessions/ses.jsonl', $6, 1, 0, 0, 0, 0, $7)`,
				string(token), project, root.Key, member.Key, string(role), string(state), given)
			return err
		}); err != nil {
			t.Fatalf("record the %s's claim: %v", role, err)
		}
	}
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
		// The implementer is the past role: the member is in testing, so no pass reads this row,
		// which names an entry above the one the implementer's claim was given.
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: member.Key, Role: claim.RoleImplementer, Claim: implementer,
			Entry: 3, EntryTask: "Implement the member. Issue: " + member.Key + ". Phase: implementing.",
			EntryRaisedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("seed the member with a past role behind its entry: %v", err)
	}
	held(implementer, claim.RoleImplementer, supervise.StateRetired, 1)
	held(tester, claim.RoleTester, supervise.StateIdle, entry)
	testerRow(entry)

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

	for i := range 3 {
		w.applyEntrySweep(ctx, time.Now().Add(time.Duration(i)*time.Second))
	}

	if n := swept(); n != 0 {
		t.Fatalf("three sweeps over a project whose one role behind its entry is a role its issue's phase has left applied %d facts, want none: no pass sends that role anything, and each fact takes the one lock and leaves a row nothing prunes", n)
	}
	if starts := sweptStarts(t, pool, member.Key); len(starts) != 0 {
		t.Fatalf("the start rows of %s = %+v, want none: the role of its phase is level and its past role is sent nothing", member.Key, starts)
	}

	testerRow(entry + 1)
	w.applyEntrySweep(ctx, time.Now().Add(4*time.Second))

	if n := swept(); n != 1 {
		t.Fatalf("the sweep over the role of the issue's own phase behind its entry applied %d facts, want the one that closes the difference", n)
	}
	starts := sweptStarts(t, pool, member.Key)
	if len(starts) != 1 || starts[0].role != claim.RoleTester || starts[0].entry != entry+1 {
		t.Fatalf("the start rows after the sweep = %+v, want the one of the tester at entry %d: the past role is sent nothing", starts, entry+1)
	}
}
