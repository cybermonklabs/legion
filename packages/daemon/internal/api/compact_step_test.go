package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// Key a's agent, `…-implementer`, works the tester's step of LEGION-209 and is ready on its session
// when the controller asks a compaction of (LEGION-209, tester). The route reads the claim of the
// agent on that step before any lock the compaction takes, so it asks with that reading as its
// standing (supervise.Standing), and the compaction's take judges it again under the machine's lock.
// A later entry of the step for another agent, `…-reviewer`, that commits after the route's read and
// before the take moves the step from under the request: the take is refused, Oh My Pi is asked for
// no compaction, and the claim stays ready on its session. The control is the same request with
// nothing moving: the session is compacted once and the claim is ready on it again.
func TestAControllersCompactionWhoseStepMovesUnderItCompactsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moves bool
	}{
		{name: "control: nothing moves the step while the compaction runs"},
		{name: "a later entry of the step names another agent before the compaction's take", moves: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := compactHarness(t, nil)
			keyed := freshSessionToken(t, "LEGION-209", claim.RoleImplementer)
			keyedTester(t, h, keyed)
			if _, err := h.store.OpenTreeLifecycle(ctx, testProject, "LEGION-208", treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the lifecycle of LEGION-208: %v", err)
			}
			m, _, err := h.supervisor.Create(ctx, supervise.Claim{Token: keyed, Project: testProject, Tree: "LEGION-208", Issue: "LEGION-209",
				Role: claim.RoleTester, State: supervise.StateQueued}, "role prompt")
			if err != nil {
				t.Fatalf("create key a's claim: %v", err)
			}
			if err := m.Handle(ctx, supervise.RequestSpawn{Claim: keyed}); err != nil {
				t.Fatalf("spawn key a's agent: %v", err)
			}
			conn := connected(h, keyed)
			h.ready(keyed, "ses_keyed", m.Claim().Generation)
			if c := m.Claim(); c.State != supervise.StateReady || c.Session != "ses_keyed" {
				t.Fatalf("key a's claim is %s recording %q, want it ready on its session: the case's premise", c.State, c.Session)
			}
			grant := controllerGrant(t, h)
			var later claim.Token
			if tc.moves {
				h.supervisor.beforeMachine = func() { later = movesTheTesterStep(t, h) }
			}

			recorder := h.request(http.MethodPost, "/legion/v1/claims/compact",
				ClaimRequest{GrantID: grant.GrantID, Issue: "LEGION-209", Role: string(claim.RoleTester)}, nil)
			m.Wait()
			compactions, after, stored := len(conn.Compactions()), m.Claim(), h.stored(keyed)
			if !tc.moves {
				if answer := decodeCompaction(t, recorder); !answer.Compacted || compactions != 1 {
					t.Fatalf("the compaction answered %+v after %d compactions, want the session compacted once", answer, compactions)
				}
				if after.State != supervise.StateReady || after.Session != "ses_keyed" {
					t.Fatalf("key a's claim is %s recording %q, want it ready on ses_keyed again", after.State, after.Session)
				}
				return
			}
			if later == "" {
				t.Fatal("the route never looked its claim up, so the step never moved: the case saw no window")
			}
			if recorder.Code != http.StatusConflict {
				t.Fatalf("the compaction of a step that moved to %s before its take = %d (%s), want 409", later, recorder.Code, recorder.Body)
			}
			var failure Failure
			decodeInto(t, recorder, &failure)
			if failure.Code != compactionFailed || !strings.Contains(failure.Error, supervise.ErrStepLeft.Error()) {
				t.Fatalf("the refusal is %+v, want %s saying the step moved (%q)", failure, compactionFailed, supervise.ErrStepLeft)
			}
			if compactions != 0 {
				t.Fatalf("Oh My Pi was asked for %d compactions, want none: the step is %s's", compactions, later)
			}
			for _, c := range []supervise.Claim{after, stored} {
				if c.State != supervise.StateReady || c.Session != "ses_keyed" {
					t.Fatalf("key a's claim is %s recording %q, want it untouched: ready on ses_keyed", c.State, c.Session)
				}
			}
		})
	}
}
