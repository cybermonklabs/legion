package supervise

// A claim write the store refused makes memory the stored row again (R2 of dispatch://INFRA-4099's
// spec, D1 to D4 and D10 of its plan): the decision makes no other claim write, reads the claim's
// row and builds the machine again over it, as a daemon start builds one, and nothing reads what
// the refused decision left before that read.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// refusingStore is the store double of a refused claim write: it counts every one of the seven
// claim writes of R1 a decision asks for, the refused one included, and refuses each call that
// refuse picks with err, writing nothing, as a server that judged the row refuses it. It is the
// project pause's stop transaction too (PauseStopped, written as stopTransaction writes it), so
// every claim write is counted in one place. It counts the reads of the claim too (ReadClaim),
// which are memStore's, as is the rest of the store.
type refusingStore struct {
	*memStore
	err    error
	mu     sync.Mutex
	calls  []string
	reads  int
	refuse func(method string, c Claim) bool
}

// refusingHarness is a machine over c, which the store holds, whose store is a refusingStore that
// refuses with a CHECK the row breaks (checkRefusal). tune sets what else the case needs of the
// harness's Deps before the machine is built.
func refusingHarness(t *testing.T, c Claim, tune ...func(h *harness)) (*harness, *refusingStore) {
	t.Helper()
	h := newBareHarness(t)
	s := &refusingStore{memStore: h.store, err: checkRefusal()}
	h.deps.Store, h.deps.PauseStopped = s, s.PauseStopped
	for _, set := range tune {
		set(h)
	}
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, s
}

// refuseWhere refuses every claim write refuse picks from now on; nil refuses none.
func (s *refusingStore) refuseWhere(refuse func(method string, c Claim) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse = refuse
}

// refuseCalls refuses every call of method from now on.
func (s *refusingStore) refuseCalls(method string) {
	s.refuseWhere(func(asked string, _ Claim) bool { return asked == method })
}

// refusedCall marks a call the store answered with its refusal in the calls it counts (since).
const refusedCall = " (refused)"

// asked counts one claim write, and answers the refusal where it is refused.
func (s *refusingStore) asked(method string, c Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse != nil && s.refuse(method, c) {
		s.calls = append(s.calls, method+refusedCall)
		return s.err
	}
	s.calls = append(s.calls, method)
	return nil
}

// mark is how many claim writes have been asked for now (since).
func (s *refusingStore) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// since is every claim write asked for after mark, in order.
func (s *refusingStore) since(mark int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls[mark:])
}

func (s *refusingStore) PutClaim(ctx context.Context, c Claim) error {
	if err := s.asked("PutClaim", c); err != nil {
		return err
	}
	return s.memStore.PutClaim(ctx, c)
}

func (s *refusingStore) PutDelivery(ctx context.Context, token claim.Token, d Delivery) error {
	if err := s.asked("PutDelivery", Claim{Token: token}); err != nil {
		return err
	}
	return s.memStore.PutDelivery(ctx, token, d)
}

func (s *refusingStore) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	if err := s.asked("PutClaimAndDelivery", c); err != nil {
		return err
	}
	return s.memStore.PutClaimAndDelivery(ctx, c, d)
}

func (s *refusingStore) RetireDelivery(ctx context.Context, c Claim, deliveryID string) error {
	if err := s.asked("RetireDelivery", c); err != nil {
		return err
	}
	return s.memStore.RetireDelivery(ctx, c, deliveryID)
}

func (s *refusingStore) AdmitClaim(ctx context.Context, c Claim) (Claim, error) {
	if err := s.asked("AdmitClaim", c); err != nil {
		return Claim{}, err
	}
	return s.memStore.AdmitClaim(ctx, c)
}

func (s *refusingStore) ReleaseHolds(ctx context.Context, c Claim, holds []Hold) ([]Hold, error) {
	if err := s.asked("ReleaseHolds", c); err != nil {
		return nil, err
	}
	return s.memStore.ReleaseHolds(ctx, c, holds)
}

// PauseStopped is the project pause's stop transaction (Deps.PauseStopped), which writes the claim,
// and the delivery the claim carries, as the plain write would.
func (s *refusingStore) PauseStopped(ctx context.Context, c Claim, _ PauseStop) error {
	if err := s.asked("PauseStopped", c); err != nil {
		return err
	}
	if c.Pending != nil {
		return s.memStore.PutClaimAndDelivery(ctx, StoredClaim(c), *c.Pending)
	}
	return s.memStore.PutClaim(ctx, c)
}

// ReadClaim is memStore's read of the claim, counted (readCount).
func (s *refusingStore) ReadClaim(ctx context.Context, token claim.Token) (Claim, bool, error) {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
	return s.memStore.ReadClaim(ctx, token)
}

// readCount is how many times the claim has been read now.
func (s *refusingStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// rowMachine is the machine a daemon start builds over the claim's row (NewMachine over
// memStore.load), and the call that quiets it: the timers it arms are the harness clock's, and a
// test that moves the clock must not have them fire.
func (h *harness) rowMachine() (*Machine, func()) {
	h.t.Helper()
	fresh, err := NewMachine(h.ctx, h.deps, h.store.load(h.token))
	if err != nil {
		h.t.Fatalf("a machine over the row: %v", err)
	}
	return fresh, func() {
		fresh.mu.Lock()
		defer fresh.mu.Unlock()
		fresh.disarmAll()
	}
}

// wantMemoryIsTheRow is the machine holding the claim a daemon start reads off the row: its claim,
// as Claim and View answer it, is the claim of a machine NewMachine builds over that row.
func (h *harness) wantMemoryIsTheRow() {
	h.t.Helper()
	fresh, quiet := h.rowMachine()
	defer quiet()
	want := fresh.Claim()
	if got := h.claim(); !reflect.DeepEqual(got, want) {
		h.t.Fatalf("Claim answers\n%+v\nwant the claim of the row\n%+v", got, want)
	}
	if got := h.m.View().Claim; !reflect.DeepEqual(got, want) {
		h.t.Fatalf("View answers\n%+v\nwant the claim of the row\n%+v", got, want)
	}
}

// wantOneRefusalLast is the decision since mark having met one refusal of the store, at its last
// claim write, of the call method ("" for any): nothing after the refusal reached the store (R2).
func (s *refusingStore) wantOneRefusalLast(t *testing.T, mark int, method string) {
	t.Helper()
	asked := s.since(mark)
	refused := slices.IndexFunc(asked, func(call string) bool { return strings.HasSuffix(call, refusedCall) })
	if n := len(asked); refused < 0 || refused != n-1 || (method != "" && asked[n-1] != method+refusedCall) {
		t.Fatalf("the decision asked for %v; want one refused %q, the last claim write it made", asked, method)
	}
}

// Every claim write a machine makes goes through the rule, and a refusal of each takes the rebuild:
// one row for each of the seven calls R1 names, each refused with 23514. The decision asks for the
// refused write once and for no claim write after it, it answers the refusal, and memory is the
// row the store holds once it has ended.
func TestEveryClaimWriteOfTheMachinePassesTheRule(t *testing.T) {
	for _, tc := range []struct {
		method string
		// decide brings the claim to the decision, calls refuse right before it, and answers
		// what the decision returned.
		decide func(h *harness, pause *pauseFlag, refuse func()) error
	}{
		{"PutClaim", func(h *harness, _ *pauseFlag, refuse func()) error {
			h.reach(StateReady)
			refuse()
			return h.handle(StreamTurnStart{Claim: h.token})
		}},
		{"PutDelivery", func(h *harness, _ *pauseFlag, refuse func()) error {
			h.reach(StateReady)
			refuse()
			return h.handle(RequestDeliver{Claim: h.token, Task: "read the plan"})
		}},
		{"PutClaimAndDelivery", func(h *harness, _ *pauseFlag, refuse func()) error {
			h.reach(StateReady)
			refuse()
			return h.handle(RequestDeliver{Claim: h.token, ID: "row-7", Task: "read the plan", Row: 7})
		}},
		{"RetireDelivery", func(h *harness, _ *pauseFlag, refuse func()) error {
			h.reach(StateWorking)
			ran := h.promptInTurn()
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			refuse()
			return h.handleQuiet(StreamPromptResult{Claim: h.token, DeliveryID: ran, AgentInvoked: true})
		}},
		{"AdmitClaim", func(h *harness, _ *pauseFlag, refuse func()) error {
			h.reach(StateSuspended)
			refuse()
			return h.handle(RequestResume{Claim: h.token, By: SuspendedByOperator})
		}},
		{"ReleaseHolds", func(h *harness, _ *pauseFlag, refuse func()) error {
			h.reach(StateReady)
			h.store.fillHolds(h.token, 2)
			refuse()
			_, err := h.m.ReleaseNames(h.ctx)
			h.m.Wait()
			return err
		}},
		{"PauseStopped", func(h *harness, pause *pauseFlag, refuse func()) error {
			h.reach(StateIdle)
			pause.set(7)
			refuse()
			return h.handle(pauseStop(h.token, 7))
		}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			pause := &pauseFlag{}
			h, s := refusingHarness(t, queuedClaim(), func(h *harness) { h.deps.Paused = pause.Paused })
			mark := 0
			err := tc.decide(h, pause, func() {
				mark = s.mark()
				s.refuseCalls(tc.method)
			})
			if !errors.Is(err, s.err) {
				t.Fatalf("the decision returned %v, want the store's refusal of %s", err, tc.method)
			}
			s.wantOneRefusalLast(t, mark, tc.method)
			if h.m.owedWrite != nil {
				t.Fatal("a refused write left a write owed")
			}
			h.wantMemoryIsTheRow()
		})
	}
}

// built is what the rebuild does with a field it builds as a daemon start builds it (build):
// rebuildFields names it so, and TestARefusedWriteLeavesTheMachineAsADaemonStartBuildsIt compares
// it with the machine NewMachine builds over the row.
const built = "built"

// rebuildFields is D3's field table of dispatch://INFRA-4099's plan as a list: every field of
// Machine, built by the rebuild, or kept by it with its reason. A field in neither fails the test.
var rebuildFields = map[string]string{
	"mu":                    "kept: the lock the decision holds",
	"idle":                  "kept: the waiters of that lock",
	"decidingMu":            "kept: the lock of the decision's event",
	"deciding":              "kept: the event of the decision that holds the lock",
	"ctx":                   "kept: the value NewMachine was given",
	"deps":                  "kept: the value NewMachine was given",
	"log":                   "kept: the value NewMachine was given",
	"onDurableState":        "kept: the daemon installs it after NewMachine",
	"view":                  "kept: readers hold the pointer; the build publishes the rebuilt claim into it",
	"stopping":              "kept: true only inside a runtime call of the decision",
	"turnsEnded":            "kept: a count the daemon reads across decisions",
	"seq":                   "kept: a timer armed before the rebuild stays fenced out by its sequence",
	"goroutines":            "kept: sends and revocations still running report to it",
	"startMu":               "kept: a start's cancel belongs to the decision that holds mu",
	"cancelStart":           "kept: a start's cancel belongs to the decision that holds mu",
	"startStopped":          "kept: a start's cancel belongs to the decision that holds mu",
	"turnOutcomeDecided":    "kept: a test's hook",
	"turnOutcomeFailed":     "kept: HandleTurnOutcome reads it and clears it after the rebuild",
	"storeFaultTold":        "kept: the hour bound of a session store's notice",
	"sentThrough":           "kept: the stream's registration sequence goes on rising in this daemon's run",
	"answer":                "kept: what this daemon observed of the agent",
	"steppedAt":             "kept: what this daemon observed of the agent, which the stall backstop's fence reads",
	"openCalls":             "kept: what this daemon observed of the agent",
	"podPark":               "kept: settled by unlock against the rebuilt claim",
	"refreshCredit":         "kept: settled by unlock against the rebuilt claim",
	"queueTold":             "kept: settled by unlock against the rebuilt claim",
	"previous":              "kept: a process this daemon let go still runs in this runtime, but for the row's own",
	"stopUnconfirmed":       "kept: goes with previous",
	"registration":          "kept: where the row records its process, whose shim sends no second hello",
	"registrationRefused":   "kept: where the row records its process",
	"identity":              "kept: where the row records its process",
	"enrollmentSent":        "kept: where the row records its process",
	"helloKey":              "kept: where the row records its process",
	"podLoad":               "kept: where the row records its process, since a load has a bound once started",
	"askDelivered":          "kept: a publish of an update round's ask that happened and whose mark the store refused (D9)",
	"refusal":               "set by the rebuild: the state of the rule itself, cleared as the rebuild begins",
	"rereadOwed":            "set by the rebuild: the state of the rule itself, set while no read of the row has landed (D5)",
	"rereadDropped":         "set by the rebuild: the events a read owed dropped, named when the read lands (D5)",
	"writesWaitUntil":       "set by the rebuild: the state of the rule itself, the end of the window after a read that lands (D5)",
	"registeredPods":        built + ": the row's joined with memory's, and no fake process registers in a pod",
	"claim":                 built,
	"timers":                built,
	"send":                  built,
	"helloDuringSend":       built,
	"heldSend":              built,
	"unsaved":               built,
	"owedWrite":             built,
	"rowState":              built,
	"lostTold":              built,
	"sessionFrom":           built,
	"lostCause":             built,
	"refusedEarlierRelease": built,
	"outstanding":           built,
	"answeredBusy":          built,
	"resulted":              built,
	"ranTask":               built,
	"carryPending":          built,
	"askFirst":              built,
	"unansweredAsks":        built,
	"held":                  built,
	"heldLaunch":            built,
	"staleRelaunch":         built,
	"busyTurn":              built,
	"turnsDelivery":         built,
	"cutTurnOf":             built,
	"carried":               built,
	"compacting":            built,
	"lostCompaction":        built,
	"heldStopBack":          built,
	"processGone":           built,
	"goneOutOfMemory":       built,
	"goneCause":             built,
	"relaunch":              built + ", and set for a row that names a launch no process carries (D4)",
	"podRoundWaiting":       built,
	"awaitsPod":             built,
	"wake":                  built,
	"pluginRelaunch":        built,
	"heldNames":             built,
	"unheldDelivery":        built,
	"refusalLogged":         built,
	"unagedSession":         built,
	"admitted":              built,
	"stale":                 built,
}

// fieldValue is the value of the field numbered i of v, a Machine, unexported as it is.
func fieldValue(v reflect.Value, i int) any {
	f := v.Field(i)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
}

// armedTimers is every timer m holds armed, by kind, with the delivery each is for.
func armedTimers(timers map[TimerKind]armed) map[TimerKind]string {
	kinds := map[TimerKind]string{}
	for kind, a := range timers {
		kinds[kind] = a.deliveryID
	}
	return kinds
}

// wantBuiltAsAtStart is every field of the machine the rebuild builds equal to the machine
// NewMachine builds over the row: the claim and everything memory alone holds, the timers by their
// kinds and the deliveries they are for. relaunchOwed is a row that names a launch no process
// carries, which the rebuild leaves to be made again at the probe interval, the one difference from
// a start it makes on purpose (D4).
func (h *harness) wantBuiltAsAtStart(relaunchOwed bool) {
	h.t.Helper()
	fresh, quiet := h.rowMachine()
	defer quiet()
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	got, want := reflect.ValueOf(h.m).Elem(), reflect.ValueOf(fresh).Elem()
	fields := got.Type()
	named := map[string]bool{}
	for i := range fields.NumField() {
		f := fields.Field(i)
		named[f.Name] = true
		how, listed := rebuildFields[f.Name]
		switch {
		case !listed:
			h.t.Errorf("Machine.%s is neither built by the rebuild nor kept by it: build it, or add it to rebuildFields with its reason", f.Name)
			continue
		case !strings.HasPrefix(how, built):
			continue
		}
		g, w := fieldValue(got, i), fieldValue(want, i)
		switch f.Name {
		case "timers":
			gotTimers, wantTimers := armedTimers(h.m.timers), armedTimers(fresh.timers)
			if relaunchOwed {
				a, ok := h.m.timers[TimerRelaunch]
				if !ok || !a.due.Equal(h.clock.Now().Add(testProbe)) {
					h.t.Errorf("the relaunch is armed %v due %s, want it due at the probe interval, %s", ok, a.due, h.clock.Now().Add(testProbe))
				}
				delete(gotTimers, TimerRelaunch)
			}
			if !reflect.DeepEqual(gotTimers, wantTimers) {
				h.t.Errorf("timers armed %v, want a daemon start's %v", gotTimers, wantTimers)
			}
		case "relaunch":
			if (h.m.relaunch != nil) != relaunchOwed || fresh.relaunch != nil {
				h.t.Errorf("a relaunch is held %v, want %v (a daemon start holds %v)", h.m.relaunch != nil, relaunchOwed, fresh.relaunch != nil)
			}
		default:
			if !reflect.DeepEqual(g, w) {
				h.t.Errorf("Machine.%s is\n%+v\nwant a daemon start's\n%+v", f.Name, g, w)
			}
		}
	}
	for name := range rebuildFields {
		if !named[name] {
			h.t.Errorf("rebuildFields names %s, which is no field of Machine", name)
		}
	}
}

// orderedConn is the fake connection with the order of what the machine asked the agent: each
// get_state and each prompt, by the prompt's delivery.
type orderedConn struct {
	*fake.Conn
	mu    sync.Mutex
	asked []string
}

func (c *orderedConn) GetState(ctx context.Context) (runtime.ConnState, error) {
	c.note("get_state")
	return c.Conn.GetState(ctx)
}

func (c *orderedConn) Prompt(ctx context.Context, deliveryID, message string) error {
	c.note("prompt " + deliveryID)
	return c.Conn.Prompt(ctx, deliveryID, message)
}

func (c *orderedConn) note(what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, what)
}

// take is everything asked since the last take, in order.
func (c *orderedConn) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	asked := c.asked
	c.asked = nil
	return asked
}

// connectOrdered launches the queued claim and connects it through an orderedConn over the
// harness's own connection.
func (h *harness) connectOrdered() *orderedConn {
	h.t.Helper()
	h.launch()
	conn := &orderedConn{Conn: h.conn}
	h.conns.Register(h.token, conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation()})
	return conn
}

// wantAskedBeforeSent is the agent asked its state before the task is sent: the decision after
// the rebuild that sends the task asks get_state first, and the agent says it is in no turn.
func (h *harness) wantAskedBeforeSent(conn *orderedConn) {
	h.t.Helper()
	if asked := conn.take(); len(asked) != 0 {
		h.t.Fatalf("the refused decision asked the agent %v, want nothing", asked)
	}
	h.conn.SetStreaming(false)
	h.observe(runtime.Alive)
	p := h.pending()
	if asked := conn.take(); !slices.Equal(asked, []string{"get_state", "prompt " + p.ID}) {
		h.t.Fatalf("the agent was asked %v, want its state and then the task %s", asked, p.ID)
	}
}

// A refused write leaves the machine as a daemon start builds it over the row the store holds
// (c-rebuild-equals-start of dispatch://INFRA-4099's plan): one row for each claim state with a
// table row that makes a claim write, named by that row, and two that a start's machine alone would
// leave stuck. Every field of Machine is compared, by reflection, with the machine NewMachine builds
// over the row, but for those the rebuild keeps (rebuildFields), so a field added to Machine fails
// this test until the build builds it or the list keeps it with its reason.
//
// A process memory records and the row does not — the locator write of a start refused — is
// stopped and becomes the process the claim last ran, and a row live with no process is left to be
// launched again at the probe interval (D4). A turn start refused over a task leaves the agent
// asked its state before the task is sent.
func TestARefusedWriteLeavesTheMachineAsADaemonStartBuildsIt(t *testing.T) {
	failed := queuedClaim()
	failed.State, failed.Generation = StateFailed, 1
	failed.FailureCause, failed.Failure, failed.FailedAt = CauseLaunchRefused, "launch failures ran out", epoch
	retired := queuedClaim()
	retired.State, retired.Generation = StateRetired, 1
	uncertain := queuedClaim()
	uncertain.State, uncertain.Generation = StateLaunchUncertain, 1
	writes := func(method string) func(string, Claim) bool {
		return func(asked string, _ Claim) bool { return asked == method }
	}
	for _, tc := range []struct {
		name  string
		claim Claim
		// before brings the claim to the decision, refuse picks the writes refused, and decide
		// makes it.
		before func(h *harness) *orderedConn
		refuse func(method string, c Claim) bool
		decide func(h *harness) error
		// relaunch is a row that names a launch no process carries (D4).
		relaunch bool
	}{
		{
			name: "queued: a spawn's launching write", claim: queuedClaim(),
			refuse: writes("PutClaim"),
			decide: func(h *harness) error { return h.handle(RequestSpawn{Claim: h.token}) },
		},
		{
			name: "launch_uncertain: the release of an uncertain launch", claim: uncertain,
			refuse: writes("PutClaim"),
			decide: func(h *harness) error {
				_, err := h.m.ReleaseUncertainLaunch(h.ctx)
				return err
			},
		},
		{
			name: "launching: the shim's hello", claim: queuedClaim(),
			before: func(h *harness) *orderedConn { h.launch(); return nil },
			refuse: writes("PutClaim"),
			decide: func(h *harness) error {
				h.conns.Register(h.token, h.conn)
				return h.handle(StreamHello{Claim: h.token, Generation: h.generation()})
			},
		},
		{
			name: "shim_connected: the registration", claim: queuedClaim(),
			before: func(h *harness) *orderedConn { h.reach(StateShimConnected); return nil },
			refuse: writes("PutClaim"),
			decide: func(h *harness) error {
				return h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
					CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
			},
		},
		{
			name: "registered: the ready", claim: queuedClaim(),
			before: func(h *harness) *orderedConn { h.reach(StateRegistered); return nil },
			refuse: writes("PutClaim"),
			decide: func(h *harness) error {
				return h.handle(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})
			},
		},
		{
			name: "ready: a turn start that confirms the task", claim: queuedClaim(),
			before: func(h *harness) *orderedConn {
				conn := h.connectOrdered()
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
				conn.take()
				return conn
			},
			refuse: writes("PutClaimAndDelivery"),
			decide: func(h *harness) error { return h.handle(StreamTurnStart{Claim: h.token}) },
		},
		{
			name: "working: a turn's end", claim: queuedClaim(),
			before: func(h *harness) *orderedConn { h.reach(StateWorking); return nil },
			refuse: writes("PutClaim"),
			decide: func(h *harness) error { return h.handleQuiet(StreamTurnEnd{Claim: h.token}) },
		},
		{
			name: "idle: an operator's suspension", claim: queuedClaim(),
			before: func(h *harness) *orderedConn { h.reach(StateIdle); return nil },
			refuse: writes("PutClaim"),
			decide: func(h *harness) error { return h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}) },
		},
		{
			name: "suspended: an operator's resume", claim: queuedClaim(),
			before: func(h *harness) *orderedConn { h.reach(StateSuspended); return nil },
			refuse: writes("AdmitClaim"),
			decide: func(h *harness) error { return h.handle(RequestResume{Claim: h.token, By: SuspendedByOperator}) },
		},
		{
			name: "failed: a retry", claim: failed,
			refuse: writes("AdmitClaim"),
			decide: func(h *harness) error { return h.handle(RequestRetry{Claim: h.token}) },
		},
		{
			name: "retired: a retry", claim: retired,
			refuse: writes("AdmitClaim"),
			decide: func(h *harness) error { return h.handle(RequestRetry{Claim: h.token}) },
		},
		{
			name: "a refused locator write after a start", claim: queuedClaim(),
			refuse:   func(method string, c Claim) bool { return method == "PutClaim" && c.Locator != nil },
			decide:   func(h *harness) error { return h.handle(RequestSpawn{Claim: h.token}) },
			relaunch: true,
		},
		{
			// The first try's start fails, which leaves the row launching with no process, and the
			// launching write of the charged try after it is refused.
			name: "a live row with no process", claim: queuedClaim(),
			before: func(h *harness) *orderedConn {
				h.specs.err = errors.New("the spawn's spec could not be built")
				return nil
			},
			refuse:   func(method string, c Claim) bool { return method == "PutClaim" && c.Generation == 2 },
			decide:   func(h *harness) error { return h.handle(RequestSpawn{Claim: h.token}) },
			relaunch: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := refusingHarness(t, tc.claim)
			var conn *orderedConn
			if tc.before != nil {
				conn = tc.before(h)
			}
			suspends := len(h.rt.CallsOf("Suspend"))
			process := h.claim().Locator
			mark := s.mark()
			s.refuseWhere(tc.refuse)
			if err := tc.decide(h); !errors.Is(err, s.err) {
				t.Fatalf("the decision returned %v, want the store's refusal", err)
			}
			s.wantOneRefusalLast(t, mark, "")
			h.wantBuiltAsAtStart(tc.relaunch)
			h.wantMemoryIsTheRow()
			if tc.relaunch {
				h.wantState(StateLaunching)
				h.m.mu.Lock()
				previous := h.m.previous
				h.m.mu.Unlock()
				stopped := h.rt.CallsOf("Suspend")[suspends:]
				switch {
				case process == nil && previous == nil && h.claim().Generation == 1 && len(stopped) == 0 && len(h.rt.CallsOf("Spawn")) == 0:
					// The live row with no process: nothing started, so nothing is stopped.
				case previous != nil && len(stopped) == 1 && stopped[0].Locator == *previous && previous.Claim == h.token:
					// The start whose locator write was refused: its process is stopped and is
					// the process the claim last ran.
				default:
					t.Fatalf("the rebuild stopped %+v and records %+v as the process the claim last ran; want the process the row does not record stopped and recorded there, or none for a row no start reached",
						stopped, previous)
				}
			}
			if conn != nil {
				h.wantAskedBeforeSent(conn)
			}
		})
	}
}

// The refusal of a claim write is logged once and told once, with what a person reads it by
// (c-refusal-logged of dispatch://INFRA-4099's plan): the claim, the store call, the state the row
// holds and the state it refused, the SQLSTATE and the constraint. A second refusal of the same
// write, with the same key, is logged and told again: what is told once for a key is the daemon's
// to decide (D11), not the machine's. The second is made a probe interval after the first, past
// the window in which no claim write reaches the store (D5).
func TestAClaimWriteRefusedForGoodIsLogged(t *testing.T) {
	var mu sync.Mutex
	var told []WriteRefusal
	h, s := refusingHarness(t, queuedClaim(), func(h *harness) {
		h.deps.ClaimWriteRefused = func(r WriteRefusal) {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, r)
		}
	})
	h.reach(StateReady)
	generation := h.generation()
	s.refuseCalls("PutClaim")

	for n := 1; n <= 2; n++ {
		if n > 1 {
			h.advance(testProbe)
		}
		if err := h.handle(StreamTurnStart{Claim: h.token}); !errors.Is(err, s.err) {
			t.Fatalf("turn start %d returned %v, want the store's refusal", n, err)
		}
		h.wantState(StateReady)
		lines := h.records.of(refusedWriteLine)
		if len(lines) != n {
			t.Fatalf("after refusal %d the refusal's line was written %d times, want %d", n, len(lines), n)
		}
		line := lines[n-1]
		if line.level != slog.LevelError {
			t.Errorf("refusal %d is logged at %s, want Error", n, line.level)
		}
		for key, want := range map[string]string{
			"claim": string(h.token), "store": "PutClaim", "stored": string(StateReady), "refused": string(StateWorking),
			"generation": fmt.Sprint(generation), "sqlstate": "23514", "constraint": "claims_state_check",
		} {
			if got, ok := line.attr(key); !ok || got.String() != want {
				t.Errorf("refusal %d's line carries %s=%v (%t), want %q", n, key, got, ok, want)
			}
		}
		mu.Lock()
		heard := slices.Clone(told)
		mu.Unlock()
		if len(heard) != n {
			t.Fatalf("after refusal %d the daemon was told %d refusals, want %d", n, len(heard), n)
		}
		got := heard[n-1]
		want := WriteRefusal{Store: "PutClaim", Stored: StateReady, Refused: StateWorking, Generation: generation,
			SQLState: "23514", Constraint: "claims_state_check"}
		if got.Claim.Token != h.token || got.Store != want.Store || got.Stored != want.Stored || got.Refused != want.Refused ||
			got.Generation != want.Generation || got.SQLState != want.SQLState || got.Constraint != want.Constraint || !errors.Is(got.Err, s.err) {
			t.Fatalf("refusal %d was told as %+v, want %+v of claim %s", n, got, want, h.token)
		}
	}
}

// The rebuild gives back what the refused decision held of the gates the daemon's machines share:
// a start parked at the issue pod's gate leaves the gate, and a refresh credit, the wake wait the
// daemon was told of and the paced wake's place in the pacing all go back, so the refusal leaks
// none of them.
func TestTheRebuildGivesBackWhatItHeldOfTheSharedGates(t *testing.T) {
	t.Run("a refresh credit, a told wake wait and a paced wake", func(t *testing.T) {
		h, gate := gatedHarness(t, wakeRate)
		s := &refusingStore{memStore: h.store, err: checkRefusal()}
		var mu sync.Mutex
		queued, letOut := 0, 0
		h.deps.Store = s
		h.deps.WakeQueued = func(token claim.Token, _ string, _ claim.Role) {
			mu.Lock()
			defer mu.Unlock()
			if token == h.token {
				queued++
			}
		}
		h.deps.WakeLetOut = func(token claim.Token, _ string, _ claim.Role, _ time.Time) {
			mu.Lock()
			defer mu.Unlock()
			if token == h.token {
				letOut++
			}
		}
		h.start(h.store.load(h.token))
		h.reach(StateIdle)
		h.m.deps.PluginContract = refreshContract
		h.mustRefreshPass(gate, "sweep")
		waiting := h.wantWaitingWake(WakePluginRefresh)
		mu.Lock()
		told, endedBefore := queued, letOut
		mu.Unlock()
		if told != 1 {
			t.Fatalf("the daemon was told the wake waits %d times, want once", told)
		}

		s.refuseCalls("PutClaim")
		err := h.handle(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: 1, Refused: true, HTTPStatus: 429})
		if !errors.Is(err, s.err) {
			t.Fatalf("the turn's outcome returned %v, want the store's refusal", err)
		}

		gate.wantAsks(t, "the refused decision", refreshAsks{asked: 1, granted: 1, released: 1})
		mu.Lock()
		ended := letOut - endedBefore
		mu.Unlock()
		if ended != 1 {
			t.Fatalf("the daemon was told the wake's wait ended %d times, want once", ended)
		}
		if held := h.m.View().WakeWaiting; held != nil {
			t.Fatalf("the claim still names wake %+v, want none", *held)
		}
		probe := h.deps.WakePace.Pacer.Join(wakepace.Second)
		defer probe.Leave()
		if next := probe.Estimate(); !next.Equal(waiting.At) {
			t.Fatalf("the next place in the pacing is due at %s, want the refresh wake's, %s: its place was not given up", next, waiting.At)
		}
	})

	t.Run("a start parked at its issue pod's gate", func(t *testing.T) {
		h := podHarness(t, "")
		s := &refusingStore{memStore: h.store, err: checkRefusal()}
		h.deps.Store = s
		first := h.podRole(claim.RoleImplementer, StateSuspended)
		second := h.podRole(claim.RoleTester, StateSuspended)
		h.mustPostTo(first.m, RequestResume{Claim: first.token, By: SuspendedByOperator})
		h.parks(second, RequestResume{Claim: second.token, By: SuspendedByOperator}, 0)

		s.refuseWhere(func(method string, c Claim) bool { return method == "PutClaim" && c.Token == second.token })
		if err := h.postTo(second.m, RequestSuspend{Claim: second.token, By: SuspendedByOperator}); !errors.Is(err, s.err) {
			t.Fatalf("the suspension of the parked start returned %v, want the store's refusal", err)
		}

		if waiting := h.podWaiting(); slices.Contains(waiting, second.token) {
			t.Fatalf("the gate still holds %v waiting, want %s's start out of it", waiting, second.token)
		}
		if held := second.m.View().WakeWaiting; held != nil {
			t.Fatalf("%s still names wake %+v, want none", second.token, *held)
		}
	})
}

// Nothing reads what a refused decision left before the claim is read again from the store (R2):
// the published view, Claim, the durable report, the confirmation of a task, the stall backstop's
// report, the forget of a session and a turn outcome's answer each read the row the store holds,
// or tell nothing.
func TestNothingReadsARefusedDecisionBeforeTheRead(t *testing.T) {
	// heard is how many times the daemon was told each result of a decision.
	type heard struct {
		reports, confirmed, backstopped, lost int
	}
	calls := func(method string) func(string, Claim) bool {
		return func(asked string, _ Claim) bool { return asked == method }
	}
	for _, tc := range []struct {
		name  string
		claim Claim
		specs func(h *harness)
		// decide brings the claim to the decision, arms the refusal (refuse) right before it, and
		// makes it.
		decide func(t *testing.T, h *harness, refuse func(func(string, Claim) bool)) error
	}{
		{
			name: "a refused turn start", claim: queuedClaim(),
			decide: func(_ *testing.T, h *harness, refuse func(func(string, Claim) bool)) error {
				h.reach(StateReady)
				refuse(calls("PutClaim"))
				return h.handle(StreamTurnStart{Claim: h.token})
			},
		},
		{
			name: "a refused confirmation", claim: queuedClaim(),
			decide: func(_ *testing.T, h *harness, refuse func(func(string, Claim) bool)) error {
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
				refuse(calls("PutClaimAndDelivery"))
				return h.handle(StreamTurnStart{Claim: h.token})
			},
		},
		{
			name: "a refused stall-backstop relaunch", claim: queuedClaim(),
			decide: func(_ *testing.T, h *harness, refuse func(func(string, Claim) bool)) error {
				h.workingOnATask("implement the plan")
				started := h.clock.Now()
				h.advance(backstopBound + time.Minute)
				refuse(func(method string, c Claim) bool { return method == "PutClaim" && c.State == StateLaunching })
				return h.handle(h.backstop(started))
			},
		},
		{
			name: "a refused locator write after a fresh start", claim: byIDClaim(StateQueued, 2),
			specs: func(h *harness) { h.deps.Specs = claudeSpecs{h.specs} },
			decide: func(_ *testing.T, h *harness, refuse func(func(string, Claim) bool)) error {
				refuse(func(method string, c Claim) bool { return method == "PutClaim" && c.Locator != nil })
				return h.handle(RequestSpawn{Claim: h.token})
			},
		},
		{
			name: "a refused turn outcome", claim: queuedClaim(),
			decide: func(t *testing.T, h *harness, refuse func(func(string, Claim) bool)) error {
				h.reach(StateReady)
				refuse(calls("PutClaim"))
				answer, err := h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(),
					Sequence: 1, Refused: true, HTTPStatus: 429})
				if want := h.store.load(h.token).RefusedRun(); answer.RefusedRun != want || answer.Failed {
					t.Errorf("the turn outcome answered %+v, want the row's run of refused turns, %d, and no failure", answer, want)
				}
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var told heard
			count := func(f func(*heard)) {
				mu.Lock()
				defer mu.Unlock()
				f(&told)
			}
			h, s := refusingHarness(t, tc.claim, func(h *harness) {
				h.deps.TaskConfirmed = func(Claim) { count(func(c *heard) { c.confirmed++ }) }
				h.deps.StallBackstopped = func(Claim, StallBackstopStop) { count(func(c *heard) { c.backstopped++ }) }
				h.deps.SessionLost = func(Claim, SessionLoss, string) { count(func(c *heard) { c.lost++ }) }
				if tc.specs != nil {
					tc.specs(h)
				}
			})
			h.m.OnDurableState(func(Claim, ClaimState) { count(func(c *heard) { c.reports++ }) })
			var before heard
			err := tc.decide(t, h, func(refuse func(string, Claim) bool) {
				mu.Lock()
				before = told
				mu.Unlock()
				s.refuseWhere(refuse)
			})
			if !errors.Is(err, s.err) {
				t.Fatalf("the decision returned %v, want the store's refusal", err)
			}
			mu.Lock()
			after := told
			mu.Unlock()
			if after != before {
				t.Errorf("the refused decision told the daemon %+v, after %+v before it; want nothing more", after, before)
			}
			h.wantMemoryIsTheRow()
		})
	}
}

// A claim rebuilt over a row that holds a task no turn confirmed asks its agent its state before
// it sends that task, as a daemon start does for a restored claim (askFirst, R2 of
// dispatch://INFRA-4099's spec): the agent's turn whose confirmation the store refused may be the
// task's, and a send into it would hand the agent the task twice. Here the claim is idle, the task
// sent and acknowledged, and the turn start's write of its confirmation is refused.
func TestARebuiltClaimAsksBeforeItSends(t *testing.T) {
	h, s := refusingHarness(t, queuedClaim())
	conn := h.connectOrdered()
	h.reach(StateIdle)
	conn.take()
	h.must(RequestDeliver{Claim: h.token, Task: "review the plan"})
	sent := conn.take()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "prompt ") {
		t.Fatalf("the idle claim's task was sent as %v, want one prompt", sent)
	}
	s.refuseCalls("PutClaimAndDelivery")

	if err := h.handle(StreamTurnStart{Claim: h.token}); !errors.Is(err, s.err) {
		t.Fatalf("the turn start returned %v, want the store's refusal of its confirmation", err)
	}

	h.wantState(StateIdle)
	if p := h.pending(); !p.ConfirmedAt.IsZero() {
		t.Fatalf("the rebuilt claim's task is confirmed at %s, want it unconfirmed as the row holds it", p.ConfirmedAt)
	}
	h.wantAskedBeforeSent(conn)
}

// A refused claim write whose read of the row fails, or finds no row, decides nothing until a read
// lands (c-read-fails of dispatch://INFRA-4099's plan, D5): the store answers every read with a
// broken connection (08006). Until then an event is dropped, a request and every other locked
// section answer a wait until the read's next try, the claim reads as it was published before the
// refused decision, and nothing is written or asked of the runtime or the agent; the read is tried
// again at the probe interval, and once it lands the machine is the one a daemon start builds over
// the row. A read that lands opens a window of one probe interval in which no claim write reaches
// the store, so a claim whose every write is refused tries one write in each probe interval, the
// first decision after each window's, and a launch in the window starts no process: its own
// launching write is held, and the launch ends at it.
func TestARefusedWriteWhoseReadFailsDecidesNothingUntilARead(t *testing.T) {
	t.Run("a read that fails holds every decision until the read at the probe interval lands", func(t *testing.T) {
		h, s := refusingHarness(t, queuedClaim())
		conn := h.connectOrdered()
		h.reach(StateReady)
		conn.take()
		before, loc := h.m.View().Claim, h.locator()
		h.store.fail("ReadClaim", brokenConnection())
		s.refuseCalls("PutClaim")
		next := h.clock.Now().Add(testProbe)
		if err := h.handle(StreamTurnStart{Claim: h.token}); !errors.Is(err, s.err) {
			t.Fatalf("the turn start returned %v, want the store's refusal", err)
		}
		if lines := h.records.of(rereadFailedLine); len(lines) != 1 || lines[0].level != slog.LevelWarn {
			t.Fatalf("the failed read was logged as %+v, want one line at Warn", lines)
		}
		writes, calls, reads := s.mark(), len(h.rt.Calls()), s.readCount()

		for _, ev := range []Event{
			RuntimeObservation{Observation: runtime.Observation{Locator: loc, Kind: runtime.Alive, At: h.clock.Now()}},
			StreamTurnStart{Claim: h.token},
		} {
			if err := h.handle(ev); err != nil {
				t.Fatalf("%T while the read is owed returned %v, want it dropped", ev, err)
			}
		}
		wantWait := func(what string, err error) {
			t.Helper()
			if at, until := wait.UntilAt(err); !errors.Is(err, wait.ErrWaiting) || !until || !at.Equal(next) {
				t.Errorf("%s while the read is owed answered %v, want a wait until the read's next try, %s", what, err, next)
			}
		}
		wantWait("a request", h.handle(RequestDeliver{Claim: h.token, Task: "review the plan"}))
		wantWait("the entry given", h.m.EntryGiven(h.ctx, 3))
		if read := h.m.ReadProcess(h.ctx); read != ProcessUnanswered {
			t.Fatalf("the read of the process while the read of the claim is owed answered %s, want %s: nothing is asked", read, ProcessUnanswered)
		}
		if asked := s.since(writes); len(asked) != 0 {
			t.Fatalf("the machine asked for the claim writes %v while its read was owed, want none", asked)
		}
		if made := h.rt.Calls()[calls:]; len(made) != 0 {
			t.Fatalf("the machine made the runtime calls %+v while its read was owed, want none", made)
		}
		if asked := conn.take(); len(asked) != 0 {
			t.Fatalf("the machine asked the agent %v while its read was owed, want nothing", asked)
		}
		if got := h.claim(); !reflect.DeepEqual(got, before) {
			t.Fatalf("Claim answers\n%+v\nwant the claim published before the refused decision\n%+v", got, before)
		}
		if got := h.m.View().Claim; !reflect.DeepEqual(got, before) {
			t.Fatalf("View answers\n%+v\nwant the claim published before the refused decision\n%+v", got, before)
		}

		h.store.fail("ReadClaim", nil)
		h.advance(testProbe)

		if tried := s.readCount() - reads; tried != 1 {
			t.Fatalf("the claim was read %d times since the failed read, want one try again at the probe interval", tried)
		}
		h.wantBuiltAsAtStart(false)
		h.wantMemoryIsTheRow()
		lines := h.records.of(rereadLine)
		if len(lines) != 1 {
			t.Fatalf("the read that landed was logged %d times, want once", len(lines))
		}
		if dropped, ok := lines[0].attr("dropped"); !ok || dropped.String() != "2" {
			t.Fatalf("the read's line names %v (%t) events dropped while it was owed, want the observation and the turn start, 2", dropped, ok)
		}
	})

	t.Run("a claim whose every write is refused tries one write in each probe interval and starts no process", func(t *testing.T) {
		h, s := refusingHarness(t, queuedClaim())
		h.reach(StateReady)
		loc := h.locator()
		starts := func() int { return len(h.rt.CallsOf("Spawn")) + len(h.rt.CallsOf("Resume")) }
		started := starts()
		s.refuseWhere(func(string, Claim) bool { return true })
		events := []func() Event{
			func() Event { return StreamTurnStart{Claim: h.token} },
			func() Event {
				return RuntimeObservation{Observation: runtime.Observation{Locator: loc, Kind: runtime.Gone, At: h.clock.Now()}}
			},
			func() Event { return RequestDeliver{Claim: h.token, Task: "review the plan"} },
			func() Event {
				return RuntimeObservation{Observation: runtime.Observation{Locator: loc, Kind: runtime.Alive, At: h.clock.Now()}}
			},
		}
		step := testProbe / time.Duration(len(events))
		for interval := range 10 {
			writes, reads := s.mark(), s.readCount()
			for _, event := range events {
				_ = h.handle(event())
				h.advance(step)
			}
			if tried := s.since(writes); len(tried) != 1 {
				t.Errorf("probe interval %d tried the claim writes %v, want one: the first decision after the window", interval, tried)
			}
			if read := s.readCount() - reads; read > 1 {
				t.Errorf("probe interval %d read the claim %d times, want at most once", interval, read)
			}
		}
		// Each process found gone in a window raised a launch there, whose launching write the
		// window held, so the launch ended at that write and started nothing.
		held := 0
		for _, line := range h.records.of("supervise: the launch could not be written; trying it again at the probe interval") {
			if got, ok := line.attr("error"); ok {
				if err, isErr := got.Any().(error); isErr && errors.Is(err, errWritesWait) {
					held++
				}
			}
		}
		if held == 0 {
			t.Fatal("no launch met the window: this row proves nothing of a launch in it")
		}
		if n := starts() - started; n != 0 {
			t.Fatalf("the claim whose every write is refused started %d processes, want none", n)
		}
	})
}

// A delivered ask of an update round whose mark the store refuses is asked no more (D9 of
// dispatch://INFRA-4099's plan): the ask answers delivered with no error, so its row ends, and the
// machine keeps that the ask of the round was delivered across the rebuild, so a later ask of the
// round answers already delivered and publishes nothing, though the row read again holds no mark.
// An ask that meets a refusal of the repair at its head, the window after a refusal or a read of the
// claim owed publishes nothing and answers the machine's busy answer (ErrMachineBusy), a wait until
// the claim's writes reach the store again; and a mark the store owes keeps the answer it has.
func TestARefusedMarkOfADeliveredAskPublishesItNoMore(t *testing.T) {
	inRound := func(t *testing.T) (*harness, *refusingStore) {
		t.Helper()
		h, s := refusingHarness(t, queuedClaim(), func(h *harness) { h.deps.PluginContract = PodUpdateContract })
		h.reach(StateReady)
		h.openUpdate(testRound, testReasons)
		return h, s
	}
	// wantBusy is an ask answered the machine's busy answer, a wait until at, and published nothing
	// more than published.
	wantBusy := func(t *testing.T, h *harness, publisher *askPublisher, published int, at time.Time) {
		t.Helper()
		outcome, err := h.m.PublishUpdateAsk(h.ctx, testRound, publisher.publish)
		due, until := wait.UntilAt(err)
		if outcome != 0 || !errors.Is(err, ErrMachineBusy) || !errors.Is(err, wait.ErrWaiting) || !until || !due.Equal(at) {
			t.Fatalf("the ask = %v, %v; want no outcome and the machine's busy answer, a wait until %s", outcome, err, at)
		}
		if publisher.calls != published {
			t.Fatalf("the ask was published %d times, want %d", publisher.calls, published)
		}
	}
	wantStoredMark := func(t *testing.T, h *harness, marked bool) {
		t.Helper()
		if stored := h.store.load(h.token); (stored.UpdateTaskedAt != nil) != marked {
			t.Fatalf("the row holds the ask's mark %v, want a mark %t", stored.UpdateTaskedAt, marked)
		}
	}

	t.Run("a delivered ask whose mark the store refuses answers delivered, and the round is asked no more", func(t *testing.T) {
		h, s := inRound(t)
		reached := &askPublisher{delivered: true}
		s.refuseCalls("PutClaim")
		refusedAt := h.clock.Now()
		if outcome, err := h.m.PublishUpdateAsk(h.ctx, testRound, reached.publish); err != nil || outcome != UpdateAskDelivered {
			t.Fatalf("the delivered ask whose mark the store refused = %v, %v; want it delivered with no error, so its row ends", outcome, err)
		}
		h.wantMemoryIsTheRow()
		if c := h.claim(); c.UpdateTaskedAt != nil {
			t.Fatalf("the rebuilt claim holds the mark %v, want none: the row holds none", c.UpdateTaskedAt)
		}
		wantBusy(t, h, reached, 1, refusedAt.Add(testProbe))

		s.refuseWhere(nil)
		h.advance(testProbe)
		if outcome, err := h.m.PublishUpdateAsk(h.ctx, testRound, reached.publish); err != nil || outcome != UpdateAskAlreadyDelivered {
			t.Fatalf("a later ask of the round = %v, %v; want it already delivered", outcome, err)
		}
		if reached.calls != 1 {
			t.Fatalf("the ask was published %d times, want once", reached.calls)
		}
		wantStoredMark(t, h, false)
	})

	t.Run("a repair the store refuses at the head of the ask publishes nothing", func(t *testing.T) {
		h, s := inRound(t)
		h.store.fail("PutClaim", brokenConnection())
		if err := h.handle(StreamTurnStart{Claim: h.token}); err == nil {
			t.Fatal("the turn start over a broken connection returned no error, want its owed write's")
		}
		h.store.fail("PutClaim", nil)
		s.refuseCalls("PutClaim")
		reached := &askPublisher{delivered: true}
		wantBusy(t, h, reached, 0, h.clock.Now().Add(testProbe))
		h.wantMemoryIsTheRow()
		wantStoredMark(t, h, false)
	})

	t.Run("an ask in the window after a refusal publishes nothing until the window ends", func(t *testing.T) {
		h, s := inRound(t)
		s.refuseCalls("PutClaim")
		refusedAt := h.clock.Now()
		if err := h.handle(StreamTurnStart{Claim: h.token}); !errors.Is(err, s.err) {
			t.Fatalf("the turn start returned %v, want the store's refusal", err)
		}
		s.refuseWhere(nil)
		reached := &askPublisher{delivered: true}
		wantBusy(t, h, reached, 0, refusedAt.Add(testProbe))

		h.advance(testProbe)
		if outcome, err := h.m.PublishUpdateAsk(h.ctx, testRound, reached.publish); err != nil || outcome != UpdateAskDelivered {
			t.Fatalf("the ask after the window = %v, %v; want it delivered", outcome, err)
		}
		wantStoredMark(t, h, true)
	})

	t.Run("an ask while the read of the claim is owed publishes nothing", func(t *testing.T) {
		h, s := inRound(t)
		h.store.fail("ReadClaim", brokenConnection())
		s.refuseCalls("PutClaim")
		refusedAt := h.clock.Now()
		if err := h.handle(StreamTurnStart{Claim: h.token}); !errors.Is(err, s.err) {
			t.Fatalf("the turn start returned %v, want the store's refusal", err)
		}
		reached := &askPublisher{delivered: true}
		wantBusy(t, h, reached, 0, refusedAt.Add(testProbe))
		wantStoredMark(t, h, false)
	})

	t.Run("a mark the store owes keeps its answer", func(t *testing.T) {
		h, _ := inRound(t)
		h.store.fail("PutClaim", brokenConnection())
		reached := &askPublisher{delivered: true}
		if outcome, err := h.m.PublishUpdateAsk(h.ctx, testRound, reached.publish); outcome != UpdateAskDelivered || SortClaimWriteFailure(err) != WriteOwed {
			t.Fatalf("the delivered ask whose mark is owed = %v, %v; want it delivered, with the owed write's error", outcome, err)
		}
		h.store.fail("PutClaim", nil)
		if outcome, err := h.m.PublishUpdateAsk(h.ctx, testRound, reached.publish); err != nil || outcome != UpdateAskAlreadyDelivered {
			t.Fatalf("the next ask once the store answers = %v, %v; want it already delivered", outcome, err)
		}
		if reached.calls != 1 {
			t.Fatalf("the ask was published %d times, want once", reached.calls)
		}
		wantStoredMark(t, h, true)
	})
}
