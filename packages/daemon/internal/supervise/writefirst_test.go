package supervise

// A decision of the daemon that ends a claim's process writes the claim's new state first, and
// stops or releases the process, revokes the agent's capability and stops its pod-session
// registration only once that write landed or failed owed (R3 of dispatch://INFRA-4099's spec, D6
// and D7 of its plan). A refused write acts on nothing; a write the window after a refusal holds
// back acts once a later write lands (D5).

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// stopCounter is a Registrar that counts the pod-session registrations a machine starts and the
// stops of each.
type stopCounter struct {
	mu               sync.Mutex
	started, stopped int
}

func (r *stopCounter) KeepPodSession(context.Context, PodSession) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started++
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.stopped++
	}, nil
}

func (*stopCounter) ReleasePodSessions(context.Context, []string) error { return nil }

func (r *stopCounter) counts() (started, stopped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, r.stopped
}

// failWith makes err the answer of every claim write the store refuses from now on (refuseWhere):
// a refusal of the row, or a failure the rule owes.
func (s *refusingStore) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// recordFirst is a machine over a claim in a refusingStore, with a broker that records the
// revocations of the agent's capability and a registrar that counts the stops of the pod-session
// registration, its process in an issue pod so that its session is registered for the pod to
// trade.
type recordFirst struct {
	*harness
	s         *refusingStore
	broker    *fakeEnroller
	registrar *stopCounter
	pause     *pauseFlag
}

func recordFirstHarness(t *testing.T, c Claim) *recordFirst {
	t.Helper()
	r := &recordFirst{registrar: &stopCounter{}, pause: &pauseFlag{}}
	r.harness, r.s = refusingHarness(t, c, func(h *harness) {
		r.broker = &fakeEnroller{now: h.clock.Now}
		h.deps.Secrets, h.deps.Registrar, h.deps.Paused = r.broker, r.registrar, r.pause.Paused
	})
	return r
}

// inPod is the process of the claim's launch at generation in the issue pod pod-uid-4099.
func inPod(h *harness, generation uint64) fake.SpawnResult {
	const pod = "pod-uid-4099"
	return fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, generation),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: generation},
	}}
}

// reachEnrolled walks the claim to state, its process in the issue pod, its agent's capability
// enrolled at the broker and its session registered for the pod: launched, the shim's hello with
// the pod's identity, registered, ready, and through a task for working and idle, as reach walks
// it. A claim that records a session is enrolled and registered at its hello, so it can stop
// short of a registration (StateShimConnected).
func (r *recordFirst) reachEnrolled(state ClaimState) {
	h := r.harness
	h.t.Helper()
	h.rt.ScriptSpawn(inPod(h, h.generation()+1))
	h.rt.ScriptResume(inPod(h, h.generation()+1))
	h.launch()
	h.helloWithIdentity()
	steps := []ClaimState{StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle}
	for _, step := range steps {
		switch step {
		case StateRegistered:
			h.register()
		case StateReady:
			h.ready()
		case StateWorking:
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
			h.must(StreamTurnStart{Claim: h.token})
		case StateIdle:
			h.must(StreamTurnEnd{Claim: h.token})
		}
		h.wantState(step)
		if step == state {
			break
		}
	}
	if c := h.claim(); c.Enrollment == nil {
		h.t.Fatalf("control: the claim reached %s holding no enrollment", h.state())
	}
	if started, _ := r.registrar.counts(); started != 1 {
		h.t.Fatalf("control: %d registrations of the pod's session were started, want 1", started)
	}
}

// acted is what the decisions so far asked of the world outside the machine: the runtime's stops
// and releases, the broker's revocations and the registrar's stops.
type acted struct{ suspends, releases, revocations, registrationStops int }

// wantMemoryIsTheRow is the harness's for a process in an issue pod: the pods an agent registered
// in are memory's alone, which the rebuild keeps (D3 of dispatch://INFRA-4099's plan), so they are
// left out of the claims compared.
func (r *recordFirst) wantMemoryIsTheRow() {
	r.t.Helper()
	fresh, quiet := r.rowMachine()
	defer quiet()
	want := fresh.Claim()
	want.RegisteredPods = RegisteredPods{}
	for what, got := range map[string]Claim{"Claim": r.claim(), "View": r.m.View().Claim} {
		got.RegisteredPods = RegisteredPods{}
		if !reflect.DeepEqual(got, want) {
			r.t.Fatalf("%s answers\n%+v\nwant the claim of the row\n%+v", what, got, want)
		}
	}
}

func (r *recordFirst) acted() acted {
	_, stopped := r.registrar.counts()
	return acted{suspends: len(r.rt.CallsOf("Suspend")), releases: len(r.rt.CallsOf("Release")),
		revocations: len(r.broker.revocations()), registrationStops: stopped}
}

// since is what the decisions asked of the world after before was read.
func (a acted) since(before acted) acted {
	return acted{a.suspends - before.suspends, a.releases - before.releases,
		a.revocations - before.revocations, a.registrationStops - before.registrationStops}
}

// staleTimer is an event every state decides by its fence, after the head of the decision has
// made the write the machine owes (repair): a timer no arming of this machine made.
func (h *harness) staleTimer() error {
	return h.handle(Timer{Claim: h.token, Kind: TimerProbe, Generation: h.generation(), Seq: 1 << 40})
}

// daemonDecision is one row of D6 of dispatch://INFRA-4099's plan: a decision of the daemon that
// ends the claim's process, brought to the moment before it (before), the claim writes that are
// its record (record), the decision itself (decide), the state its record writes (to), and what
// it asks of the world once its record is written or owed (acts).
type daemonDecision struct {
	name   string
	claim  func() Claim
	before func(r *recordFirst)
	record func(method string, c Claim) bool
	decide func(r *recordFirst) error
	to     ClaimState
	acts   acted
}

// writesState picks the claim writes of a claim in state.
func writesState(state ClaimState) func(string, Claim) bool {
	return func(_ string, c Claim) bool { return c.State == state }
}

// daemonDecisions is the decisions of D6, one row each, with the failing branches of a prompt
// retirement and of a registration deadline.
func daemonDecisions() []daemonDecision {
	withBudgets := func(b Budgets) func() Claim {
		return func() Claim {
			c := queuedClaim()
			c.Budgets = b
			return c
		}
	}
	recordsSession := func() Claim {
		c := queuedClaim()
		c.Session, c.SessionFile, c.Generation = session, sessionFile, 1
		return c
	}
	live := acted{suspends: 1, revocations: 1, registrationStops: 1}
	released := acted{releases: 1, revocations: 1, registrationStops: 1}
	return []daemonDecision{
		{
			name: "the suspension of a live process: an operator's suspension of an idle claim", claim: queuedClaim,
			before: func(r *recordFirst) { r.reachEnrolled(StateIdle) },
			record: writesState(StateSuspended),
			decide: func(r *recordFirst) error {
				return r.handle(RequestSuspend{Claim: r.token, By: SuspendedByOperator})
			},
			to: StateSuspended, acts: live,
		},
		{
			name: "the suspension of a live process: a held suspension at its turn's end", claim: queuedClaim,
			before: func(r *recordFirst) {
				r.reachEnrolled(StateWorking)
				if err := r.handle(RequestSuspend{Claim: r.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
					r.t.Fatalf("control: the suspension of a working claim answered %v, want it held", err)
				}
			},
			record: writesState(StateSuspended),
			decide: func(r *recordFirst) error { return r.handleQuiet(StreamTurnEnd{Claim: r.token}) },
			to:     StateSuspended, acts: live,
		},
		{
			name: "the suspension over a process found gone: a death under a held suspension", claim: queuedClaim,
			before: func(r *recordFirst) {
				r.reachEnrolled(StateWorking)
				if err := r.handle(RequestSuspend{Claim: r.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
					r.t.Fatalf("control: the suspension of a working claim answered %v, want it held", err)
				}
			},
			record: writesState(StateSuspended),
			decide: func(r *recordFirst) error {
				return r.handle(RuntimeObservation{Observation: runtime.Observation{Locator: r.locator(), Kind: runtime.Gone, At: r.clock.Now()}})
			},
			to: StateSuspended, acts: live,
		},
		{
			name: "the suspension with no process: a waiting relaunch that a held suspension ends", claim: queuedClaim,
			before: func(r *recordFirst) {
				// The launching write fails owed, so the launch waits for the probe interval with
				// no process, and the suspension is held for it.
				r.s.failWith(brokenConnection())
				r.s.refuseWhere(writesState(StateLaunching))
				_ = r.handle(RequestSpawn{Claim: r.token})
				r.wantState(StateLaunching)
				if err := r.handle(RequestSuspend{Claim: r.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
					r.t.Fatalf("control: the suspension of a launch waiting with no process answered %v, want it held", err)
				}
				r.s.refuseWhere(nil)
				r.staleTimer() // the owed launching write is made, so the row holds the launch
			},
			record: writesState(StateSuspended),
			decide: func(r *recordFirst) error { return r.fireNow(TimerRelaunch) },
			to:     StateSuspended,
		},
		{
			name: "a tree root's exit", claim: rootClaim,
			before: func(r *recordFirst) { r.reachEnrolled(StateReady) },
			record: writesState(StateSuspended),
			decide: func(r *recordFirst) error {
				return r.handle(RequestExit{Claim: r.token, Generation: r.generation(), Session: session, Reason: "done"})
			},
			to: StateSuspended, acts: live,
		},
		{
			name: "a stop: the operator's stop of an idle claim", claim: queuedClaim,
			before: func(r *recordFirst) { r.reachEnrolled(StateIdle) },
			record: writesState(StateRetired),
			decide: func(r *recordFirst) error { return r.handle(RequestStop{Claim: r.token, By: SuspendedByOperator}) },
			to:     StateRetired, acts: released,
		},
		{
			name: "a close: the workflow's close of an issue as done", claim: queuedClaim,
			before: func(r *recordFirst) { r.reachEnrolled(StateIdle) },
			record: writesState(StateRetired),
			decide: func(r *recordFirst) error { return r.handle(RequestIssueClose{Claim: r.token}) },
			to:     StateRetired, acts: released,
		},
		{
			name: "a worker's exit", claim: queuedClaim,
			before: func(r *recordFirst) { r.reachEnrolled(StateIdle) },
			record: writesState(StateRetired),
			decide: func(r *recordFirst) error {
				return r.handle(RequestExit{Claim: r.token, Generation: r.generation(), Session: session, Reason: "done"})
			},
			to: StateRetired, acts: released,
		},
		{
			name: "a failure: the death that spends the deaths", claim: withBudgets(Budgets{Deaths: 2}),
			before: func(r *recordFirst) { r.reachEnrolled(StateWorking) },
			record: writesState(StateFailed),
			decide: func(r *recordFirst) error {
				return r.handle(RuntimeObservation{Observation: runtime.Observation{Locator: r.locator(), Kind: runtime.Gone, At: r.clock.Now()}})
			},
			to: StateFailed, acts: live,
		},
		{
			name: "a failure at the last prompt retirement", claim: withBudgets(Budgets{PromptFailures: 2, PromptRetires: 1}),
			before: func(r *recordFirst) {
				r.reachEnrolled(StateReady)
				r.must(RequestDeliver{Claim: r.token, Task: "implement the plan"})
			},
			record: writesState(StateFailed),
			decide: func(r *recordFirst) error { return r.fireNow(TimerTurn) },
			to:     StateFailed, acts: live,
		},
		{
			name: "a failure at the registration deadline with the launch budget spent", claim: func() Claim {
				c := recordsSession()
				c.Budgets.LaunchFailures = 2
				return c
			},
			before: func(r *recordFirst) { r.reachEnrolled(StateShimConnected) },
			record: writesState(StateFailed),
			decide: func(r *recordFirst) error { return r.fireNow(TimerRegistration) },
			to:     StateFailed, acts: live,
		},
		{
			name: "a project pause's stop of an idle claim", claim: queuedClaim,
			before: func(r *recordFirst) {
				r.reachEnrolled(StateIdle)
				r.pause.set(7)
			},
			record: func(method string, c Claim) bool { return method == "PauseStopped" },
			decide: func(r *recordFirst) error { return r.handle(pauseStop(r.token, 7)) },
			to:     StateSuspended, acts: live,
		},
	}
}

// fireNow decides the timer of kind the machine holds armed, as its firing would, and answers the
// decision's error, which a firing logs rather than returns (fire). The timer's own firing later
// is fenced out: the decision took it off the machine.
func (h *harness) fireNow(kind TimerKind) error {
	h.t.Helper()
	h.m.mu.Lock()
	a, armed := h.m.timers[kind]
	generation := h.m.claim.Generation
	h.m.mu.Unlock()
	if !armed {
		h.t.Fatalf("control: no %s timer is armed", kind)
	}
	return h.handle(Timer{Claim: h.token, Kind: kind, Generation: generation, Seq: a.seq, DeliveryID: a.deliveryID})
}

// A decision of the daemon whose record the store refuses acts on nothing (c-daemon-decisions of
// dispatch://INFRA-4099's verification): no stop and no release of the process, no revocation of
// the agent's capability and no stop of its session's registration, memory and the row as they
// were before the decision, and the decision answers the store's refusal.
func TestADaemonDecisionWhoseRecordIsRefusedActsOnNothing(t *testing.T) {
	for _, tc := range daemonDecisions() {
		t.Run(tc.name, func(t *testing.T) {
			r := recordFirstHarness(t, tc.claim())
			tc.before(r)
			r.s.failWith(checkRefusal())
			before, memory, row := r.acted(), r.state(), r.store.load(r.token).State
			r.s.refuseWhere(tc.record)

			err := tc.decide(r)

			if !errors.Is(err, r.s.err) {
				t.Fatalf("the decision answered %v, want the store's refusal", err)
			}
			if got := r.acted().since(before); got != (acted{}) {
				t.Errorf("the refused decision asked the world for %+v, want nothing", got)
			}
			if got, stored := r.state(), r.store.load(r.token).State; got != memory || stored != row {
				t.Errorf("after the refused decision memory reads %s and the row %s, want %s and %s as before it", got, stored, memory, row)
			}
			r.wantMemoryIsTheRow()
		})
	}
}

// A decision of the daemon whose record fails owed acts, and the write stays owed: the claim's next
// decision writes the row (R3 of dispatch://INFRA-4099's spec).
func TestADaemonDecisionWhoseRecordIsOwedActsAndTheRepairWritesIt(t *testing.T) {
	for _, tc := range daemonDecisions() {
		t.Run(tc.name, func(t *testing.T) {
			r := recordFirstHarness(t, tc.claim())
			tc.before(r)
			r.s.failWith(brokenConnection())
			before, row := r.acted(), r.store.load(r.token).State
			r.s.refuseWhere(tc.record)

			err := tc.decide(r)

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != brokenConnection().Code {
				t.Fatalf("the decision answered %v, want the store's failure", err)
			}
			if got := r.acted().since(before); got != tc.acts {
				t.Errorf("the decision whose record is owed asked the world for %+v, want %+v", got, tc.acts)
			}
			if got, stored := r.state(), r.store.load(r.token).State; got != tc.to || stored != row {
				t.Fatalf("memory reads %s and the row %s, want %s in memory and the row %s it held", got, stored, tc.to, row)
			}
			r.m.mu.Lock()
			owed := r.m.owedWrite != nil
			r.m.mu.Unlock()
			if !owed {
				t.Fatal("the failed record is not owed")
			}

			r.s.refuseWhere(nil)
			_ = r.staleTimer()
			if stored := r.store.load(r.token).State; stored != tc.to {
				t.Fatalf("after the claim's next decision the row reads %s, want %s", stored, tc.to)
			}
			if got := r.acted().since(before); got != tc.acts {
				t.Errorf("the repair asked the world for %+v in all, want the %+v the decision asked once", got, tc.acts)
			}
		})
	}
}

// An operator's suspension of an idle claim whose claim write the server refuses leaves the claim
// as the store holds it, in memory too, and its process running, its capability and its
// registration standing (c-suspend-refused of dispatch://INFRA-4099's verification, the shape of
// INFRA-3577's N1). A machine a daemon start builds over the row reads the same. Once the store
// takes writes and the window after the refusal has ended, a second suspension is written.
func TestASuspendWhoseWriteIsRefusedLeavesTheClaimAsStored(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	if h.claim().Enrollment == nil {
		t.Fatal("control: the idle claim holds no enrollment")
	}
	process := h.locator()

	refusal := &pgconn.PgError{Code: "23514", Message: `new row for relation "claims" violates check constraint "claims_suspended_by_check"`,
		Severity: "ERROR", ConstraintName: "claims_suspended_by_check"}
	h.store.fail("PutClaim", refusal)
	err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	if !errors.Is(err, refusal) {
		t.Fatalf("the refused suspension answered %v, want the server's refusal", err)
	}
	if got, stored := h.state(), h.store.load(h.token).State; got != StateIdle || stored != StateIdle {
		t.Fatalf("after the refused suspension memory reads %s and the row %s, want both idle", got, stored)
	}
	if suspends := h.rt.CallsOf("Suspend"); len(suspends) != 0 {
		t.Fatalf("the runtime was asked to stop %+v, want the process left running", suspends)
	}
	if revoked := broker.revocations(); len(revoked) != 0 {
		t.Fatalf("the broker revoked %v, want the agent's capability standing", revoked)
	}
	if loc := h.claim().Locator; loc == nil || *loc != process {
		t.Fatalf("the claim records the process %+v, want %+v", loc, process)
	}
	fresh, quiet := h.rowMachine()
	if got := fresh.Claim().State; got != StateIdle {
		quiet()
		t.Fatalf("a machine built from the store reads %s, want idle", got)
	}
	quiet()
	h.wantMemoryIsTheRow()

	h.store.fail("PutClaim", nil)
	h.advance(testProbe)
	if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); err != nil {
		t.Fatalf("the second suspension answered %v", err)
	}
	if got, stored := h.state(), h.store.load(h.token).State; got != StateSuspended || stored != StateSuspended {
		t.Fatalf("after the second suspension memory reads %s and the row %s, want both suspended", got, stored)
	}
	if suspends := h.rt.CallsOf("Suspend"); len(suspends) != 1 || suspends[0].Locator != process {
		t.Fatalf("the runtime was asked to stop %+v, want the process stopped once", suspends)
	}
}

// An operator's stop of a claim that holds its update round's unanswered task writes the retired
// claim with the delete of that task in one transaction (c-retire-failed of dispatch://INFRA-4099's
// verification, the shape of INFRA-3577's F2). A failure the rule owes leaves the retirement
// owed, with the process released, and the claim's next decision writes the row retired with no
// delivery. A refusal leaves the claim as the store holds it, in memory too, the task held and the
// process not released, as a machine built from the store reads it.
func TestAStopWhoseRetireWriteFailsLeavesTheClaimAsStored(t *testing.T) {
	stopped := func(t *testing.T, fail error) (*harness, Delivery, error) {
		t.Helper()
		h := updateHarness(t, PodUpdateContract)
		h.reach(StateIdle)
		h.openUpdate(testRound, testReasons)
		ask := h.unsentAsk(testRound)
		h.store.fail("RetireDelivery", fail)
		err := h.handle(RequestStop{Claim: h.token, By: SuspendedByOperator})
		return h, ask, err
	}
	t.Run("owed", func(t *testing.T) {
		owed := brokenConnection()
		h, _, err := stopped(t, owed)
		if !errors.Is(err, owed) {
			t.Fatalf("the stop answered %v, want the store's failure", err)
		}
		h.wantState(StateRetired)
		if releases := h.rt.CallsOf("Release"); len(releases) != 1 {
			t.Fatalf("the runtime released %+v, want the process released once", releases)
		}
		h.m.mu.Lock()
		owing := h.m.owedWrite != nil && h.m.owedWrite.retire != ""
		h.m.mu.Unlock()
		if !owing {
			t.Fatal("the retirement is not owed")
		}
		if row := h.store.load(h.token); row.State != StateIdle {
			t.Fatalf("control: the failed write left the row %s, want idle", row.State)
		}

		h.store.fail("RetireDelivery", nil)
		if err := h.handle(RequestStop{Claim: h.token, By: SuspendedByOperator}); err != nil {
			t.Fatalf("the claim's next decision answered %v", err)
		}
		row := h.store.load(h.token)
		if row.State != StateRetired || row.Pending != nil {
			t.Fatalf("after the claim's next decision the row reads %s holding %+v, want retired with no delivery", row.State, row.Pending)
		}
		if releases := h.rt.CallsOf("Release"); len(releases) != 1 {
			t.Fatalf("the runtime released %+v, want the one release", releases)
		}
	})
	t.Run("refused", func(t *testing.T) {
		refused := checkRefusal()
		h, ask, err := stopped(t, refused)
		if !errors.Is(err, refused) {
			t.Fatalf("the stop answered %v, want the server's refusal", err)
		}
		for _, c := range []struct {
			what  string
			claim Claim
		}{{"memory", h.claim()}, {"the row", h.store.load(h.token)}} {
			if c.claim.State != StateIdle || c.claim.Pending == nil || c.claim.Pending.ID != ask.ID {
				t.Fatalf("%s reads %s holding %+v, want idle holding the round's task %s", c.what, c.claim.State, c.claim.Pending, ask.ID)
			}
		}
		if releases := h.rt.CallsOf("Release"); len(releases) != 0 {
			t.Fatalf("the runtime released %+v, want the process kept", releases)
		}
		fresh, quiet := h.rowMachine()
		defer quiet()
		if got := fresh.Claim(); got.State != StateIdle || got.Pending == nil || got.Pending.ID != ask.ID {
			t.Fatalf("a machine built from the store reads %s holding %+v, want idle with the task", got.State, got.Pending)
		}
		h.wantMemoryIsTheRow()
	})
}

// A stop or a release the runtime refuses once its record is written leaves the record standing:
// the decision answers no error, and the stop is made again at the probe interval until the
// runtime takes it (D7 of dispatch://INFRA-4099's plan).
func TestAStopTheRuntimeRefusesAfterItsRecordIsMadeAgainAtTheProbeInterval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		refuse func(h *harness, err error)
		decide Event
		to     ClaimState
	}{
		{"suspend", "Suspend", func(h *harness, err error) { h.rt.FailSuspend(err) },
			RequestSuspend{Claim: testToken, By: SuspendedByOperator}, StateSuspended},
		{"release", "Release", func(h *harness, err error) { h.rt.FailRelease(err) },
			RequestStop{Claim: testToken, By: SuspendedByOperator}, StateRetired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			process := h.locator()
			tc.refuse(h, errBoom)

			if err := h.handle(tc.decide); err != nil {
				t.Fatalf("the decision whose stop the runtime refused answered %v, want none: its record stands", err)
			}
			if got, stored := h.state(), h.store.load(h.token).State; got != tc.to || stored != tc.to {
				t.Fatalf("memory reads %s and the row %s, want both %s", got, stored, tc.to)
			}
			h.wantCalls(tc.method, 1)
			tc.refuse(h, nil)

			h.advance(testProbe - time.Nanosecond)
			h.wantCalls(tc.method, 1)
			h.advance(time.Nanosecond)
			calls := h.wantCalls(tc.method, 2)
			again := calls[1].Locator
			if tc.method == "Release" {
				if calls[1].Released.Locator == nil {
					t.Fatalf("the release was made again with no process: %+v", calls[1].Released)
				}
				again = *calls[1].Released.Locator
			}
			if again != process {
				t.Fatalf("the stop was made again of %+v, want the process the record ended, %+v", again, process)
			}
			h.advance(testProbe)
			h.wantCalls(tc.method, 2)
			h.wantState(tc.to)
		})
	}
}

// A suspension decided in the window after a refused claim write, while the claim's writes wait
// (D5 of dispatch://INFRA-4099's plan), stops nothing, revokes nothing and stops no registration
// until the first decision after the window makes its write; where that write is refused, the
// process is never stopped.
func TestADecisionInTheWindowAfterARefusalActsOnlyOnceItsRecordIsWritten(t *testing.T) {
	inTheWindow := func(t *testing.T) (*recordFirst, acted) {
		t.Helper()
		r := recordFirstHarness(t, queuedClaim())
		r.reachEnrolled(StateIdle)
		// A refused write opens the window: the queue of a task of the outbox's, written with
		// the claim in one transaction.
		r.s.refuseCalls("PutClaimAndDelivery")
		if err := r.handle(RequestDeliver{Claim: r.token, ID: "row-7", Task: "read the plan", Row: 7}); !errors.Is(err, r.s.err) {
			t.Fatalf("control: the refused queue answered %v, want the store's refusal", err)
		}
		r.s.refuseWhere(nil)
		before := r.acted()
		err := r.handle(RequestSuspend{Claim: r.token, By: SuspendedByOperator})
		if !errors.Is(err, errWritesWait) {
			t.Fatalf("the suspension in the window answered %v, want its write held", err)
		}
		r.wantState(StateSuspended)
		if got := r.acted().since(before); got != (acted{}) {
			t.Fatalf("the suspension in the window asked the world for %+v, want nothing until its record is written", got)
		}
		if stored := r.store.load(r.token).State; stored != StateIdle {
			t.Fatalf("the row reads %s in the window, want idle", stored)
		}
		return r, before
	}
	t.Run("the first decision after the window writes the record, and the suspension acts", func(t *testing.T) {
		r, before := inTheWindow(t)
		r.advance(testProbe)
		if got := r.acted().since(before); got != (acted{}) {
			t.Fatalf("the window's end asked the world for %+v before any decision wrote the record", got)
		}
		_ = r.staleTimer()
		if stored := r.store.load(r.token).State; stored != StateSuspended {
			t.Fatalf("after the first decision past the window the row reads %s, want suspended", stored)
		}
		if got, want := r.acted().since(before), (acted{suspends: 1, revocations: 1, registrationStops: 1}); got != want {
			t.Fatalf("once its record was written the suspension asked the world for %+v, want %+v", got, want)
		}
	})
	t.Run("the write after the window is refused, and nothing is ever stopped", func(t *testing.T) {
		r, before := inTheWindow(t)
		r.s.refuseWhere(writesState(StateSuspended))
		for range 3 {
			r.advance(testProbe)
			_ = r.staleTimer()
		}
		if got := r.acted().since(before); got != (acted{}) {
			t.Fatalf("a suspension whose record was refused asked the world for %+v, want nothing", got)
		}
		if got, stored := r.state(), r.store.load(r.token).State; got != StateIdle || stored != StateIdle {
			t.Fatalf("memory reads %s and the row %s, want both idle", got, stored)
		}
		r.wantMemoryIsTheRow()
	})
}
