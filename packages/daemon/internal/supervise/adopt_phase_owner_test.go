package supervise

// Every role of an issue works one jj workspace, so an adoption writes the adopting role's App as
// the author of the working copy's commit, and only the role whose phase the issue is in may do
// that (dispatch://INFRA-3916 R5, dispatch://INFRA-2695). On OSSV-122 on 2026-11-15 a daemon that
// adopted for every task sent the reviewer an update round's ask, queued as a task while the issue
// was in implementing, and its adoption wrote the reviewer's App onto the implementer's
// undescribed commit. The daemon adopts only for a task of a phase (adopts), drops a task of a
// phase the issue has left (phaseHolds), and drops an ask an older daemon queued as a task when a
// restored claim brings one back (sendPending, updateRoundOf).
//
// Each drop is held alone: a phaseHolds that held back no task of another phase than the issue's
// would turn the reviewer's phase-task row red, and a sendPending that did not drop a restored
// `update:` task would turn the `update:` row red.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// reviewerToken and reviewerClaim are the reviewer of the implementer's own issue (queuedClaim),
// the claim the wake tests name thirdToken: two roles of one issue, and so of the one jj workspace
// its roles share.
const reviewerToken = thirdToken

func reviewerClaim() Claim {
	c := queuedClaim()
	c.Token, c.Role = reviewerToken, claim.RoleReviewer
	return c
}

// The issue is in implementing, and the implementer's App last adopted its working copy and left
// the commit undescribed, so the next adoption, whoever makes it, writes its own App as that
// commit's author (the shim's metaedit branch). The fake runtime records each adoption with the
// identity it carries: the commit's author is the last of those, or the implementer's App where a
// row made none. No task of a role off the issue's phase adopts, whatever it is; the implementer's
// own phase task adopts once, as the implementer's App.
func TestATaskOfARoleOffItsPhaseLeavesTheWorkingCopysAuthor(t *testing.T) {
	appOf := func(role claim.Role) runtime.GitIdentity {
		return runtime.GitIdentity{Name: "legion-" + string(role) + "[bot]", Email: "7+legion-" + string(role) + "[bot]@users.noreply.github.com"}
	}
	implementerApp := appOf(claim.RoleImplementer)
	for _, tc := range []struct {
		name  string
		claim Claim
		// act hands the row's claim, ready and holding nothing, the row's task.
		act func(h *harness)
		// adoptions and prompts are what the row's claim gets: adoptions the runtime records and
		// prompts its agent is sent. A row whose task is dropped holds nothing afterwards.
		adoptions, prompts int
	}{
		{name: "a reviewer's phase task queued for reviewing", claim: reviewerClaim(),
			act: func(h *harness) {
				h.must(RequestDeliver{Claim: reviewerToken, Task: "review the pull request", ID: "outbox:21", Row: 21,
					Phase: phase.Reviewing, Generation: 5})
			}},
		// An older daemon queued the round's ask as a task, and this daemon restores the claim
		// holding it. The ask carries the phase the issue is in, as no ask of an older daemon did:
		// a task of no phase adopts nothing whether it is dropped or sent (adopts), so this one
		// leaves the drop of an older daemon's ask as the one thing between it and the reviewer's
		// adoption.
		{name: "an update: delivery restored from the store on the reviewer's claim", claim: reviewerClaim(),
			act: func(h *harness) {
				ask := Delivery{ID: updateTaskID(testRound), Task: UpdateTask(claim.RoleReviewer, testRound, testReasons, epoch.Add(time.Hour)),
					Phase: phase.Implementing, Generation: 5, QueuedAt: h.clock.Now()}
				if err := h.store.PutDelivery(h.ctx, h.token, ask); err != nil {
					h.t.Fatalf("store the older daemon's ask: %v", err)
				}
				h.restart()
				if p := h.claim().Pending; p == nil || p.ID != ask.ID {
					h.t.Fatalf("the restored claim holds %+v, want the older daemon's ask %s", p, ask.ID)
				}
				h.observe(runtime.Alive) // the sweep, which sends what the claim holds
			}},
		{name: "a reviewer's message with no phase", claim: reviewerClaim(),
			act: func(h *harness) {
				h.must(RequestDeliver{Claim: reviewerToken, Task: "the implementer asks where the review stands"})
			},
			prompts: 1},
		{name: "the implementer's own phase task", claim: queuedClaim(),
			act: func(h *harness) {
				h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:22", Row: 22,
					Phase: phase.Implementing, Generation: 5})
			},
			adoptions: 1, prompts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, tc.claim)
			h.deps.Identity = func(_ context.Context, role claim.Role, _ string) (runtime.GitIdentity, error) {
				return appOf(role), nil
			}
			h.deps.PhaseHolds = func(_ context.Context, issue string, p phase.Phase, _ uint64) (bool, error) {
				return issue == queuedClaim().Issue && p == phase.Implementing, nil
			}
			h.restart()
			h.reach(StateReady)

			tc.act(h)

			author := implementerApp
			for _, adoption := range h.wantCalls("AdoptWorkingCopy", tc.adoptions) {
				author = adoption.Identity
			}
			if author != implementerApp {
				t.Fatalf("the working copy's commit is authored by %v, want the implementer's App %v", author, implementerApp)
			}
			h.wantPrompts(tc.prompts)
			if held := h.claim().Pending; tc.prompts == 0 && held != nil {
				t.Fatalf("the claim holds %+v, want the task dropped", held)
			}
		})
	}
}
