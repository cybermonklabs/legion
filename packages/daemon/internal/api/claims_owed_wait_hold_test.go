package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// owedServed is the owed answer c's own session is served, raw by its top-level keys, asked with
// the secret its registration issued it and stating contract in pluginContractHeader: a key is in
// the map exactly when it is on the wire.
func (c liveClaim) owedServed(t *testing.T, contract int) map[string]json.RawMessage {
	t.Helper()
	recorder := c.h.request(http.MethodPost, "/legion/v1/claims/owed",
		OwedRequest{ClaimToken: c.token, SessionID: c.session, Secret: c.secret},
		http.Header{pluginContractHeader: []string{strconv.Itoa(contract)}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("owed for %s at contract %d = %d, want 200; body %s", c.token, contract, recorder.Code, recorder.Body)
	}
	var served map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &served); err != nil {
		t.Fatalf("decode the owed answer of %s: %v; body %s", c.token, err, recorder.Body)
	}
	return served
}

// wantWait asserts the owed answer got gives the wait want, with its four facts as the
// declaration's answer named them.
func wantWait(t *testing.T, got OwedResponse, want WaitView) {
	t.Helper()
	if got.Wait == nil {
		t.Fatalf("the owed answer is %+v, want it to give the wait %+v", got, want)
	}
	if wait := *got.Wait; wait.ID != want.ID || wait.WaitingOn != want.WaitingOn ||
		!wait.WakeAt.Equal(want.WakeAt) || !wait.DeclaredAt.Equal(want.DeclaredAt) {
		t.Fatalf("the owed answer gives the wait %+v, want %+v", wait, want)
	}
}

// applyGateFact applies fact under id through the wait harness's fact handlers, the workflow
// first, as the Dispatch consumer would, and fails the test on an error or a refusal.
func (h *waitHarness) applyGateFact(t *testing.T, id string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", id, fact, h.handlers()...)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply %s (%T) = %+v, %v; want it taken", id, fact, result.Refusal, err)
	}
}

// specGate is LEGION-208's spec gate as the record holds it now.
func (h *waitHarness) specGate(t *testing.T) record.DesignGate {
	t.Helper()
	var gates map[record.DesignStep]record.DesignGate
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gates, err = record.NewStore().GatesOf(ctx, tx, "LEGION-208")
		return err
	}); err != nil {
		t.Fatalf("read the gates of LEGION-208: %v", err)
	}
	return gates[record.DesignStepSpec]
}

// The owed answer gives the wait the claim's worker declared while that wait stands, with the four
// keys the declaration's answer named, beside the work the claim owes: the plugin reads from it that
// the worker waits rather than stopped (dispatch://INFRA-2040 R3, R4). It gives no wait once the
// wait has ended, by the plugin's report of news here. It gives none either for a wait with no end
// that no longer stands, here because its phase moved, since the answer reads the one rule every
// reader of a wait asks (workflow.WaitStands) and not the row.
func TestClaimOwedGivesTheStandingWaitAndNoneOnceItEnds(t *testing.T) {
	t.Run("the wait stands, then news ends it", func(t *testing.T) {
		h := newWaitHarness(t)
		h.waitIssue(phase.Implementing, nil)
		c := h.waitingWorker(t, claim.RoleImplementer)
		if got := c.owed(t); got.Wait != nil || got.Hold != nil {
			t.Fatalf("the implementer that declared nothing is told %+v, want no wait and no hold", got)
		}

		wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
		got := c.owed(t)
		if !got.Owes || got.Work != "the implementing phase of LEGION-208" || got.Hold != nil {
			t.Fatalf("the waiting implementer is told %+v, want it to owe its phase, held by nothing", got)
		}
		wantWait(t, got, wait)

		if answer := newsAnswer(t, h.news(c, wait.ID)); !answer.Ended {
			t.Fatalf("the report of news = %+v, want the wait ended: the case's own premise", answer)
		}
		if got := c.owed(t); got.Wait != nil || !got.Owes {
			t.Fatalf("the implementer whose wait news ended is told %+v, want its phase owed and no wait", got)
		}
	})
	t.Run("a wait with no end whose phase moved", func(t *testing.T) {
		h := newWaitHarness(t)
		h.waitIssue(phase.Implementing, nil)
		c := h.waitingWorker(t, claim.RoleImplementer)
		wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
		wantWait(t, c.owed(t), wait)

		h.waitIssue(phase.Testing, nil)
		if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || !rows[0].EndedAt.IsZero() {
			t.Fatalf("claim_waits holds %+v, want wait %s with no end: the case's own premise", rows, wait.ID)
		}
		if got := c.owed(t); got.Wait != nil {
			t.Fatalf("the implementer whose phase moved is told %+v, want no wait: a wait whose phase moved does not stand", got)
		}
	})
}

// An update round's relaunch of the same session keeps the wait (R3: "a relaunch of the same
// session" ends no wait), so the relaunched process, which remembers nothing and holds a new
// secret, is told the wait its predecessor declared, during the round and after it.
func TestClaimOwedGivesTheWaitThroughTheUpdateRoundsRelaunchOfTheSameSession(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	wait := declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
	h.openRound(t, c)
	before := h.stored(c.token)

	relaunched := c
	relaunched.secret = h.registered(h.relaunch(c.token), c.session).Secret
	if after := h.stored(c.token); after.Generation <= before.Generation || after.Session != c.session {
		t.Fatalf("the claim after the round's relaunch is at generation %d in session %q, want past %d in %q",
			after.Generation, after.Session, before.Generation, c.session)
	}
	wantWait(t, relaunched.owed(t), wait)

	h.endRound(t, c)
	got := relaunched.owed(t)
	if !got.Owes || got.Work != "the implementing phase of LEGION-208" {
		t.Fatalf("the relaunched implementer is told %+v after the round, want its phase owed", got)
	}
	wantWait(t, got, wait)
}

// A merger in merging whose tree's design gate closes while it works is told, at its turn's end,
// the merge it owes and the gate's hold: the gated step, the document and the version that is with
// its reviewer (dispatch://INFRA-2040 R4, R9), read by the rule the outbox's fence reads
// (workflow.HeldByDesignGate). Once a person approves that version the gate opens, and the merger
// is told the merge with no hold. The gate closes and opens by the workflow's own facts.
func TestClaimOwedGivesTheMergeAndTheGatesHoldUntilTheGateOpens(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Merging, nil)
	recordPullRequest(t, h.harness, record.PullRequestOpen)
	h.putGate(t, 3, true)
	merger := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleMerger)
	merging := "the merging phase of LEGION-208"
	if got := merger.owed(t); !got.Owes || got.Work != merging || got.Hold != nil {
		t.Fatalf("the merger under the open gate is told %+v, want the merge and no hold", got)
	}

	h.applyGateFact(t, "spec-4", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: waitSpec,
		Kind: intake.DispatchArtifactVersion, Version: 4})
	got := merger.owed(t)
	if !got.Owes || got.Work != merging {
		t.Fatalf("the held merger is told %+v, want the merge it owes", got)
	}
	if want := (HoldView{Step: record.DesignStepSpec, ArtifactID: waitSpec, Version: 4}); got.Hold == nil || *got.Hold != want {
		t.Fatalf("the merger under the closed gate is told the hold %+v, want %+v", got.Hold, want)
	}
	if got.Wait != nil {
		t.Fatalf("the held merger is told the wait %+v, want none: it declared none", got.Wait)
	}

	h.applyGateFact(t, "spec-4-approved", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: waitSpec,
		Kind: intake.DispatchArtifactApproved, Version: 4, Actor: dispatch.Actor{Kind: "user", ID: "alice"}})
	if gate := h.specGate(t); gate.ApprovedVersion == nil || *gate.ApprovedVersion != 4 {
		t.Fatalf("the spec gate after the approval = %+v, want version 4 approved: the case's own premise", gate)
	}
	if got := merger.owed(t); !got.Owes || got.Work != merging || got.Hold != nil {
		t.Fatalf("the merger once the gate opened is told %+v, want the merge and no hold", got)
	}
}

// A gated step that nobody registered holds no merger (dispatch://INFRA-2040 R9), and the owed
// answer reads the hold by the one rule (workflow.HeldByDesignGate), so it names no hold. The
// deployment gates the criteria and the spec steps, the spec's row is approved and the criteria
// step has no row: the tree's gate reads closed and no step stands closed with a row of its own.
// Once someone registers the criteria step and its document waits on its reviewer, the step holds
// the merger, and the answer names it.
func TestNoRowHoldsNoMergerInTheOwedAnswer(t *testing.T) {
	steps := []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}
	h := newWaitHarnessGating(t, steps)
	h.waitIssue(phase.Merging, nil)
	recordPullRequest(t, h.harness, record.PullRequestOpen)
	h.putGate(t, 1, true)
	if open := (record.TreeGate{Steps: steps, Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: h.specGate(t)}}).Open(); open {
		t.Fatal("the tree's gate reads open, want it closed on the criteria step, which has no row: the case's own premise")
	}
	merger := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleMerger)
	if got := merger.owed(t); !got.Owes || got.Work != "the merging phase of LEGION-208" || got.Hold != nil {
		t.Fatalf("the merger is told %+v, want the merge and no hold: a gated step nobody registered holds no merger", got)
	}

	criteria := record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepCriteria, ArtifactID: "criteria-208", LatestVersion: 2}
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutGate(ctx, tx, criteria)
	}); err != nil {
		t.Fatalf("register the criteria step of LEGION-208: %v", err)
	}
	want := HoldView{Step: record.DesignStepCriteria, ArtifactID: "criteria-208", Version: 2}
	if got := merger.owed(t); got.Hold == nil || *got.Hold != want {
		t.Fatalf("the merger once the criteria step is registered is told the hold %+v, want %+v", got.Hold, want)
	}
}

// The owed answer arrived at the repository correction's entry, and the wait entry added `wait`
// and `hold` to it, so a caller below the wait entry's number (entryNumber(waitContract)) is served
// the answer without either key, exactly as it is served every claim view of the state
// (shaping.go's withoutOwedWaitAndHold), and the keys it does read are as a caller at that number
// reads them. A caller states its contract in pluginContractHeader: every plugin that knows the
// route does.
func TestClaimOwedServesACallerBelowTheWaitContractNoWaitAndNoHold(t *testing.T) {
	added := entryNumber(waitContract)
	for _, tc := range []struct {
		name string
		// key is the key the claim is served at the wait entry's number.
		key     string
		arrange func(t *testing.T, h *waitHarness) liveClaim
	}{
		{name: "a waiting implementer", key: "wait", arrange: func(t *testing.T, h *waitHarness) liveClaim {
			h.waitIssue(phase.Implementing, nil)
			c := h.waitingWorker(t, claim.RoleImplementer)
			declared(t, h.declare(c, "the review of LEGION-207's pull request", waitNow.Add(time.Hour)))
			return c
		}},
		{name: "a merger the closed gate holds", key: "hold", arrange: func(t *testing.T, h *waitHarness) liveClaim {
			h.waitIssue(phase.Merging, nil)
			recordPullRequest(t, h.harness, record.PullRequestOpen)
			h.putGate(t, 4, false)
			return newLiveClaim(t, h.harness, "LEGION-208", claim.RoleMerger)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWaitHarness(t)
			c := tc.arrange(t, h)
			at := c.owedServed(t, added)
			if _, served := at[tc.key]; !served {
				t.Fatalf("the caller at %d is served %v, want `%s`: the case's own premise", added, at, tc.key)
			}
			below := c.owedServed(t, added-1)
			for _, key := range waitKeys {
				if _, served := below[key]; served {
					t.Errorf("the caller at %d is served `%s` (%s), want no such key", added-1, key, below[key])
				}
			}
			for _, key := range []string{"owes", "work", "architect"} {
				if string(below[key]) != string(at[key]) {
					t.Errorf("the caller at %d is served `%s` as %s, want %s, as the caller at %d is", added-1, key, below[key], at[key], added)
				}
			}
		})
	}
}
