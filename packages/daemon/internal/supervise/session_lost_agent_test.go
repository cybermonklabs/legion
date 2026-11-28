package supervise

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// Key a's claim `…-implementer` works the tester's step, so it holds the role tester, and the
// session it recorded is in no store. The start runs a fresh agent for the key: the same claim with
// its role and no session to resume, told to the workflow once with the claim. The fresh agent
// records its session, and the key's next step, on which the claim takes the implementer's role
// again, resumes that session: nothing asks for the lost one again or starts a second fresh agent.
func TestALostSessionOfAKeyedAgentStartsItFreshForItsKey(t *testing.T) {
	const freshSession, freshFile = "ses_fresh", "/sessions/ses_fresh.jsonl"
	h := newBareHarness(t)
	var reported []Claim
	h.deps.SessionLost = func(c Claim, _ SessionLoss, _ string) { reported = append(reported, c) }
	keyed := queuedClaim()
	keyed.Role = claim.RoleTester
	h.store.PutClaim(h.ctx, keyed)
	h.start(keyed)
	h.reach(StateIdle)
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no OMP session anywhere", testToken, runtime.ErrSessionFileMissing)})

	h.observe(runtime.Gone)

	fresh := h.wantCalls("Spawn", 2)[1]
	if fresh.Spec.Claim != testToken || fresh.Spec.Role != claim.RoleTester || fresh.Spec.ResumeSessionFile != "" {
		t.Fatalf("the fresh start is %+v, want %s as the tester resuming nothing", fresh.Spec, testToken)
	}
	if len(reported) != 1 || reported[0].Token != testToken {
		t.Fatalf("the lost session was told %d times (%+v), want once with %s", len(reported), reported, testToken)
	}

	h.connect()
	h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: freshSession, SessionFile: freshFile,
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
	h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: freshSession})
	if c := h.claim(); c.Session != freshSession || c.SessionFile != freshFile {
		t.Fatalf("the claim records %q at %q, want the fresh agent's session", c.Session, c.SessionFile)
	}

	// A failed review sends the work back to step 1, the implementer's, which key a's agent does.
	h.must(RequestSuspend{Claim: testToken, By: SuspendedByDaemon})
	if err := h.m.Rerole(stepStands(context.Background()), claim.RoleImplementer); err != nil {
		t.Fatalf("give key a's claim the implementer's role: %v", err)
	}
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	resumed := h.wantCalls("Resume", 2)[1]
	if resumed.Spec.Role != claim.RoleImplementer || resumed.Spec.ResumeSessionFile != freshFile {
		t.Fatalf("the key's next step started %+v, want the implementer resuming the fresh session %s", resumed.Spec, freshFile)
	}
	if spawns := h.calls("Spawn"); len(spawns) != 2 {
		t.Fatalf("spawns = %d, want no second fresh agent", len(spawns))
	}
	if len(reported) != 1 {
		t.Fatalf("the lost session was told %d times, want once", len(reported))
	}
}
