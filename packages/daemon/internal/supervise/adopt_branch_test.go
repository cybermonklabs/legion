package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The adoption a phase task makes is of the working copy of the branch its start named
// (Delivery.Branch), the branch of the task its step is of, and it is the one each send of that
// task makes again, after a restart of the daemon too, since the claim keeps the delivery. A task
// of no branch adopts the issue's own working copy, as every task did before tasks had branches
// of their own.
func TestTheAdoptionIsOfTheDeliverysBranch(t *testing.T) {
	const branch = "legion/LEGION-208.b"
	h := adoptingHarness(t)
	h.must(RequestDeliver{Claim: testToken, Task: "implement task b", Phase: phase.Implementing, Branch: branch})
	adoptions := h.wantCalls("AdoptWorkingCopy", 1)
	if adoptions[0].Branch != branch {
		t.Fatalf("the adoption names the branch %q, want the delivery's, %q", adoptions[0].Branch, branch)
	}
	if p := h.pending(); p.Branch != branch {
		t.Fatalf("the pending delivery names the branch %q, want %q: the claim keeps it for each send", p.Branch, branch)
	}
	h.restart()
	if p := h.pending(); p.Branch != branch {
		t.Fatalf("after a restart the pending delivery names the branch %q, want %q", p.Branch, branch)
	}

	plain := adoptingHarness(t)
	plain.must(RequestDeliver{Claim: testToken, Task: "implement it", Phase: phase.Implementing})
	if adoptions := plain.wantCalls("AdoptWorkingCopy", 1); adoptions[0].Branch != "" {
		t.Fatalf("the adoption of a task of no branch names %q, want none: the issue's own working copy", adoptions[0].Branch)
	}
}

// A fresh session after an abandoned one is told to read the branch its task works on: the
// delivery's branch, else the issue's own.
func TestTheAbandonedSessionsSentenceNamesTheDeliverysBranch(t *testing.T) {
	c := Claim{Token: testToken, Issue: "LEGION-208", Role: "implementer", Abandoned: AbandonedSession{Session: "ses-1", Why: "too large"}}
	for branch, want := range map[string]string{"legion/LEGION-208.b": "your branch legion/LEGION-208.b ", "": "your branch legion/LEGION-208 "} {
		if got := abandonedTask(c, branch, true); !strings.Contains(got, want) {
			t.Fatalf("the abandoned session's sentence for the branch %q = %q, want it to name %q", branch, got, want)
		}
	}
}
