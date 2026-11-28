package api

// going_plan_near_copy_test.go holds what a plan's create of an issue comes to when the near-copy
// gate of POST /api/v1/issues refuses it 409 POSSIBLE_DUPLICATE (forcedSiblingCopy,
// going_plan_run.go): a near copy of issues other creates of the same plan made, and of no other
// issue, goes in with force, sent as one more request of its call, and a near copy of any other
// issue stays refused, with each row that needs it skipped.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// newNearCopyWritesRig is the writes rig whose near-copy gate runs under nearCopyVerdictBound, set
// before anything listens, so a create the gate refuses is refused for its words and never for the
// machine's speed (newDuplicateTestHandler).
func newNearCopyWritesRig(t *testing.T) goingWritesRig {
	t.Helper()
	writes := newGoingWritesRig(t)
	writes.served.duplicateBound = nearCopyVerdictBound(t)
	return writes
}

// titled is the key of each issue titled title, in order.
func (rig goingWritesRig) titled(t *testing.T, title string) []string {
	t.Helper()
	var keys []string
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select coalesce(array_agg(key order by key), '{}') from issues where title = $1`, title).Scan(&keys); err != nil {
		t.Fatalf("read the issues titled %q: %v", title, err)
	}
	return keys
}

// nearCopyRefused is the key of each issue the near-copy gate named when it refused the create
// titled title 409 POSSIBLE_DUPLICATE: each candidate of the refusal's near_copy_checks row at or
// above duplicateRefuseScore, which is what the 409 names (duplicateGateResult.refusals), sorted.
func (rig goingWritesRig) nearCopyRefused(t *testing.T, title string) []string {
	t.Helper()
	var encoded []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select candidates from near_copy_checks where title = $1 and outcome = 'refused'
	`, title).Scan(&encoded); err != nil {
		t.Fatalf("read the near-copy refusal of the create titled %q: %v", title, err)
	}
	var candidates []model.NearCopyCheckCandidate
	if err := json.Unmarshal(encoded, &candidates); err != nil {
		t.Fatalf("decode the candidates %s of the refusal of %q: %v", encoded, title, err)
	}
	var keys []string
	for _, candidate := range candidates {
		if candidate.Score >= duplicateRefuseScore {
			keys = append(keys, candidate.Key)
		}
	}
	slices.Sort(keys)
	return keys
}

// TestAPlanCreateRefusedAsANearCopy: the near-copy gate refuses a create whose title and spec hold
// the rare words of an open issue's, 409 POSSIBLE_DUPLICATE. A plan that splits an epic into two
// issues whose specs differ in one word makes both: the second create, refused as a near copy of
// the first, which the same plan made, goes in with force, sent as the second request of its call,
// and the move under it runs. A create refused as a near copy of an issue made before the plan
// stays refused, its row failed with the 409 that names that issue, and the move that needs it is
// skipped, ROW_SKIPPED; so does a create refused as a near copy of both an issue the plan made and
// an older one. Each case writes words of its own, so no case's issues are near copies of
// another's.
func TestAPlanCreateRefusedAsANearCopy(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRigOver(t, newNearCopyWritesRig(t))
	parent := func(t *testing.T, issue string) string {
		t.Helper()
		if held := rig.issueRead(t, issue).Parent; held != nil {
			return *held
		}
		return ""
	}
	only := func(t *testing.T, title string) string {
		t.Helper()
		keys := rig.titled(t, title)
		if len(keys) != 1 {
			t.Fatalf("the issues titled %q are %v, want one", title, keys)
		}
		return keys[0]
	}

	t.Run("a near copy of an issue the plan made goes in with force, and the move under it runs", func(t *testing.T) {
		export := func(format string) string {
			return "Export the board's cards as a " + format + " file: one line for each card, with its column, " +
				"its assignee, its labels and its due date, sorted by column.\n"
		}
		epic := rig.issue(t, "Reporting epic")
		child := rig.childOf(t, "Printable legend for the totals", epic)
		run := rig.ran(t, "split the reporting epic into CSV export and PDF export, and move its legend under PDF export",
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "CSV export", "parent": epic, "spec": export("CSV")}),
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "PDF export", "parent": epic, "spec": export("PDF")}),
			goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$1"}, 1))
		plan := rig.plan(t, run)
		if states := rowStates(plan); !slices.Equal(states, []string{"done", "done", "done"}) {
			t.Fatalf("the rows are %v, want all three done; the calls were told %q", states, rig.toolResults(t, run))
		}
		csv, pdf := only(t, "CSV export"), only(t, "PDF export")
		if refused := rig.nearCopyRefused(t, "PDF export"); !slices.Equal(refused, []string{csv}) {
			t.Fatalf("the gate refused PDF export as a near copy of %v, want %s alone, the issue the plan made", refused, csv)
		}
		if made := plan.Rows[1].Result; made == nil || made.Status != http.StatusCreated || made.Code != "" {
			t.Errorf("PDF export's row holds %+v, want the forced create's 201", made)
		}
		if forced := createdEventPayload(t, rig.mux, pdf).Forced; forced == nil || !slices.Equal(forced.Past, []string{csv}) {
			t.Errorf("PDF export's issue.created records forced %+v, want it forced past %s", forced, csv)
		}
		if landed := rig.count(t, `
			select count(*) from going_landings where run_id = $1::uuid and phase = 'do' and path = '/api/v1/issues' and request = 1
		`, run); landed != 1 {
			t.Errorf("the run landed %d creates as the second request of a call, want PDF export's forced create", landed)
		}
		if moved := parent(t, child); moved != pdf {
			t.Errorf("the legend's parent is %q, want %s, PDF export", moved, pdf)
		}
	})

	t.Run("a near copy of an issue made before the plan stays refused, and the move that needs it is skipped", func(t *testing.T) {
		tare := func(verb string) string {
			return "Each weighbridge ticket " + verb + " its tare weight after the scale reboots, so the net weight " +
				"printed on the ticket is the gross weight of the lorry.\n"
		}
		older := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Weighbridge tickets lose the tare", "spec": tare("loses"), "force": true,
		})).Key
		epic := rig.issue(t, "Yard office epic")
		child := rig.childOf(t, "Gatehouse barrier sticks", epic)
		const title = "Weighbridge tickets shed the tare"
		run := rig.ran(t, "file the weighbridge tare fault under the yard office epic, and move the barrier under it",
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": title, "parent": epic, "spec": tare("sheds")}),
			goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$0"}, 0))
		plan := rig.plan(t, run)
		if states := rowStates(plan); !slices.Equal(states, []string{"failed", "skipped"}) {
			t.Fatalf("the rows are %v, want the create failed and the move skipped; the calls were told %q", states, rig.toolResults(t, run))
		}
		if refused := rig.nearCopyRefused(t, title); !slices.Equal(refused, []string{older}) {
			t.Fatalf("the gate refused the create as a near copy of %v, want %s alone, the issue made before the plan", refused, older)
		}
		if refused := plan.Rows[0].Result; refused == nil || refused.Status != http.StatusConflict || refused.Code != "POSSIBLE_DUPLICATE" ||
			!strings.Contains(refused.Message, older) {
			t.Errorf("the create's row holds %+v, want its 409 POSSIBLE_DUPLICATE naming %s", refused, older)
		}
		if skipped := plan.Rows[1].Result; skipped == nil || skipped.Code != agentwire.CodeRowSkipped || !strings.Contains(skipped.Message, "issue "+title) {
			t.Errorf("the move's row holds %+v, want ROW_SKIPPED naming the create it needs, issue %s", skipped, title)
		}
		if made := rig.titled(t, title); len(made) != 0 {
			t.Errorf("the run made %v titled %q, want none", made, title)
		}
		if landed := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and path = '/api/v1/issues'`, run); landed != 0 {
			t.Errorf("the run landed %d creates, want none", landed)
		}
		if kept := parent(t, child); kept != epic {
			t.Errorf("the barrier's parent is %q, want %s kept", kept, epic)
		}
	})

	t.Run("a near copy of an issue the plan made and of an older issue stays refused", func(t *testing.T) {
		kiln := func(verb string) string {
			return "The kiln's firing log " + verb + " the witness cone slump whenever the thermocouple restarts, " +
				"so the glaze schedule cannot be audited against the cone.\n"
		}
		older := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Kiln log skips the witness cone", "spec": kiln("skips"), "force": true,
		})).Key
		epic := rig.issue(t, "Studio epic")
		child := rig.childOf(t, "Wheel splash pan cracks", epic)
		const title = "Kiln log drops the witness cone"
		run := rig.ran(t, "file the two kiln log faults under the studio epic, and move the splash pan under the second",
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Kiln log misses the witness cone", "parent": epic,
				"spec": kiln("misses"), "force": true}),
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": title, "parent": epic, "spec": kiln("drops")}),
			goingChange("dispatch_issue_update", map[string]any{"issue": child, "parent": "$1"}, 1))
		plan := rig.plan(t, run)
		if states := rowStates(plan); !slices.Equal(states, []string{"done", "failed", "skipped"}) {
			t.Fatalf("the rows are %v, want the first create done, the second failed and the move skipped; the calls were told %q",
				states, rig.toolResults(t, run))
		}
		ours := only(t, "Kiln log misses the witness cone")
		if refused, want := rig.nearCopyRefused(t, title), slices.Sorted(slices.Values([]string{ours, older})); !slices.Equal(refused, want) {
			t.Fatalf("the gate refused the create as a near copy of %v, want %v: %s, which the plan made, and %s, made before it",
				refused, want, ours, older)
		}
		if refused := plan.Rows[1].Result; refused == nil || refused.Status != http.StatusConflict || refused.Code != "POSSIBLE_DUPLICATE" {
			t.Errorf("the second create's row holds %+v, want its 409 POSSIBLE_DUPLICATE", refused)
		}
		if skipped := plan.Rows[2].Result; skipped == nil || skipped.Code != agentwire.CodeRowSkipped || !strings.Contains(skipped.Message, "issue "+title) {
			t.Errorf("the move's row holds %+v, want ROW_SKIPPED naming the create it needs, issue %s", skipped, title)
		}
		if made := rig.titled(t, title); len(made) != 0 {
			t.Errorf("the run made %v titled %q, want none", made, title)
		}
		if kept := parent(t, child); kept != epic {
			t.Errorf("the splash pan's parent is %q, want %s kept", kept, epic)
		}
	})
}

// TestASecondTryOfAForcedCreateAnswersItsLanding: the create that goes in with force, as a near
// copy of the issue an earlier create of its plan made, is the second request of its call, with a
// landing of its own under the call's key. A second try of that key once the forced create landed
// and before its row recorded it - the server ended between the create's commit and its answer -
// answers that landing, replayed, and sends nothing: neither the create the gate refused nor the
// forced one reaches the route again, the plan holds one issue for the row, and the row is done
// with the forced create's 201.
func TestASecondTryOfAForcedCreateAnswersItsLanding(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRigOver(t, newNearCopyWritesRig(t), nil)
	sieve := func(verb string) string {
		return "The sieve's brass mesh " + verb + " at the rim after a wet sift, so the flour carries grit " +
			"and the bran settles into the hopper.\n"
	}
	run, token := rig.working(t, "alice", "split the sieve fault in two")
	first := map[string]any{"project": "DEMO", "title": "Sieve mesh tears at the rim", "spec": sieve("tears")}
	second := map[string]any{"project": "DEMO", "title": "Sieve mesh frays at the rim", "spec": sieve("frays")}
	rig.approve(t, run, "dispatch_issue", first)
	rig.approve(t, run, "dispatch_issue", second)
	if made, err := rig.call(goingRouteCall(run, token, 3, 1, "dispatch_issue", first)); err != nil || made.Status != http.StatusCreated {
		t.Fatalf("the first create answered %d %s, %v; want 201", made.Status, made.Body, err)
	}
	var creates atomic.Int32
	rig.hooks.set(t, http.MethodPost, "/api/v1/issues", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
		creates.Add(1)
		inner.ServeHTTP(w, r)
	})
	keyOf := func(t *testing.T, body []byte) string {
		t.Helper()
		var issue struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(body, &issue); err != nil || issue.Key == "" {
			t.Fatalf("the create answered %s (%v), which names no issue", body, err)
		}
		return issue.Key
	}
	forced, err := rig.call(goingRouteCall(run, token, 3, 2, "dispatch_issue", second))
	if err != nil || forced.Status != http.StatusCreated || forced.Replayed {
		t.Fatalf("the second create answered %d %s (replayed %v), %v; want the forced create's 201", forced.Status, forced.Body, forced.Replayed, err)
	}
	if sent := creates.Load(); sent != 2 {
		t.Fatalf("the second create reached its route %d times, want twice: the create the gate refused and the forced one", sent)
	}
	if landed := rig.count(t, `
		select count(*) from going_landings where run_id = $1::uuid and phase = 'do' and step = 3 and call = 2 and request = 1
	`, run); landed != 1 || rig.count(t, `select count(*) from going_landings where run_id = $1::uuid and step = 3 and call = 2`, run) != 1 {
		t.Fatalf("the second create's key holds %d landings at its second request, want the forced create's one landing there and none else", landed)
	}
	// The server ended after the forced create committed and before the row recorded it: the row
	// reads running under the call's key.
	rig.exec(t, `update going_plan_rows set state = 'running', result = jsonb_build_object('step', 3, 'call', 2) where run_id = $1::uuid and row = 2`, run)
	again, err := rig.call(goingRouteCall(run, token, 3, 2, "dispatch_issue", second))
	if err != nil || !again.Replayed || again.Status != http.StatusCreated || keyOf(t, again.Body) != keyOf(t, forced.Body) {
		t.Fatalf("the second try answered %d %s (replayed %v), %v; want the forced create's landing, %s, replayed",
			again.Status, again.Body, again.Replayed, err, keyOf(t, forced.Body))
	}
	if sent := creates.Load(); sent != 2 {
		t.Fatalf("the creates reached their route %d times, want twice: the second try sends nothing", sent)
	}
	if made := rig.titled(t, "Sieve mesh frays at the rim"); len(made) != 1 {
		t.Fatalf("the issues of the second create are %v, want one", made)
	}
	if done := rig.count(t, `
		select count(*) from going_plan_rows where run_id = $1::uuid and row = 2 and state = 'done' and (result->>'status')::int = 201
	`, run); done != 1 {
		t.Fatalf("after the second try %d rows are the second create's done with 201, want its one row", done)
	}
}
