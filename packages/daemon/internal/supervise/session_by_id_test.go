package supervise

// A claim can record a session and no session file. One way a claim came to it: from
// legion-v18.0.1 to legion-v20.4.0 a close of the claim's issue cleared the file and kept the id
// (clearIssueResumePath), and the issue was then opened again. Such a claim is resumed by its
// session id where the session store holds the session, and otherwise forgets the session once,
// charged nothing. It never spawns a fresh agent while it still records the session: the fence
// refuses that agent's registration as another session (claim.SameAgentRefusal), the plugin exits
// on the 409, and the claim was launched the same way again until launch_failure_limit failed it
// (dispatch://INFRA-4108 R1). The start through a pod launcher older than the lookup, which has
// Oh My Pi resume the session by its id itself, is walked state by state in
// session_by_id_states_test.go (R3), and the ends that are charged nothing and the log lines in
// session_by_id_ends_test.go.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// idSession and idSessionFile are a real Oh My Pi session and the path its transcript is stored
// under, captured with `printf '%s\n' "$PI_SESSION_FILE"` in a Legion pod on 2026-11-19 (the plan
// of dispatch://INFRA-4108, "Output of another system that the code reads"). The file's name ends
// in `_<session id>.jsonl`, which is what a lookup by the id finds. otherSession is a session a
// process registers as in place of idSession: a fresh agent an earlier release spawned, or a resume
// that came back as another session. newSession is the session of the fresh agent a forget starts.
// Each file is named the way Oh My Pi names one, for its own session.
const (
	idSession        = "01a1f5de-4835-7667-b005-e99e3e17edd3"
	idSessionFile    = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-cybermonklabs-legion-infra-4108--/2026-11-19T22-12-27-829Z_01a1f5de-4835-7667-b005-e99e3e17edd3.jsonl"
	otherSession     = "019a9f3e-2c41-7b5d-8e60-4f1a2b3c4d5e"
	otherSessionFile = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-cybermonklabs-legion-infra-4108--/2026-11-20T08-41-03-117Z_019a9f3e-2c41-7b5d-8e60-4f1a2b3c4d5e.jsonl"
	newSession       = "019aa0b1-6d72-7c3e-9f18-2a5b6c7d8e9f"
	newSessionFile   = "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-cybermonklabs-legion-infra-4108--/2026-11-20T09-05-44-302Z_019aa0b1-6d72-7c3e-9f18-2a5b6c7d8e9f.jsonl"
)

// byIDClaim is the harness's claim in state at generation, recording idSession and no file, as the
// store holds it for the machine a daemon builds over it. A launching claim records a process: one
// a launch of an earlier daemon started, which this daemon adopts at its boot and which has not
// registered. A suspended claim is the operator's, and a failed one spent its launch budget.
func byIDClaim(state ClaimState, generation uint64) Claim {
	c := queuedClaim()
	c.State, c.Generation, c.Session = state, generation, idSession
	switch state {
	case StateLaunching, StateShimConnected:
		c.Locator = &runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       testToken,
			Incarnation: "4242:77",
			Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
		}
	case StateSuspended:
		c.SuspendedBy = SuspendedByOperator
	case StateFailed:
		c.FailureCause, c.Failure, c.FailedAt = CauseLaunchRefused, "launch failures ran out", epoch
		c.Budgets.LaunchFailures = testLimits().LaunchFailures
	}
	return c
}

// olderLauncher stands the rig's runtime for a pod whose launcher has not said in its hello that it
// can look a session up by its id, on every try (fake.Runtime.RefuseLookupByID).
func olderLauncher(h *harness) { h.rt.RefuseLookupByID = true }

// noSuchSession is the runtime's answer to a resume of a session no store holds
// (runtime.ErrSessionFileMissing), as the launcher answers an id its session table holds no row
// for.
func noSuchSession() error {
	return fmt.Errorf("resume %s by session %s: %w: the session table holds no such session", testToken, idSession, runtime.ErrSessionFileMissing)
}

// byIDRig is a harness over a claim that records a session and no file, with what the tests of its
// launches read beside the harness's own records: the session the claim recorded at each Spawn the
// runtime was asked for (spawnWatch), and each forget the machine told the workflow of
// (Deps.SessionLost).
type byIDRig struct {
	*harness
	watch *spawnWatch
	lost  []lostReport
}

// lostReport is one call of Deps.SessionLost, with how many Spawns the runtime had been asked for
// when it came: that says whether the forget was told before its fresh agent was spawned.
type lostReport struct {
	claim  Claim
	cause  SessionLoss
	detail string
	spawns int
}

// newByIDRig builds the machine over c, which the store holds, once deps has set what the case
// needs of the harness's Deps and of its fake runtime: the machine takes its Deps when it is built.
func newByIDRig(t *testing.T, c Claim, deps func(h *harness)) *byIDRig {
	t.Helper()
	h := newBareHarness(t)
	r := &byIDRig{harness: h}
	r.watch = &spawnWatch{Runtime: h.rt, rig: r}
	h.deps.Runtime = r.watch
	h.deps.SessionLost = func(c Claim, cause SessionLoss, detail string) {
		r.lost = append(r.lost, lostReport{claim: c, cause: cause, detail: detail, spawns: len(h.rt.CallsOf("Spawn"))})
	}
	if deps != nil {
		deps(h)
	}
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return r
}

// spawnWatch is the fake runtime, recording at each Spawn the session the claim's machine records
// at that moment. Spawn is called inside the machine's decision, which holds the machine's lock, so
// the claim is read as that decision holds it. A Spawn of a claim that still records a session
// starts the fresh agent whose registration the fence then refuses.
type spawnWatch struct {
	*fake.Runtime
	rig      *byIDRig
	sessions []string
}

func (w *spawnWatch) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	w.sessions = append(w.sessions, w.rig.m.claim.Session)
	return w.Runtime.Spawn(ctx, spec)
}

// refusingLaunchWrites is the store refusing the next refuse writes of a claim written launching,
// which is the write of a launch's try (launchTries), and taking every other write, the admission's
// included: AdmitClaim is memStore's own, which writes through memStore.PutClaim.
type refusingLaunchWrites struct {
	*memStore
	refuse int
}

func (s *refusingLaunchWrites) PutClaim(ctx context.Context, c Claim) error {
	if c.State == StateLaunching && s.refuse > 0 {
		s.refuse--
		return errBoom
	}
	return s.memStore.PutClaim(ctx, c)
}

// claudeSpecs builds every launch as testSpecs does, on Claude Code.
type claudeSpecs struct{ *testSpecs }

func (s claudeSpecs) SpawnSpec(ctx context.Context, c Claim) (runtime.SpawnSpec, error) {
	spec, err := s.testSpecs.SpawnSpec(ctx, c)
	spec.Backend = runtime.BackendClaude
	return spec, err
}

// starts is every Spawn and Resume the runtime was asked for from its call numbered from on, in
// order: the start calls of the launches made since.
func (r *byIDRig) starts(from int) []fake.Call {
	var starts []fake.Call
	for _, call := range r.rt.Calls()[from:] {
		if call.Method == "Spawn" || call.Method == "Resume" {
			starts = append(starts, call)
		}
	}
	return starts
}

// builds is every claim the spec builder was handed, in order (testSpecs).
func (r *byIDRig) builds() []Claim {
	r.specs.mu.Lock()
	defer r.specs.mu.Unlock()
	return slices.Clone(r.specs.claims)
}

// slept is every backoff the machine waited, in order (fakeClock.Sleep).
func (r *byIDRig) slept() []time.Duration {
	r.clock.mu.Lock()
	defer r.clock.mu.Unlock()
	return slices.Clone(r.clock.slept)
}

// launchWrite is the first claim write that records a process at generation: the write of the
// launch that started it (launchTries).
func (r *byIDRig) launchWrite(generation uint64) (Claim, bool) {
	for _, c := range r.store.history() {
		if c.Generation == generation && c.Locator != nil {
			return c, true
		}
	}
	return Claim{}, false
}

// resumedByID is whether call is a Resume of idSession by its id alone, which the runtime resolves:
// no file, and no ResumeByAgent.
func resumedByID(call fake.Call) bool {
	return call.Method == "Resume" && call.Spec.ResumeSession == idSession && call.Spec.ResumeSessionFile == "" && !call.Spec.ResumeByAgent
}

// resumedByAgent is whether call is a Resume of idSession whose id Oh My Pi's own command
// resolves (ResumeByAgent), with no file.
func resumedByAgent(call fake.Call) bool {
	return call.Method == "Resume" && call.Spec.ResumeSession == idSession && call.Spec.ResumeSessionFile == "" && call.Spec.ResumeByAgent
}

// startsText is calls as a failure message names them: each start call, with what it resumed.
func startsText(calls []fake.Call) string {
	if len(calls) == 0 {
		return "no start call"
	}
	parts := make([]string, len(calls))
	for i, call := range calls {
		if call.Method != "Resume" {
			parts[i] = fmt.Sprintf("%s at generation %d", call.Method, call.Spec.Generation)
			continue
		}
		parts[i] = fmt.Sprintf("Resume at generation %d of session %q, file %q, ResumeByAgent %t",
			call.Spec.Generation, call.Spec.ResumeSession, call.Spec.ResumeSessionFile, call.Spec.ResumeByAgent)
	}
	return strings.Join(parts, "; ")
}

// lostText is reports as a failure message names them.
func lostText(reports []lostReport) string {
	if len(reports) == 0 {
		return "no forget"
	}
	parts := make([]string, len(reports))
	for i, l := range reports {
		parts[i] = fmt.Sprintf("a forget of cause %q (%s)", l.cause, l.detail)
	}
	return strings.Join(parts, "; ")
}

// wantFirstStartResumesByID is that the first start call from the runtime's call numbered from on
// is a Resume of idSession by its id, with no file.
func (r *byIDRig) wantFirstStartResumesByID(from int) {
	r.t.Helper()
	starts := r.starts(from)
	if len(starts) == 0 || !resumedByID(starts[0]) {
		r.t.Fatalf("the launch made %s (every call of the case: %v); want its first start call a Resume of session %s by its id, with no file and no ResumeByAgent",
			startsText(starts), r.rt.Methods(), idSession)
	}
}

// wantNoSpawnWhileRecorded is that no Spawn ran while the claim recorded a session: a fresh agent
// starts only after the claim has forgotten its session.
func (r *byIDRig) wantNoSpawnWhileRecorded() {
	r.t.Helper()
	for i, recorded := range r.watch.sessions {
		if recorded != "" {
			r.t.Errorf("Spawn %d ran while the claim recorded session %q; want no Spawn before the claim forgets its session", i+1, recorded)
		}
	}
}

// wantLost is the one forget the workflow was told of, of cause: the issue, its architect and the
// controller each hear of a forgotten session once.
func (r *byIDRig) wantLost(cause SessionLoss) lostReport {
	r.t.Helper()
	if len(r.lost) != 1 || r.lost[0].cause != cause {
		r.t.Fatalf("the workflow was told of %s; want one forget of cause %q", lostText(r.lost), cause)
	}
	return r.lost[0]
}

// wantNothingLost is that the workflow was told of no forget.
func (r *byIDRig) wantNothingLost() {
	r.t.Helper()
	if len(r.lost) != 0 {
		r.t.Errorf("the workflow was told of %s; want no forget", lostText(r.lost))
	}
}

// registerAs is a registration at the claim's generation as session, on file, from a plugin that
// speaks contract.
func (r *byIDRig) registerAs(session, file string, contract int) error {
	return r.handle(RequestRegister{
		Claim: r.token, Generation: r.generation(), Session: session, SessionFile: file,
		CapabilityHash: []byte("capability"), PluginContract: contract,
	})
}

// readyAs is the agent of session saying it is ready.
func (r *byIDRig) readyAs(session string) {
	r.t.Helper()
	r.must(RequestReady{Claim: r.token, Generation: r.generation(), Session: session})
}

// olderContract is the plugin contract of a process an earlier release started: one below the
// daemon's (Deps.PluginContract).
func (r *byIDRig) olderContract() int { return r.deps.PluginContract - 1 }

// The launch paths of a claim that records a session and no file, as the plan of
// dispatch://INFRA-4108 lists them ("Lists named before the code"): every caller of launch,
// launchAfter, revive and relaunchAfterFailure that can reach such a claim, and the repoint and the
// displacement, which relaunch an agent that has not registered. Each path's first start call is a
// Resume of the recorded session by its id, with no file, and no path spawns a fresh agent while
// the claim still records the session (R1: "The daemon starts a fresh agent only for a claim that
// records no session"). The runtime finds the session in every row, so no row forgets it.
func TestEveryLaunchOfAClaimWithASessionAndNoFileNamesItsSession(t *testing.T) {
	storeDown := fmt.Errorf("resume %s: %w: dial tcp: connection refused", testToken, runtime.ErrSessionStore)
	type path struct {
		// name tells two paths of one row apart, and is empty for a row of one path.
		name  string
		claim Claim
		deps  func(h *harness)
		// before brings the claim to where the path's event finds it, and trigger is that event.
		before  func(r *byIDRig)
		trigger func(r *byIDRig)
	}
	suspendedByDaemon := byIDClaim(StateSuspended, 3)
	suspendedByDaemon.SuspendedBy = SuspendedByDaemon
	spawn := func(r *byIDRig) { r.must(RequestSpawn{Claim: testToken}) }
	run := func(t *testing.T, p path) {
		r := newByIDRig(t, p.claim, p.deps)
		if p.before != nil {
			p.before(r)
		}
		from := len(r.rt.Calls())

		p.trigger(r)

		r.wantNoSpawnWhileRecorded()
		r.wantFirstStartResumesByID(from)
	}
	for _, row := range []struct {
		name  string
		paths []path
	}{
		{"the retry of a retired claim", []path{{
			claim:   byIDClaim(StateRetired, 3),
			trigger: func(r *byIDRig) { r.must(RequestRetry{Claim: testToken}) },
		}}},
		{"the operator's retry of a failed claim", []path{{
			claim:   byIDClaim(StateFailed, 3),
			trigger: func(r *byIDRig) { r.must(RequestOperatorRetry{Claim: testToken}) },
		}}},
		{"the resume of a suspended claim", []path{
			{
				name:    "the operator's resume",
				claim:   byIDClaim(StateSuspended, 3),
				trigger: func(r *byIDRig) { r.must(RequestResume{Claim: testToken}) },
			},
			{
				name:  "the resume by rule",
				claim: suspendedByDaemon,
				trigger: func(r *byIDRig) {
					r.must(RequestRuleResume{Claim: testToken, Reason: "the outbox starts the claim's phase"})
				},
			},
		}},
		{"the delivery of a task to a suspended claim", []path{{
			claim:   byIDClaim(StateSuspended, 3),
			trigger: func(r *byIDRig) { r.must(RequestDeliver{Claim: testToken, Task: "the next task"}) },
		}}},
		{"the spawn of a queued claim", []path{
			{name: "a claim queued at the generation of its last launch", claim: byIDClaim(StateQueued, 2), trigger: spawn},
			{
				name:  "a launch_uncertain claim released to queued",
				claim: byIDClaim(StateLaunchUncertain, 2),
				before: func(r *byIDRig) {
					if released, err := r.m.ReleaseUncertainLaunch(r.ctx); err != nil || !released {
						r.t.Fatalf("release = %t, %v; want the uncertain launch released to queued", released, err)
					}
				},
				trigger: spawn,
			},
		}},
		{"the death of an adopted process that never registered", []path{{
			claim:   byIDClaim(StateLaunching, 3),
			trigger: func(r *byIDRig) { r.observe(runtime.Gone) },
		}}},
		{"the registration deadline of an adopted process that never registered", []path{{
			claim:   byIDClaim(StateLaunching, 3),
			trigger: func(r *byIDRig) { r.advance(deadline) },
		}}},
		{"the timer relaunch of a launch that waits", []path{
			{
				name:  "after a start the session store could not answer",
				claim: byIDClaim(StateQueued, 2),
				before: func(r *byIDRig) {
					// Both start calls answer the store's fault, so the first try waits for the
					// store whichever of them it makes.
					r.rt.ScriptResume(fake.SpawnResult{Err: storeDown})
					r.rt.ScriptSpawn(fake.SpawnResult{Err: storeDown})
					_ = r.handle(RequestSpawn{Claim: testToken})
				},
				trigger: func(r *byIDRig) { r.advance(SessionStoreRetry) },
			},
			{
				name:  "after a launch whose write failed",
				claim: byIDClaim(StateQueued, 2),
				deps:  func(h *harness) { h.deps.Store = &refusingLaunchWrites{memStore: h.store, refuse: 1} },
				before: func(r *byIDRig) {
					_ = r.handle(RequestSpawn{Claim: testToken})
				},
				trigger: func(r *byIDRig) { r.advance(testProbe) },
			},
			{
				name:  "after a hold the store did not answer",
				claim: byIDClaim(StateQueued, 2),
				before: func(r *byIDRig) {
					r.store.fail("HoldSessions", errBoom)
					_ = r.handle(RequestSpawn{Claim: testToken})
					r.store.fail("HoldSessions", nil)
				},
				trigger: func(r *byIDRig) { r.advance(testProbe) },
			},
		}},
		{"the relaunch of a process whose addresses went stale before it registered", []path{{
			claim:   byIDClaim(StateLaunching, 3),
			trigger: func(r *byIDRig) { r.observe(runtime.StaleAddress) },
		}}},
		{"the relaunch of a process displaced before it registered", []path{{
			claim:   byIDClaim(StateLaunching, 3),
			trigger: func(r *byIDRig) { r.observeDisplaced(runtime.Gone) },
		}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, p := range row.paths {
				if p.name == "" {
					run(t, p)
					continue
				}
				t.Run(p.name, func(t *testing.T) { run(t, p) })
			}
		})
	}
}

// R1's first way: a claim whose record kept the session id and lost its file — a close of an
// earlier release cleared the file, and the issue was then opened again — is resumed by that id
// where the session store holds the session. One Resume names the session and no file, no fresh
// agent is spawned, nothing is told lost and nothing is charged. The agent that comes up is the
// session, on the file the store holds it in: its registration is accepted, and the claim, the
// store and the hold table record the session and that file again, the file's names held to the
// claim as every registration holds them (RecordedHolds).
func TestAClaimWithASessionAndNoFileResumesTheSessionItsStoreHolds(t *testing.T) {
	r := newByIDRig(t, byIDClaim(StateRetired, 3), nil)

	r.must(RequestRetry{Claim: testToken})

	r.wantState(StateLaunching)
	r.wantNoSpawnWhileRecorded()
	if starts := r.starts(0); len(starts) != 1 || !resumedByID(starts[0]) {
		t.Fatalf("the reopened claim's launch made %s; want one Resume of session %s by its id, with no file, and no Spawn", startsText(starts), idSession)
	}
	r.wantNothingLost()
	r.wantBudgets(Budgets{})

	r.connect()
	if err := r.registerAs(idSession, idSessionFile, r.deps.PluginContract); err != nil {
		t.Fatalf("the resumed agent's registration as %s on %s = %v; want it accepted", idSession, idSessionFile, err)
	}

	r.wantState(StateRegistered)
	if c := r.claim(); c.Session != idSession || c.SessionFile != idSessionFile {
		t.Errorf("the claim records session %q and file %q; want %s and %s", c.Session, c.SessionFile, idSession, idSessionFile)
	}
	if stored := r.store.load(testToken); stored.Session != idSession || stored.SessionFile != idSessionFile {
		t.Errorf("the store holds session %q and file %q; want %s and %s", stored.Session, stored.SessionFile, idSession, idSessionFile)
	}
	for _, h := range []Hold{
		{Kind: HoldSessionID, Name: idSession},
		{Kind: HoldFileName, Name: filepath.Base(idSessionFile)},
		{Kind: HoldFileID, Name: idSession},
	} {
		if holder, held := r.store.holdOf(h); !held || holder != testToken {
			t.Errorf("the hold table holds the %s name %q for %q (held %t); want it held for %s", h.Kind, h.Name, holder, held, testToken)
		}
	}
	r.wantNothingLost()
	r.wantBudgets(Budgets{})
}

// R1's second way: where no store holds the session, the Resume by its id answers
// runtime.ErrSessionFileMissing, and the same try forgets the session and spawns one fresh agent,
// charged nothing. The fresh agent is told in its own prompt (Claim.SessionLost), the workflow is
// told once, and the fresh agent's registration as a session of its own is accepted, since the
// claim records no session by then. No Spawn runs while the claim still records the session.
func TestAClaimWithASessionAndNoFileWhoseStoreHoldsNoneForgetsItOnce(t *testing.T) {
	r := newByIDRig(t, byIDClaim(StateRetired, 3), nil)
	r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})

	r.must(RequestRetry{Claim: testToken})

	r.wantState(StateLaunching)
	r.wantNoSpawnWhileRecorded()
	starts := r.starts(0)
	if len(starts) != 2 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" {
		t.Fatalf("the launch made %s; want a Resume of session %s by its id, then one Spawn", startsText(starts), idSession)
	}
	if spawned := starts[1].Spec; spawned.ResumeSession != "" || spawned.ResumeSessionFile != "" || spawned.ResumeByAgent {
		t.Errorf("the fresh spawn names session %q, file %q and ResumeByAgent %t; want it to resume nothing", spawned.ResumeSession, spawned.ResumeSessionFile, spawned.ResumeByAgent)
	}
	if built := r.specs.last(t); built.SessionLost != SessionLossGone || built.Session != "" {
		t.Errorf("the fresh spawn was built for a claim recording session %q with SessionLost %q; want no session and %q", built.Session, built.SessionLost, SessionLossGone)
	}
	if c := r.claim(); c.Abandoned.Session != idSession || c.Session != "" || c.SessionFile != "" {
		t.Errorf("the claim abandoned %q and records session %q and file %q; want %s abandoned and no session", c.Abandoned.Session, c.Session, c.SessionFile, idSession)
	}
	if lost := r.wantLost(SessionLossGone); !strings.Contains(lost.detail, idSession) {
		t.Errorf("the workflow was told %q; want the forget to name session %s", lost.detail, idSession)
	}
	r.wantBudgets(Budgets{})

	r.connect()
	if err := r.registerAs(newSession, newSessionFile, r.deps.PluginContract); err != nil {
		t.Fatalf("the fresh agent's registration as %s = %v; want it accepted", newSession, err)
	}

	r.wantState(StateRegistered)
	if c := r.claim(); c.Session != newSession || c.SessionFile != newSessionFile {
		t.Errorf("the claim records session %q and file %q; want the fresh agent's %s and %s", c.Session, c.SessionFile, newSession, newSessionFile)
	}
	r.wantLost(SessionLossGone)
	r.wantBudgets(Budgets{})
}

// The forget is told once, at the forget and before the fresh agent's spawn, so a first fresh spawn
// that fails still leaves the issue, its architect and the controller told. The spawn's failure is
// the one charge, as any refused start's is, and the retry's spawn is built with the same cause, so
// the agent that does start is told in its own prompt that it does not continue the session.
func TestAForgottenSessionIsReportedOnceWhenTheFirstFreshSpawnFails(t *testing.T) {
	r := newByIDRig(t, byIDClaim(StateQueued, 2), nil)
	r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})
	r.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom})

	r.must(RequestSpawn{Claim: testToken})

	r.wantState(StateLaunching)
	r.wantNoSpawnWhileRecorded()
	starts := r.starts(0)
	if len(starts) != 3 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" || starts[2].Method != "Spawn" {
		t.Fatalf("the launch made %s; want a Resume of session %s by its id, the fresh Spawn the runtime refused, and its retry", startsText(starts), idSession)
	}
	if lost := r.wantLost(SessionLossGone); lost.spawns != 0 {
		t.Errorf("the forget was told after %d Spawns; want it told at the forget, before the fresh agent's first Spawn", lost.spawns)
	}
	built := r.specs.last(t)
	if built.Generation != starts[2].Spec.Generation {
		t.Fatalf("the last spec was built at generation %d; want the retry's, at %d", built.Generation, starts[2].Spec.Generation)
	}
	if built.SessionLost != SessionLossGone {
		t.Errorf("the retry's spawn was built with SessionLost %q; want %q, so the agent that starts is told", built.SessionLost, SessionLossGone)
	}
	r.wantBudgets(Budgets{LaunchFailures: 1})
}

// The cause of a forget is kept for the fresh agent's launches until its first ready, and no
// longer than the next forget that drops a session: an operator who forgets the fresh agent's own
// session before that ready (`legion claims forget-session`) leaves a claim whose next launch
// follows that forget, not the launch's. So the spawn after it is built with no cause, and the
// agent it starts is told nothing of the first forget's cause, which says something of a session
// that is no longer the one it replaces.
func TestAnOperatorForgetBeforeTheFreshAgentIsReadyClearsTheForgetsCause(t *testing.T) {
	r := newByIDRig(t, byIDClaim(StateRetired, 3), nil)
	r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})
	r.must(RequestRetry{Claim: testToken})
	if built := r.specs.last(t); built.SessionLost != SessionLossGone {
		t.Fatalf("the fresh spawn after the forget was built with SessionLost %q; want %q", built.SessionLost, SessionLossGone)
	}
	r.connect()
	if err := r.registerAs(newSession, newSessionFile, r.deps.PluginContract); err != nil {
		t.Fatalf("the fresh agent's registration as %s = %v; want it accepted", newSession, err)
	}
	r.wantState(StateRegistered)

	r.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator})
	r.must(RequestForgetSession{Claim: testToken, Session: newSession})
	if c := r.claim(); c.Abandoned.Session != newSession || c.Session != "" || c.SessionFile != "" {
		t.Fatalf("the claim abandoned %q and records session %q and file %q after the operator's forget; want %s abandoned and no session",
			c.Abandoned.Session, c.Session, c.SessionFile, newSession)
	}
	from := len(r.rt.Calls())
	r.must(RequestResume{Claim: testToken})

	starts := r.starts(from)
	if len(starts) != 1 || starts[0].Method != "Spawn" {
		t.Fatalf("the resume after the operator's forget made %s; want one Spawn", startsText(starts))
	}
	if built := r.specs.last(t); built.Generation != starts[0].Spec.Generation || built.SessionLost != "" {
		t.Errorf("the spawn after the operator's forget was built at generation %d with SessionLost %q; want the spawn's, at %d, with no cause",
			built.Generation, built.SessionLost, starts[0].Spec.Generation)
	}
	r.wantLost(SessionLossGone)
}

// A forget that drops nothing keeps the cause of the forget before it (Machine.lostCause). Where
// every fresh spawn after a forget fails until the launch budget runs out, the claim fails
// recording no session, and a retry in a new session — the controller's fresh_session, or
// `legion claims retry --fresh-session` — drops nothing: the session the claim abandoned is still
// the one the cause is about. So the spawn of that retry is built with the cause, and the agent it
// starts is told in its own prompt that it does not continue that session.
func TestAFreshSessionRetryBeforeTheFreshAgentRegistersKeepsTheForgetsCause(t *testing.T) {
	r := newByIDRig(t, byIDClaim(StateRetired, 3), nil)
	r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})
	r.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom})
	if err := r.handle(RequestRetry{Claim: testToken}); !errors.Is(err, errBoom) {
		t.Fatalf("the retry whose fresh spawns the runtime refused = %v; want the runtime's last refusal", err)
	}
	r.wantState(StateFailed)
	if c := r.claim(); c.Abandoned.Session != idSession || c.Session != "" || c.SessionFile != "" {
		t.Fatalf("the failed claim abandoned %q and records session %q and file %q; want %s abandoned and no session",
			c.Abandoned.Session, c.Session, c.SessionFile, idSession)
	}
	from := len(r.rt.Calls())

	r.must(RequestRetry{Claim: testToken, FreshSession: true})

	starts := r.starts(from)
	if len(starts) != 1 || starts[0].Method != "Spawn" {
		t.Fatalf("the retry in a new session made %s; want one Spawn", startsText(starts))
	}
	if built := r.specs.last(t); built.Generation != starts[0].Spec.Generation || built.SessionLost != SessionLossGone {
		t.Errorf("the spawn of the retry in a new session was built at generation %d with SessionLost %q; want the spawn's, at %d, with the forget's cause %q",
			built.Generation, built.SessionLost, starts[0].Spec.Generation, SessionLossGone)
	}
	if c := r.claim(); c.Abandoned.Session != idSession {
		t.Errorf("the claim abandoned %q after the retry in a new session; want %s still", c.Abandoned.Session, idSession)
	}
	r.wantLost(SessionLossGone)
}

// A Claude Code claim that records a session and no file has no transcript to resume, and the
// launch asks for no lookup by the id (the architect's answer D1): it forgets the session at once,
// with the cause claude-no-file, spawns one fresh agent told so in its own prompt, tells the
// workflow once, and charges nothing.
func TestAClaudeCodeClaimWithASessionAndNoFileForgetsItWithoutALookup(t *testing.T) {
	r := newByIDRig(t, byIDClaim(StateQueued, 2), func(h *harness) { h.deps.Specs = claudeSpecs{h.specs} })

	r.must(RequestSpawn{Claim: testToken})

	r.wantState(StateLaunching)
	r.wantNoSpawnWhileRecorded()
	starts := r.starts(0)
	if len(starts) != 1 || starts[0].Method != "Spawn" {
		t.Fatalf("the Claude Code claim's launch made %s; want one Spawn and no Resume", startsText(starts))
	}
	if spawned := starts[0].Spec; runtime.Of(spawned.Backend) != runtime.BackendClaude || spawned.ResumeSession != "" || spawned.ResumeSessionFile != "" {
		t.Errorf("the spawn is on %q, naming session %q and file %q; want a fresh Claude Code spawn that resumes nothing", spawned.Backend, spawned.ResumeSession, spawned.ResumeSessionFile)
	}
	if built := r.specs.last(t); built.SessionLost != SessionLossClaudeNoFile {
		t.Errorf("the fresh spawn was built with SessionLost %q; want %q", built.SessionLost, SessionLossClaudeNoFile)
	}
	if c := r.claim(); c.Abandoned.Session != idSession || c.Session != "" {
		t.Errorf("the claim abandoned %q and records session %q; want %s abandoned and no session", c.Abandoned.Session, c.Session, idSession)
	}
	r.wantLost(SessionLossClaudeNoFile)
	r.wantBudgets(Budgets{})
}

// A resume by the session id runs under the id's names, so both are held to the claim before
// anything starts, as a resume of a file holds the file's (holdRecorded): the session id, and the
// id an Oh My Pi file `<timestamp>_<id>.jsonl` gives (HoldFileID), under which the lookup by the id
// finds the file. Another claim holding the file-id name refuses the launch, charged as a hold
// refusal of a file is, so the claim fails at its launch budget with no start reaching the runtime;
// and the operator's resume of such a suspended claim is refused before anything changes
// (resumeRefusal). The rows of a claim that records the file are today's, beside which the rows of
// a claim that records the id alone are held.
func TestAResumeByIDHoldsTheSessionAndItsFileID(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	records := []struct{ name, file string }{
		{"a claim that records the session's file", idSessionFile},
		{"a claim that records the session's id alone", ""},
	}
	t.Run("the launch", func(t *testing.T) {
		for _, rec := range records {
			t.Run(rec.name, func(t *testing.T) {
				c := byIDClaim(StateQueued, 2)
				c.SessionFile = rec.file
				r := newByIDRig(t, c, nil)
				r.store.setHold(Hold{Kind: HoldFileID, Name: idSession}, other)

				if err := r.handle(RequestSpawn{Claim: testToken}); err != nil {
					var refusal claim.Refusal
					if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(idSession) {
						t.Fatalf("the launch = %v; want nil or %v", err, claim.SessionHeldRefusal(idSession))
					}
				}

				if starts := r.starts(0); len(starts) != 0 {
					t.Fatalf("the runtime was asked for %s; want no start under a name %s holds", startsText(starts), other)
				}
				limit := testLimits().LaunchFailures
				if c := r.claim(); c.State != StateFailed || c.Budgets.LaunchFailures != limit || !strings.Contains(c.Failure, "recorded for another claim") {
					t.Fatalf("the claim is %s with %d launch failures (%q); want it failed at its launch budget of %d by the hold's refusal",
						c.State, c.Budgets.LaunchFailures, c.Failure, limit)
				}
			})
		}
	})
	t.Run("the operator's resume", func(t *testing.T) {
		for _, rec := range records {
			t.Run(rec.name, func(t *testing.T) {
				c := byIDClaim(StateSuspended, 3)
				c.SessionFile = rec.file
				r := newByIDRig(t, c, nil)
				r.store.setHold(Hold{Kind: HoldFileID, Name: idSession}, other)

				err := r.handle(RequestResume{Claim: testToken})

				wantRefusedResume(t, r.harness, err)
				if want := "legion claims forget-session --claim " + string(testToken); !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
				if starts := r.starts(0); len(starts) != 0 {
					t.Errorf("the runtime was asked for %s; want nothing started", startsText(starts))
				}
			})
		}
	})
}
