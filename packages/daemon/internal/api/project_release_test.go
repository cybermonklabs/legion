package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// projectReleaseHarness is the daemon both doors of every start are driven against, with the
// project release's mark the test sets (Options.ProjectReleaseHolds): the operator's bearer and a
// controller grant on one server, with the workflow's phase restarts recorded, as the fresh
// session's and the compaction's own harnesses build it.
func projectReleaseHarness(t *testing.T) (*harness, *atomic.Bool) {
	t.Helper()
	h := newHarness(t)
	released := &atomic.Bool{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		Handlers: []intake.Handler{&phaseRestarter{}}, RoleFor: phase.RoleFor,
		ProjectReleaseHolds: released.Load,
	}).Handler
	return h, released
}

// wantReleaseRefusal holds recorder to the project release's refusal: 409, its code, and a
// sentence that names the request and the release.
func wantReleaseRefusal(t *testing.T, recorder *httptest.ResponseRecorder, request string) {
	t.Helper()
	if recorder.Code != http.StatusConflict {
		t.Fatalf("%s while the project is released = %d, want 409; body %s", request, recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != projectReleased || failure.Error != request+" refused: "+ProjectReleaseHeldSentence {
		t.Fatalf("%s while the project is released answered %+v, want code %s and %q", request, failure, projectReleased,
			request+" refused: "+ProjectReleaseHeldSentence)
	}
}

// dispatch://INFRA-3583 R7: while the daemon's project release holds every start, the operator's
// spawn, retry, resume and deliver, and either door's fresh session and compaction, are refused
// 409 with a sentence naming the release, and the claim is asked nothing: no runtime call, no
// task, no state change. The same request on the same claim with the mark clear goes through, so
// the refusal is the mark's and nothing else's.
func TestProjectReleaseRefusesEveryStartAndPromptOfARole(t *testing.T) {
	t.Run("spawn", func(t *testing.T) {
		h, released := projectReleaseHarness(t)
		released.Store(true)
		wantReleaseRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()), "spawn")
		if _, ok := h.supervisor.Machine(architectToken); ok {
			t.Fatal("the refused spawn created the claim")
		}
		if calls := h.runtime.Methods(); len(calls) != 0 {
			t.Fatalf("the refused spawn asked the runtime %v, want nothing", calls)
		}
		released.Store(false)
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("spawn with the mark clear = %d, want 201; body %s", recorder.Code, recorder.Body)
		}
		if spawns := h.runtime.CallsOf("Spawn"); len(spawns) != 1 {
			t.Fatalf("spawns with the mark clear = %d, want 1", len(spawns))
		}
	})

	t.Run("deliver", func(t *testing.T) {
		h, released := projectReleaseHarness(t)
		token := readyImplementer(t, h)
		conn := connected(h, token)
		released.Store(true)
		route := "/legion/v1/operator/claims/" + string(token) + "/deliver"
		wantReleaseRefusal(t, h.operator(http.MethodPost, route, DeliverRequest{Task: "Continue implementing."}), "deliver")
		if pending := h.stored(token).Pending; pending != nil {
			t.Fatalf("the refused deliver left the task %+v on the claim", pending)
		}
		released.Store(false)
		if recorder := h.operator(http.MethodPost, route, DeliverRequest{Task: "Continue implementing."}); recorder.Code != http.StatusOK {
			t.Fatalf("deliver with the mark clear = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		settled(h, token)
		if prompts := conn.Prompts(); len(prompts) != 1 {
			t.Fatalf("the connection was sent %d prompts with the mark clear, want the task", len(prompts))
		}
	})

	t.Run("resume", func(t *testing.T) {
		h, released := projectReleaseHarness(t)
		token := suspendedImplementer(t, h, supervise.SuspendedByOperator)
		resumes := len(h.runtime.CallsOf("Resume"))
		released.Store(true)
		route := "/legion/v1/operator/claims/" + string(token) + "/resume"
		wantReleaseRefusal(t, h.operator(http.MethodPost, route, nil), "resume")
		if got := h.stored(token); got.State != supervise.StateSuspended || len(h.runtime.CallsOf("Resume")) != resumes {
			t.Fatalf("after the refused resume the claim is %s with %d resumes, want it suspended and none", got.State,
				len(h.runtime.CallsOf("Resume"))-resumes)
		}
		released.Store(false)
		if recorder := h.operator(http.MethodPost, route, nil); recorder.Code != http.StatusOK {
			t.Fatalf("resume with the mark clear = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		if got := len(h.runtime.CallsOf("Resume")) - resumes; got != 1 {
			t.Fatalf("resumes with the mark clear = %d, want 1", got)
		}
	})

	t.Run("retry", func(t *testing.T) {
		h, released := projectReleaseHarness(t)
		h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
		h.ready(architectToken, "ses_architect", 1)
		refused := fake.SpawnResult{Err: errors.New("the runtime refused the relaunch")}
		h.runtime.ScriptResume(refused, refused)
		machine, _ := h.supervisor.Machine(architectToken)
		gone := runtime.Observation{Locator: *machine.Claim().Locator, Kind: runtime.Gone}
		if err := machine.Handle(h.ctx, supervise.RuntimeObservation{Observation: gone}); err == nil {
			t.Fatal("the relaunches the runtime refused answered no error")
		}
		failed := h.stored(architectToken)
		if failed.State != supervise.StateFailed {
			t.Fatalf("the architect is %s, want failed", failed.State)
		}
		released.Store(true)
		route := "/legion/v1/operator/claims/" + string(architectToken) + "/retry"
		wantReleaseRefusal(t, h.operator(http.MethodPost, route, nil), "retry")
		if got := h.stored(architectToken); got.State != supervise.StateFailed || got.Generation != failed.Generation {
			t.Fatalf("after the refused retry the architect is %s at generation %d, want failed at %d", got.State, got.Generation, failed.Generation)
		}
		released.Store(false)
		if recorder := h.operator(http.MethodPost, route, nil); recorder.Code != http.StatusOK {
			t.Fatalf("retry with the mark clear = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		if got := h.stored(architectToken); got.State != supervise.StateLaunching || got.Generation != failed.Generation+1 {
			t.Fatalf("after the retry the architect is %s at generation %d, want launching at %d", got.State, got.Generation, failed.Generation+1)
		}
	})

	for _, door := range freshSessionDoors() {
		t.Run("fresh session: "+door.name, func(t *testing.T) {
			h, released := projectReleaseHarness(t)
			token := idleOnTheLadder(t, h)
			before := h.stored(token)
			released.Store(true)
			// The operator's door is the retry route, which refuses as the retry before its
			// effect is reached; the controller's door reaches the effect, which refuses as the
			// fresh session.
			request := "fresh-session"
			if strings.Contains(door.name, "retry") {
				request = "retry"
			}
			wantReleaseRefusal(t, door.ask(t, h, "LEGION-209", claim.RoleImplementer, nil), request)
			if got := h.stored(token); got.Session != before.Session || got.State != before.State || got.Generation != before.Generation {
				t.Fatalf("after the refused fresh session the claim is %s at generation %d on %q, want it untouched: %s at %d on %q",
					got.State, got.Generation, got.Session, before.State, before.Generation, before.Session)
			}
			released.Store(false)
			if recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer, nil); recorder.Code != http.StatusOK {
				t.Fatalf("fresh session with the mark clear = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
			if got := h.stored(token); got.Session == before.Session {
				t.Fatalf("the fresh session with the mark clear kept session %q", got.Session)
			}
		})
	}

	for _, door := range compactDoors() {
		t.Run("compaction: "+door.name, func(t *testing.T) {
			h, released := projectReleaseHarness(t)
			token := readyImplementer(t, h)
			conn := connected(h, token)
			released.Store(true)
			wantReleaseRefusal(t, door.ask(t, h, "LEGION-209", claim.RoleImplementer), "compact")
			if got := len(conn.Compactions()); got != 0 {
				t.Fatalf("the refused compaction asked Oh My Pi for %d compactions, want none", got)
			}
			released.Store(false)
			if answer := decodeCompaction(t, door.ask(t, h, "LEGION-209", claim.RoleImplementer)); !answer.Compacted {
				t.Fatalf("the compaction with the mark clear answered %+v, want it compacted", answer)
			}
			if got := len(conn.Compactions()); got != 1 {
				t.Fatalf("Oh My Pi was asked for %d compactions with the mark clear, want one", got)
			}
		})
	}
}

// The routes the project release does not hold still answer while it holds: an operator's suspend
// and stop end processes and start none, which is what a release needs of a left pod's claims.
func TestProjectReleaseLeavesTheSuspendOfAClaimToTheOperator(t *testing.T) {
	t.Run("suspend", func(t *testing.T) {
		h, released := projectReleaseHarness(t)
		token := readyImplementer(t, h)
		released.Store(true)
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusOK {
			t.Fatalf("suspend while the project is released = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByOperator {
			t.Fatalf("after the suspend the claim is %s by %q, want suspended by the operator", got.State, got.SuspendedBy)
		}
	})
	t.Run("stop", func(t *testing.T) {
		h, released := projectReleaseHarness(t)
		token := readyImplementer(t, h)
		released.Store(true)
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil); recorder.Code != http.StatusOK {
			t.Fatalf("stop while the project is released = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		if state := h.stored(token).State; state != supervise.StateRetired {
			t.Fatalf("after the stop the claim is %s, want it retired", state)
		}
	})
}

// dispatch://INFRA-3583 R7, at the controller's own doors: while the daemon's project release
// holds every start, the controller's resume of an agent it suspended, its retry of an agent its
// stop retired, and its pod update of an issue are refused 409 PROJECT_RELEASED with a sentence
// naming the release (releaseRefused in resumeAgent's event builder, in retryAgent and in
// issuePodUpdate), and nothing is asked of a claim: no claim changes and the runtime is asked
// nothing. The controller's suspend and stop start nothing and still run. With the mark clear the
// same resume and retry start their agents, and the same pod update reaches this daemon's own
// answer, so the refusal is the mark's and nothing else's.
//
// The negative control is any of the three releaseRefused calls undone: the resume and the retry
// reach the agent and are answered 200, and the pod update is answered 404 NO_POD by this daemon,
// which runs no pod update rounds.
func TestProjectReleaseRefusesTheControllersResumeRetryAndPodUpdate(t *testing.T) {
	h, released := projectReleaseHarness(t)
	// The implementer the controller suspended, which its resume would lift, and the tester its
	// stop retired, which its retry would start.
	implementer := controllerSuspendedImplementer(t, h)
	tester, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleTester)
	h.ready(tester, "ses_tester", 1)
	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/stop", "LEGION-209", claim.RoleTester),
		http.StatusOK, AgentActionResponse{State: "retired"})
	reviewer, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleReviewer)
	h.ready(reviewer, "ses_reviewer", 1)
	planner, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RolePlanner)
	h.ready(planner, "ses_planner", 1)

	grant := controllerGrant(t, h).GrantID
	released.Store(true)
	before, err := h.store.Claims(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	calls := len(h.runtime.Calls())
	wantReleaseRefusal(t, h.request(http.MethodPost, "/legion/v1/agents/resume",
		ClaimRequest{GrantID: grant, Issue: "LEGION-209", Role: string(claim.RoleImplementer)}, nil), "resume")
	wantReleaseRefusal(t, h.request(http.MethodPost, "/legion/v1/agents/retry",
		ClaimRequest{GrantID: grant, Issue: "LEGION-209", Role: string(claim.RoleTester)}, nil), "retry")
	wantReleaseRefusal(t, askPodUpdate(h, grant, "LEGION-209"), "the pod update of LEGION-209")
	after, err := h.store.Claims(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused requests changed the claims:\nbefore %+v\nafter  %+v", before, after)
	}
	if got := len(h.runtime.Calls()); got != calls {
		t.Fatalf("the refused requests reached the runtime: %v", h.runtime.Methods()[calls:])
	}

	// A suspension and a stop start nothing, and still run: the controller's suspend and stop.
	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/suspend", "LEGION-209", claim.RoleReviewer),
		http.StatusOK, AgentActionResponse{State: "suspended", SuspendedBy: "controller"})
	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/stop", "LEGION-209", claim.RolePlanner),
		http.StatusOK, AgentActionResponse{State: "retired"})

	released.Store(false)
	resumes := len(h.runtime.CallsOf("Resume"))
	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/resume", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, AgentActionResponse{State: "launching"})
	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/retry", "LEGION-209", claim.RoleTester),
		http.StatusOK, AgentActionResponse{State: "launching"})
	if got := len(h.runtime.CallsOf("Resume")) - resumes; got != 2 {
		t.Fatalf("the resume and the retry with the mark clear made %d resumes of a session, want 2: the implementer's and the tester's", got)
	}
	if got := h.stored(implementer); got.State != supervise.StateLaunching {
		t.Fatalf("after the resume with the mark clear the implementer is %s, want launching", got.State)
	}
	refused(t, askPodUpdate(h, grant, "LEGION-209"), http.StatusNotFound, "NO_POD")
}
