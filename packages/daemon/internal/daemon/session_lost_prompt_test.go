package daemon

// The fourth place a lost session is reported is the session that replaces it
// (dispatch://INFRA-714 R8): nothing in a new session says an older one existed, so an agent
// started on a claim whose session is gone would read an empty transcript as a fresh start and
// redo work that is already pushed.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestTheAddressingTellsAFreshAgentThatItsSessionWasLost(t *testing.T) {
	t.Parallel()
	token, err := claim.NewToken("legion", "LEGION-209", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	c := supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleTester}

	kept, err := AddressingFragment("legion", c, ghrepo.Repository{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(kept, "no session store") {
		t.Fatalf("a claim whose session is intact is told %q, want nothing of a lost session", kept)
	}

	c.SessionLost = supervise.SessionLossGone
	lost, err := AddressingFragment("legion", c, ghrepo.Repository{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"found in no session store", "only what you pushed and committed survives", "tell the architect named above what you found"} {
		if !strings.Contains(lost, want) {
			t.Errorf("the fresh agent is told %q, want it to say %q", lost, want)
		}
	}
}

// A launch that has Oh My Pi resume the session by its id through an older pod launcher is told
// that its workspace may have been recreated, holding what was pushed to the claim's own branch:
// the branch of the task its pending delivery is of (supervise.Delivery.Branch), and the issue's
// own for a delivery that names none or a claim that holds none. For an agent on a task's own
// branch, legion/LEGION-209.b, the issue's branch is another task's.
//
// The fault it catches is a resumed agent of a task told that its workspace holds the issue's
// branch (its control: the sentence naming claim.IssueBranch, under which the agent of b is told
// legion/LEGION-209).
func TestTheRecreatedWorkspaceSentenceNamesTheClaimsOwnBranch(t *testing.T) {
	t.Parallel()
	token, err := claim.NewToken("legion", "LEGION-209", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	resumed := supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleTester,
		Generation: 4, AgentResumeGeneration: 4}
	for _, tc := range []struct {
		name    string
		pending *supervise.Delivery
		branch  string
	}{
		{"a task on its own branch", &supervise.Delivery{ID: "outbox:7", Task: "test it", Branch: "legion/LEGION-209.b"}, "legion/LEGION-209.b"},
		{"a task on the issue's branch", &supervise.Delivery{ID: "outbox:7", Task: "test it"}, "legion/LEGION-209"},
		{"a claim that holds no delivery", nil, "legion/LEGION-209"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := resumed
			c.Pending = tc.pending
			told, err := AddressingFragment("legion", c, ghrepo.Repository{}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := "it holds what was pushed to " + tc.branch + " ("; !strings.Contains(told, want) {
				t.Errorf("the resumed agent is told %q, want it to say %q", told, want)
			}
		})
	}
}
