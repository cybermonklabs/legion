package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// sessionHeldSentence is the sentence of claim.SessionHeldRefusal after its session id, which
// names no other claim's token.
const sessionHeldSentence = "is recorded for another claim of this daemon; a claim registers only a session it recorded itself, or one no claim has recorded"

// A Legion session id is held to the first claim that recorded it, whatever that claim's state
// (plan E's t-e-54). The daemon enrolls the session a claim's agent reports
// with the secrets broker, and the broker's enrollment takes over the grants and pending requests
// of every other enrollment of the same principal and session (packages/envoy/internal/cipher/
// enroll's adopt), while every pod of a project enrolls as that project's one principal. So a role
// that registered a sibling's session id, or the id of another claim of its project, live or
// ended, took that session's secret grants. Through POST /legion/v1/claims/register on the real
// store: a registration of an id another claim recorded is refused with 409 before anything of the
// claim changes, and nothing is enrolled; an id no claim recorded is admitted for the first claim
// that reports it and refused to a second; and a claim's own earlier id is admitted again, after
// it moved to a new session and when its relaunched process resumes it.
//
// The controls (the pull request records both runs): the daemon before the hold admitted the
// three refused rows and enrolled them; and a hold of live claims alone, a reported id refused only
// while a live claim's current session is that id, admits the ended and the suspended rows.
func TestAReclaimRegistersANewIdNoOtherClaimHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		// holderIssue is the issue of the claim that records the session first: the issue the
		// registering claim works on, so the two are roles of one pod, or another issue of the
		// project.
		holderIssue string
		// leave brings the holder from ready to the state the case names.
		leave func(h *harness, holder claim.Token, registered claim.RegisterResponse)
	}{
		{
			name:        "an ended claim's id, reported by another claim of the project",
			holderIssue: "LEGION-902",
			leave: func(h *harness, holder claim.Token, registered claim.RegisterResponse) {
				if recorder := h.request(http.MethodPost, "/legion/v1/claims/exit", claim.ExitRequest{
					ClaimToken: holder, SessionID: "ses_holder", Secret: registered.Secret,
					Generation: registered.Generation, Reason: "phase complete",
				}, nil); recorder.Code != http.StatusNoContent {
					h.t.Fatalf("exit %s = %d; body %s", holder, recorder.Code, recorder.Body)
				}
				h.wantRecords(holder, supervise.StateRetired, "ses_holder")
			},
		},
		{
			name:        "a suspended claim's id",
			holderIssue: "LEGION-901",
			leave: func(h *harness, holder claim.Token, _ claim.RegisterResponse) {
				h.suspend(holder)
				h.wantRecords(holder, supervise.StateSuspended, "ses_holder")
			},
		},
		{
			name:        "two claims in one issue pod, the second reporting the first's id",
			holderIssue: "LEGION-901",
			leave: func(h *harness, holder claim.Token, _ claim.RegisterResponse) {
				h.wantRecords(holder, supervise.StateReady, "ses_holder")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, broker := sessionHoldHarness(t)
			holder, holderBoot := h.podRole(tc.holderIssue, claim.RoleImplementer, 1)
			registered := h.registered(holderBoot, "ses_holder")
			h.readyAt(holder, "ses_holder", registered)
			tc.leave(h, holder, registered)

			taker, takerBoot := h.podRole("LEGION-901", claim.RoleTester, 1)
			h.wantSessionRefused(broker, taker, takerBoot, "ses_holder", holder)
			if enrolled := broker.enrolledAs(holder); len(enrolled) != 1 || enrolled[0] != "ses_holder" {
				t.Errorf("the holder was enrolled as %v, want once as ses_holder", enrolled)
			}
		})
	}

	t.Run("a fresh id, admitted for the first claim that reports it and then refused to a second", func(t *testing.T) {
		h, broker := sessionHoldHarness(t)
		first, firstBoot := h.podRole("LEGION-901", claim.RoleImplementer, 1)
		h.registered(firstBoot, "ses_new")
		h.wantRecords(first, supervise.StateRegistered, "ses_new")
		if got := broker.enrolledAs(first); len(got) != 1 || got[0] != "ses_new" {
			t.Errorf("the first claim was enrolled as %v, want once as ses_new", got)
		}

		second, secondBoot := h.podRole("LEGION-901", claim.RoleTester, 1)
		h.wantSessionRefused(broker, second, secondBoot, "ses_new", first)
	})

	t.Run("this claim's own earlier id, after a new session, is admitted", func(t *testing.T) {
		h, broker := sessionHoldHarness(t)
		owner, boot := h.podRole("LEGION-901", claim.RoleImplementer, 1)
		h.readyAt(owner, "ses_first", h.registered(boot, "ses_first"))

		// The claim moves to a new session: the operator has it start fresh, and its agent
		// registers an id of its own.
		boot = h.freshSession(owner, "ses_first", 2)
		h.readyAt(owner, "ses_second", h.registered(boot, "ses_second"))

		// And back to the first: an id this claim recorded before is its own.
		boot = h.freshSession(owner, "ses_second", 3)
		h.registered(boot, "ses_first")
		h.wantRecords(owner, supervise.StateRegistered, "ses_first")
		if got := broker.enrolledAs(owner); strings.Join(got, ",") != "ses_first,ses_second,ses_first" {
			t.Errorf("the claim was enrolled as %v, want ses_first, ses_second, then ses_first again", got)
		}
	})

	// What the hold leaves as it was (R9): a relaunched process resumes the claim's own session and
	// registers it again, and is enrolled under it again, exactly as before the hold.
	t.Run("a relaunch registers this claim's own id again", func(t *testing.T) {
		h, broker := sessionHoldHarness(t)
		owner, boot := h.podRole("LEGION-901", claim.RoleImplementer, 1)
		registered := h.registered(boot, "ses_own")
		h.readyAt(owner, "ses_own", registered)

		h.runtime.ScriptResume(fake.SpawnResult{Locator: podLocator(owner, "pod-legion-901", 2)})
		relaunchBoot := h.relaunch(owner)
		h.hello(owner, "pod-legion-901", "thumbprint-"+string(owner))
		again := h.registered(relaunchBoot, "ses_own")
		if again.ClaimToken != owner || again.Generation != registered.Generation+1 {
			t.Fatalf("the relaunch registered as %+v, want %s at generation %d", again, owner, registered.Generation+1)
		}
		if got := broker.enrolledAs(owner); strings.Join(got, ",") != "ses_own,ses_own" {
			t.Errorf("the claim was enrolled as %v, want twice as ses_own, once per process", got)
		}
	})
}

// wantSessionRefused registers session on the launch boot names and holds the answer to the
// same-session refusal: a 409 naming the session and not the holder, the claim recording no
// session, no transcript and no secret, and no enrollment of the claim's role.
func (h *harness) wantSessionRefused(broker *enrollmentRecorder, token claim.Token, boot, session string, holder claim.Token) {
	h.t.Helper()
	before, calls := h.stored(token), broker.calls()
	recorder := h.register(boot, session)
	if recorder.Code != http.StatusConflict {
		h.t.Errorf("%s registering %s, which %s recorded first, = %d, want 409; body %s", token, session, holder, recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, session+" "+sessionHeldSentence) {
		h.t.Errorf("the refusal %s does not say that %s is another claim's", body, session)
	}
	if strings.Contains(body, string(holder)) {
		h.t.Errorf("the refusal %s names %s, the claim that holds the session", body, holder)
	}
	got := h.stored(token)
	if got.State != before.State || got.Session != "" || got.SessionFile != "" || len(got.CapabilityHash) != 0 {
		h.t.Errorf("the refused claim is %s recording session %q, file %q and a capability %t; want it %s with none of them",
			got.State, got.Session, got.SessionFile, len(got.CapabilityHash) != 0, before.State)
	}
	if enrolled := broker.enrolledAs(token); len(enrolled) != 0 {
		h.t.Errorf("the refused claim %s was enrolled with the broker as %v, want no enrollment", token, enrolled)
	}
	if got := broker.calls(); got != calls {
		h.t.Errorf("the broker received %d calls from the refused registration, want none", got-calls)
	}
}

// wantRecords holds the claim's stored row to state and session.
func (h *harness) wantRecords(token claim.Token, state supervise.ClaimState, session string) {
	h.t.Helper()
	if got := h.stored(token); got.State != state || got.Session != session {
		h.t.Fatalf("%s is %s recording %q, want it %s recording %s", token, got.State, got.Session, state, session)
	}
}

// suspend has the operator suspend the claim.
func (h *harness) suspend(token claim.Token) {
	h.t.Helper()
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator}); err != nil {
		h.t.Fatalf("suspend %s: %v", token, err)
	}
}

// freshSession has the operator start the ready claim on a new session — suspended, its session
// forgotten, and resumed — into its issue's pod at the pod's process generation, and the new
// process's shim say hello. It returns the boot token of the launch.
func (h *harness) freshSession(token claim.Token, session string, generation uint64) string {
	h.t.Helper()
	h.suspend(token)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.RequestForgetSession{Claim: token, Session: session, By: supervise.SuspendedByOperator}); err != nil {
		h.t.Fatalf("forget %s's session %s: %v", token, session, err)
	}
	h.runtime.ScriptSpawn(fake.SpawnResult{Locator: podLocator(token, "pod-legion-901", generation)})
	if err := m.Handle(h.ctx, supervise.RequestResume{Claim: token, By: supervise.SuspendedByOperator}); err != nil {
		h.t.Fatalf("resume %s: %v", token, err)
	}
	h.hello(token, "pod-legion-901", "thumbprint-"+string(token))
	return h.bootToken(token)
}

// sessionHoldHarness is the API harness with a secrets broker, so the session every registration
// a machine accepts records is enrolled, as under runtime.kubernetes.agent_secrets.
func sessionHoldHarness(t *testing.T) (*harness, *enrollmentRecorder) {
	t.Helper()
	h := newHarness(t)
	broker := &enrollmentRecorder{}
	h.supervisor.deps.Secrets = broker
	return h, broker
}

// podRole launches role on issue as one role process of the issue's own pod, at the pod's process
// generation, so roles of one issue share the pod's uid as they do in a Sandbox, and has the
// shim say hello with the pod's identity (hello): the machine then holds every half of an
// enrollment but the session. It returns the claim and its launch's boot token.
func (h *harness) podRole(issue string, role claim.Role, generation uint64) (claim.Token, string) {
	h.t.Helper()
	token, err := claim.NewToken(testProject, issue, role)
	if err != nil {
		h.t.Fatalf("token: %v", err)
	}
	h.runtime.ScriptSpawn(fake.SpawnResult{Locator: podLocator(token, "pod-"+strings.ToLower(issue), generation)})
	launched, boot := h.launch(issue, role)
	if launched != token {
		h.t.Fatalf("launched %s, want %s", launched, token)
	}
	h.hello(token, "pod-"+strings.ToLower(issue), "thumbprint-"+string(token))
	return token, boot
}

// readyAt has the claim's agent say it is ready with the secret its registration was issued.
func (h *harness) readyAt(token claim.Token, session string, registered claim.RegisterResponse) {
	h.t.Helper()
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: registered.Generation,
	}, nil); recorder.Code != http.StatusNoContent {
		h.t.Fatalf("ready %s = %d; body %s", token, recorder.Code, recorder.Body)
	}
}

// podLocator is one role process of the issue pod podUID names, at the pod's process generation.
func podLocator(token claim.Token, podUID string, generation uint64) runtime.Locator {
	return runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(podUID, generation),
		Sandbox: &runtime.SandboxLocator{
			Namespace: "legion", Name: "legion-" + podUID, PodUID: podUID, Container: runtime.IssuePodContainer, Generation: generation,
		},
	}
}

// enrollmentRecorder is the secrets broker's enrollment routes as the daemon calls them,
// recording each enrollment and counting every call, revocations too. An enrollment naming
// another claim's session is the grant move itself: the broker binds the session to the pod's
// account and moves onto it every other enrollment's grants of that account and session. carried
// is what POST /v1/enrollments/carried answers for each session id (Carried), and carriedErr,
// when set, the error it answers instead.
type enrollmentRecorder struct {
	mu          sync.Mutex
	enrollments []supervise.PodEnrollment
	revocations int
	carried     map[string][]supervise.CarriedEnrollment
	carriedErr  error
	asked       int
}

func (e *enrollmentRecorder) Enroll(_ context.Context, p supervise.PodEnrollment) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enrollments = append(e.enrollments, p)
	return fmt.Sprintf("enr-%d", len(e.enrollments)), nil
}

func (e *enrollmentRecorder) Revoke(context.Context, string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revocations++
	return nil
}

func (e *enrollmentRecorder) Carried(_ context.Context, sessionIDs []string) ([]supervise.CarriedEnrollment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked++
	if e.carriedErr != nil {
		return nil, e.carriedErr
	}
	answer := []supervise.CarriedEnrollment{}
	for _, id := range sessionIDs {
		answer = append(answer, e.carried[id]...)
	}
	return answer, nil
}

// carry makes Carried answer c for its session id; endCarry makes it answer nothing for it.
func (e *enrollmentRecorder) carry(c supervise.CarriedEnrollment) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.carried == nil {
		e.carried = map[string][]supervise.CarriedEnrollment{}
	}
	e.carried[c.SessionID] = append(e.carried[c.SessionID], c)
}

// failCarried makes Carried answer err instead of its rows; nil gives them back.
func (e *enrollmentRecorder) failCarried(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.carriedErr = err
}

func (e *enrollmentRecorder) endCarry(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.carried, sessionID)
}

// calls is how many calls the daemon made of the broker, of any route.
func (e *enrollmentRecorder) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.enrollments) + e.revocations + e.asked
}

// enrolledAs is the session of every enrollment the claim's role made, in order: an issue pod's
// role enrolls under its pod's uid with a slot of its own role and process generation
// (supervise's ensureEnrolled), and the claims of these tests are of one role each.
func (e *enrollmentRecorder) enrolledAs(token claim.Token) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	role, _ := token.Role()
	var sessions []string
	for _, p := range e.enrollments {
		if strings.HasPrefix(p.Slot, string(role)+"-g") {
			sessions = append(sessions, p.Session)
		}
	}
	return sessions
}
