package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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

// ratedAs is whether fact carries the rating score with reason.
func ratedAs(fact intake.HandoffComplete, score int, reason string) bool {
	return fact.PlanRating != nil && *fact.PlanRating == (record.PlanRating{Rating: score, Reason: reason})
}

// The agent that does the retro gives the rating of the plan, and the route judges nothing of the
// issue for it: which completion ends the retro is the workflow's to judge where it ends it
// (TestTheRatingIsJudgedOnTheIssueTheWorkflowEndsTheRetroOn). So the fact the route applies
// carries a rating of a right form as the completion named it, and that the claim must rate,
// wherever the route read the issue: in retro, past it in merging, and held from it. The workflow
// writes it on the retro's record for the completion in retro alone: that completion moves the
// issue on to merging (TriggerRetroCompleted), so a late one finds the implementer's retro
// standing and is refused HANDOFF_NOT_CURRENT_PHASE (TestARatedRetroSentAgainKeepsItsFirstRating).
func TestTheRetrosAgentGivesTheRating(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   phase.Phase
		held bool
	}{
		{name: "in retro", at: phase.Retro},
		{name: "in merging", at: phase.Merging},
		{name: "held from its retro", at: phase.Retro, held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", tc.at)
			implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
			if tc.held {
				holdFrom(t, h, "LEGION-208", tc.at)
			}

			completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee",
				PlanRating: new(4), PlanReason: new("The tester found the bug.")}))
			fact := lastCompletion(t, facts)
			if !ratedAs(fact, 4, "The tester found the bug.") || !fact.MustRate {
				t.Fatalf("the retro's fact carries the rating %+v, must rate %t, want 4 with its reason from a claim that must rate", fact.PlanRating, fact.MustRate)
			}
		})
	}
}

// A rating rides the completion that ends the retro and no other. Every other completion that
// names one is refused PLAN_RATING_UNEXPECTED, naming the phase the workflow read, with nothing
// recorded: a phase of a role that does no retro (the tester's testing, the merger's merging) and
// another phase of the very role that does it (the implementer's implementing). The workflow
// refuses it where it would end the retro, on the issue the fact's transaction reads, and leaves
// the fact unspent, so the same completion without the rating is taken under the same key: the
// rating is what the refusal is about.
func TestARatingOutsideTheRetroIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   phase.Phase
		role claim.Role
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
			seedIssueAt(t, h, "LEGION-208", tc.at)
			worker := newLiveClaim(t, h, "LEGION-208", tc.role)
			request := tc.request(worker)

			rating := request
			rating.GrantID, rating.PlanRating, rating.PlanReason = worker.grant(t), new(3), new("The plan fit the work.")
			message := refused(t, h.complete(rating), http.StatusBadRequest, "PLAN_RATING_UNEXPECTED")
			for _, want := range []string{fmt.Sprintf("this completion of phase %s of LEGION-208", tc.at), "only the completion that ends the retro rates the plan", "nothing was recorded"} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal = %q, want it to say %q", message, want)
				}
			}
			assertIssuePhase(t, h, tc.at)
			if row := phaseRowOf(t, h, "LEGION-208", tc.role); row != nil && row.Completed() {
				t.Fatalf("the %s's row after the refusal = %+v, want no completion recorded", tc.role, *row)
			}
			if completionSpent(t, h, "LEGION-208") {
				t.Fatal("the refusal left the completion processed, want it unspent")
			}

			request.GrantID = worker.grant(t)
			completion(t, h.complete(request))
			if row := phaseRowOf(t, h, "LEGION-208", tc.role); row == nil || !row.Completed() {
				t.Fatalf("the %s's row after the same completion without the rating = %+v, want its completion recorded", tc.role, row)
			}
		})
	}
}

// A report on a look of a closed issue's production check ends no phase, so the route refuses a
// rating on it PLAN_RATING_UNEXPECTED, whatever the rating's form, before anything is read or
// recorded, and the same report without the rating is taken.
func TestARatingOnAProductionChecksReportIsRefused(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleImplementer, false, false)
	worker := newCheckLookClaim(t, h, "LEGION-208", claim.RoleImplementer, 2)
	for _, named := range []struct {
		rating *int
		reason *string
	}{{rating: new(3), reason: new("The plan fit the work.")}, {reason: new("")}} {
		message := refused(t, h.complete(HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "looked", Verdict: "pass",
			PlanRating: named.rating, PlanReason: named.reason, CheckOpenedAt: &checkOpenedAt}), http.StatusBadRequest, "PLAN_RATING_UNEXPECTED")
		for _, want := range []string{"this report on look 2 of LEGION-208's production check", "only the completion that ends the retro rates the plan", "nothing was recorded"} {
			if !strings.Contains(message, want) {
				t.Fatalf("the refusal = %q, want it to say %q", message, want)
			}
		}
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusals = %#v, want none applied", got)
	}
	if served := github.serves(); len(served) != 0 {
		t.Fatalf("GitHub was asked for %v before the refusals, want nothing: the rating is refused before any read", served)
	}

	completion(t, h.complete(HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "looked", Verdict: "pass", CheckOpenedAt: &checkOpenedAt}))
	got := facts.recorded()
	if report, ok := got[len(got)-1].(intake.CheckReport); len(got) != 1 || !ok || report.Claim != worker.token {
		t.Fatalf("facts after the report without the rating = %#v, want %s's one report", got, worker.token)
	}
}

// The architect ends no phase: phase.Of names none for it outside admitted and accepting, and the
// route falls back to the issue's phase only to read the handoff record. So an architect's
// completion while its issue stands in retro is not the retro's, though the phase it falls back to
// is, and the workflow refuses it of its own accord (ACCEPTANCE_IS_SIGN_OFF), rated or not: that
// refusal comes before any rule of the rating. The issue stays in retro either way. That refusal
// is recorded under the completion's key, which leaves the rating out, so the unrated completion
// names a commit of its own.
func TestAnArchitectsCompletionInTheRetroIsNotTheRetros(t *testing.T) {
	if of := phase.Of(claim.RoleArchitect, phase.Retro); of != "" {
		t.Fatalf("the architect's completion at retro is of %q, want none: this case proves nothing", of)
	}
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	if got := pluginContractOf(t, h, architect.token); got < PlanRatingContract {
		t.Fatalf("the architect's plugin declared contract %d, below PlanRatingContract %d: this case proves nothing", got, PlanRatingContract)
	}

	refused(t, h.complete(HandoffCompleteRequest{GrantID: architect.grant(t), Summary: "accepted", Commit: "c0ffee",
		PlanRating: new(3), PlanReason: new("The plan fit the work.")}), http.StatusConflict, "ACCEPTANCE_IS_SIGN_OFF")
	assertIssuePhase(t, h, phase.Retro)
	refused(t, h.complete(HandoffCompleteRequest{GrantID: architect.grant(t), Summary: "accepted", Commit: "c0ffee1"}),
		http.StatusConflict, "ACCEPTANCE_IS_SIGN_OFF")
	assertIssuePhase(t, h, phase.Retro)
}

// A rating is a whole number from 1 to 5 and its reason says why, in one to
// steprecord.RatingReasonMax characters that are not all blank; the two come together. Each other
// shape is refused PLAN_RATING_INPUT naming what is wrong, with nothing recorded. The bounds
// themselves are taken: 1, and 5 with a reason of exactly steprecord.RatingReasonMax characters of
// two bytes each, which a count of bytes would refuse. Each is the completion that ends the retro
// of an issue of its own, since the workflow moves an issue on to merging at its retro's first
// completion taken.
func TestARatingIsOneToFiveWithAReason(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	for _, tc := range []struct {
		name   string
		rating *int
		reason *string
		says   string
	}{
		{name: "a rating of 0", rating: new(0), reason: new("The plan fit the work."), says: "planRating 0 is outside 1 to 5"},
		{name: "a rating of 6", rating: new(6), reason: new("The plan fit the work."), says: "planRating 6 is outside 1 to 5"},
		{name: "a rating with no reason", rating: new(4), says: "planRating 4 comes with no planReason"},
		{name: "a reason with no rating", reason: new("The plan fit the work."), says: "planReason comes with no planRating"},
		{name: "a blank reason", rating: new(4), reason: new(" \t\n"), says: "planReason is blank"},
		{name: "a reason too long", rating: new(4), reason: new(strings.Repeat("é", steprecord.RatingReasonMax+1)),
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

	for i, taken := range []struct {
		rating int
		reason string
	}{
		{rating: 1, reason: "The plan cost the work."},
		{rating: 5, reason: strings.Repeat("é", steprecord.RatingReasonMax)},
	} {
		key := fmt.Sprintf("LEGION-%d", 300+i)
		seedIssueAt(t, h, key, phase.Retro)
		rater := newLiveClaim(t, h, key, claim.RoleImplementer)
		completion(t, h.complete(HandoffCompleteRequest{GrantID: rater.grant(t), Summary: "retro done", Commit: "c0ffee",
			PlanRating: new(taken.rating), PlanReason: new(taken.reason)}))
		if fact := lastCompletion(t, facts); fact.Issue != key || !ratedAs(fact, taken.rating, taken.reason) {
			t.Fatalf("the retro's fact is %s's, rated %t, want %s's carrying the rating %d with its reason of %d bytes", fact.Issue, fact.PlanRating != nil, key, taken.rating, len(taken.reason))
		}
	}
}

// A completion names a rating by its members written with a value, whatever the value:
// `"planReason": ""` is a reason named, and empty. The route holds a named rating to its form
// wherever the completion stands, since which completion ends the retro is the workflow's to judge
// (TestTheRatingIsJudgedOnTheIssueTheWorkflowEndsTheRetroOn). So a completion carrying it, or
// either member alone, is refused PLAN_RATING_INPUT for the member it lacks, outside the retro and
// in it, from a plugin at PlanRatingContract or later and from one before it alike, rather than
// told it carries no rating or taken unrated; and with a rating it is refused as blank. A member
// written null names nothing: it decodes to nil and reads as left out, so the completion outside
// the retro that writes both members null is taken, and its fact carries no rating. That body
// alone is written by hand, since HandoffCompleteRequest leaves a nil member out rather than
// writing it null. A refusal records nothing, and the completion each case corrects to is taken.
func TestAnEmptyPlanReasonIsAReasonNamed(t *testing.T) {
	type rating struct {
		rating *int
		reason *string
	}
	complete := func(h *harness, grant string, r rating) *httptest.ResponseRecorder {
		return h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "done", Commit: "c0ffee", PlanRating: r.rating, PlanReason: r.reason})
	}

	t.Run("outside the retro", func(t *testing.T) {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		seedIssueAt(t, h, "LEGION-208", phase.Merging)
		merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
		for _, row := range []struct {
			named rating
			says  string
		}{
			{named: rating{reason: new("")}, says: "planReason comes with no planRating"},
			{named: rating{reason: new("fit")}, says: "planReason comes with no planRating"},
			{named: rating{rating: new(4)}, says: "planRating 4 comes with no planReason"},
		} {
			if message := refused(t, complete(h, merger.grant(t), row.named), http.StatusBadRequest, "PLAN_RATING_INPUT"); !strings.Contains(message, row.says) {
				t.Fatalf("the refusal = %q, want it to say %q", message, row.says)
			}
		}
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("facts after the refusals = %#v, want none applied", got)
		}
		completion(t, h.completeRaw(merger.grant(t), `,"summary":"done","commit":"c0ffee","planRating":null,"planReason":null`))
		if fact := lastCompletion(t, facts); fact.PlanRating != nil {
			t.Fatalf("the completion writing both members null was taken rated %+v, want no rating", *fact.PlanRating)
		}
	})

	for _, band := range []struct {
		name     string
		contract int
		// taken is the rating of the completion the claim's plugin is taken with.
		taken rating
	}{
		{name: "a plugin at PlanRatingContract or later", contract: DaemonAPIVersion(), taken: rating{rating: new(4), reason: new("fit")}},
		{name: "a plugin before it", contract: PlanRatingContract - 1},
	} {
		t.Run("the retro of "+band.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Retro)
			implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
			// A live claim registers at this daemon's own contract (DaemonAPIVersion), so a band at
			// another is a claim recorded stale. A stale claim is served in its turn (staleClaim), and
			// its grants are minted first, as TestAnOldPluginRetroStillCompletesWhereItIsServed mints
			// its one.
			grants := []string{implementer.grant(t), implementer.grant(t), implementer.grant(t)}
			if band.contract != DaemonAPIVersion() {
				h.makeStale(t, implementer.token, implementer.session, implementer.secret, band.contract)
			}
			if c := h.stored(implementer.token); c.State != supervise.StateWorking || c.PluginContract != band.contract {
				t.Fatalf("the claim is %s at contract %d, want working at %d: this case proves nothing", c.State, c.PluginContract, band.contract)
			}
			for i, row := range []struct {
				named rating
				says  string
			}{
				{named: rating{reason: new("")}, says: "planReason comes with no planRating"},
				{named: rating{rating: new(4), reason: new("")}, says: "planReason is blank"},
			} {
				message := refused(t, complete(h, grants[i], row.named), http.StatusBadRequest, "PLAN_RATING_INPUT")
				if !strings.Contains(message, row.says) {
					t.Fatalf("the refusal = %q, want it to say %q", message, row.says)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusals = %#v, want none applied", got)
			}
			completion(t, complete(h, grants[2], band.taken))
		})
	}
}

// A planReason that holds a NUL character (a JSON body carries it as \u0000) is a reason
// PostgreSQL cannot hold: its jsonb, where the daemon's outbox row holds the rating, and its text,
// where Dispatch's record holds it, both refuse it, and the daemon's store would keep U+FFFD in its
// place unseen by the agent. The decoder of the body refuses it before the route reads it, as it
// refuses a NUL in any string of any body (decodeJSON): 400 naming planReason and where in it the
// NUL stands, with nothing recorded, so the agent sends it again corrected.
func TestARatingReasonWithANULIsRefusedAsInput(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	wantRefusal(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee",
		PlanRating: new(4), PlanReason: new("The plan fit\x00 the work.")}), http.StatusBadRequest,
		"invalid request body: "+nulError("planReason", 13))
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusal = %#v, want none applied", got)
	}
}

// mustRate holds h's LEGION-208, in retro and worked by an implementer, to the rule that its claim
// must rate: the retro's completion with no rating, sent with the grant unrated, is refused
// PLAN_RATING_REQUIRED by the workflow and answers the refusal's sentence, with nothing recorded:
// the issue still in retro, no completion on the implementer's row, no rated step record and the
// fact left unspent. The same completion with the rating score and reason, sent with the grant
// rated, is then taken under the same key: the issue moves on to merging and one step record
// carries the reason.
func mustRate(t *testing.T, h *harness, unrated, rated string, score int, reason string) string {
	t.Helper()
	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: unrated, Summary: "retro done", Commit: "c0ffee"}),
		http.StatusBadRequest, "PLAN_RATING_REQUIRED")
	assertIssuePhase(t, h, phase.Retro)
	if row := phaseRowOf(t, h, "LEGION-208", claim.RoleImplementer); row != nil && row.Completed() {
		t.Fatalf("the implementer's row after the refusal = %+v, want no completion recorded", *row)
	}
	if got := ratedStepRecords(t, h, "LEGION-208"); got != 0 || completionSpent(t, h, "LEGION-208") {
		t.Fatalf("after the refusal LEGION-208 has %d rated step records, processed %t, want none and unspent", got, completionSpent(t, h, "LEGION-208"))
	}

	completion(t, h.complete(HandoffCompleteRequest{GrantID: rated, Summary: "retro done", Commit: "c0ffee",
		PlanRating: new(score), PlanReason: new(reason)}))
	assertIssuePhase(t, h, phase.Merging)
	if written := stepRecordPayloads(t, h, "LEGION-208"); ratedStepRecords(t, h, "LEGION-208") != 1 ||
		!slices.ContainsFunc(written, func(p string) bool { return strings.Contains(p, reason) }) {
		t.Fatalf("after the rated completion the step records are %v, want one rated %d with its reason", written, score)
	}
	return message
}

// A plugin at PlanRatingContract or later must rate: its retro's completion with no rating is
// refused PLAN_RATING_REQUIRED, which says how to rate, with nothing recorded, and the same
// completion with a rating is taken (mustRate).
func TestANewPluginMustRate(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	if got := pluginContractOf(t, h, implementer.token); got < PlanRatingContract {
		t.Fatalf("the implementer's plugin declared contract %d, below PlanRatingContract %d: this case proves nothing", got, PlanRatingContract)
	}

	message := mustRate(t, h, implementer.grant(t), implementer.grant(t), 5, "The plan fit the work.")
	for _, want := range []string{"this completion of phase retro of LEGION-208 carries no rating of the plan", "planRating", "planReason", "1 (the plan cost the work) to 5 (the plan fit the work)", "nothing was recorded"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal = %q, want it to say %q", message, want)
		}
	}
}

// "PlanRatingContract or later" holds above this daemon's own contract too, which no claim the
// harness registers declares: TestANewPluginMustRate's claim records this daemon's own contract
// (DaemonAPIVersion), and TestAPluginAtTheRatingContractMustRate's records PlanRatingContract
// itself. The claim here records one above this daemon's and the daemon still serves it, a claim
// in its turn (staleClaim), as after a later release rolled back to this daemon while its worker
// was in the retro's turn. Its retro's completion with no rating is refused PLAN_RATING_REQUIRED
// with nothing recorded, and the same completion with a rating is taken (mustRate).
func TestAPluginAboveTheRatingContractMustRateToo(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	grants := []string{implementer.grant(t), implementer.grant(t)}
	later := DaemonAPIVersion() + 1
	h.makeStale(t, implementer.token, implementer.session, implementer.secret, later)
	if c := h.stored(implementer.token); c.State != supervise.StateWorking || c.UpdateRound != "" || c.PluginContract != later {
		t.Fatalf("the claim is %s in round %q at contract %d, want working in no round at %d: this case proves nothing",
			c.State, c.UpdateRound, c.PluginContract, later)
	}

	mustRate(t, h, grants[0], grants[1], 2, "The plan left out a step the tester needed.")
}

// A plugin at PlanRatingContract itself must rate: the boundary of the rule (the completion's
// MustRate, `c.PluginContract >= PlanRatingContract`). Once this daemon's own contract is above
// PlanRatingContract, no other case here posts at it: the harness's claims declare the daemon's
// contract (TestANewPluginMustRate) and the case above records the one after. The claim here
// records PlanRatingContract itself, which a daemon of a later contract serves in its turn
// (staleClaim). Its retro's completion with no rating is refused PLAN_RATING_REQUIRED with nothing
// recorded, and the same completion with a rating is taken (mustRate); the rule read as `>` fails
// it.
func TestAPluginAtTheRatingContractMustRate(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	grants := []string{implementer.grant(t), implementer.grant(t)}
	h.makeStale(t, implementer.token, implementer.session, implementer.secret, PlanRatingContract)
	if c := h.stored(implementer.token); c.State != supervise.StateWorking || c.UpdateRound != "" || c.PluginContract != PlanRatingContract {
		t.Fatalf("the claim is %s in round %q at contract %d, want working in no round at %d: this case proves nothing",
			c.State, c.UpdateRound, c.PluginContract, PlanRatingContract)
	}

	mustRate(t, h, grants[0], grants[1], 4, "The plan fit the work.")
}

// stepRecordPayloads is the text of every step record row key's outbox holds, in the order they
// were written: what the workflow sends Dispatch of each run of a step, a retro's rating among it.
func stepRecordPayloads(t *testing.T, h *harness, key string) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(context.Background(), `select payload::text from outbox where issue = $1 and kind = $2 order by id`,
		key, string(record.OutboxKindStepRecord))
	if err != nil {
		t.Fatalf("read the step records of %s: %v", key, err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the step records of %s: %v", key, err)
	}
	return payloads
}

// The retro's completion the workflow takes moves the issue on to merging (TriggerRetroCompleted)
// and writes its rating on the retro's record. The same completion sent again from there is still
// the retro's, the late retro's (phase.Of), so the workflow decides it: the implementer's retro
// already stands, so the first retry, rated otherwise, is refused HANDOFF_NOT_CURRENT_PHASE, and
// that refusal is recorded under the completion's key. The rating is left out of the key, as the
// summary is, so each later retry at the same commit, with the first rating and reason, another
// rating or another reason, is that same completion and is answered HANDOFF_ALREADY_RECORDED. The
// retro's record keeps the first rating alone: the step records stand as the first completion
// wrote them, its reason in one of them and no retry's reason in any.
func TestARatedRetroSentAgainKeepsItsFirstRating(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	retro := func(rating int, reason string) *httptest.ResponseRecorder {
		return h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee",
			PlanRating: new(rating), PlanReason: new(reason)})
	}
	const first = "The plan left out a step the tester needed."
	completion(t, retro(2, first))
	assertIssuePhase(t, h, phase.Merging)
	written := stepRecordPayloads(t, h, "LEGION-208")
	if rated := slices.IndexFunc(written, func(p string) bool { return strings.Contains(p, first) }); rated < 0 ||
		slices.ContainsFunc(written[rated+1:], func(p string) bool { return strings.Contains(p, first) }) {
		t.Fatalf("the retro's completion wrote the step records %v, want its reason in exactly one", written)
	}

	const refusedFirst = "The plan fit the work."
	refused(t, retro(5, refusedFirst), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
	again := []struct {
		name   string
		rating int
		reason string
	}{
		{name: "the first rating and reason", rating: 2, reason: first},
		{name: "another rating", rating: 4, reason: first},
		{name: "another reason", rating: 2, reason: "The reviewer's two rounds were the step the plan left out."},
	}
	for _, retry := range again {
		t.Run(retry.name, func(t *testing.T) {
			message := refused(t, retro(retry.rating, retry.reason), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
			if !strings.Contains(message, "was already received; this call changed nothing") {
				t.Fatalf("the refusal = %q, want it to say the call changed nothing", message)
			}
		})
	}
	assertIssuePhase(t, h, phase.Merging)
	if got := stepRecordPayloads(t, h, "LEGION-208"); !slices.Equal(got, written) {
		t.Fatalf("after the retries the step records are %v, want them as the first completion wrote them: %v", got, written)
	}
	for _, reason := range []string{refusedFirst, again[2].reason} {
		if slices.ContainsFunc(written, func(p string) bool { return strings.Contains(p, reason) }) {
			t.Fatalf("the step records %v carry a retry's reason %q", written, reason)
		}
	}
}

// A plugin of the release before PlanRatingContract never sends a rating. Where the daemon serves
// its claim — here a claim in a turn, which the stale-caller rule serves so the turn can push what
// it holds (staleClaim) — its retro's completion is taken with no rating, and the fact carries
// none and says the claim need not rate, which is what lets the workflow end its retro unrated.
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
	if fact := lastCompletion(t, facts); fact.PlanRating != nil || fact.MustRate || fact.Summary != "retro done" {
		t.Fatalf("the old plugin's retro fact = %+v, want it with no rating from a claim that need not rate", fact)
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

	refused(t, h.complete(HandoffCompleteRequest{GrantID: grantID, Summary: "retro done", Commit: "c0ffee"}),
		http.StatusConflict, "STALE_PLUGIN_CONTRACT")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusal = %#v, want none applied", got)
	}
}

// The completion that ends the retro, rated, is the one request of the completion route a plugin
// at PlanRatingContract must send: Go writes it from HandoffCompleteRequest (`-update`), and
// `packages/contracts/src/legion-api.test.ts` parses it with LegionHandoffCompleteRequest, a strict
// schema, so a JSON name of the rating Go writes and the contract does not read, or the reverse,
// fails there. The rating and its reason are values no default and no other field of the body
// holds.
func TestHandoffCompleteRequestRatedGolden(t *testing.T) {
	golden(t, "handoff-complete-request-rated.json", HandoffCompleteRequest{
		GrantID: "grant-for-one-command", Summary: "Retro done.", Commit: "c0ffee5a1d2b3c4d5e6f708192a3b4c5d6e7f809",
		PlanRating: new(3), PlanReason: new("The tester found the bug the plan had left out."),
	})
}

// planRatingCase is one row of plan-rating-cases.json: the rating and the reason a retro's
// completion carries, a member left out of the body where its pointer is nil, and the verdict the
// completion route answers it with, `accepted` or the code of its refusal.
type planRatingCase struct {
	Name       string  `json:"name"`
	PlanRating *int    `json:"planRating,omitempty"`
	PlanReason *string `json:"planReason,omitempty"`
	Verdict    string  `json:"verdict"`
}

// The daemon is the rule of a rating, and the contract's schema and the plugin's tool state it
// again in TypeScript, where blank is Go's set written out by hand: goBlankPattern, whose comment
// in `packages/contracts/src/go-blank.ts` names the characters JavaScript's trim and Go's
// strings.TrimSpace part on. So the cases a rule of the scale, of the pair, of blank or of length
// can part on are a golden. Each is posted as its raw body, from a plugin at PlanRatingContract or
// later, to the completion that ends the retro of an issue of its own: the workflow moves an issue
// on to merging at the first completion of its retro it takes, so each case is that first
// completion. The route's answer is the verdict Go writes (`-update`), never one typed here;
// `packages/contracts` and the plugin hold their own verdicts to the file. An answer that is
// neither a 200 nor a 400 of the rating fails the test, so no trouble of the harness is written
// down as a verdict, and an accepted case's fact is its issue's and carries the rating and the
// reason as they were sent.
func TestPlanRatingCasesGolden(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)

	rated := func(name string, rating int, reason string) planRatingCase {
		return planRatingCase{Name: name, PlanRating: new(rating), PlanReason: new(reason)}
	}
	cases := []planRatingCase{
		rated("a rating of 4 with a reason", 4, "fit"),
		rated(fmt.Sprintf("a rating of %d", steprecord.RatingLeast-1), steprecord.RatingLeast-1, "fit"),
		rated(fmt.Sprintf("a rating of %d", steprecord.RatingLeast), steprecord.RatingLeast, "fit"),
		rated(fmt.Sprintf("a rating of %d", steprecord.RatingMost), steprecord.RatingMost, "fit"),
		rated(fmt.Sprintf("a rating of %d", steprecord.RatingMost+1), steprecord.RatingMost+1, "fit"),
		{Name: "a rating with no reason", PlanRating: new(4)},
		{Name: "a reason with no rating", PlanReason: new("fit")},
		rated("an empty reason", 4, ""),
	}
	for _, blank := range []struct{ name, text string }{
		{"three spaces", "   "}, {"U+0085", "\u0085"}, {"U+00A0", "\u00a0"}, {"U+FEFF", "\ufeff"},
	} {
		cases = append(cases,
			rated("a reason of "+blank.name+" alone", 4, blank.text),
			rated("a reason of "+blank.name+" before a word", 4, blank.text+"fit"))
	}
	for _, long := range []struct{ name, char string }{
		{"U+0061 (a), one UTF-16 unit each", "a"}, {"U+1F600, two UTF-16 units each", "\U0001F600"},
	} {
		for _, count := range []int{steprecord.RatingReasonMax, steprecord.RatingReasonMax + 1} {
			cases = append(cases, rated(fmt.Sprintf("a reason of %d characters %s", count, long.name), 4, strings.Repeat(long.char, count)))
		}
	}

	for i := range cases {
		c := &cases[i]
		key := fmt.Sprintf("LEGION-%d", 300+i)
		seedIssueAt(t, h, key, phase.Retro)
		implementer := newLiveClaim(t, h, key, claim.RoleImplementer)
		if got := pluginContractOf(t, h, implementer.token); got < PlanRatingContract {
			t.Fatalf("the implementer's plugin declared contract %d, below PlanRatingContract %d: this case proves nothing", got, PlanRatingContract)
		}
		body := map[string]any{"grantId": implementer.grant(t), "summary": "retro done", "verdict": "", "commit": "c0ffee"}
		if c.PlanRating != nil {
			body["planRating"] = *c.PlanRating
		}
		if c.PlanReason != nil {
			body["planReason"] = *c.PlanReason
		}
		before := len(facts.recorded())
		recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", body, nil)
		switch recorder.Code {
		case http.StatusOK:
			fact := lastCompletion(t, facts)
			if len(facts.recorded()) != before+1 || fact.Issue != key || c.PlanRating == nil || c.PlanReason == nil || !ratedAs(fact, *c.PlanRating, *c.PlanReason) {
				t.Fatalf("%s was taken with the fact %+v, want one new fact of %s carrying the rating and the reason as sent", c.Name, fact, key)
			}
			c.Verdict = "accepted"
		case http.StatusBadRequest:
			var failure Failure
			decodeInto(t, recorder, &failure)
			if !strings.HasPrefix(failure.Code, "PLAN_RATING_") {
				t.Fatalf("%s was refused %s (%s), want a refusal of the rating", c.Name, failure.Code, failure.Error)
			}
			if got := len(facts.recorded()); got != before {
				t.Fatalf("%s was refused %s with %d facts applied, want none", c.Name, failure.Code, got-before)
			}
			c.Verdict = failure.Code
		default:
			t.Fatalf("%s was answered %d: %s, want 200 or a 400 of the rating", c.Name, recorder.Code, recorder.Body)
		}
	}
	golden(t, "plan-rating-cases.json", struct {
		Cases []planRatingCase `json:"cases"`
	}{Cases: cases})
}

// unholding is a handler the harness runs ahead of the workflow. For a completion of an issue it
// names that stands held, it gives the issue back the phase it was held from inside the fact's own
// transaction, as an architect's retry of the hold leaves it (workflow's retryPhase): the retry
// committing after the completion route read the issue held and before the fact took the lock
// every fact applies under. So the route reads one phase and the workflow another.
type unholding map[string]bool

func (u unholding) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if completion, ok := fact.(intake.HandoffComplete); ok && u[completion.Issue] {
		if _, err := tx.Exec(ctx, "update issues set phase = held_from, held_from = null, hold_reason = null where key = $1 and phase = $2",
			completion.Issue, string(phase.Held)); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

// ratedStepRecords is how many of the step records key's outbox holds carry a rating of the plan.
func ratedStepRecords(t *testing.T, h *harness, key string) int {
	t.Helper()
	rated := 0
	for _, payload := range stepRecordPayloads(t, h, key) {
		if strings.Contains(payload, `"rating_reason"`) {
			rated++
		}
	}
	return rated
}

// completionSpent is whether a completion of key is recorded as processed, so that the same
// completion sent again is answered HANDOFF_ALREADY_RECORDED (intake.ApplyFact).
func completionSpent(t *testing.T, h *harness, key string) bool {
	t.Helper()
	var spent bool
	if err := h.store.Pool().QueryRow(context.Background(),
		"select exists(select 1 from processed_events where source = 'api' and event_id like 'handoff:' || $1 || ':%')", key).Scan(&spent); err != nil {
		t.Fatalf("read whether a completion of %s is processed: %v", key, err)
	}
	return spent
}

// The rating of the plan is judged where the retro ends: on the issue as the completion's own
// transaction reads it, under the lock every fact applies under, where the workflow ends the
// retro (TriggerRetroCompleted). The route reads the issue before that lock, and the issue can
// move between the two reads: an architect's retry of a hold puts the issue back in its phase at
// the same generation (workflow's retryPhase). unholding plays that retry inside the fact's
// transaction for the rows retried, so the route reads the issue held and the workflow reads the
// phase it was held from. Each row is an issue of its own, worked by an implementer whose claim is
// live and records the plugin contract the row names.
//   - A rated completion from a plugin that must rate, read held and judged in retro: the retro
//     ends with its rating written, in one rated step record.
//   - Its unrated twin: PLAN_RATING_REQUIRED, naming the retro the workflow read, with nothing
//     recorded: no completion on the implementer's row, no rated record, and the fact left
//     unspent. The same completion, rated, is then taken.
//   - A held issue's rated and unrated completions, with no retry: the workflow's own refusal of a
//     held issue (HANDOFF_NOT_CURRENT_PHASE) comes before any rule of the rating.
//   - An unrated completion from a plugin before the rating, in retro: taken, with no rating.
//   - A rated completion read held and judged in implementing: PLAN_RATING_UNEXPECTED, naming
//     implementing, the phase the workflow read, with nothing recorded.
func TestTheRatingIsJudgedOnTheIssueTheWorkflowEndsTheRetroOn(t *testing.T) {
	// standing is how the issue stands when its completion is sent: in its phase, held from it, or
	// held from it and retried inside the fact's transaction before the workflow reads it.
	type standing int
	const (
		inPhase standing = iota
		held
		retried
	)
	retroDone := func(liveClaim) HandoffCompleteRequest {
		return HandoffCompleteRequest{Summary: "retro done", Commit: "c0ffee"}
	}
	implemented := func(c liveClaim) HandoffCompleteRequest {
		return HandoffCompleteRequest{Summary: "implemented", Commit: headSHA, HandoffID: c.writeHandoff("implement", 1), HandoffVersion: 1}
	}
	retry := unholding{}
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard, retry)
	for i, row := range []struct {
		name     string
		at       phase.Phase
		stands   standing
		contract int
		request  func(liveClaim) HandoffCompleteRequest
		rated    bool
		status   int
		code     string
		// says is a part of the refusal's sentence, %s the issue's key.
		says         string
		standAt      phase.Phase
		ratedRecords int
	}{
		{name: "a rated completion from a plugin that must rate, its hold retried after the route read it", at: phase.Retro, stands: retried,
			contract: DaemonAPIVersion(), request: retroDone, rated: true, status: http.StatusOK, standAt: phase.Merging, ratedRecords: 1},
		{name: "its unrated twin", at: phase.Retro, stands: retried, contract: DaemonAPIVersion(), request: retroDone,
			status: http.StatusBadRequest, code: "PLAN_RATING_REQUIRED", says: "this completion of phase retro of %s carries no rating of the plan", standAt: phase.Retro},
		{name: "a rated completion of an issue held from its retro", at: phase.Retro, stands: held, contract: DaemonAPIVersion(), request: retroDone, rated: true,
			status: http.StatusConflict, code: "HANDOFF_NOT_CURRENT_PHASE", standAt: phase.Held},
		{name: "an unrated completion of an issue held from its retro", at: phase.Retro, stands: held, contract: DaemonAPIVersion(), request: retroDone,
			status: http.StatusConflict, code: "HANDOFF_NOT_CURRENT_PHASE", standAt: phase.Held},
		{name: "an unrated completion from a plugin before the rating, in retro", at: phase.Retro, stands: inPhase, contract: PlanRatingContract - 1, request: retroDone,
			status: http.StatusOK, standAt: phase.Merging},
		{name: "a rated completion judged in implementing, its hold retried after the route read it", at: phase.Implementing, stands: retried,
			contract: DaemonAPIVersion(), request: implemented, rated: true, status: http.StatusBadRequest, code: "PLAN_RATING_UNEXPECTED",
			says: "this completion of phase implementing of %s names planRating or planReason", standAt: phase.Implementing},
	} {
		t.Run(row.name, func(t *testing.T) {
			key := fmt.Sprintf("LEGION-%d", 400+i)
			seedIssueAt(t, h, key, row.at)
			implementer := newLiveClaim(t, h, key, claim.RoleImplementer)
			// A live claim registers at this daemon's own contract (DaemonAPIVersion), so a row at
			// another is a claim recorded stale. A stale claim is served in its turn (staleClaim), and
			// its grant is minted first, as TestAnOldPluginRetroStillCompletesWhereItIsServed mints its
			// one.
			req := row.request(implementer)
			req.GrantID = implementer.grant(t)
			if row.contract != DaemonAPIVersion() {
				h.makeStale(t, implementer.token, implementer.session, implementer.secret, row.contract)
			}
			if c := h.stored(implementer.token); c.State != supervise.StateWorking || c.PluginContract != row.contract {
				t.Fatalf("the claim is %s at contract %d, want working at %d: this row proves nothing", c.State, c.PluginContract, row.contract)
			}
			if row.stands != inPhase {
				holdFrom(t, h, key, row.at)
			}
			retry[key] = row.stands == retried
			if row.rated {
				req.PlanRating, req.PlanReason = new(3), new("The plan fit the work but for its tests.")
			}

			recorder := h.complete(req)
			if row.status == http.StatusOK {
				completion(t, recorder)
			} else if message := refused(t, recorder, row.status, row.code); row.says != "" && !strings.Contains(message, fmt.Sprintf(row.says, key)) {
				t.Fatalf("the refusal = %q, want it to say %q", message, fmt.Sprintf(row.says, key))
			}
			var at phase.Phase
			if err := h.store.Pool().QueryRow(context.Background(), "select phase from issues where key = $1", key).Scan(&at); err != nil {
				t.Fatalf("read %s's phase: %v", key, err)
			}
			if at != row.standAt {
				t.Fatalf("after the completion %s stands at %s, want %s", key, at, row.standAt)
			}
			if recorded := phaseRowOf(t, h, key, claim.RoleImplementer); (recorded != nil && recorded.Completed()) != (row.status == http.StatusOK) {
				t.Fatalf("the implementer's row after the completion = %+v, want a completion recorded only on a 200", recorded)
			}
			if got := ratedStepRecords(t, h, key); got != row.ratedRecords {
				t.Fatalf("%s has %d rated step records, want %d: %v", key, got, row.ratedRecords, stepRecordPayloads(t, h, key))
			}
			// A refusal of the rating leaves the fact unspent, so the same completion corrected is
			// applied under its key rather than answered as one already received.
			if strings.HasPrefix(row.code, "PLAN_RATING_") && completionSpent(t, h, key) {
				t.Fatalf("the %s refusal left the completion of %s processed, want it unspent", row.code, key)
			}
			if row.code != "PLAN_RATING_REQUIRED" {
				return
			}
			rated := row.request(implementer)
			rated.GrantID, rated.PlanRating, rated.PlanReason = implementer.grant(t), new(4), new("The plan fit the work.")
			completion(t, h.complete(rated))
			if got := ratedStepRecords(t, h, key); got != 1 {
				t.Fatalf("after the rated completion %s has %d rated step records, want 1", key, got)
			}
		})
	}
}

// A completion ends the retro only while its role works the retro. The reviewer's own approval
// moves the issue on before the reviewer's completion lands (the review event takes reviewing to
// its task's next step, accepting in the default steps this issue walks), and the architect's
// sign-off takes it on to retro, since the acceptance is the task's last step there, so the
// reviewer's late completion of reviewing finds the issue in retro and is recorded on its own row
// (workflow's leftItsPhase), ending nothing. From a plugin that must rate, it is not asked for a
// rating; carrying one, it is refused PLAN_RATING_UNEXPECTED, naming reviewing, with nothing
// recorded. The same completion without the rating is then taken on the reviewer's row, and the
// issue stays in retro unrated.
func TestALateCompletionWhileTheIssueStandsInRetroEndsNoRetro(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	seedApproval(t, h, "LEGION-208", "ccddee")
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	markPhaseStarted(t, h, "LEGION-208", claim.RoleReviewer)
	if got := pluginContractOf(t, h, reviewer.token); got < PlanRatingContract {
		t.Fatalf("the reviewer's plugin declared contract %d, below PlanRatingContract %d: this case proves nothing", got, PlanRatingContract)
	}
	own := reviewer.writeHandoff("review", 1)
	late := HandoffCompleteRequest{Summary: "approved", HandoffID: own, HandoffVersion: 1}

	rated := late
	rated.GrantID, rated.PlanRating, rated.PlanReason = reviewer.grant(t), new(4), new("The plan fit the work.")
	if message := refused(t, h.complete(rated), http.StatusBadRequest, "PLAN_RATING_UNEXPECTED"); !strings.Contains(message, "this completion of phase reviewing of LEGION-208") {
		t.Fatalf("the refusal = %q, want it to name the reviewing the reviewer left", message)
	}
	if row := phaseRowOf(t, h, "LEGION-208", claim.RoleReviewer); row != nil && row.HandoffRecord == own {
		t.Fatalf("the reviewer's row after the refusal = %+v, want its record not recorded", *row)
	}

	late.GrantID = reviewer.grant(t)
	completion(t, h.complete(late))
	if row := phaseRowOf(t, h, "LEGION-208", claim.RoleReviewer); row == nil || row.HandoffRecord != own {
		t.Fatalf("the reviewer's row after its late completion = %+v, want it naming %s", row, own)
	}
	assertIssuePhase(t, h, phase.Retro)
	if got := ratedStepRecords(t, h, "LEGION-208"); got != 0 {
		t.Fatalf("LEGION-208 has %d rated step records after the reviewer's late completion, want none", got)
	}
}
