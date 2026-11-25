package daemon

// A claim that records a session and no session file, in a pod whose launcher is older than the
// lookup of a session by its id (dispatch://INFRA-4108 R3), end to end over the real supervision, a
// real Postgres, the daemon's own spec builder and its own workflow, with the runtime faked. The
// fake answers a resume of the session by its id alone runtime.ErrSessionLookupUnavailable, as the
// Sandbox runtime answers for a launcher that has not said in its hello that it can make the
// lookup (fake.Runtime.RefuseLookupByID). The same try then has Oh My Pi resume the session by its
// id itself, and that launch's prompt says the workspace may have been recreated. Where that start
// registers as the session, the claim records its file again. Where it does not, the next try
// forgets the session, neither that end nor the forget is charged, and every text of the forget
// says why in true words. The supervise tests walk each row of the plan's table "The start by Oh
// My Pi's own lookup, by state and event" over a store in memory (session_by_id_states_test.go);
// these walk rows 1 to 7 over the store the daemon runs on.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// olderLauncherProject and olderLauncherIssue are the project and the root issue the rig's
	// claim works.
	olderLauncherProject = "legion"
	olderLauncherIssue   = "LEGION-4108"
	// olderLauncherSession and olderLauncherSessionFile are a real Oh My Pi session and the path
	// its transcript is stored under, captured with `printf '%s\n' "$PI_SESSION_FILE"` in a Legion
	// pod on 2026-11-19 (the plan of dispatch://INFRA-4108, "Output of another system that the
	// code reads"). olderLauncherOther is a session the start's agent registers as in place of it,
	// and olderLauncherFresh the session of the fresh agent a forget starts. Each file is named the
	// way Oh My Pi names one, for its own session.
	olderLauncherSession      = "01a1f5de-4835-7667-b005-e99e3e17edd3"
	olderLauncherSessionFile  = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-cybermonklabs-legion-infra-4108--/2026-11-19T22-12-27-829Z_01a1f5de-4835-7667-b005-e99e3e17edd3.jsonl"
	olderLauncherOther        = "019a9f3e-2c41-7b5d-8e60-4f1a2b3c4d5e"
	olderLauncherOtherFile    = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-cybermonklabs-legion-infra-4108--/2026-11-20T08-41-03-117Z_019a9f3e-2c41-7b5d-8e60-4f1a2b3c4d5e.jsonl"
	olderLauncherFresh        = "019aa0b1-6d72-7c3e-9f18-2a5b6c7d8e9f"
	olderLauncherFreshFile    = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-cybermonklabs-legion-infra-4108--/2026-11-20T09-05-44-302Z_019aa0b1-6d72-7c3e-9f18-2a5b6c7d8e9f.jsonl"
	olderLauncherNotFoundExit = `the role's process exited 1: Error: Session "` + olderLauncherSession + `" not found.`
	// recreatedWords are the words of the sentence the daemon adds to the prompt of a start that
	// has Oh My Pi resume the session by its id (specs.go, addressingFragment): that launcher reads
	// no row, so its word that the workspace was not recreated was not checked.
	recreatedWords = "may have been recreated since the session last ran"
	// sessionForgottenKind is the notice kind of a forget through a launcher without the lookup,
	// to the architect and to the controller alike.
	sessionForgottenKind = "session-forgotten"
)

// forgetForbidden are the words no text of a forget through a launcher without the lookup holds:
// the session was not lost, and a store may still hold it.
var forgetForbidden = []string{"lost", "no session store", "no store holds", "found nowhere"}

// ohMyPisResumeByID is a sentence that names Oh My Pi's resume of the session by its id: Oh My Pi,
// then a resume, then the id, with no full stop between them.
var ohMyPisResumeByID = regexp.MustCompile(`Oh My Pi[^.]*\bresum[a-z]*\b[^.]*\bid\b`)

// forgetTextFaults is what text, one text of a forget through a launcher without the lookup, gets
// wrong: each thing it must say and does not — the session, the launcher, and Oh My Pi's resume of
// the session by its id — and each forbidden word it holds, read without regard to case.
func forgetTextFaults(text string) []string {
	var faults []string
	if !strings.Contains(text, olderLauncherSession) {
		faults = append(faults, "does not name the session "+olderLauncherSession)
	}
	if !strings.Contains(text, "launcher") {
		faults = append(faults, "does not name the pod's launcher")
	}
	if !ohMyPisResumeByID.MatchString(text) {
		faults = append(faults, "does not name Oh My Pi's resume of the session by its id")
	}
	faults = append(faults, forbiddenIn(text)...)
	return faults
}

// forbiddenIn is a fault for each forbidden word text holds, read without regard to case.
func forbiddenIn(text string) []string {
	var faults []string
	lower := strings.ToLower(text)
	for _, banned := range forgetForbidden {
		if strings.Contains(lower, banned) {
			faults = append(faults, fmt.Sprintf("says %q", banned))
		}
	}
	return faults
}

// builtSpecs is the daemon's own spec builder, recording every claim it is handed: the copy a
// launch builds its spec from, which carries what the stored claim does not — the cause of a
// forget (supervise.Claim.SessionLost), and the mark start sets on the copy for its second Resume
// (supervise.Claim.AgentResumeGeneration).
type builtSpecs struct {
	specs
	mu    sync.Mutex
	built []supervise.Claim
}

func (s *builtSpecs) SpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	s.mu.Lock()
	s.built = append(s.built, c)
	s.mu.Unlock()
	return s.specs.SpawnSpec(ctx, c)
}

// last is the claim the builder was last handed, failing the test when it was handed none.
func (s *builtSpecs) last(t *testing.T) supervise.Claim {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.built) == 0 {
		t.Fatal("no spec was built")
	}
	return s.built[len(s.built)-1]
}

// olderLauncherRig is one root issue implementing, its implementer's claim recording the session
// olderLauncherSession and no file, supervised over a real Postgres. Its Deps are those
// newPodUpdateRig builds — the daemon's store, the fake runtime and a held clock the test fires —
// with the daemon's own spec builder in place of the stub outboxSpecs, the fake runtime refusing
// every resume by the id alone, and the forget told to a real workflow engine
// (workflowRuntime.sessionLost), whose rows the rig's outbox runs with a recording publisher and a
// recording Dispatch, as newFreshSessionRig wires its runner.
type olderLauncherRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	st      *store.Store
	sup     *supervisor
	rt      *fake.Runtime
	clock   *heldClock
	specs   *builtSpecs
	token   claim.Token
	machine *supervise.Machine
	// workflowLog is what the workflow wrote, where a fact it could not apply is logged with its
	// error (workflowRuntime.applySessionFact).
	workflowLog *syncBuffer
	runner      *outbox
	notices     *outboxPublisher
	dispatch    *outboxDispatch

	mu sync.Mutex
	// told is the cause of each forget the machine told the workflow of (Deps.SessionLost), in
	// order. The machine tells it inside its own decision, so a decision that told nothing has
	// started no write of the workflow's either.
	told []supervise.SessionLoss
}

// newOlderLauncherRig seeds the claim, queued at generation 1 with failures launch failures, and
// what a forget starts again — the outcome of one turn of its session and a run of two provider
// errors — and restores it as a boot does.
func newOlderLauncherRig(t *testing.T, failures int) *olderLauncherRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()
	token := mustClaimToken(t, olderLauncherIssue, claim.RoleImplementer)
	seedWorkflowTree(t, dsn, olderLauncherProject, record.Issue{
		Key: olderLauncherIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: olderLauncherIssue, Title: "A session recorded with no file", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, supervise.Claim{
		Token: token, Project: olderLauncherProject, Tree: olderLauncherIssue, Issue: olderLauncherIssue, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateQueued, PluginContract: api.DaemonAPIVersion(), Session: olderLauncherSession,
		Budgets:          supervise.Budgets{LaunchFailures: failures},
		TurnOutcomes:     []supervise.TurnOutcome{{Generation: 1, Sequence: 1, Refused: true, Message: "overloaded"}},
		ProviderErrorRun: 2,
	})

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	clock := &heldClock{}
	rt := fake.NewRuntime()
	rt.Now = clock.Now
	rt.RefuseLookupByID = true
	conns := fake.NewConns()
	conns.Register(token, fake.NewConn())
	records := record.NewStore()
	workflowLog := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(workflowLog, nil))
	engine := workflow.New(records, workflow.Config{Project: olderLauncherProject, Repositories: testRepositories}, log)
	w := &workflowRuntime{pool: pool, records: records, log: log, handlers: []intake.Handler{engine}}

	r := &olderLauncherRig{t: t, ctx: store.ForConcurrentWorker(ctx), pool: pool, st: st, rt: rt, clock: clock, token: token,
		specs:       &builtSpecs{specs: specs{stateDir: t.TempDir(), project: olderLauncherProject, prompts: composer, appOwners: noAppOwners}},
		workflowLog: workflowLog, notices: &outboxPublisher{}, dispatch: &outboxDispatch{}}
	sup := newSupervisor(ctx, st, olderLauncherProject, t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: st, Specs: r.specs, Clock: clock, Log: quietLogger(),
		ClaimActivity:      st.ClaimActivity,
		Limits:             supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:           supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract:     api.DaemonAPIVersion(),
		TaskTakeContract:   api.TaskTakeContract(),
		PluginRelaunchable: true,
		RefreshGate:        supervise.NewRefreshGate(0),
		Worker:             store.ForConcurrentWorker,
		SessionLost: func(c supervise.Claim, cause supervise.SessionLoss, detail string) {
			r.mu.Lock()
			r.told = append(r.told, cause)
			r.mu.Unlock()
			w.sessionLost(c, cause, detail)
		},
	}
	t.Cleanup(sup.stop)
	r.boot(sup)
	r.runner = &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		project: olderLauncherProject, stateDir: t.TempDir(), tokens: outboxTokens{}, notices: r.notices, dispatch: r.dispatch,
		handlers: []intake.Handler{engine}, now: time.Now, trees: st}
	return r
}

// boot builds the machines of every claim the store holds under sup, as a daemon's boot does.
func (r *olderLauncherRig) boot(sup *supervisor) {
	r.t.Helper()
	stored, err := sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	machine, ok := sup.Machine(r.token)
	if !ok {
		r.t.Fatalf("the boot supervises no claim %s", r.token)
	}
	r.sup, r.machine = sup, machine
}

// restart is a second daemon over the same Postgres and the same pod: the first one's machines
// stop, and new ones are built from what the store holds, with nothing of the first one's memory.
func (r *olderLauncherRig) restart() {
	r.t.Helper()
	r.machine.Wait()
	r.sup.stop()
	sup := newSupervisor(context.Background(), r.st, olderLauncherProject, r.t.TempDir(), quietLogger())
	sup.deps = r.sup.deps
	r.t.Cleanup(sup.stop)
	r.boot(sup)
	r.runner.supervisor = sup
}

// stopInTheFreshSpawn hands the claim's machine ev on a context of its own, as a daemon's machines
// decide on the daemon's, and ends that context — the daemon's stop — while the Spawn of the fresh
// agent the decision launches waits in the runtime (fake.Runtime.OnSpawn): that Spawn alone waits
// for its context to end, as the Sandbox runtime's Spawn waits on a pod that is coming up, and then
// answers that context's error, so the call is recorded and nothing is started. The launch is cut
// short, and writes nothing after the write its try made before its start (launchTries).
func (r *olderLauncherRig) stopInTheFreshSpawn(ev supervise.Event) {
	r.t.Helper()
	waiting := make(chan struct{})
	var held atomic.Bool
	r.rt.OnSpawn = func(ctx context.Context) error {
		if held.Swap(true) {
			return nil // a Spawn after the held one is the fake's own
		}
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, stop := context.WithCancel(store.ForConcurrentWorker(r.ctx))
	defer stop()
	decided := make(chan error, 1)
	go func() { decided <- r.machine.Handle(ctx, ev) }()
	select {
	case <-waiting:
	case err := <-decided:
		r.t.Fatalf("the decision on %T ended (%v) with no Spawn held; want the fresh agent's Spawn held", ev, err)
	}
	stop()
	<-decided
	r.machine.Wait()
	r.rt.OnSpawn = nil
}

func (r *olderLauncherRig) claim() supervise.Claim { return r.machine.Claim() }

// handle hands the claim's machine one event and waits for what it set going.
func (r *olderLauncherRig) handle(ev supervise.Event) error {
	r.t.Helper()
	err := r.machine.Handle(r.ctx, ev)
	r.machine.Wait()
	return err
}

func (r *olderLauncherRig) must(ev supervise.Event) {
	r.t.Helper()
	if err := r.handle(ev); err != nil {
		r.t.Fatalf("handle %T: %v", ev, err)
	}
}

// stored is the claim as the store holds it.
func (r *olderLauncherRig) stored() supervise.Claim {
	r.t.Helper()
	claims, err := r.st.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == r.token {
			return c
		}
	}
	r.t.Fatalf("the store holds no claim %s", r.token)
	return supervise.Claim{}
}

// starts is every Spawn and Resume the runtime was asked for from its call numbered from on.
func (r *olderLauncherRig) starts(from int) []fake.Call {
	var starts []fake.Call
	for _, call := range r.rt.Calls()[from:] {
		if call.Method == "Spawn" || call.Method == "Resume" {
			starts = append(starts, call)
		}
	}
	return starts
}

// olderLauncherStarts is starts as a failure reads them: each call's method, generation, and the
// session, file and ResumeByAgent its spec names.
func olderLauncherStarts(starts []fake.Call) string {
	if len(starts) == 0 {
		return "no start"
	}
	described := make([]string, len(starts))
	for i, call := range starts {
		described[i] = fmt.Sprintf("%s at generation %d (session %q, file %q, ResumeByAgent %t)", call.Method,
			call.Spec.Generation, call.Spec.ResumeSession, call.Spec.ResumeSessionFile, call.Spec.ResumeByAgent)
	}
	return strings.Join(described, ", then ")
}

// resumedByID is whether call is a Resume of the rig's session by its id alone, which a launcher
// that can make the lookup resolves: no file, and no ResumeByAgent. The rig's runtime refuses it.
func resumedByID(call fake.Call) bool {
	return call.Method == "Resume" && call.Spec.ResumeSession == olderLauncherSession && call.Spec.ResumeSessionFile == "" && !call.Spec.ResumeByAgent
}

// resumedByAgent is whether call is a Resume of the rig's session that Oh My Pi resolves by the id
// itself (`--resume=<session id>`): ResumeByAgent, and no file.
func resumedByAgent(call fake.Call) bool {
	return call.Method == "Resume" && call.Spec.ResumeSession == olderLauncherSession && call.Spec.ResumeSessionFile == "" && call.Spec.ResumeByAgent
}

// startByAgentLookup launches the claim the boot restored as state-table row 1 says a launch
// through a launcher without the lookup goes: a Resume of the session by its id, which the runtime
// refuses, then one in the same generation with ResumeByAgent, no Spawn, and the mark at that
// generation. It answers the second Resume. A launch that went any other way ends the test here,
// since everything after it reads what follows that start.
//
// The claim is stored queued at a generation above zero, so the boot restores it launch_uncertain
// (supervise.Claim.ReleasedLaunch), and it is released to queued before the spawn, as the daemon's
// own path for an unfinished launch does (releaseUnfinishedClaim, launchUnfinished). That release
// keeps its session.
func (r *olderLauncherRig) startByAgentLookup() fake.Call {
	r.t.Helper()
	if r.claim().State == supervise.StateLaunchUncertain {
		if _, err := r.machine.ReleaseUncertainLaunch(r.ctx); err != nil {
			r.t.Fatalf("release the claim's unfinished launch: %v", err)
		}
	}
	from := len(r.rt.Calls())
	r.must(supervise.RequestSpawn{Claim: r.token})
	starts := r.starts(from)
	if len(starts) != 2 || !resumedByID(starts[0]) || !resumedByAgent(starts[1]) || starts[0].Spec.Generation != starts[1].Spec.Generation {
		r.t.Fatalf("the launch of a claim recording %s and no file, through a launcher without the lookup, made %s; want a Resume of the session by its id, which the launcher refuses, then one with ResumeByAgent in the same generation, and no Spawn",
			olderLauncherSession, olderLauncherStarts(starts))
	}
	if g, mark := starts[1].Spec.Generation, r.claim().AgentResumeGeneration; mark != g {
		r.t.Fatalf("the claim is marked %d after the start by Oh My Pi's own lookup at generation %d; want the mark at that generation", mark, g)
	}
	return starts[1]
}

// gone is the runtime's probe finding the process of the claim's last launch gone (goneEvent).
func (r *olderLauncherRig) gone() {
	r.t.Helper()
	r.must(r.goneEvent())
}

// goneEvent is the observation of the process of the claim's last launch gone, with Oh My Pi's
// own account of a resume by an id its session table holds no row for.
func (r *olderLauncherRig) goneEvent() supervise.Event {
	r.t.Helper()
	loc := r.claim().Locator
	if loc == nil {
		r.t.Fatal("the claim records no process to end")
	}
	return supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *loc, Kind: runtime.Gone, At: r.clock.Now(), Detail: olderLauncherNotFoundExit}}
}

// deadline fires the registration deadline the last launch armed.
func (r *olderLauncherRig) deadline() {
	r.t.Helper()
	timeouts := r.sup.deps.Timeouts
	if fired := r.clock.fire(runtime.RegistrationDeadline(timeouts.Boot, timeouts.RegistrationIntervals, r.rt.ProvisionBound())); fired == 0 {
		r.t.Fatal("no registration deadline was armed")
	}
	r.machine.Wait()
}

// registerAs is the shim's hello and its agent's registration as session with file, on the plugin
// contract given, at the claim's current generation. It answers the registration.
func (r *olderLauncherRig) registerAs(session, file string, contract int) error {
	r.t.Helper()
	generation := r.claim().Generation
	r.must(supervise.StreamHello{Claim: r.token, Generation: generation})
	return r.handle(supervise.RequestRegister{Claim: r.token, Generation: generation, Session: session, SessionFile: file, PluginContract: contract})
}

// toldCauses is the cause of each forget the machine told the workflow of, in order.
func (r *olderLauncherRig) toldCauses() []supervise.SessionLoss {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]supervise.SessionLoss(nil), r.told...)
}

// rows is every outbox row of kind, oldest first.
func (r *olderLauncherRig) rows(kind record.OutboxKind) []record.OutboxRow {
	r.t.Helper()
	query, err := r.pool.Query(r.ctx, "select id, kind, issue, payload from outbox where kind = $1 order by id", string(kind))
	if err != nil {
		r.t.Fatalf("read the %s rows: %v", kind, err)
	}
	defer query.Close()
	var rows []record.OutboxRow
	for query.Next() {
		var row record.OutboxRow
		if err := query.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("read a %s row: %v", kind, err)
		}
		rows = append(rows, row)
	}
	if err := query.Err(); err != nil {
		r.t.Fatalf("read the %s rows: %v", kind, err)
	}
	return rows
}

// noticeKinds is the kind of every notice row of kind — the architect's (record.Notice) or the
// controller's (record.ControllerNotice) — oldest first.
func (r *olderLauncherRig) noticeKinds(kind record.OutboxKind) []string {
	r.t.Helper()
	var kinds []string
	for _, row := range r.rows(kind) {
		var notice record.Notice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode the %s row %d: %v", kind, row.ID, err)
		}
		kinds = append(kinds, string(notice.Kind))
	}
	return kinds
}

// awaitForgetTold holds that the machine told the workflow of one forget, for the cause
// not-resumed-by-id, and waits for the workflow's rows of it: the workflow applies the fact on a
// goroutine of its own (applySessionFact), whose message and notices commit in one transaction. A
// workflow that could not apply it logs that with its error, which ends the wait at once.
func (r *olderLauncherRig) awaitForgetTold() {
	r.t.Helper()
	if told := r.toldCauses(); len(told) != 1 || told[0] != supervise.SessionLossNotResumedByID {
		r.t.Fatalf("the machine told the workflow of forgets for the causes %q; want one, for %q", told, supervise.SessionLossNotResumedByID)
	}
	failed := func() bool { return strings.Contains(r.workflowLog.String(), `"error":`) }
	testwait.Eventually(r.t, "the workflow's rows of the forget, or its line that it could not write them", func() bool {
		return len(r.rows(record.OutboxKindDispatchMessage)) > 0 || failed()
	})
	if failed() {
		r.t.Fatalf("the workflow could not write what it tells of the forget:\n%s", r.workflowLog.String())
	}
}

// wantToldOnce holds that the workflow told the forget once: one message on the issue, and one
// session-forgotten notice each to the architect and the controller.
func (r *olderLauncherRig) wantToldOnce() {
	r.t.Helper()
	if posted := r.rows(record.OutboxKindDispatchMessage); len(posted) != 1 {
		r.t.Errorf("the outbox holds %d messages for the issue; want one", len(posted))
	}
	for _, kind := range []record.OutboxKind{record.OutboxKindNotice, record.OutboxKindControllerNotice} {
		if kinds := r.noticeKinds(kind); len(kinds) != 1 || kinds[0] != sessionForgottenKind {
			r.t.Errorf("the outbox holds the %s rows %q; want one %s", kind, kinds, sessionForgottenKind)
		}
	}
}

// wantNothingForgotten is a launch that forgot nothing: the workflow was told of no forget, and
// the outbox holds no message for the issue and no notice of a lost or a forgotten session.
func (r *olderLauncherRig) wantNothingForgotten() {
	r.t.Helper()
	if told := r.toldCauses(); len(told) != 0 {
		r.t.Errorf("the machine told the workflow of forgets for the causes %q; want none", told)
	}
	if posted := r.rows(record.OutboxKindDispatchMessage); len(posted) != 0 {
		r.t.Errorf("the outbox holds %d messages for the issue; want none", len(posted))
	}
	for _, kind := range []record.OutboxKind{record.OutboxKindNotice, record.OutboxKindControllerNotice} {
		for _, told := range r.noticeKinds(kind) {
			if told == "session-lost" || told == sessionForgottenKind {
				r.t.Errorf("the outbox holds a %s %s; want no notice of a lost or a forgotten session", kind, told)
			}
		}
	}
}

// wantForgotAtTheNextTry is state-table row 7, read from the runtime's call numbered from on, the
// first call after the end of the start by Oh My Pi's own lookup: that end and the forget are
// charged nothing, so the claim keeps failures launch failures, counts no death and does not fail.
// The next try asks the launcher for the lookup once more and is refused it, then forgets the
// session in the same try, with the cause not-resumed-by-id: one Spawn of a fresh agent, built for
// that cause and resuming nothing, and no second Resume with ResumeByAgent. The claim abandons the
// session, and the outcomes of its session's turns and its run of provider errors start again, as
// at every forget. The issue is told in one message, and the architect and the controller in one
// session-forgotten notice each. The fresh agent's registration is accepted.
func (r *olderLauncherRig) wantForgotAtTheNextTry(from, failures int) {
	r.t.Helper()
	c := r.claim()
	if c.Budgets.LaunchFailures != failures || c.Budgets.Deaths != 0 || c.State == supervise.StateFailed || c.FailureCause != "" {
		r.t.Errorf("the claim is %s (cause %q) with %d launch failures and %d deaths; want it not failed, with %d launch failures and no death: neither the end nor the forget is charged",
			c.State, c.FailureCause, c.Budgets.LaunchFailures, c.Budgets.Deaths, failures)
	}
	starts := r.starts(from)
	if len(starts) != 2 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" {
		r.t.Fatalf("the next try made %s; want one Resume of %s by its id, which the launcher refuses, then one Spawn, and no Resume with ResumeByAgent",
			olderLauncherStarts(starts), olderLauncherSession)
	}
	spawned := starts[1].Spec
	if spawned.ResumeSession != "" || spawned.ResumeSessionFile != "" || spawned.ResumeByAgent {
		r.t.Errorf("the fresh spawn names session %q, file %q and ResumeByAgent %t; want it to resume nothing", spawned.ResumeSession, spawned.ResumeSessionFile, spawned.ResumeByAgent)
	}
	if built := r.specs.last(r.t); built.Generation != spawned.Generation || built.SessionLost != supervise.SessionLossNotResumedByID {
		r.t.Errorf("the last spec was built at generation %d with SessionLost %q; want the fresh spawn's, at %d, with %q",
			built.Generation, built.SessionLost, spawned.Generation, supervise.SessionLossNotResumedByID)
	}
	if c.Abandoned.Session != olderLauncherSession || c.Session != "" || c.SessionFile != "" {
		r.t.Errorf("the claim abandoned %q and records session %q and file %q; want %s abandoned and no session recorded", c.Abandoned.Session, c.Session, c.SessionFile, olderLauncherSession)
	}
	if len(c.TurnOutcomes) != 0 || c.ProviderErrorRun != 0 {
		r.t.Errorf("the claim keeps turn outcomes %+v and a run of %d provider errors; want both started again with the fresh session", c.TurnOutcomes, c.ProviderErrorRun)
	}

	r.awaitForgetTold()
	r.wantToldOnce()

	if err := r.registerAs(olderLauncherFresh, olderLauncherFreshFile, api.DaemonAPIVersion()); err != nil {
		r.t.Fatalf("the fresh agent's registration as %s = %v; want it accepted", olderLauncherFresh, err)
	}
	if c := r.claim(); c.Session != olderLauncherFresh || c.SessionFile != olderLauncherFreshFile {
		r.t.Errorf("the claim records session %q and file %q after the fresh registration; want %s and %s", c.Session, c.SessionFile, olderLauncherFresh, olderLauncherFreshFile)
	}
	for _, call := range r.rt.Calls()[from:] {
		if call.Method == "Resume" && call.Spec.ResumeByAgent {
			r.t.Errorf("the runtime was asked for a Resume with ResumeByAgent at generation %d after the start that did not register; want none", call.Spec.Generation)
		}
	}
}

// drain runs the outbox until no row is due: each pass runs every row due when it starts
// (outbox.RunOnce). A row that waits or fails is retried on its backoff, which is due after the
// drain.
func (r *olderLauncherRig) drain() {
	r.t.Helper()
	for range 8 {
		due, err := r.runner.listDue(r.ctx, "", nil, time.Now().UTC())
		if err != nil {
			r.t.Fatalf("list the due outbox rows: %v", err)
		}
		if len(due) == 0 {
			return
		}
		if err := r.runner.RunOnce(r.ctx); err != nil {
			r.t.Fatalf("run the outbox: %v", err)
		}
	}
}

// publishedOn is every publish the outbox made to topic, in order.
func (r *olderLauncherRig) publishedOn(topic string) []outboxPublish {
	var on []outboxPublish
	for _, published := range r.notices.publishes() {
		if published.topic == topic {
			on = append(on, published)
		}
	}
	return on
}

// Through a launcher that has not said it can look a session up by its id, the claim's launch has
// Oh My Pi resume the session by its id itself, and the agent registers as that session: the claim
// records the session's file again, nothing is charged, nothing is forgotten and nobody is told of
// a lost session. The daemon's prompt for that launch says the workspace may have been recreated,
// since that launcher read no row to say it was not; the claim's next launch, a resume of the
// recorded file, says nothing of it (state-table rows 1 and 2).
func TestAnOlderLauncherResumesTheSessionByItsIDWithTheRecreatedSentence(t *testing.T) {
	t.Parallel()
	r := newOlderLauncherRig(t, 0)

	byAgent := r.startByAgentLookup()

	if addressing := byAgent.Spec.Prompt.Addressing; !strings.Contains(addressing, olderLauncherSession) || !strings.Contains(addressing, recreatedWords) {
		t.Errorf("the prompt of the Resume with ResumeByAgent addresses the agent %q; want it to name %s and say the workspace %s",
			addressing, olderLauncherSession, recreatedWords)
	}
	if err := r.registerAs(olderLauncherSession, olderLauncherSessionFile, api.DaemonAPIVersion()); err != nil {
		t.Fatalf("the registration as %s on %s = %v; want it accepted", olderLauncherSession, olderLauncherSessionFile, err)
	}
	if stored := r.stored(); stored.Session != olderLauncherSession || stored.SessionFile != olderLauncherSessionFile || stored.AgentResumeGeneration != 0 {
		t.Errorf("the store holds the claim with session %q and file %q, marked %d; want %s, %s and no mark",
			stored.Session, stored.SessionFile, stored.AgentResumeGeneration, olderLauncherSession, olderLauncherSessionFile)
	}
	c := r.claim()
	if c.Budgets.LaunchFailures != 0 {
		t.Errorf("the claim carries %d launch failures; want none", c.Budgets.LaunchFailures)
	}
	if c.Abandoned.Session != "" || c.Abandoned.Why != "" {
		t.Errorf("the claim abandoned session %q because %q; want nothing abandoned", c.Abandoned.Session, c.Abandoned.Why)
	}
	r.wantNothingForgotten()
	if spawns := r.rt.CallsOf("Spawn"); len(spawns) != 0 {
		t.Errorf("the runtime was asked for %d Spawns; want none", len(spawns))
	}

	from := len(r.rt.Calls())
	r.gone()

	next := r.starts(from)
	if len(next) == 0 || next[0].Method != "Resume" || next[0].Spec.ResumeSessionFile != olderLauncherSessionFile {
		t.Fatalf("the launch after the registered process died made %s; want a Resume of the recorded file %s", olderLauncherStarts(next), olderLauncherSessionFile)
	}
	if addressing := next[0].Spec.Prompt.Addressing; strings.Contains(addressing, recreatedWords) {
		t.Errorf("the prompt of the resume of the recorded file addresses the agent %q; want nothing of a workspace that %s", addressing, recreatedWords)
	}
}

// Through a launcher that has not said it can look a session up by its id, the start that has Oh
// My Pi resume the session by its id does not register as the session: its process ends first,
// its registration deadline passes, or its agent comes back as another session, which is refused,
// and its process then ends. The machines may be built again from the store between the start and
// its end, which they read the start's mark back from. That end is charged nothing and the claim's
// next try forgets the session, charged nothing either, so a claim one launch failure below its
// limit does not fail (state-table rows 3 to 7).
func TestAnOlderLauncherStartThatDoesNotRegisterForgetsTheSessionAtTheNextTry(t *testing.T) {
	t.Parallel()
	// seeded is the launch failures the claim carries: one below the rig's limit of two, so a
	// charge of the end or of the forget would fail the claim.
	const seeded = 1
	for _, tc := range []struct {
		name string
		end  func(r *olderLauncherRig, g uint64)
	}{
		{"a probe reports the process gone", func(r *olderLauncherRig, _ uint64) { r.gone() }},
		{"the registration deadline passes", func(r *olderLauncherRig, _ uint64) { r.deadline() }},
		{"the agent registers as another session, which is refused, and its process is gone", func(r *olderLauncherRig, _ uint64) {
			// The older launcher's pod carries the earlier release's plugin, so the registration
			// speaks an older contract: the mark names this generation, which is what keeps the
			// roll edge's rule from it.
			if err := r.registerAs(olderLauncherOther, olderLauncherOtherFile, api.DaemonAPIVersion()-1); !errors.Is(err, claim.SameAgentRefusal) {
				r.t.Fatalf("the registration as %s = %v; want the same-agent refusal, 409", olderLauncherOther, err)
			}
			r.gone()
		}},
		{"the machines are built again from the store, then the process is gone", func(r *olderLauncherRig, g uint64) {
			r.restart()
			if mark := r.claim().AgentResumeGeneration; mark != g {
				r.t.Fatalf("the machine built again from the store reads the mark %d; want %d, the generation of the start by Oh My Pi's own lookup", mark, g)
			}
			r.gone()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOlderLauncherRig(t, seeded)
			g := r.startByAgentLookup().Spec.Generation
			from := len(r.rt.Calls())

			tc.end(r, g)

			r.wantForgotAtTheNextTry(from, seeded)
		})
	}
}

// A forget through a launcher that has not said it can look a session up by its id says why in
// every text a reader meets it in: the message on the issue, the notice its architect and the
// controller are each published, with the one-line message it is published with, the fresh
// agent's own prompt (its addressing, which the daemon builds; the role's prompt files use the word
// "lost" in other senses), and the reason the claim keeps with the session it abandoned. Each names
// the session, the pod's launcher and Oh My Pi's resume of the session by its id, and none says
// the session was lost or that no store holds it: the session was not looked up, and a store may
// hold it still (state-table row 7).
func TestAForgetThroughALauncherWithoutTheLookupSaysSoInEveryText(t *testing.T) {
	t.Parallel()
	r := newOlderLauncherRig(t, 0)
	r.startByAgentLookup()
	from := len(r.rt.Calls())
	r.gone()
	starts := r.starts(from)
	if len(starts) != 2 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" {
		t.Fatalf("the next try made %s; want one Resume of %s by its id, which the launcher refuses, then one Spawn", olderLauncherStarts(starts), olderLauncherSession)
	}
	r.awaitForgetTold()

	r.drain()

	type text struct{ what, says string }
	var texts []text
	posted := r.dispatch.messages
	if len(posted) != 1 {
		t.Fatalf("the outbox posted %d messages on the issue; want the forget's one", len(posted))
	}
	texts = append(texts, text{"the message posted on the issue", posted[0]})
	architect := mustClaimToken(t, olderLauncherIssue, claim.RoleArchitect)
	for _, to := range []struct{ who, topic string }{
		{"its architect", notify.RoleTopicPrefix + string(architect)},
		{"the controller", notify.ControllerTopic(olderLauncherProject)},
	} {
		published := r.publishedOn(to.topic)
		if len(published) != 1 {
			t.Errorf("the outbox published %d times on %s's topic %s; want the forget's one notice", len(published), to.who, to.topic)
			continue
		}
		if want := sessionForgottenKind + " on " + olderLauncherIssue; published[0].message != want {
			t.Errorf("the notice to %s was published as %q; want %q", to.who, published[0].message, want)
		}
		for _, fault := range forbiddenIn(published[0].message) {
			t.Errorf("the line the notice to %s was published with, %q, %s", to.who, published[0].message, fault)
		}
		notice, ok := published[0].payload.(record.Notice)
		if !ok {
			t.Errorf("the publish to %s carries %#v; want a notice", to.who, published[0].payload)
			continue
		}
		if notice.Kind != sessionForgottenKind {
			t.Errorf("the notice to %s is of the kind %q; want %q", to.who, notice.Kind, sessionForgottenKind)
		}
		for _, fault := range forbiddenIn(string(notice.Kind)) {
			t.Errorf("the kind of the notice to %s, %q, %s", to.who, notice.Kind, fault)
		}
		texts = append(texts, text{"the reason of the notice to " + to.who, notice.Reason})
	}
	texts = append(texts,
		text{"the fresh agent's addressing", starts[1].Spec.Prompt.Addressing},
		text{"the reason the claim keeps with the session it abandoned", r.claim().Abandoned.Why})

	for _, each := range texts {
		for _, fault := range forgetTextFaults(each.says) {
			t.Errorf("%s, %q, %s", each.what, each.says, fault)
		}
	}
}

// A forget of a session is told once, however many launches forget it. The machine tells the
// workflow at the forget, before the fresh agent's Spawn, while the forget is in memory alone: a
// daemon stop while that Spawn waits on its pod cuts the launch short, and the store still holds
// the claim recording the session, as the write the launch made before its start left it. The
// next daemon builds its machines from the store, its launch forgets the session again, at a
// generation of its own, and its fresh agent's Spawn succeeds. The issue is told in one message,
// and the architect and the controller in one session-forgotten notice each: the fact's event id
// names the session forgotten, so the second forget's fact is the first's (workflowRuntime's
// sessionLost).
func TestAForgetADaemonStopCutShortIsToldOnce(t *testing.T) {
	t.Parallel()
	r := newOlderLauncherRig(t, 0)
	r.startByAgentLookup()

	r.stopInTheFreshSpawn(r.goneEvent())

	if stopped := r.stored(); stopped.State != supervise.StateLaunching || stopped.Session != olderLauncherSession || stopped.Locator != nil {
		t.Fatalf("the store holds the claim %s, recording session %q and process %v, after the stop cut the fresh spawn short; want it launching, recording %s and no process",
			stopped.State, stopped.Session, stopped.Locator, olderLauncherSession)
	}
	r.awaitForgetTold()

	// The next daemon's machine tells the workflow of its forget inside its decision, and the
	// workflow applies the fact on a goroutine of its own (applySessionFact), which writes nothing
	// where the fact was applied before. So the first tell takes the lock every fact applies under
	// (intake.SerializeFacts) before it tells, and lets it go once that goroutine waits on it; the
	// test then takes the lock itself, which it is granted only once that goroutine's transaction
	// has ended, and counts the rows after.
	tell, queued := r.sup.deps.SessionLost, make(chan bool, 1)
	var first sync.Once
	r.sup.deps.SessionLost = func(c supervise.Claim, cause supervise.SessionLoss, detail string) {
		var release func()
		first.Do(func() {
			held := holdFactLock(t, r.pool)
			release = func() {
				waited := factLockWaited(r.pool)
				_ = held.Rollback(context.Background())
				queued <- waited
			}
		})
		tell(c, cause, detail)
		if release != nil {
			go release()
		}
	}
	r.restart()
	if _, err := r.machine.ReleaseUncertainLaunch(r.ctx); err != nil {
		t.Fatalf("release the claim's unfinished launch: %v", err)
	}
	from := len(r.rt.Calls())
	r.must(supervise.RequestSpawn{Claim: r.token})

	starts := r.starts(from)
	if len(starts) != 2 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" {
		t.Fatalf("the next daemon's launch made %s; want a Resume of %s by its id, which the launcher refuses, then one Spawn",
			olderLauncherStarts(starts), olderLauncherSession)
	}
	if told := r.toldCauses(); len(told) != 2 || told[0] != supervise.SessionLossNotResumedByID || told[1] != supervise.SessionLossNotResumedByID {
		t.Fatalf("the machines told the workflow of forgets for the causes %q; want two, each %q: one by each daemon", told, supervise.SessionLossNotResumedByID)
	}
	if !<-queued {
		t.Fatal("the workflow's apply of the second forget never waited on the fact lock the tell held")
	}
	if err := holdFactLock(t, r.pool).Rollback(context.Background()); err != nil {
		t.Fatalf("let the fact lock go: %v", err)
	}
	if c := r.claim(); c.Locator == nil || c.Abandoned.Session != olderLauncherSession || c.Session != "" {
		t.Errorf("the claim records process %v and session %q, and abandoned %q; want the fresh agent's process, no session, and %s abandoned",
			c.Locator, c.Session, c.Abandoned.Session, olderLauncherSession)
	}
	if strings.Contains(r.workflowLog.String(), `"error":`) {
		t.Fatalf("the workflow could not apply a forget:\n%s", r.workflowLog.String())
	}
	r.wantToldOnce()
}
