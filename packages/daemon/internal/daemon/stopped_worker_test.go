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

// stopTickInterval is how often the daemon of the stop's tests applies its controller tick, which
// is one admission pass each (workflowRuntime.tickController). They drive the daemon's own
// ticks rather than applying a ControllerTick of their own, as boot_slots_test.go and
// hold_place_test.go do, because what each test reads is what the outbox did with the rows a pass
// wrote: a pass of the test's own writes through a store that wakes no outbox lane, so a row it
// wrote would wait up to one outbox lease (30 seconds) before the daemon ran it, while a row the
// daemon's own pass writes wakes its lane at the commit. The tick's period is the whole wait in
// these tests, so it is short; no other timer of the daemon is armed anywhere near as soon.
const stopTickInterval = 20 * time.Millisecond

// stoppedWorkerDaemon is the configuration the stop's tests boot: its own database, its own
// project, the shipped defaults, and a controller tick every stopTickInterval from shortly after
// the runtime starts.
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

// theOperatorStops is the stop itself, over the route an operator uses, and the two reads that
// say the route made it: the request is answered 200, and the claim reads retired from the next
// read of the operator's own list. Every test of the stop goes through here, so none of them
// reads what a pass did to a claim the route left in some other state.
func theOperatorStops(t *testing.T, d *daemon, token claim.Token) {
	t.Helper()
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil, true); status != http.StatusOK {
		t.Fatalf("the operator's stop of %s = %d; body %s; want 200, the stop made", token, status, body)
	}
	if state := d.claim(token).State; state != string(supervise.StateRetired) {
		t.Fatalf("%s reads %s straight after the stop, want %s: the stop route ends the claim", token, state, supervise.StateRetired)
	}
}

// stopTheWorker is what a test whose issue owes its worker nothing does before its start: the
// daemon's own passes over the fixture write the worker nothing, the operator stops it over
// their route, and the passes after the stop start nothing of it. It answers how many launches
// of that claim the runtime had been asked for before any of those passes, which is the number
// every read of the launches after it is made against.
//
// A test whose issue owes its worker a start row before the stop calls theOperatorStops itself:
// the read here that the issue owes no start reads that row and fails on it.
func stopTheWorker(t *testing.T, pool *pgxpool.Pool, rt *fake.Runtime, d *daemon, issue string, token claim.Token) int {
	t.Helper()
	launches := len(launchesOf(rt, token))
	passed(t, pool, "before the stop")
	nothingStartedTheWorker(t, pool, rt, issue, token, launches, "before the stop")
	theOperatorStops(t, d, token)
	passed(t, pool, "after the stop")
	nothingStartedTheWorker(t, pool, rt, issue, token, launches, "three passes after the stop")
	return launches
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
	launches := stopTheWorker(t, pool, rt, d, root, implementer)

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
	return &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "pane-" + string(token),
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

// seedCappedRoots records a running tree for each of roots, its root at work in implementing.
// The tests of a waiting member seed exactly two, the members' own tree and a tree of another
// root, which is the cap: with both slots held, nothing is granted a member until one of the two
// gives its slot back.
func seedCappedRoots(t *testing.T, cfg config.Config, project string, roots ...string) {
	t.Helper()
	for _, at := range roots {
		seedWorkflowTree(t, cfg.PostgresDSN, project, record.Issue{Key: at, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: cfg.Project, Tree: at, Title: "root " + at, Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", HandedOver: true, Rank: "A"})
	}
}

// seedWaitingTester records key as a member of root's tree waiting for an admission slot, which
// is the fixture every test of a stop that meets a waiting issue is built from: the member stands
// in testing holding no slot, the relaunch half of its start has already run — its tester is
// ready, with no task — and the half that carries the task waits for that slot. rank is the
// member's rank; the order two of these are written in is what orders them in the line, which
// goes by the age of the row each issue is owed (record.Waiting).
func seedWaitingTester(t *testing.T, cfg config.Config, project, root, key, rank string, tester claim.Token) {
	t.Helper()
	parent := root
	seedWaitingMember(t, cfg.PostgresDSN, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: cfg.Project, Tree: root, Parent: &parent, Title: "member " + key, Phase: phase.Testing, Generation: 1,
		Status: "testing", DispatchStatus: "testing", Rank: rank},
		supervise.Claim{Token: tester, Project: project, Tree: root, Issue: key, Role: claim.RoleTester, Generation: 1,
			State: supervise.StateReady, Session: "ses_" + key, SessionFile: filepath.Join(cfg.StateDir, key+".jsonl"),
			PluginContract: api.DaemonAPIVersion, Locator: workingPane(tester)},
		record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing,
			Task: "Test the change.", NeedsSlot: true})
	writePrompt(t, cfg, tester)
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
	seedCappedRoots(t, cfg, project, root, other)
	seedWaitingTester(t, cfg, project, root, member, "B", tester)
	d := startDaemon(t, cfg, o)

	passed(t, pool, "before the stop")
	if line := d.state().Admission.Waiting; !slices.Contains(line, member) {
		t.Fatalf("the waiting line is %v, want %s in it before the stop: the member owes its tester a start and holds no slot, which is what puts an issue in the line's first part",
			line, member)
	}
	launches := len(launchesOf(rt, tester))
	theOperatorStops(t, d, tester)

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

	// The way back, which the stop leaves open: the issue is held and its architect retries the
	// held phase, which writes the start pair again.
	heldAndRetried(t, pool, d, startedAgainEngine(cfg), member, claim.RoleTester)
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

// A slot freed by the pass that meets a stop goes past the stopped issue to the next issue in
// the line. The stop ends the start its issue was owed, and the issue is out of the line of that
// same pass, so the one free slot reaches the issue behind it and the stopped issue holds no
// slot after it.
//
// This is what the repair's place in the pass decides (admit's promoteHolds). The line offers a
// slot to any unslotted issue that owes its worker a start, and reads no claim at all
// (record.Waiting), so a stopped worker's issue stands in it exactly as it stood before the stop
// — and stands first, its row being the oldest. Ending the rows after the line was judged
// therefore granted that issue the free slot and then deleted the row it was granted for: a slot
// held with nobody in it, and the issue behind it still waiting for one. The repair runs before
// the line is judged now, and the pass re-reads what it ended.
//
// The fixture is TestAStopEndsTheOwedStartOfAWaitingIssue's with a second waiting member behind
// the stopped one: two roots hold the cap's two slots, and two members of the first root's tree
// each owe their tester the task half of a start. Their order in the line is read before the
// stop, since the whole test is which of the two the one free slot reaches.
//
// The stop and the slot coming free have to reach the daemon as one state: a pass that met the
// stop with the cap still full would end the owed row with no slot to give out, and the pass
// that freed the slot would have nothing left to decide. So the test makes both while it holds
// the lock every fact applies under (intake.SerializeFacts, which boot_replay_test.go's live
// fact holds in the same way). The daemon's own ticks wait on that lock, and the first pass to
// take it once the test lets it go is the pass this test is about: one pass, with the retired
// claim and the free slot both in front of it.
func TestAFreedSlotGoesPastTheStoppedIssueToTheNextInLine(t *testing.T) {
	pool, cfg, o, rt := stoppedWorkerDaemon(t)
	cfg.AdmissionCap = 2
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root, stopped, next, other := cfg.Project+"-1", cfg.Project+"-2", cfg.Project+"-3", cfg.Project+"-4"
	stoppedTester, err := claim.NewToken(project, stopped, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	nextTester, err := claim.NewToken(project, next, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	// The members' own tree, whose root is at work and holds one of the two slots, and a tree of
	// another root holding the other: the cap is full until this test frees the second, which is
	// the one slot the two members compete for.
	seedCappedRoots(t, cfg, project, root, other)
	// Both members wait as TestAStopEndsTheOwedStartOfAWaitingIssue's member does. The stopped one
	// is written first, which is what puts it ahead of the other in the line (record.Waiting
	// orders that part by the age of the row the issue is owed).
	seedWaitingTester(t, cfg, project, root, stopped, "B", stoppedTester)
	seedWaitingTester(t, cfg, project, root, next, "C", nextTester)
	d := startDaemon(t, cfg, o)

	passed(t, pool, "before the stop")
	line := []string{stopped, next}
	if waits := d.state().Admission.Waiting; !slices.Equal(waits, line) {
		t.Fatalf("the waiting line is %v, want %v before the stop: both members owe their tester a start and hold no slot, and the oldest owed row stands first, so the issue about to be stopped is the one the next free slot would reach",
			waits, line)
	}
	launches := len(launchesOf(rt, stoppedTester))

	// The stop and the end of the other root's work, under the lock every fact takes, so that the
	// first pass after them is one pass with both.
	lock, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("open the transaction the fact lock is held in: %v", err)
	}
	// A failed read below must not leave the lock held: the pool's close at cleanup waits for
	// this connection.
	defer func() { _ = lock.Rollback(context.Background()) }()
	if err := intake.SerializeFacts(context.Background(), lock); err != nil {
		t.Fatalf("take the lock every fact applies under, which holds the daemon's passes: %v", err)
	}
	theOperatorStops(t, d, stoppedTester)
	finish(t, pool, record.NewStore(), other)
	if err := lock.Rollback(context.Background()); err != nil {
		t.Fatalf("let the daemon's passes have the fact lock back: %v", err)
	}

	passed(t, pool, "after the stop and the freed slot")
	nothingStartedTheWorker(t, pool, rt, stopped, stoppedTester, launches, "three passes after the stop that freed a slot")
	outOfTheLine(t, d, stopped, "after the pass that met the stop with a slot free")
	held := slotHolders(t, pool)
	if !slices.Contains(held, next) {
		t.Fatalf("the admission slots are held by %v, want %s among them: the pass that ended the stopped issue's owed start judged the line after it, so the slot the other root gave back goes to the issue behind it",
			held, next)
	}
	if slices.Contains(held, stopped) {
		t.Fatalf("the admission slots are held by %v, want %s to hold none: the pass ended the start it was owed before it judged the line, so nothing offered it the freed slot",
			held, stopped)
	}
}

// applyFact applies one fact against the daemon's database, under the source and through the
// handlers the route that carries it applies it under (intake.ApplyFact): a fact of the
// workflow's own under `api` through the engine, and a Dispatch event under `dispatch` through
// the pair of handlers the daemon applies one through, the workflow then admission. The rows it
// writes wake no outbox lane of the daemon's (projectRecords is what wakes them), so the passes
// the daemon's own ticks make while those rows wait are passes over the state the fact left. The
// event id is the test's, so it is never one of the daemon's.
func applyFact(t *testing.T, pool *pgxpool.Pool, source, eventID string, fact intake.Fact, handlers ...intake.Handler) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, source, eventID, fact, handlers...)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply %s = %+v, %v; want it accepted", eventID, result.Refusal, err)
	}
}

// heldAndRetried is the held phase's retry, one of the workflow's ways back from a stop: the stop
// fails no claim, so the hold is applied here as the daemon applies it when the worker of a live
// phase is gone (workflow's claimFailed), the issue is read standing at held, and the architect's
// retry of that held phase is applied. What the retry then writes is each caller's own read: the
// phase it put the issue back in, the start rows it wrote, or the launch they reach.
func heldAndRetried(t *testing.T, pool *pgxpool.Pool, d *daemon, engine *workflow.Engine, issue string, role claim.Role) {
	t.Helper()
	applyFact(t, pool, "api", "worker-died:"+issue, intake.ClaimFailed{Issue: issue, Role: role,
		Reason: "the operator stopped it"}, engine)
	if held := d.state().Issues[issue].Phase; held != phase.Held {
		t.Fatalf("%s stands at %s, want %s: the worker of its phase is gone, so the phase is held for its architect to decide",
			issue, held, phase.Held)
	}
	applyFact(t, pool, "api", "retry:"+issue, intake.RetryOrEscalate{Issue: issue, Decision: intake.RetryDecision}, engine)
}
