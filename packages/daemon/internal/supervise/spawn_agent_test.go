package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// A claim queued again after a launch (ReleasedLaunch) still records the session it ran, and the
// agent of that session (Claim.SessionAgent). A spawn of it for another step's agent leaves that
// session behind, as every start of another agent does (revive): the launch
// spawns the new agent in a session of its own and the claim records the new agent. The control,
// a spawn for the agent of the session, resumes it and keeps the record. The fault: the spawn
// brought another agent back into the session of the agent that held the name before.
func TestASpawnOfAReleasedLaunchForAnotherAgentLeavesItsSessionBehind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent plan.AgentID
		fresh bool
	}{
		{name: "a spawn for another agent", agent: keyAgentB, fresh: true},
		{name: "control: a spawn for the agent of the session", agent: stepAgentA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			uncertain := queuedClaim()
			uncertain.State, uncertain.Generation = StateLaunchUncertain, 1
			uncertain.Session, uncertain.SessionFile, uncertain.SessionAgent = session, sessionFile, stepAgentA
			if err := h.store.PutClaim(h.ctx, uncertain); err != nil {
				t.Fatal(err)
			}
			h.start(uncertain)
			if released, err := h.m.ReleaseUncertainLaunch(h.ctx); err != nil || !released {
				t.Fatalf("release = %t, %v; want the uncertain launch released", released, err)
			}
			if c := h.claim(); !c.ReleasedLaunch() || c.Session != session {
				t.Fatalf("the released claim = %+v, want a released launch that records the session %q", c, session)
			}

			h.must(RequestSpawn{Claim: h.token, Start: StartWork{Agent: tc.agent}})

			got := h.claim()
			spawned, resumed := len(h.calls("Spawn")), len(h.calls("Resume"))
			if tc.fresh {
				if spawned != 1 || resumed != 0 || got.Session != "" {
					t.Fatalf("the spawn for %s of a claim whose session is %s's made %d spawns and %d resumes, the claim recording session %q; want one spawn of a new session",
						tc.agent, stepAgentA, spawned, resumed, got.Session)
				}
				if got.Abandoned.Session != session || got.Abandoned.Agent != stepAgentA || !got.Abandoned.OfAnotherAgent(got.SessionAgent) {
					t.Fatalf("the claim of agent %q records the abandoned session %+v, want %q left behind as %s's", got.SessionAgent, got.Abandoned, session, stepAgentA)
				}
			} else if resumed != 1 || spawned != 0 || got.Session != session {
				t.Fatalf("the spawn for %s of a claim whose session is %s's made %d spawns and %d resumes, the claim recording session %q; want the session %q resumed",
					tc.agent, stepAgentA, spawned, resumed, got.Session, session)
			}
			if got.SessionAgent != tc.agent {
				t.Fatalf("the claim records the agent %q of its session, want %q", got.SessionAgent, tc.agent)
			}
			if stored := h.store.load(h.token).SessionAgent; stored != tc.agent {
				t.Fatalf("the store holds the agent %q of the claim's session, want %q", stored, tc.agent)
			}
		})
	}
}
