package supervise

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The heads a fix's task names: the head the fix's working copy is made at, and the head the
// task's working copy moves to first; and the fix's own branch.
const (
	fixFromHead   = "c0de000000000000000000000000000000000001"
	moveToHead    = "c0de000000000000000000000000000000000002"
	fixBranchName = "legion-beside/LEGION-208/5/default/fix-1/implementer"
)

// olderShimLine is the one line a send writes when the issue's pod runs a shim built before an
// adoption named a fix's commit or the task's move: the task waits, charged nothing, for the pod's
// update round. It is pinned whole, as the adoption's other lines are (adoptedLine).
const olderShimLine = "supervise: the issue's pod runs a shim that does not make a fix's working copy or move the task's; the task waits, charged nothing, until the pod's update round replaces it"

// adoptingBot is the App identity the adopting harnesses mint for the implementer.
var adoptingBot = runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}

// fixTask is a fix's task as the start executor hands it to its claim (the daemon's startTask):
// phase implementing, run 5, fix 1, the head the fix's working copy is made at, the fix's own
// branch and the head the task's working copy moves to.
func fixTask() RequestDeliver {
	return RequestDeliver{Claim: testToken, Task: "Fix the CI check k-ci, which failed at " + fixFromHead + ".",
		Phase: phase.Implementing, Generation: 5, Fix: 1, FixFrom: fixFromHead, FixBranch: fixBranchName, MoveTo: moveToHead}
}

// promptCountingRuntime is the fake runtime with one thing more: each adoption it is asked for
// notes how many prompts the agent's connection had been sent when it was asked, so a test reads
// whether the adoption came before the task's send.
type promptCountingRuntime struct {
	*fake.Runtime
	conn *fake.Conn

	mu   sync.Mutex
	sent []int
}

func (r *promptCountingRuntime) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity, adoption runtime.Adoption) (runtime.Adopted, error) {
	r.mu.Lock()
	r.sent = append(r.sent, len(r.conn.Prompts()))
	r.mu.Unlock()
	return r.Runtime.AdoptWorkingCopy(ctx, loc, id, adoption)
}

// promptsAtAdoptions is how many prompts had been sent at each adoption, in order.
func (r *promptCountingRuntime) promptsAtAdoptions() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sent)
}

// fixAdoptingHarness is adoptingHarness over the prompt-counting runtime: a ready claim whose
// role has an App identity to adopt its working copy with, holding nothing.
func fixAdoptingHarness(t *testing.T) (*harness, *promptCountingRuntime) {
	t.Helper()
	h := newHarness(t)
	h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) { return adoptingBot, nil }
	counting := &promptCountingRuntime{Runtime: h.rt, conn: h.conn}
	h.deps.Runtime = counting
	h.restart()
	h.reach(StateReady)
	return h, counting
}

// A fix's task carries its fix, the head its working copy is made at, the fix's own branch and the
// head the task's working copy moves to, from the start that queued it into the delivery the claim
// holds. The send adopts first: the runtime is asked to make the fix's working copy at that head,
// or at the branch where origin holds it, and to move the task's, under the role's identity, before
// the task's prompt goes out, and the task is sent once the shim echoes them.
func TestAFixDeliveryAdoptsItsCopyFirst(t *testing.T) {
	h, counting := fixAdoptingHarness(t)

	h.must(fixTask())

	prompts := h.wantPrompts(1)
	p := h.pending()
	if p.Fix != 1 || p.FixFrom != fixFromHead || p.MoveTo != moveToHead || p.FixBranch != fixBranchName {
		t.Fatalf("the pending delivery holds fix %d from %q on %q moving to %q, want fix 1 from %s on %s moving to %s",
			p.Fix, p.FixFrom, p.FixBranch, p.MoveTo, fixFromHead, fixBranchName, moveToHead)
	}
	if prompts[0].DeliveryID != p.ID {
		t.Fatalf("the prompt carries delivery %s, want the fix's task %s", prompts[0].DeliveryID, p.ID)
	}
	adoptions := h.rt.CallsOf("AdoptWorkingCopy")
	want := runtime.Adoption{FixFrom: fixFromHead, MoveTo: moveToHead, FixBranch: fixBranchName}
	if len(adoptions) != 1 || adoptions[0].Adoption != want || adoptions[0].Identity != adoptingBot {
		t.Fatalf("adoptions = %+v, want one naming %+v under %v", adoptions, want, adoptingBot)
	}
	if sent := counting.promptsAtAdoptions(); !slices.Equal(sent, []int{0}) {
		t.Fatalf("the adoptions were asked with %v prompts sent, want one adoption, asked before any prompt", sent)
	}
	if lines := h.records.of(olderShimLine); len(lines) != 0 {
		t.Fatalf("%q was written %d times for a shim that echoed both members, want none", olderShimLine, len(lines))
	}
}

// A pod built before a fix ran beside the other steps runs a shim that ignores the adoption's
// members and answers ok with no echo. The daemon reads the missing echo as that pod: the task is
// not sent, the claim's prompt budget is not charged, and one WARN line names the claim and the
// pod. The task stays pending, unconfirmed, and goes once the pod answers the echo, as the pod its
// update round builds does. A task that only moves the task's working copy is held the same way,
// and so is a fix's task that names its own branch and moves nothing, whose echo is the fix's
// working copy. The control is the same task to a shim that echoes: it is sent at once, so the hold
// is the missing echo's.
func TestAnOlderShimHoldsTheFixWithoutCharge(t *testing.T) {
	moveOnly := fixTask()
	moveOnly.Fix, moveOnly.FixFrom, moveOnly.FixBranch, moveOnly.Task = 0, "", "", "Implement the plan. Move to "+moveToHead+" first."
	branchOnly := fixTask()
	branchOnly.MoveTo = ""
	for _, tc := range []struct {
		name    string
		deliver RequestDeliver
	}{
		{name: "a fix's task", deliver: fixTask()},
		{name: "a task that moves the task's working copy alone", deliver: moveOnly},
		{name: "a fix's task that names its own branch and moves nothing", deliver: branchOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := fixAdoptingHarness(t)
			h.rt.AnswerAdoptionsAsAnOlderShim(true)

			h.must(tc.deliver)

			h.wantPrompts(0)
			h.wantBudgets(Budgets{})
			p := h.pending()
			if p.Task != tc.deliver.Task || !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("pending %+v, want the task still queued and unread", p)
			}
			if adoptions := h.rt.CallsOf("AdoptWorkingCopy"); len(adoptions) != 1 ||
				adoptions[0].Adoption != (runtime.Adoption{FixFrom: tc.deliver.FixFrom, MoveTo: tc.deliver.MoveTo, FixBranch: tc.deliver.FixBranch}) {
				t.Fatalf("adoptions = %+v, want the one adoption naming the task's members", adoptions)
			}
			lines := h.records.of(olderShimLine)
			if len(lines) != 1 || lines[0].level != slog.LevelWarn {
				t.Fatalf("%q was written %d times, want once at WARN: %+v", olderShimLine, len(lines), lines)
			}
			for _, want := range []struct{ key, value string }{
				{"claim", string(h.token)},
				{"pod", h.claim().Locator.Incarnation},
				{"delivery", p.ID},
			} {
				if got, ok := lines[0].attr(want.key); !ok || got.String() != want.value {
					t.Fatalf("%q carries %s %v (present %v), want %s", olderShimLine, want.key, got, ok, want.value)
				}
			}
			if refusals := h.records.of(adoptRefusedLine); len(refusals) != 0 {
				t.Fatalf("%q was written %d times, want none: the shim refused nothing", adoptRefusedLine, len(refusals))
			}

			// The pod its update round builds runs this release's shim, which echoes: the next
			// sweep sends the task.
			h.rt.AnswerAdoptionsAsAnOlderShim(false)
			h.observe(runtime.Alive)
			if prompts := h.wantPrompts(1); prompts[0].DeliveryID != h.pending().ID {
				t.Fatalf("the prompt carries delivery %s, want the held task %s", prompts[0].DeliveryID, h.pending().ID)
			}
			h.wantBudgets(Budgets{})
		})
	}

	t.Run("control: the same fix's task to a shim that echoes is sent at once", func(t *testing.T) {
		h, _ := fixAdoptingHarness(t)

		h.must(fixTask())

		h.wantPrompts(1)
		h.wantBudgets(Budgets{})
		if lines := h.records.of(olderShimLine); len(lines) != 0 {
			t.Fatalf("%q was written %d times, want none", olderShimLine, len(lines))
		}
	})
}

// A task of no fix and no move adopts as it did before either member existed: the adoption names
// neither, and the task is sent, also to a shim built before them, which echoes nothing because
// nothing was asked of it.
func TestADeliveryWithNoFixAdoptsAsToday(t *testing.T) {
	for _, older := range []bool{false, true} {
		name := "a shim of this release"
		if older {
			name = "a shim built before a fix ran beside the other steps"
		}
		t.Run(name, func(t *testing.T) {
			h, counting := fixAdoptingHarness(t)
			h.rt.AnswerAdoptionsAsAnOlderShim(older)

			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 5})

			h.wantPrompts(1)
			h.wantBudgets(Budgets{})
			if p := h.pending(); p.Fix != 0 || p.FixFrom != "" || p.MoveTo != "" {
				t.Fatalf("the pending delivery holds fix %d from %q moving to %q, want none of them", p.Fix, p.FixFrom, p.MoveTo)
			}
			adoptions := h.rt.CallsOf("AdoptWorkingCopy")
			if len(adoptions) != 1 || adoptions[0].Adoption != (runtime.Adoption{}) || adoptions[0].Identity != adoptingBot {
				t.Fatalf("adoptions = %+v, want one naming no fix and no move under %v", adoptions, adoptingBot)
			}
			if sent := counting.promptsAtAdoptions(); !slices.Equal(sent, []int{0}) {
				t.Fatalf("the adoptions were asked with %v prompts sent, want one adoption, asked before any prompt", sent)
			}
			if lines := h.records.of(adoptedLine); len(lines) != 1 {
				t.Fatalf("%q was written %d times, want once", adoptedLine, len(lines))
			}
		})
	}
}
