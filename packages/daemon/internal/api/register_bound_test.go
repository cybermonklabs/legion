package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// R4 of dispatch://INFRA-2883: POST /legion/v1/claims/register answers within a bound, for a
// claim and for a controller alike, and the answer waits on no other process — not on a pod's
// launch turn, not on a launch, a stop or a resume of any claim, and not on a lock held across
// one of those. These rows drive each of those waits and hold the route to the bound.

// shortBound is what the tests about the bound answer within: short enough that a row which meets
// it finishes at once, and far longer than a route that is not blocked needs, so a row that
// passes says the route answered rather than that the machine was quick.
const shortBound = 300 * time.Millisecond

// boundHarness is a harness whose registration and ready answer within shortBound, with the
// memory of what they refused unanswered (Options.Refusals).
func boundHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, harnessOptions{relaunchable: true, bound: shortBound, refusals: true})
}

// inARuntimeCall leaves token's machine inside a runtime call that does not return: the relaunch
// of a process the daemon found gone, held where the Sandbox runtime waits out a pod it is
// building and the pod's launch turn (fake.Runtime.HoldResume). The machine holds itself for the
// whole call, which is what every wait R4 names looks like from a route.
//
// The claim is registered and ready on session first, because the relaunch of a claim with a
// session to resume is the Resume the fake runtime can hold — and because a pane that comes back
// while its claim is being relaunched is exactly the caller this bound is for. It returns the
// call that lets the runtime answer and waits out the decision, which each test makes before it
// ends.
func (h *harness) inARuntimeCall(token claim.Token, session string) func() {
	h.t.Helper()
	h.ready(token, session, 1)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.runtime.HoldResume(func() {
		once.Do(func() { close(entered) })
		<-release
	})
	m, supervised := h.supervisor.Machine(token)
	if !supervised {
		h.t.Fatalf("no machine for %s", token)
	}
	locator := m.Claim().Locator
	if locator == nil {
		h.t.Fatalf("%s records no process to lose", token)
	}
	go func() {
		defer close(done)
		// The relaunch's answer is the runtime's, which this test holds: what it returns once
		// released is no part of what the row is about.
		_ = m.Handle(h.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: *locator, Kind: runtime.Gone},
		})
	}()
	<-entered
	return sync.OnceFunc(func() {
		h.runtime.HoldResume(nil)
		close(release)
		<-done
		m.Wait()
	})
}

// answeredWithin runs one request and holds it to the bound, reporting what it answered. The
// allowance over the bound is the room a loaded machine takes to schedule the answering
// goroutine; what the row is about is that the request does not wait out the call that holds the
// machine, which no allowance hides.
func (h *harness) answeredWithin(bound time.Duration, send func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	h.t.Helper()
	started := time.Now()
	recorder := send()
	if took := time.Since(started); took > bound+5*time.Second {
		h.t.Fatalf("the request answered after %s, want it answered inside its %s bound", took, bound)
	}
	return recorder
}

// wantRetryable holds a response to the retryable refusal a caller gets when the daemon could not
// answer it inside its bound: the uncoded 503 the start handler gives, which the agent's own
// client reads as a daemon that is not there and retries.
func wantRetryable(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	wantRefusal(t, recorder, http.StatusServiceUnavailable, startRefusal)
}

// staleRegistration registers session on the launch bootToken names, declaring a plugin contract
// this daemon does not speak, and returns what the route answered.
func (h *harness) staleRegistration(bootToken, session string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent-" + session, PluginContract: DaemonAPIVersion - 1,
	}, nil)
}

// staleSentence is the refusal a registration on another plugin contract gets, verbatim.
func staleSentence() string {
	return claim.StalePluginContract(DaemonAPIVersion-1, DaemonAPIVersion).Message
}

// A registration whose claim's machine is inside a runtime call that does not return is answered
// all the same: the machine holds itself across that call — a resume waits out the previous
// incarnation — and before this change the route waited there with it, which left the agent
// running its whole phase with no `legion` tool.
func TestRegisterAnswersWhileTheClaimsMachineIsInARuntimeCall(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	release := h.inARuntimeCall(token, "ses_implementer")
	defer release()

	recorder := h.answeredWithin(shortBound, func() *httptest.ResponseRecorder {
		return h.register(boot, "ses_implementer")
	})

	wantRetryable(t, recorder)
	if !h.refusals.Refused(token, 1, supervise.HashBootToken(boot)) {
		t.Fatal("the launch this process could not answer is not remembered, want it remembered so nothing charges it")
	}
}

// A registration answers while another claim of the same pod holds the launch turn: the turn is
// one pod's six roles' to share and nothing bounds it, so a registration whose own decision would
// take it posts that work instead of waiting for it.
func TestRegisterAnswersWhileAnotherClaimHoldsThePodTurn(t *testing.T) {
	h := boundHarness(t)
	holder, _ := h.launch("LEGION-2883", claim.RoleTester)
	release := h.inARuntimeCall(holder, "ses_tester")
	defer release()

	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	recorder := h.answeredWithin(shortBound, func() *httptest.ResponseRecorder {
		return h.staleRegistration(boot, "ses_implementer")
	})

	wantRefusal(t, recorder, http.StatusConflict, staleSentence())
	if posted := h.supervisor.posted; len(posted) != 1 || posted[0].token != token {
		t.Fatalf("the route posted %+v, want the one relaunch its refusal raised, for %s", posted, token)
	}
	if calls := h.runtime.CallsOf("Resume"); len(calls) != 0 {
		t.Fatalf("the runtime recorded %d resumes, want none: the other claim's relaunch is still inside the turn it holds, and this registration awaited no launch at all", len(calls))
	}

	// The turn is given up and the relaunch this registration raised takes it, as it would have
	// had the route waited — minutes later, and after the agent was answered.
	release()
	if ran := h.runPosted(); ran != 1 {
		t.Fatalf("the daemon ran %d posted relaunches, want the one this registration raised", ran)
	}
	if calls := h.runtime.CallsOf("Resume"); len(calls) != 2 {
		t.Fatalf("the runtime made %d resumes, want two: the one that held the turn and the one this registration posted", len(calls))
	}
}

// The work a registration's decision would do beyond recording the registration — the relaunch a
// stale plugin contract raises, which takes the pod's launch turn — is posted to the claim's
// machine and not awaited. The caller that meets its refusal has had nothing relaunched yet; the
// relaunch runs when the machine takes it.
func TestRegisterPostsALaunchAndDoesNotAwaitIt(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)

	recorder := h.answeredWithin(shortBound, func() *httptest.ResponseRecorder {
		return h.staleRegistration(boot, "ses_implementer")
	})

	wantRefusal(t, recorder, http.StatusConflict, staleSentence())
	if calls := h.runtime.CallsOf("Resume"); len(calls) != 0 {
		t.Fatalf("the route made %d resumes, want none: the relaunch its refusal raised is posted, not awaited", len(calls))
	}
	if stored := h.stored(token); stored.PluginRelaunchFor != 0 {
		t.Fatalf("the claim is marked for a relaunch for %d, want 0: no relaunch has run yet", stored.PluginRelaunchFor)
	}

	if ran := h.runPosted(); ran != 1 {
		t.Fatalf("the daemon ran %d posted relaunches, want the one the registration raised", ran)
	}
	if calls := h.runtime.CallsOf("Resume"); len(calls) != 1 {
		t.Fatalf("the posted relaunch made %d resumes, want one", len(calls))
	}
	if stored := h.stored(token); stored.PluginRelaunchFor != DaemonAPIVersion {
		t.Fatalf("the relaunched claim is marked for %d, want this daemon's own contract %d",
			stored.PluginRelaunchFor, DaemonAPIVersion)
	}
}

// heldController is a controller record whose every read waits for the test, which is what one
// caller's mint looks like to the next: the route takes the controller lock and sits in the
// store under it.
type heldController struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *heldController) Controller(ctx context.Context, _ string) (controller.Record, bool, error) {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return controller.Record{}, false, ctx.Err()
	}
	return controller.Record{}, false, nil
}

func (c *heldController) MintController(context.Context, string, []byte) (uint64, error) {
	return 1, nil
}

func (c *heldController) RegisterController(context.Context, string, uint64, string, []byte, time.Time, int) (bool, error) {
	return true, nil
}

// noLaunches is a boot token store that holds no launch: every token it is asked about is the
// controller capability's or nothing, which is what sends a registration to the controller path.
type noLaunches struct{}

func (noLaunches) ClaimByBootTokenHash(context.Context, []byte) (supervise.Claim, bool, error) {
	return supervise.Claim{}, false, nil
}

// A controller registration answers while the controller lock is held by another caller — a mint,
// a grant, a registration of its own — rather than waiting it out: the lock orders those writes,
// and R4's bound is on the answer, so the lock is taken under it.
func TestControllerRegistrationAnswersWhileItsLockIsHeld(t *testing.T) {
	held := &heldController{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(held.release)
	refusals := testAnswering(t)
	handler := NewServer(Options{
		Project: testProject, Controller: held, BootTokens: NewBootTokens(noLaunches{}),
		WorkerRPCTimeout: shortBound, Refusals: refusals,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	body := claim.RegisterRequest{
		BootToken: "controller-capability", SessionID: "ses_controller",
		OmpSessionFile: "/sessions/ses_controller.jsonl", AgentID: "agent-controller",
		PluginContract: DaemonAPIVersion,
	}
	// The first registration takes the lock and sits in the store under it.
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, jsonRequest(t, "/legion/v1/claims/register", body))
	}()
	<-held.entered

	started := time.Now()
	recorder := httptest.NewRecorder()
	second := body
	second.BootToken = "controller-capability-again"
	handler.ServeHTTP(recorder, jsonRequest(t, "/legion/v1/claims/register", second))

	if took := time.Since(started); took > shortBound+5*time.Second {
		t.Fatalf("the second registration answered after %s, want it answered inside its %s bound", took, shortBound)
	}
	wantRetryable(t, recorder)
	if !refusals.Refused("", 0, supervise.HashBootToken(second.BootToken)) {
		t.Fatal("the controller launch this process could not answer is not remembered")
	}
}

// jsonRequest is one request to a route, with body as its JSON.
func jsonRequest(t *testing.T, target string, body any) *http.Request {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
}

// A registration that cannot have the claim's machine inside its bound is refused the retryable
// refusal having recorded nothing: no session, no secret, nothing for the agent to hold that the
// daemon has not written down.
func TestRegisterRefusesWithNothingRecordedWhenTheMachineIsNotFreeInTime(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	release := h.inARuntimeCall(token, "ses_implementer")
	defer release()
	before := h.stored(token)

	recorder := h.answeredWithin(shortBound, func() *httptest.ResponseRecorder {
		return h.register(boot, "ses_implementer")
	})

	wantRetryable(t, recorder)
	after := h.stored(token)
	if after.Session != before.Session || string(after.CapabilityHash) != string(before.CapabilityHash) ||
		after.PluginContract != before.PluginContract || after.State != before.State || after.Generation != before.Generation {
		t.Fatalf("the refused registration left the claim %+v, want it exactly as it was: %+v", after, before)
	}
	if posted := h.supervisor.posted; len(posted) != 0 {
		t.Fatalf("the refused registration posted %+v, want nothing: it decided nothing", posted)
	}
}

// The rule a registration's answer rests on is unchanged by the bound: the secret the agent is
// given is one whose hash the store already holds, so no agent holds a secret the daemon forgot.
func TestRegisterKeepsTheSecretPersistedBeforeItAnswers(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)

	registered := h.registered(boot, "ses_implementer")

	if stored := h.stored(token); len(stored.CapabilityHash) == 0 ||
		string(stored.CapabilityHash) != string(capabilityHash(registered.Secret)) {
		t.Fatal("the store does not hold the hash of the secret the registration answered with")
	}
	// The agent's own next call proves it is that secret the daemon authenticates by.
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret, Generation: 1,
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("ready with the registration's secret = %d, want 204; body %s", recorder.Code, recorder.Body)
	}
}

// Every refusal the route gave before the bound it still gives, with its status, its code and its
// words, where the decision behind it is made inside the bound.
func TestEachSynchronousRefusalIsGivenWhenTheMachineAnswersInTime(t *testing.T) {
	for _, row := range []struct {
		name    string
		status  int
		message string
		send    func(t *testing.T, h *harness) *httptest.ResponseRecorder
	}{
		{
			name:    "an invalid boot token",
			status:  claim.InvalidBootToken.Status,
			message: claim.InvalidBootToken.Message,
			send: func(t *testing.T, h *harness) *httptest.ResponseRecorder {
				return h.register("no-launch-minted-this", "ses_nobody")
			},
		},
		{
			name:   "a launch of a controller this deployment does not launch",
			status: http.StatusConflict,
			send: func(t *testing.T, h *harness) *httptest.ResponseRecorder {
				_, boot := h.launchController()
				return h.register(boot, "ses_controller")
			},
		},
		{
			name:    "a stale generation",
			status:  claim.StaleGeneration.Status,
			message: claim.StaleGeneration.Message,
			send: func(t *testing.T, h *harness) *httptest.ResponseRecorder {
				token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
				h.relaunch(token)
				return h.register(boot, "ses_implementer")
			},
		},
		{
			name:    "a session that is not the recorded one",
			status:  claim.SameAgentRefusal.Status,
			message: claim.SameAgentRefusal.Message,
			send: func(t *testing.T, h *harness) *httptest.ResponseRecorder {
				_, boot := h.launch("LEGION-2883", claim.RoleImplementer)
				h.registered(boot, "ses_implementer")
				return h.register(boot, "ses_intruder")
			},
		},
		{
			name:    "a stale plugin contract",
			status:  http.StatusConflict,
			message: staleSentence(),
			send: func(t *testing.T, h *harness) *httptest.ResponseRecorder {
				_, boot := h.launch("LEGION-2883", claim.RoleImplementer)
				return h.staleRegistration(boot, "ses_implementer")
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := boundHarness(t)

			recorder := h.answeredWithin(shortBound, func() *httptest.ResponseRecorder { return row.send(t, h) })

			if row.message != "" {
				wantRefusal(t, recorder, row.status, row.message)
				return
			}
			if recorder.Code != row.status {
				t.Fatalf("status = %d, want %d; body %s", recorder.Code, row.status, recorder.Body)
			}
		})
	}
}

// A boot token the daemon cannot resolve keeps its 500 too: the store answered, with a failure of
// its own, inside the bound.
func TestABootTokenTheDaemonCannotResolveKeepsItsAnswer(t *testing.T) {
	handler := NewServer(Options{
		Project: testProject, BootTokens: NewBootTokens(failingBootTokens{}),
		WorkerRPCTimeout: shortBound, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, jsonRequest(t, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: "a-token", SessionID: "ses", OmpSessionFile: "/sessions/ses.jsonl", AgentID: "agent",
		PluginContract: DaemonAPIVersion,
	}))

	wantRefusal(t, recorder, http.StatusInternalServerError,
		"register failed: the daemon could not resolve the boot token")
}

// A caller whose plugin contract this daemon does not speak meets the retryable refusal, not its
// own 409, while the machine that would decide that 409 is busy: the stale refusal is the
// machine's to make, and a route that could not reach the machine recorded nothing to refuse
// from. The caller asks again, and meets the refusal for good on that attempt or after its
// relaunch.
func TestAStaleCallerIsToldToRetryWhileTheMachineIsBusy(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	release := h.inARuntimeCall(token, "ses_implementer")

	recorder := h.answeredWithin(shortBound, func() *httptest.ResponseRecorder {
		return h.staleRegistration(boot, "ses_implementer")
	})

	wantRetryable(t, recorder)
	if posted := h.supervisor.posted; len(posted) != 0 {
		t.Fatalf("the refused registration posted %+v, want nothing: it decided nothing", posted)
	}
	if stored := h.stored(token); stored.PluginContract != DaemonAPIVersion {
		t.Fatalf("the refused registration recorded plugin contract %d, want the %d the claim already held: it recorded nothing",
			stored.PluginContract, DaemonAPIVersion)
	}

	// The machine frees up and the caller asks again: the refusal it was going to get is the one
	// it gets, in its own words.
	release()
	again := h.staleRegistration(h.bootToken(token), "ses_implementer")
	wantRefusal(t, again, http.StatusConflict, staleSentence())
}

// The daemon's stop waits for the work a route posted as it waits for a decision made inside the
// request: the claim stays recorded as decided by a route until that work has run, which is what
// keeps its worker stream connection open through the drain (RouteDecisions).
func TestTheDrainWaitsForWorkTheRoutePosted(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)

	wantRefusal(t, h.staleRegistration(boot, "ses_implementer"), http.StatusConflict, staleSentence())

	if !h.decisions.Holds(token) {
		t.Fatal("the answered registration left the claim undecided, want it held for the relaunch it posted")
	}
	if ran := h.runPosted(); ran != 1 {
		t.Fatalf("the daemon ran %d posted relaunches, want one", ran)
	}
	if h.decisions.Holds(token) {
		t.Fatal("the claim is still held after the posted relaunch ran, want it released with it")
	}
}

// A registration whose answer never reached the agent is made again, on the session the claim
// already records: the state table routes it to reregister. The second secret is the one that
// works from then on, nothing is relaunched, and the task the claim already holds is not sent a
// second time.
func TestARegistrationRetriedAfterALostAnswerReregisters(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	first := h.registered(boot, "ses_implementer")
	conn := connected(h, token)
	h.ready(token, "ses_implementer", 1)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		DeliverRequest{Task: "Carry on implementing."}); recorder.Code != http.StatusOK {
		t.Fatalf("deliver = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	settled(h, token)
	if prompts := conn.Prompts(); len(prompts) != 1 {
		t.Fatalf("the claim was sent %#v before its registration was retried, want the one task it was given", prompts)
	}

	second := h.registered(boot, "ses_implementer")

	if second.Secret == first.Secret {
		t.Fatal("the retried registration answered the first secret, want one of its own")
	}
	settled(h, token)
	if prompts := conn.Prompts(); len(prompts) != 1 {
		t.Fatalf("the claim was sent %#v after its registration was retried, want the one task it already had", prompts)
	}
	if calls := h.runtime.CallsOf("Resume"); len(calls) != 0 {
		t.Fatalf("the retried registration made %d resumes, want none: nothing about it is stale", len(calls))
	}
	if posted := h.supervisor.posted; len(posted) != 0 {
		t.Fatalf("the retried registration posted %+v, want nothing", posted)
	}
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: second.Secret, Generation: 1,
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("ready with the second secret = %d, want 204; body %s", recorder.Code, recorder.Body)
	}
}

// A registration that arrives after the registration deadline has retired its process and
// relaunched the claim is refused 409: the pane it comes from answers for a generation the claim
// has left, and the fence is the machine's, made inside the bound like every other refusal.
func TestARegistrationAfterTheDeadlineSuspendedTheClaimIsRefused(t *testing.T) {
	h := boundHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)

	deadline := runtime.RegistrationDeadline(time.Minute, 3, h.runtime.ProvisionBound())
	if fired := h.clock.fire(deadline); fired != 1 {
		t.Fatalf("fired %d timers armed at %s, want the claim's registration deadline", fired, deadline)
	}
	if stored := h.stored(token); stored.Generation != 2 {
		t.Fatalf("the claim after its registration deadline is at generation %d, want 2: the deadline retired its process and relaunched it", stored.Generation)
	}

	recorder := h.answeredWithin(shortBound, func() *httptest.ResponseRecorder {
		return h.register(boot, "ses_implementer")
	})

	wantRefusal(t, recorder, claim.StaleGeneration.Status, claim.StaleGeneration.Message)
}

// hangingBootTokens is a boot token store whose read never answers of its own accord: a Postgres
// that has stopped answering, which the route reaches before it has a claim to post anything to.
type hangingBootTokens struct {
	entered chan struct{}
	once    sync.Once
}

func (b *hangingBootTokens) ClaimByBootTokenHash(ctx context.Context, _ []byte) (supervise.Claim, bool, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return supervise.Claim{}, false, ctx.Err()
}

// failingBootTokens is a store that refuses the read outright.
type failingBootTokens struct{}

func (failingBootTokens) ClaimByBootTokenHash(context.Context, []byte) (supervise.Claim, bool, error) {
	return supervise.Claim{}, false, context.Canceled
}

// The boot token's resolve is inside the bound too: a degraded Postgres that never answers it
// leaves the agent with the retryable refusal rather than with a request that hangs for ever.
func TestABootTokenResolveThatHangsStillAnswers(t *testing.T) {
	hanging := &hangingBootTokens{entered: make(chan struct{})}
	refusals := testAnswering(t)
	handler := NewServer(Options{
		Project: testProject, BootTokens: NewBootTokens(hanging),
		WorkerRPCTimeout: shortBound, Refusals: refusals,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	started := time.Now()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, jsonRequest(t, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: "a-token-the-store-cannot-answer-for", SessionID: "ses_implementer",
		OmpSessionFile: "/sessions/ses_implementer.jsonl", AgentID: "agent-implementer",
		PluginContract: DaemonAPIVersion,
	}))

	if took := time.Since(started); took > shortBound+5*time.Second {
		t.Fatalf("the registration answered after %s, want it answered inside its %s bound", took, shortBound)
	}
	<-hanging.entered
	wantRetryable(t, recorder)
	if !refusals.Refused("", 0, supervise.HashBootToken("a-token-the-store-cannot-answer-for")) {
		t.Fatal("the launch whose resolve never answered is not remembered, want it remembered so nothing charges it")
	}
}
