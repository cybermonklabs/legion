package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// pluginContractOf is the contract the plugin of token's claim declared at its registration, as
// the completion route reads it (supervise.Claim.PluginContract).
func pluginContractOf(t *testing.T, h *harness, token claim.Token) int {
	t.Helper()
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	return machine.Claim().PluginContract
}

// The completion that ends the retro carries the rating of the plan: the agent that does the retro
// gives it. The route reads which completion that is from phase.Of and never from the role: the
// implementer's completion while the issue is in retro, and its late completion of retro once the
// issue has moved on to merging, which is still of retro. The fact the route applies carries the
// rating and its reason, for the workflow to write on the retro's record.
func TestTheRetrosAgentGivesTheRating(t *testing.T) {
	for _, at := range []phase.Phase{phase.Retro, phase.Merging} {
		t.Run(string(at), func(t *testing.T) {
			if of := phase.Of(claim.RoleImplementer, at); of != phase.Retro {
				t.Fatalf("the implementer's completion at %s is of %s, want retro: this case proves nothing", at, of)
			}
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", at)
			implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

			completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee",
				PlanRating: new(4), PlanReason: "The tester found the bug."}))
			fact := lastCompletion(t, facts)
			if fact.PlanRating != 4 || fact.PlanReason != "The tester found the bug." {
				t.Fatalf("the retro's fact = %+v, want plan rating 4 with its reason", fact)
			}
		})
	}
}

// A rating rides the completion that ends the retro and no other. Every other completion that
// names one is refused PLAN_RATING_UNEXPECTED with nothing recorded: a phase of a role that does
// no retro (the tester's testing, the merger's merging), another phase of the very role that does
// it (the implementer's implementing, refused before GitHub is read for its pull request), and a
// report on a look of a closed issue's production check. The same completion without the rating
// is taken, so the rating is what the refusal is about.
func TestARatingOutsideTheRetroIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   phase.Phase
		role claim.Role
		// look is whether the worker's live assignment is look 2 of the issue's production check.
		look bool
		// request is the completion before the grant and the rating are added.
		request func(c liveClaim) HandoffCompleteRequest
	}{
		{name: "the tester in testing", at: phase.Testing, role: claim.RoleTester, request: func(c liveClaim) HandoffCompleteRequest {
			return HandoffCompleteRequest{Summary: "tests pass", Verdict: "pass", HandoffID: c.writeHandoff("test", 1), HandoffVersion: 1}
		}},
		{name: "the implementer in implementing", at: phase.Implementing, role: claim.RoleImplementer, request: func(c liveClaim) HandoffCompleteRequest {
			return HandoffCompleteRequest{Summary: "implemented", Commit: headSHA, HandoffID: c.writeHandoff("implement", 1), HandoffVersion: 1}
		}},
		{name: "the merger in merging", at: phase.Merging, role: claim.RoleMerger, request: func(liveClaim) HandoffCompleteRequest {
			return HandoffCompleteRequest{Summary: "merged", Commit: "c0ffee"}
		}},
		{name: "a report on a look of the production check", at: phase.Done, role: claim.RoleImplementer, look: true, request: func(liveClaim) HandoffCompleteRequest {
			return HandoffCompleteRequest{Summary: "looked", Verdict: "pass"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			h, facts := newHandoffHarness(t, github, nil, nil)
			seedIssueAt(t, h, "LEGION-208", tc.at)
			var worker liveClaim
			if tc.look {
				seedCheck(t, h, "LEGION-208", tc.role, false, false)
				worker = newCheckLookClaim(t, h, "LEGION-208", tc.role, 2)
			} else {
				worker = newLiveClaim(t, h, "LEGION-208", tc.role)
			}
			request := tc.request(worker)

			rating := request
			rating.GrantID, rating.PlanRating, rating.PlanReason = worker.grant(t), new(3), "The plan fit the work."
			message := refused(t, h.complete(rating), http.StatusBadRequest, "PLAN_RATING_UNEXPECTED")
			for _, want := range []string{"only the completion that ends the retro rates the plan", "nothing was recorded"} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal = %q, want it to say %q", message, want)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusal = %#v, want none applied", got)
			}
			if served := github.serves(); len(served) != 0 {
				t.Fatalf("GitHub was asked for %v before the refusal, want nothing: the rating is refused before any read", served)
			}

			// The implementer's completion also applies the pull request GitHub shows on its branch
			// (implementationPullRequest) before its own fact, so the completion is the newest fact.
			request.GrantID = worker.grant(t)
			completion(t, h.complete(request))
			got := facts.recorded()
			if len(got) == 0 {
				t.Fatal("no fact after the same completion without the rating, want it applied")
			}
			switch fact := got[len(got)-1].(type) {
			case intake.HandoffComplete:
				if fact.Claim != worker.token || fact.PlanRating != 0 || fact.PlanReason != "" {
					t.Fatalf("the completion's fact = %+v, want %s's with no rating", fact, worker.token)
				}
			case intake.CheckReport:
				if fact.Claim != worker.token {
					t.Fatalf("the report's fact = %+v, want %s's", fact, worker.token)
				}
			default:
				t.Fatalf("the newest fact is %T, want the completion or the report", fact)
			}
		})
	}
}

// A rating is a whole number from 1 to 5 and its reason says why, in one to planReasonMax
// characters that are not all blank; the two come together. Each other shape is refused
// PLAN_RATING_INPUT naming what is wrong, with nothing recorded. The bounds themselves are taken:
// 1, and 5 with a reason of exactly planReasonMax characters of two bytes each, which a count of
// bytes would refuse.
func TestARatingIsOneToFiveWithAReason(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	for _, tc := range []struct {
		name   string
		rating *int
		reason string
		says   string
	}{
		{name: "a rating of 0", rating: new(0), reason: "The plan fit the work.", says: "planRating 0 is outside 1 to 5"},
		{name: "a rating of 6", rating: new(6), reason: "The plan fit the work.", says: "planRating 6 is outside 1 to 5"},
		{name: "a rating with no reason", rating: new(4), says: "planRating 4 comes with no planReason"},
		{name: "a reason with no rating", reason: "The plan fit the work.", says: "planReason comes with no planRating"},
		{name: "a blank reason", rating: new(4), reason: " \t\n", says: "planReason is blank"},
		{name: "a reason too long", rating: new(4), reason: strings.Repeat("é", planReasonMax+1),
			says: "planReason is 2001 characters, and it holds at most 2000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee",
				PlanRating: tc.rating, PlanReason: tc.reason}), http.StatusBadRequest, "PLAN_RATING_INPUT")
			for _, want := range []string{tc.says, "nothing was recorded"} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal = %q, want it to say %q", message, want)
				}
			}
		})
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusals = %#v, want none applied", got)
	}

	for _, taken := range []struct {
		commit string
		rating int
		reason string
	}{
		{commit: "c0ffee01", rating: 1, reason: "The plan cost the work."},
		{commit: "c0ffee05", rating: 5, reason: strings.Repeat("é", planReasonMax)},
	} {
		completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: taken.commit,
			PlanRating: new(taken.rating), PlanReason: taken.reason}))
		if fact := lastCompletion(t, facts); fact.PlanRating != taken.rating || fact.PlanReason != taken.reason {
			t.Fatalf("the retro's fact at %s = rating %d with a reason of %d bytes, want rating %d with its reason of %d bytes",
				taken.commit, fact.PlanRating, len(fact.PlanReason), taken.rating, len(taken.reason))
		}
	}
}

// A planReason that holds a NUL character (a JSON body carries it as \u0000) is a reason nothing
// can store: PostgreSQL's jsonb, where the daemon's outbox row holds the rating, and its text,
// where Dispatch's record holds it, both refuse it, and the whole daemon answered the retro's
// completion 500 FACT_APPLY_FAILED naming no field when the engine's transaction failed on the
// rating row. The decoder of the body refuses it before the route reads it, as it refuses a NUL in
// any string of any body (decodeJSON): 400 naming planReason and where in it the NUL stands, with
// nothing recorded, so the agent sends it again corrected.
func TestARatingReasonWithANULIsRefusedAsInput(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	wantRefusal(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee",
		PlanRating: new(4), PlanReason: "The plan fit\x00 the work."}), http.StatusBadRequest,
		"invalid request body: planReason holds a NUL character (U+0000) at character 13, which the daemon cannot store")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusal = %#v, want none applied", got)
	}
}

// A plugin at PlanRatingContract or later must rate: its retro's completion with no rating is
// refused PLAN_RATING_REQUIRED, which says how to rate, with nothing recorded, and the same
// completion with a rating is taken.
func TestANewPluginMustRate(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	if got := pluginContractOf(t, h, implementer.token); got != PlanRatingContract {
		t.Fatalf("the implementer's plugin declared contract %d, want PlanRatingContract %d: this case proves nothing", got, PlanRatingContract)
	}

	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee"}),
		http.StatusBadRequest, "PLAN_RATING_REQUIRED")
	for _, want := range []string{"ends the retro of LEGION-208", "planRating", "planReason", "1 (the plan cost the work) to 5 (the plan fit the work)", "nothing was recorded"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal = %q, want it to say %q", message, want)
		}
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusal = %#v, want none applied", got)
	}

	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee",
		PlanRating: new(5), PlanReason: "The plan fit the work."}))
	if fact := lastCompletion(t, facts); fact.PlanRating != 5 || fact.PlanReason != "The plan fit the work." {
		t.Fatalf("the retro's fact = %+v, want plan rating 5 with its reason", fact)
	}
}

// A plugin of the release before PlanRatingContract never sends a rating. Where the daemon serves
// its claim — here a claim in a turn, which the stale-caller rule serves so the turn can push what
// it holds (staleClaim) — its retro's completion is taken with no rating, and the fact carries
// none.
func TestAnOldPluginRetroStillCompletesWhereItIsServed(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	grantID := implementer.grant(t)
	h.makeStale(t, implementer.token, implementer.session, implementer.secret, PlanRatingContract-1)
	if c := h.stored(implementer.token); c.State != supervise.StateWorking || c.UpdateRound != "" || c.PluginContract != PlanRatingContract-1 {
		t.Fatalf("the claim is %s in round %q at contract %d, want working in no round at %d: this case proves nothing",
			c.State, c.UpdateRound, c.PluginContract, PlanRatingContract-1)
	}

	completion(t, h.complete(HandoffCompleteRequest{GrantID: grantID, Summary: "retro done", Commit: "c0ffee"}))
	if fact := lastCompletion(t, facts); fact.PlanRating != 0 || fact.PlanReason != "" || fact.Summary != "retro done" {
		t.Fatalf("the old plugin's retro fact = %+v, want it with no rating", fact)
	}
}

// The control of TestAnOldPluginRetroStillCompletesWhereItIsServed: the same claim out of its turn
// is not served at all, and its completion is refused STALE_PLUGIN_CONTRACT before any rule of the
// rating is read.
func TestAnOldPluginOutsideATurnIsRefusedStale(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	grantID := implementer.grant(t)
	endTurn(t, h, implementer.token)
	h.makeStale(t, implementer.token, implementer.session, implementer.secret, PlanRatingContract-1)
	if c := h.stored(implementer.token); c.State == supervise.StateWorking || c.UpdateRound != "" || c.PluginContract != PlanRatingContract-1 {
		t.Fatalf("the claim is %s in round %q at contract %d, want out of its turn in no round at %d: this case proves nothing",
			c.State, c.UpdateRound, c.PluginContract, PlanRatingContract-1)
	}

	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: grantID, Summary: "retro done", Commit: "c0ffee"}),
		http.StatusConflict, "STALE_PLUGIN_CONTRACT")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusal = %#v, want none applied", got)
	}
}
