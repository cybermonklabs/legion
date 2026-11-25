package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// applyStates applies one check.state_changed event of issue at head of repo for each of states,
// the first at serial and each next one at the serial after, through intake.ApplyFact and the
// harness's workflow (newWorkflowHandoffHarness), as the live consumer applies a fact
// (checkstatetest.Apply). A harness that records facts rather than deciding them applies the
// events through a workflow of the project's own configuration, which holds them as the daemon's
// does.
func applyStates(t *testing.T, h *harness, issue, repo, head string, serial int64, states ...intake.DispatchCheckState) {
	t.Helper()
	checkstatetest.Apply(t, h.store.Pool(), issue, repo, head, serial, states, statesEngine(h))
}

// statesEngine is the workflow a state event of h applies through: the harness's own, or, for a
// harness that records facts rather than deciding them, one of the project's own configuration.
func statesEngine(h *harness) *workflow.Engine {
	if h.engine != nil {
		return h.engine
	}
	return workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// testedAs applies, ahead of a tester's completion of issue's testing whose record names no
// serial, the state event that decides its move (checkstatetest.Tested): k1, the head's one check,
// yes from the tester where passes, and no for a fail where not, at head, the commit the
// completion names, on acme/widgets where issue has no pull request.
func testedAs(t *testing.T, h *harness, issue, head string, passes bool) {
	t.Helper()
	fact, id := checkstatetest.Tested(t, h.store.Pool(), issue, "acme/widgets", head, passes)
	if result, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", id, fact, statesEngine(h)); err != nil || result.Refusal != nil {
		t.Fatalf("apply the state event %s: %+v, %v", id, result.Refusal, err)
	}
}

// passOf is check's state yes from roles, at a head whose set is checks.
func passOf(check string, checks []string, roles ...string) intake.DispatchCheckState {
	return intake.DispatchCheckState{Check: check, State: "yes", Roles: roles, Checks: checks}
}

// writeTestHandoff is c writing version of its issue's test handoff under its own session, which
// Dispatch stored with serial as the issue's check states serial once the states its results
// changed were published (`check_states_serial`), and returns its id.
func (c liveClaim) writeTestHandoff(version int, serial int64) string {
	id := c.writeHandoff("test", version)
	handoff := c.h.handoffs.handoffs[id]
	handoff.CheckStatesSerial = serial
	c.h.handoffs.handoffs[id] = handoff
	return id
}

// A phase completion carries no verdict. A tester's completion naming one, from a plugin at
// CheckStatesContract, is refused 400 VERDICT_NOT_TAKEN with nothing recorded, and the same
// completion with none is taken, carrying the check states serial of the record it names. Another
// role's completion naming one is refused alike. A tester's plugin below the contract, mid-turn
// when its daemon was upgraded, still sends pass: the route takes the completion and ignores the
// verdict, whose key is the completion's with none, and the tester's phase row holds no verdict.
// Another role below the contract is refused.
func TestAPhaseCompletionCarriesNoVerdict(t *testing.T) {
	if DaemonAPIVersion < CheckStatesContract {
		t.Fatalf("the daemon declares contract %d, below CheckStatesContract %d", DaemonAPIVersion, CheckStatesContract)
	}
	t.Run("a tester's plugin at the contract", func(t *testing.T) {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		seedIssueAt(t, h, "LEGION-208", phase.Testing)
		tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
		written := tester.writeTestHandoff(1, 5)
		refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "Every check passes.", Verdict: "pass", Commit: headSHA, HandoffID: written, HandoffVersion: 1}),
			http.StatusBadRequest, "VERDICT_NOT_TAKEN")
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("facts after the refusal = %#v, want none", got)
		}
		completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "Every check passes.", Commit: headSHA, HandoffID: written, HandoffVersion: 1}))
		if fact := lastCompletion(t, facts); fact.Role != claim.RoleTester || fact.HandoffRecord != written || fact.StatesSerial != 5 || fact.Commit != headSHA {
			t.Fatalf("the completion = %+v, want the tester's naming %s at %s with the record's serial 5", fact, written, headSHA)
		}
	})
	t.Run("another role's plugin at the contract", func(t *testing.T) {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		seedIssueAt(t, h, "LEGION-208", phase.Implementing)
		implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
		written := implementer.writeHandoff("implement", 1)
		refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Verdict: "pass", Commit: headSHA, HandoffID: written, HandoffVersion: 1}),
			http.StatusBadRequest, "VERDICT_NOT_TAKEN")
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("facts after the refusal = %#v, want none", got)
		}
	})
	t.Run("a tester's plugin below the contract", func(t *testing.T) {
		h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
		seedIssueAt(t, h, "LEGION-208", phase.Testing)
		seedPullRequest(t, h, "LEGION-208", headSHA)
		tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
		grant := tester.grant(t)
		h.makeStale(t, tester.token, tester.session, tester.secret, CheckStatesContract-1)
		if c := h.stored(tester.token); c.State != supervise.StateWorking || c.PluginContract != CheckStatesContract-1 {
			t.Fatalf("the claim is %s at contract %d, want working at %d: this case proves nothing", c.State, c.PluginContract, CheckStatesContract-1)
		}
		written := tester.writeTestHandoff(1, 0)
		completion(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "Every check passes.", Verdict: "pass", Commit: headSHA, HandoffID: written, HandoffVersion: 1}))
		var row map[string]any
		var keys []string
		if err := h.store.Pool().QueryRow(context.Background(), `select to_jsonb(p) from phases p where issue = 'LEGION-208' and role = 'tester'`).Scan(&row); err != nil {
			t.Fatalf("read the tester's phase row: %v", err)
		}
		// phases.verdict stays for the release before, and this release writes nothing to it, so
		// the pass the plugin sent is not stored: the row holds the column's default.
		if row["verdict"] != "" || row["handoff_record"] != written {
			t.Fatalf("the tester's phase row = %v, want the completion naming %s and an empty verdict", row, written)
		}
		rows, err := h.store.Pool().Query(context.Background(), `select event_id from processed_events where source = 'api'`)
		if err != nil {
			t.Fatalf("read the processed completions: %v", err)
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				t.Fatalf("read a processed completion: %v", err)
			}
			keys = append(keys, key)
		}
		if len(keys) != 1 || !strings.HasSuffix(keys[0], ":"+written+":") {
			t.Fatalf("the processed completions = %q, want one whose key ends with the record and an empty verdict segment", keys)
		}
		if at, _, _ := handoffRecord(t, h, "LEGION-208"); at != phase.Testing {
			t.Fatalf("after the completion LEGION-208 is in %s, want testing: no state is held, so the verdict pass moved nothing", at)
		}
	})
	t.Run("another role's plugin below the contract", func(t *testing.T) {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		seedIssueAt(t, h, "LEGION-208", phase.Reviewing)
		reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
		grant := reviewer.grant(t)
		h.makeStale(t, reviewer.token, reviewer.session, reviewer.secret, CheckStatesContract-1)
		written := reviewer.writeHandoff("review", 1)
		refused(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "reviewed", Verdict: "pass", Commit: headSHA, HandoffID: written, HandoffVersion: 1}),
			http.StatusBadRequest, "VERDICT_NOT_TAKEN")
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("facts after the refusal = %#v, want none", got)
		}
	})
}

// A tester's phase completion names the head it tested, since the testing move reads Dispatch's
// check states at that head and nothing else names it: not the pull request's head, which can
// have moved since. One that names no commit is refused 400 TESTED_HEAD_REQUIRED before anything
// is recorded, telling the tester to write a new test handoff naming the commit it tested and
// complete again, and the same completion naming the commit is then taken. A tester's plugin
// below CheckStatesContract, mid-turn when its daemon was upgraded, sends the head its record names
// beside its verdict: it is taken, the verdict ignored; one that sends no head is refused the same
// way.
func TestATestersCompletionNamesTheHeadItTested(t *testing.T) {
	for _, tc := range []struct {
		name    string
		below   bool
		verdict string
	}{
		{"a tester's plugin at the contract", false, ""},
		{"a tester's plugin below the contract", true, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			grant := tester.grant(t)
			if tc.below {
				h.makeStale(t, tester.token, tester.session, tester.secret, CheckStatesContract-1)
				if c := h.stored(tester.token); c.PluginContract != CheckStatesContract-1 {
					t.Fatalf("the claim is at contract %d, want %d: this case proves nothing", c.PluginContract, CheckStatesContract-1)
				}
			}
			written := tester.writeTestHandoff(1, 3)
			refused(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "Every check passes.", Verdict: tc.verdict, HandoffID: written, HandoffVersion: 1}),
				http.StatusBadRequest, "TESTED_HEAD_REQUIRED")
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusal = %#v, want none", got)
			}
			completion(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "Every check passes.", Verdict: tc.verdict, Commit: headSHA, HandoffID: written, HandoffVersion: 1}))
			if fact := lastCompletion(t, facts); fact.Role != claim.RoleTester || fact.Commit != headSHA || fact.HandoffRecord != written || fact.StatesSerial != 3 {
				t.Fatalf("the completion = %+v, want the tester's at %s naming %s with the record's serial 3", fact, headSHA, written)
			}
		})
	}
}

// The tester's move reads nothing of Dispatch but the test handoff record the completion names:
// the tester completes testing at the head before the state of k2 arrives, and the issue waits;
// the state event of k2 then arrives and moves it to reviewing. The Dispatch stand-in's log of
// every request it answered holds the one read of that record, and no read of results, events or
// a check.
func TestTheTesterMoveReadsNothingOfDispatchButTheHandoff(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	checks := []string{"k1", "k2"}
	applyStates(t, h, "LEGION-208", "acme/widgets", headSHA, 1, passOf("k1", checks, "tester"))
	written := tester.writeTestHandoff(1, 1)
	grant := tester.grant(t)
	h.handoffs.requests = nil

	completion(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "Every check passes.", Commit: headSHA, HandoffID: written, HandoffVersion: 1}))
	if at, _, _ := handoffRecord(t, h, "LEGION-208"); at != phase.Testing {
		t.Fatalf("after the completion LEGION-208 is in %s, want testing: no state of k2 is held", at)
	}
	applyStates(t, h, "LEGION-208", "acme/widgets", headSHA, 2, passOf("k2", checks, "tester"))
	if at, _, _ := handoffRecord(t, h, "LEGION-208"); at != phase.Reviewing {
		t.Fatalf("after the state of k2 LEGION-208 is in %s, want reviewing", at)
	}
	if want := []string{"GET /api/v1/handoffs/" + written}; !slices.Equal(h.handoffs.requests, want) {
		t.Fatalf("the daemon asked Dispatch %q, want %q alone", h.handoffs.requests, want)
	}
}
