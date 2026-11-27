package supervise

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The agents of two steps in the identities a start names them by (plan.AgentID): the agent of
// step 1 of task a, which has no key, and the agent of the key b.
const (
	stepAgentA plan.AgentID = "step:1:a"
	keyAgentB  plan.AgentID = "key:b"
)

// The claim records the agent whose session it holds (Claim.SessionAgent), set by the decision
// that begins a session for a step's agent: a spawn of a queued claim for that agent records it,
// and the store holds it beside the session.
func TestASpawnForAStepsAgentRecordsTheAgentOfTheSession(t *testing.T) {
	h := newHarness(t)

	h.must(RequestSpawn{Claim: h.token, Start: StartWork{Agent: stepAgentA}})

	if got := h.claim().SessionAgent; got != stepAgentA {
		t.Fatalf("the claim spawned for %s records the agent %q, want %q", stepAgentA, got, stepAgentA)
	}
	if got := h.store.load(h.token).SessionAgent; got != stepAgentA {
		t.Fatalf("the store holds the agent %q, want %q", got, stepAgentA)
	}
}

// A workflow start names the agent of the step it brings its claim back for (StartWork.Agent),
// and the claim records the agent whose session it holds (Claim.SessionAgent). A start for
// another agent leaves that session behind in the decision that brings the claim back, recording
// the new agent with it, so the launch spawns that agent in a session of its own: the rule's resume
// of a suspended claim and the retry of a retired one alike. A start for the recorded agent and a
// start that names none resume the session; so does a start of a claim that records no agent, a
// claim the release before last ran, which then records the start's agent as the agent of the
// session it holds. The fault: a new agent on a held name continues the session of the agent that
// held it before, or an agent's own return is given a new session.
func TestAStartForAnotherAgentLeavesTheClaimsSessionBehind(t *testing.T) {
	for _, bring := range []struct {
		name  string
		stop  func(h *harness)
		start func(agent plan.AgentID) Event
	}{
		{
			name: "the rule's resume of a suspended claim",
			stop: func(h *harness) {
				h.must(RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "its task was stopped"})
				h.wantState(StateSuspended)
			},
			start: func(agent plan.AgentID) Event {
				return RequestRuleResume{Claim: testToken, Reason: "start row 9", Start: StartWork{Agent: agent}}
			},
		},
		{
			name: "the retry of a retired claim",
			stop: func(h *harness) {
				h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
				h.wantState(StateRetired)
			},
			start: func(agent plan.AgentID) Event { return RequestRetry{Claim: testToken, Start: StartWork{Agent: agent}} },
		},
	} {
		for _, tc := range []struct {
			name             string
			recorded, agent  plan.AgentID
			fresh            bool
			recordsAfterward plan.AgentID
		}{
			{name: "a start for another agent", recorded: stepAgentA, agent: keyAgentB, fresh: true, recordsAfterward: keyAgentB},
			{name: "control: a start for the agent of the session", recorded: keyAgentB, agent: keyAgentB, recordsAfterward: keyAgentB},
			{name: "control: a start that names no agent", recorded: stepAgentA, recordsAfterward: stepAgentA},
			{name: "a claim that records no agent", agent: keyAgentB, recordsAfterward: keyAgentB},
		} {
			t.Run(bring.name+"/"+tc.name, func(t *testing.T) {
				c := queuedClaim()
				c.SessionAgent = tc.recorded
				h := newHarnessOf(t, c)
				h.reach(StateIdle)
				bring.stop(h)
				spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))

				h.must(bring.start(tc.agent))

				got := h.claim()
				spawned, resumed := len(h.calls("Spawn"))-spawns, len(h.calls("Resume"))-resumes
				if tc.fresh {
					if spawned != 1 || resumed != 0 || got.Session != "" {
						t.Fatalf("the start for %s of a claim whose session is %s's made %d spawns and %d resumes, the claim recording session %q; want one spawn of a new session",
							tc.agent, tc.recorded, spawned, resumed, got.Session)
					}
					if got.Abandoned.Session != session {
						t.Fatalf("the claim records the abandoned session %+v, want %q left behind", got.Abandoned, session)
					}
				} else if resumed != 1 || spawned != 0 || got.Session != session {
					t.Fatalf("the start for %q of a claim whose session is %q's made %d spawns and %d resumes, the claim recording session %q; want the session %q resumed",
						tc.agent, tc.recorded, spawned, resumed, got.Session, session)
				}
				if got.SessionAgent != tc.recordsAfterward {
					t.Fatalf("the claim records the agent %q of its session, want %q", got.SessionAgent, tc.recordsAfterward)
				}
				if stored := h.store.load(h.token).SessionAgent; stored != tc.recordsAfterward {
					t.Fatalf("the store holds the agent %q of the claim's session, want %q", stored, tc.recordsAfterward)
				}
			})
		}
	}
}

// The agent a start for another step's agent starts is new on that step, not the agent of the
// session left behind: the claim records the session it left as the earlier agent's, another
// agent's than the one it records now (AbandonedSession.OfAnotherAgent), and the new agent's first
// task names that session, the issue's branch and the command that reads the earlier steps'
// records, the sentence abandonedTask gives a session of another agent. The control, a session the
// operator's fresh session left behind, is recorded as the same agent's. The fault: the session
// left behind was recorded as the new agent's own, which abandonedTask tells as its own lost work.
func TestTheFirstTaskOfANewAgentOnAHeldNameSaysItIsNewOnTheStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		leave   func(h *harness)
		another bool
	}{
		{name: "another agent's session", another: true, leave: func(h *harness) {
			h.must(RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "its task was stopped"})
			h.must(RequestRuleResume{Claim: testToken, Reason: "start row 9", Start: StartWork{Agent: keyAgentB}})
		}},
		{name: "control: the operator's fresh session", leave: func(h *harness) {
			h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
			h.must(RequestRetry{Claim: testToken, FreshSession: true})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := queuedClaim()
			c.SessionAgent = stepAgentA
			h := newHarnessOf(t, c)
			h.reach(StateIdle)
			tc.leave(h)
			if got := h.claim(); got.Abandoned.Session != session || got.Abandoned.Agent != stepAgentA || got.Abandoned.OfAnotherAgent(got.SessionAgent) != tc.another {
				t.Fatalf("the claim of agent %q records the abandoned session %+v, want %q left behind as %s's, another agent's = %v",
					got.SessionAgent, got.Abandoned, session, stepAgentA, tc.another)
			}
			h.registerFresh()
			base := len(h.prompts())

			h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})

			first := h.wantPrompts(base + 1)[base].Message
			if !strings.HasSuffix(first, "Continue Workflow. Issue: LEGION-209. Phase: implementing.") {
				t.Errorf("the first task is %q, want it to end with the task itself", first)
			}
			if tc.another {
				for _, want := range []string{strconv.Quote(session), "legion/LEGION-209", "dispatch handoff-read --issue LEGION-209 --phase", handoffWordsClause()} {
					if !strings.Contains(first, want) {
						t.Errorf("the first task of the new agent does not name %q:\n%s", want, first)
					}
				}
			}
		})
	}
}

// A start for another step's agent can find the claim holding no session at all: the session of
// the agent before was found in no store and forgotten, and every launch of that agent's fresh
// session failed before one registered. The claim then records that agent still, an abandoned
// session nobody was told of, and the cause of the forget that agent's launches were built with.
// The start for the new agent leaves no session behind, but what the claim keeps of that loss is
// the other agent's: the new agent's launch is built with no cause of a forget (Claim.SessionLost),
// the claim records the lost session as another agent's than the new one
// (AbandonedSession.OfAnotherAgent), the record abandonedTask tells a session of another agent by,
// and the first task names that session. The fault: the lost session was recorded with no agent,
// so the new agent was told, in its first task, that a session of its own was lost.
func TestANewAgentIsNotToldAnotherAgentsLostSessionAsItsOwn(t *testing.T) {
	c := byIDClaim(StateRetired, 3)
	c.SessionAgent = stepAgentA
	r := newByIDRig(t, c, nil)
	r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})
	r.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom})
	if err := r.handle(RequestRetry{Claim: testToken, Start: StartWork{Agent: stepAgentA}}); !errors.Is(err, errBoom) {
		t.Fatalf("the retry of %s whose fresh spawns the runtime refused = %v; want the runtime's last refusal", stepAgentA, err)
	}
	r.wantState(StateFailed)
	if got := r.claim(); got.Abandoned.Session != idSession || got.Session != "" || got.SessionAgent != stepAgentA {
		t.Fatalf("the failed claim abandoned %q, records session %q of agent %q; want %s abandoned, no session, and %s's",
			got.Abandoned.Session, got.Session, got.SessionAgent, idSession, stepAgentA)
	}
	from := len(r.rt.Calls())

	r.must(RequestRetry{Claim: testToken, Start: StartWork{Agent: keyAgentB}})

	starts := r.starts(from)
	if len(starts) != 1 || starts[0].Method != "Spawn" {
		t.Fatalf("the retry for %s made %s; want one Spawn", keyAgentB, startsText(starts))
	}
	if built := r.specs.last(t); built.Generation != starts[0].Spec.Generation || built.SessionLost != "" {
		t.Errorf("the spawn for %s was built at generation %d with SessionLost %q; want the spawn's, at %d, with no cause: the session lost was %s's",
			keyAgentB, built.Generation, built.SessionLost, starts[0].Spec.Generation, stepAgentA)
	}
	if got := r.claim(); got.SessionAgent != keyAgentB || got.Abandoned.Agent != stepAgentA || !got.Abandoned.OfAnotherAgent(got.SessionAgent) {
		t.Fatalf("the claim records the agent %q of its session and the lost session %+v; want %s's, and %s lost as %s's, another agent's",
			got.SessionAgent, got.Abandoned, keyAgentB, idSession, stepAgentA)
	}
	r.registerFresh()
	base := len(r.prompts())

	r.must(RequestDeliver{Claim: testToken, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})

	first := r.wantPrompts(base + 1)[base].Message
	if !strings.Contains(first, strconv.Quote(idSession)) {
		t.Errorf("the first task of %s does not name %s, the session of the agent before:\n%s", keyAgentB, idSession, first)
	}
}

// A start that the admission refuses changes nothing of the claim, its session among it: a start
// for another agent, and a retry that asks for a new session, leave the session behind only once
// the claim is admitted. The admission refuses while the tree's cleanup is reserved; the claim
// keeps its session, the agent of that session and no abandoned session, and that agent's own
// start after the cleanup resumes the session. The fault: the session was dropped before the
// admission, so the refused start left the claim with no session, and the agent's own return was
// spawned in a new one and told its own session was another agent's.
func TestARefusedAdmissionLeavesTheClaimsSessionWhereItWas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stop  func(h *harness)
		start Event
	}{
		{
			name: "the rule's resume for another agent",
			stop: func(h *harness) {
				h.must(RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "its task was stopped"})
			},
			start: RequestRuleResume{Claim: testToken, Reason: "start row 9", Start: StartWork{Agent: keyAgentB}},
		},
		{
			name:  "the retry for another agent",
			stop:  func(h *harness) { h.must(RequestStop{Claim: h.token, By: SuspendedByOperator}) },
			start: RequestRetry{Claim: testToken, Start: StartWork{Agent: keyAgentB}},
		},
		{
			name:  "the retry that asks for a new session",
			stop:  func(h *harness) { h.must(RequestStop{Claim: h.token, By: SuspendedByOperator}) },
			start: RequestRetry{Claim: testToken, FreshSession: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &epochStore{epoch: 1}
			c := queuedClaim()
			c.SessionAgent = stepAgentA
			h := lifecycleHarness(t, store, c)
			h.reach(StateIdle)
			tc.stop(h)
			stopped := h.claim().State
			store.reserved = true

			if err := h.handle(tc.start); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
				t.Fatalf("the start while the tree's cleanup is reserved = %v, want the reservation's refusal", err)
			}
			for _, got := range []struct {
				where string
				claim Claim
			}{{"the machine", h.claim()}, {"the store", h.store.load(h.token)}} {
				if got.claim.State != stopped || got.claim.Session != session || got.claim.SessionAgent != stepAgentA || got.claim.Abandoned != (AbandonedSession{}) {
					t.Fatalf("after the refused start %s holds the claim %s in session %q of agent %q, abandoned %+v; want it %s in session %q of agent %q, nothing abandoned",
						got.where, got.claim.State, got.claim.Session, got.claim.SessionAgent, got.claim.Abandoned, stopped, session, stepAgentA)
				}
			}

			store.reserved = false
			spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))
			var own Event = RequestRetry{Claim: testToken, Start: StartWork{Agent: stepAgentA}}
			if stopped == StateSuspended {
				own = RequestRuleResume{Claim: testToken, Reason: "start row 10", Start: StartWork{Agent: stepAgentA}}
			}
			h.must(own)
			if spawned, resumed := len(h.calls("Spawn"))-spawns, len(h.calls("Resume"))-resumes; spawned != 0 || resumed != 1 || h.claim().Session != session {
				t.Fatalf("the agent's own start after the cleanup made %d spawns and %d resumes in session %q, want its session %q resumed",
					spawned, resumed, h.claim().Session, session)
			}
		})
	}
}

// Leaving another agent's session is a step of the admission itself (revive), not one each
// decision that brings the claim back must remember to ask for: a decision that brings the claim
// back for another step's agent and gives revive nothing to run once the claim is admitted still
// leaves the session the claim holds for the agent before, so the launch spawns the new agent in a
// session of its own. The control, a start for the agent of the session, resumes it. The fault:
// the session was left only where the caller asked for it, so a caller that did not recorded the
// new agent as the agent of the old agent's session and resumed that session.
func TestReviveLeavesAnotherAgentsSessionWhoeverCallsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent plan.AgentID
		fresh bool
	}{
		{name: "a start for another agent", agent: keyAgentB, fresh: true},
		{name: "control: a start for the agent of the session", agent: stepAgentA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := queuedClaim()
			c.SessionAgent = stepAgentA
			h := newHarnessOf(t, c)
			h.reach(StateIdle)
			h.must(RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "its task was stopped"})
			h.wantState(StateSuspended)
			spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))

			if err := h.insideDecision(h.m, func() error {
				return h.m.revive(h.ctx, "a decision that runs nothing at the admission", false, nil, StartWork{Agent: tc.agent}, nil)
			}); err != nil {
				t.Fatalf("revive for %s = %v, want the claim brought back", tc.agent, err)
			}

			got := h.claim()
			spawned, resumed := len(h.calls("Spawn"))-spawns, len(h.calls("Resume"))-resumes
			if tc.fresh {
				if spawned != 1 || resumed != 0 || got.Session != "" || got.Abandoned.Session != session {
					t.Fatalf("revive for %s of a claim whose session is %s's made %d spawns and %d resumes, the claim recording session %q and abandoned %q; want one spawn of a new session and %q left behind",
						tc.agent, stepAgentA, spawned, resumed, got.Session, got.Abandoned.Session, session)
				}
			} else if spawned != 0 || resumed != 1 || got.Session != session {
				t.Fatalf("revive for %s of a claim whose session is its own made %d spawns and %d resumes, the claim recording session %q; want the session %q resumed",
					tc.agent, spawned, resumed, got.Session, session)
			}
			if got.SessionAgent != tc.agent {
				t.Fatalf("the claim records the agent %q of its session, want %q", got.SessionAgent, tc.agent)
			}
		})
	}
}
