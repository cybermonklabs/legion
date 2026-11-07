package daemon

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stopTickInterval is how often the daemon of these two tests applies its controller tick, which
// is one admission pass each (workflowRuntime.tickController). Both tests drive the daemon's own
// ticks rather than applying a ControllerTick of their own, as boot_slots_test.go and
// hold_place_test.go do, because what each test reads is what the outbox did with the rows a pass
// wrote: a pass of the test's own writes through a store that wakes no outbox lane, so a row it
// wrote would wait up to one outbox lease (30 seconds) before the daemon ran it, while a row the
// daemon's own pass writes wakes its lane at the commit. The tick's period is the whole wait in
// these tests, so it is short; no other timer of the daemon is armed anywhere near as soon.
const stopTickInterval = 20 * time.Millisecond

// stoppedWorkerDaemon is the configuration both tests boot: its own database, its own project,
// the shipped defaults, and a controller tick every stopTickInterval from shortly after the
// runtime starts.
func stoppedWorkerDaemon(t *testing.T) (*pgxpool.Pool, config.Config, overrides, *fake.Runtime) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.ControllerWakeInterval = stopTickInterval
	rt := fake.NewRuntime()
	// A pods runtime provisions each claim's workspace itself, so the daemon clones and fetches
	// nothing: these issues name a repository that exists in no GitHub these tests reach. The
	// relaunch of a retired claim provisions the workspace before it launches (the outbox's start
	// executor), so without this the relaunch a stop must not make would fail on that fetch rather
	// than reach the runtime, and the launch the way back reads for would never be asked for.
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.controllerFirstWake = stopTickInterval
	return pool, cfg, o, rt
}

// controllerTicks is how many controller ticks this database has recorded. Each tick's event id
// is its own — the project, the boot and the tick's time — and every one of them is applied under
// the source `controller` (workflowRuntime.applyControllerTick), so counting the rows of that
// source counts the admission passes the daemon has actually made, across a restart as well.
func controllerTicks(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var ticks int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from processed_events where source = 'controller'`).Scan(&ticks); err != nil {
		t.Fatalf("count the controller ticks the daemon applied: %v", err)
	}
	return ticks
}

// passed waits for the daemon to apply three more controller ticks than it had, which is three
// more admission passes over the whole waiting line.
func passed(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	ticks := controllerTicks(t, pool)
	testwait.Eventually(t, "three admission passes "+what, func() bool {
		return controllerTicks(t, pool) >= ticks+3
	})
}

// startRowsOf is every start the outbox still owes issue, spelled for a failure message: the role,
// the phase, and whether the row waits for the issue's admission slot. The repair of a slot writes
// two such rows for a worker it starts again (workflow.StartOrOwe), and the repair of a start that
// can no longer act deletes the one that waits.
func startRowsOf(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select coalesce(payload->>'role', ''), coalesce(payload->>'phase', ''),
		coalesce(payload->>'needsSlot', 'false') from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' order by id`, issue)
	if err != nil {
		t.Fatalf("read the starts %s still owes its workers: %v", issue, err)
	}
	defer rows.Close()
	var owed []string
	for rows.Next() {
		var role, stands, needsSlot string
		if err := rows.Scan(&role, &stands, &needsSlot); err != nil {
			t.Fatalf("read the starts %s still owes its workers: %v", issue, err)
		}
		owed = append(owed, role+" "+stands+" needsSlot="+needsSlot)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the starts %s still owes its workers: %v", issue, err)
	}
	return owed
}

// launchesOf is every launch of token the runtime was asked for: its spawns and its resumes. Those
// two are the whole of it — the fake records no method named Launch — so a worker brought back
// after it was stopped is one more of these calls and nothing else.
func launchesOf(rt *fake.Runtime, token claim.Token) []fake.Call {
	launches := resumesOf(rt, token)
	for _, call := range rt.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			launches = append(launches, call)
		}
	}
	return launches
}

// nothingStartedTheWorker is the two reads that together say the stop held over the passes just
// made: the issue owes its worker no start row, and the runtime was asked for no launch of that
// worker since the stop.
//
// The rows are read first and the launches second, and that order is the whole of why the pair is
// an answer rather than a race. A row the repair wrote is deleted only after the launch it carries
// has been made (the outbox's execute, then finish), so a read that finds no row has either found
// a pass that wrote none or a row whose launch was already recorded — and the launches read after
// it then names that launch.
func nothingStartedTheWorker(t *testing.T, pool *pgxpool.Pool, rt *fake.Runtime, issue string, token claim.Token, before int, when string) {
	t.Helper()
	if owed := startRowsOf(t, pool, issue); len(owed) != 0 {
		t.Fatalf("%s %s owes the start rows %v, want none: the operator stopped that worker, so no admission pass writes its phase a start again",
			when, issue, owed)
	}
	if launches := launchesOf(rt, token); len(launches) != before {
		t.Fatalf("%s the runtime was asked for %d launches of %s, want the %d it had been asked for before the stop: a stopped worker is not launched again",
			when, len(launches), token, before)
	}
}

// outOfTheLine reads that issue waits for an admission slot in no part of the line the state
// serves, and holds none either: owing a worker a start is what puts a mid-phase issue in the
// line (record.Waiting), so an issue whose owed start the repair ended is offered nothing, free
// slot or no free slot.
func outOfTheLine(t *testing.T, d *daemon, issue, when string) {
	t.Helper()
	state := d.state()
	if slices.Contains(state.Admission.Waiting, issue) {
		t.Fatalf("%s the waiting line is %v, want %s in no part of it: the repair ended the start it was owed, which is what held its place",
			when, state.Admission.Waiting, issue)
	}
	if slices.Contains(state.Admission.Active, issue) {
		t.Fatalf("%s the admission slots are held by %v, want %s to hold none: nothing offers a slot to an issue that owes its worker no start",
			when, state.Admission.Active, issue)
	}
}

// An operator's `legion claims stop` of a phase worker holds, over the whole daemon and over the
// route an operator uses: POST /legion/v1/operator/claims/{token}/stop. The claim retires, and
// from then on nothing of the daemon's starts that worker again — not the admission pass the
// controller tick makes, not the next one, and not the pass a restart makes on its boot.
//
// The repair this locks is the one that used to undo the stop (admit's resumeSlottedWorkers). A
// slot whose worker holds no task is a slot with nobody in it, so the repair starts that issue's
// phase again; a stopped worker's slot reads exactly so, and the start it wrote reached the
// outbox, which met the retired claim, provisioned its workspace and relaunched it. The operator
// had their worker back about a second after they stopped it.
//
// The fixture is one the repair acts on but for the stop. The member holds a slot, stands in a
// worker phase of a tree that runs, and its implementer works the task of that phase confirmed —
// which is why no pass before the stop writes it anything (supervise.Delivery.StillWorked is true
// for a confirmed task only while the claim works it). The stop is the one thing that changes, and
// after it the same task is a task the claim no longer works.
//
// What the stop costs is read here too: the issue keeps its phase and keeps its admission slot,
// with nobody in it, until a start of the workflow's own — the held phase's retry, the tree's
// re-admission — or a person moves it. TestAStopEndsTheOwedStartOfAWaitingIssue drives that retry.
func TestAnOperatorsStopOfASlottedWorkerHolds(t *testing.T) {
	pool, cfg, o, rt := stoppedWorkerDaemon(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	implementer, err := claim.NewToken(project, root, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedWorkflowTree(t, cfg.PostgresDSN, project, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: cfg.Project, Tree: root, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
		DispatchStatus: "in_progress", HandedOver: true, Rank: "U"},
		supervise.Claim{Token: implementer, Project: project, Tree: root, Issue: root, Role: claim.RoleImplementer, Generation: 1,
			State: supervise.StateWorking, Session: "ses_implementer", SessionFile: filepath.Join(cfg.StateDir, "implementer.jsonl"),
			PluginContract: api.DaemonAPIVersion, Locator: workingPane(implementer),
			Pending: &supervise.Delivery{ID: "outbox:11", Task: "Carry on implementing.", Generation: 1, Phase: phase.Implementing,
				QueuedAt: now, DeliveredAt: now, ConfirmedAt: now}})
	writePrompt(t, cfg, implementer)
	d := startDaemon(t, cfg, o)

	// The passes before the stop write the working implementer nothing, so what the passes after
	// it do is the claim's state and nothing else about the fixture.
	passed(t, pool, "before the stop")
	nothingStartedTheWorker(t, pool, rt, root, implementer, len(launchesOf(rt, implementer)), "before the stop")

	launches := len(launchesOf(rt, implementer))
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(implementer)+"/stop", nil, true); status != http.StatusOK {
		t.Fatalf("the operator's stop of %s = %d; body %s; want 200, the stop made", implementer, status, body)
	}
	if state := d.claim(implementer).State; state != string(supervise.StateRetired) {
		t.Fatalf("%s reads %s straight after the stop, want %s: the stop route ends the claim", implementer, state, supervise.StateRetired)
	}

	passed(t, pool, "after the stop")
	nothingStartedTheWorker(t, pool, rt, root, implementer, launches, "three passes after the stop")

	// The restart is the other pass the stop has to hold against: a boot makes an admission pass
	// of its own, over records it reads fresh, and resumes by rule every claim that owns its
	// issue's phase (TestABootResumesASuspendedClaimThatOwnsItsIssuesPhase). A retired claim is
	// not one of them.
	d.stop()
	rebindHeldPorts(t, &cfg)
	d = startDaemon(t, cfg, o)
	passed(t, pool, "after the restart")
	nothingStartedTheWorker(t, pool, rt, root, implementer, launches, "three passes after the restart")

	if state := d.claim(implementer).State; state != string(supervise.StateRetired) {
		t.Fatalf("the daemon serves %s as %s, want %s: nothing of a restart brings a stopped worker back", implementer, state, supervise.StateRetired)
	}
	if state := storedClaim(t, cfg, implementer).State; state != supervise.StateRetired {
		t.Fatalf("the store holds %s as %s, want %s: the stop is what the next boot reads", implementer, state, supervise.StateRetired)
	}
	state := d.state()
	if stands := state.Issues[root].Phase; stands != phase.Implementing {
		t.Fatalf("%s stands at %s, want %s: a stop ends the worker and moves no issue", root, stands, phase.Implementing)
	}
	if !slices.Contains(state.Admission.Active, root) {
		t.Fatalf("the admission slots are held by %v, want %s among them: the stop leaves the slot held with nobody in it until a start of the workflow's own",
			state.Admission.Active, root)
	}
}

// workingPane is the process a claim at work records, as a launch under the tmux runtime leaves
// it. A claim restored with no process of its own is put back to launch_uncertain, whatever state
// it was stored in (supervisor.restore), and a worker the operator is about to stop is one whose
// process is up.
func workingPane(token claim.Token) *runtime.Locator {
	return &runtime.Locator{Runtime: "tmux", Claim: token, Incarnation: "pane-" + string(token),
		Tmux: &runtime.TmuxLocator{Window: "legion", Pane: "%1"}}
}

// seedWaitingMember records member as a tree member holding no admission slot, with the claim of
// its role and the start its transition owes that role. The claim is ready: the relaunch half of
// the start has run and brought the worker's session back, and the half that carries the task
// waits for the issue's slot (record.SuperviseRequest.NeedsSlot), which is where the cap is
// applied (the outbox's own ordering rules).
func seedWaitingMember(t *testing.T, dsn string, member record.Issue, worker supervise.Claim, owed record.SuperviseRequest) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, member); err != nil {
			return err
		}
		row, err := record.NewOutboxRow(member.Key, owed, time.Now().UTC())
		if err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, row)
	}); err != nil {
		t.Fatalf("record %s and the start it owes its %s: %v", member.Key, owed.Role, err)
	}
	if _, err := st.AdmitClaim(ctx, worker); err != nil {
		t.Fatalf("put %s: %v", worker.Token, err)
	}
}

// A stop ends the start its issue was still owed. An issue waiting for an admission slot holds the
// other half of its transition in the outbox — the row that carries the worker's task — and that
// row would start the worker the operator stopped the moment a slot came. The repair of a start
// that can no longer act ends it instead (admit's finishFencedStarts), so the stop holds for a
// waiting issue as it holds for a slotted one.
//
// The fixture is the waiting half of that pair. The member stands in a worker phase with no slot
// of its own, the relaunch half has already run — its tester is ready, with no task — and the task
// half waits. Two roots hold the cap's two slots, so the member is in the line and nothing is
// granted it. The stop is made over the operator's own route.
//
// What the passes after the stop do is read twice, because the cap would explain the first read
// on its own. First with both slots still held: the issue owes its tester nothing, the runtime is
// asked for no launch of it, and it stands in no part of the waiting line, since what put it
// there was the row the repair has now ended (record.Waiting's first part is the issues that owe
// their worker a start). Then with a slot freed, which is the read that says the stop and not the
// cap is what holds: a slot stands free pass after pass and the stopped worker is still not
// started, and the issue takes none of it — an issue that owes nothing is offered nothing.
//
// The second half of the test is the way back the repair leaves, which is the start the rule's own
// comment names: the held phase's retry. The worker being gone holds the issue out of its phase,
// and the architect's retry restores the phase and writes the pair again. The issue is then in the
// line, takes the free slot, and its tester is launched — so what the stop ended is one start, not
// the issue's ability to run again.
func TestAStopEndsTheOwedStartOfAWaitingIssue(t *testing.T) {
	pool, cfg, o, rt := stoppedWorkerDaemon(t)
	cfg.AdmissionCap = 2
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root, member, other := cfg.Project+"-1", cfg.Project+"-2", cfg.Project+"-3"
	tester, err := claim.NewToken(project, member, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	// The member's own tree, whose root is at work and holds one of the two slots, and a tree of
	// another root holding the other: with the cap full, nothing is granted the member until the
	// test frees a slot.
	for _, at := range []string{root, other} {
		seedWorkflowTree(t, cfg.PostgresDSN, project, record.Issue{Key: at, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: cfg.Project, Tree: at, Title: "root " + at, Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", HandedOver: true, Rank: "A"})
	}
	parent := root
	seedWaitingMember(t, cfg.PostgresDSN, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: cfg.Project, Tree: root, Parent: &parent, Title: "member", Phase: phase.Testing, Generation: 1, Status: "testing",
		DispatchStatus: "testing", Rank: "B"},
		supervise.Claim{Token: tester, Project: project, Tree: root, Issue: member, Role: claim.RoleTester, Generation: 1,
			State: supervise.StateReady, Session: "ses_tester", SessionFile: filepath.Join(cfg.StateDir, "tester.jsonl"),
			PluginContract: api.DaemonAPIVersion, Locator: workingPane(tester)},
		record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing,
			Task: "Test the change.", NeedsSlot: true})
	writePrompt(t, cfg, tester)
	d := startDaemon(t, cfg, o)

	passed(t, pool, "before the stop")
	if line := d.state().Admission.Waiting; !slices.Contains(line, member) {
		t.Fatalf("the waiting line is %v, want %s in it before the stop: the member owes its tester a start and holds no slot, which is what puts an issue in the line's first part",
			line, member)
	}
	launches := len(launchesOf(rt, tester))
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(tester)+"/stop", nil, true); status != http.StatusOK {
		t.Fatalf("the operator's stop of %s = %d; body %s; want 200, the stop made", tester, status, body)
	}

	// The first read, with both slots still held. The repair ends the owed row on the first pass
	// that meets the retired claim, whether a slot is free or not, so the issue is out of the line
	// from here on.
	passed(t, pool, "after the stop")
	nothingStartedTheWorker(t, pool, rt, member, tester, launches, "three passes after the stop")
	outOfTheLine(t, d, member, "after the stop")

	// The second read, with a slot free: the other root's work ends, so the next pass takes its
	// slot back. Nothing offers it to the issue whose start the repair ended, and nothing starts
	// the worker the operator stopped.
	finish(t, pool, record.NewStore(), other)
	passed(t, pool, "after the slot came free")
	nothingStartedTheWorker(t, pool, rt, member, tester, launches, "three passes after the slot came free")
	outOfTheLine(t, d, member, "once a slot stood free")

	// The way back. The stop fails no claim, so the hold is applied here as the daemon applies it
	// when the worker of a live phase is gone (workflow's claimFailed): the issue leaves its phase
	// for `held`, which is the phase the architect's retry acts on.
	engine := workflow.New(record.NewStore(), workflow.Config{Project: cfg.Project, Repositories: testRepositories}, quietLogger())
	applyWorkflowFact(t, pool, engine, "worker-died:"+member, intake.ClaimFailed{Issue: member, Role: claim.RoleTester,
		Reason: "the operator stopped it"})
	if stands := d.state().Issues[member].Phase; stands != phase.Held {
		t.Fatalf("%s stands at %s, want %s: the worker of its phase is gone, so the phase is held for its architect to decide", member, stands, phase.Held)
	}

	// The architect's retry of the held phase restores it and writes the pair again, so the issue
	// owes its tester a start once more and stands in the line for a slot.
	applyWorkflowFact(t, pool, engine, "retry:"+member, intake.RetryOrEscalate{Issue: member, Decision: intake.RetryDecision})
	if owed := startRowsOf(t, pool, member); len(owed) == 0 {
		t.Fatalf("after the retry of the held phase %s owes the start rows %v, want the relaunch and the task the retry writes: the retry is the way back the repair leaves",
			member, owed)
	}
	if line := d.state().Admission.Waiting; !slices.Contains(line, member) {
		t.Fatalf("after the retry the waiting line is %v, want %s in it: it holds no slot and owes its tester a start again", line, member)
	}
	testwait.Eventually(t, "the retried phase to launch its tester once a slot comes", func() bool {
		return len(launchesOf(rt, tester)) > launches
	})
}

// applyWorkflowFact applies one fact of the workflow's own through engine, against the daemon's
// database, as the route that carries it applies it (intake.ApplyFact): the daemon's handlers see
// what it wrote on their next pass. The event id is the test's, so it is never one of the
// daemon's.
func applyWorkflowFact(t *testing.T, pool *pgxpool.Pool, engine *workflow.Engine, eventID string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "api", eventID, fact, engine)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply %s = %+v, %v; want it accepted", eventID, result.Refusal, err)
	}
}
