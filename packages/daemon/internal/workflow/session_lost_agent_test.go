package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// Key a's agent, named `implementer`, lost its session on its tester step. The owner is told which
// agent lost it, on which step, and that each later step of that agent runs in the fresh session,
// so it can tell that key a's later steps start fresh too. A claim named by its role keeps the
// sentence it had: the tester of LEGION-209 lost its session.
func TestTheOwnerIsToldWhichAgentLostItsSession(t *testing.T) {
	const detail = "the session ses-abc this claim recorded is in no session store the runtime could resume it from"
	for _, tc := range []struct {
		name  string
		claim claim.Token
		want  string
	}{
		{"key a's agent on its tester step", "legion-legion-legion-209-implementer",
			`The agent implementer of LEGION-209, on its tester step, lost the session it recorded: "` + detail + `". It is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives. Each later step of that agent runs in the fresh session.`},
		{"the tester's own claim", "legion-legion-legion-209-tester",
			`The tester of LEGION-209 lost the session it recorded: "` + detail + `". It is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedTestingIssue(t, pool, "LEGION-209")

			if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "session-lost:"+string(tc.claim)+":3", intake.SessionLost{
				Issue: "LEGION-209", Role: claim.RoleTester, Claim: tc.claim, Detail: detail,
			}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the lost session: %v", err)
			}

			if posted := messageBodies(t, pool); len(posted) != 1 || posted[0] != tc.want {
				t.Fatalf("the issue was told %q, want %q", posted, tc.want)
			}
			if reason := noticeKindReasons(t, pool, "LEGION-209")["session-lost"]; reason != tc.want {
				t.Fatalf("the owner was told %q, want %q", reason, tc.want)
			}
		})
	}
}
