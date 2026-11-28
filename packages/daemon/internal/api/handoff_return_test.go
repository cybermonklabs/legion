package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// returnToNeedsFail is the refusal of a returnTo on what names it, as returnToRefusal writes it.
func returnToNeedsFail(what string) string {
	return what + " names returnTo, and only the completion of a step that failed, with the verdict fail, names the step its task returns to: send it again without returnTo; nothing was recorded"
}

// completionKeys is the key of each completion of key the intake processed, in order.
func completionKeys(t *testing.T, h *harness, key string) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(context.Background(), "select event_id from processed_events where event_id like $1 order by event_id", "handoff:"+key+":%")
	if err != nil {
		t.Fatalf("read the completion keys of %s: %v", key, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read a completion key of %s: %v", key, err)
		}
		keys = append(keys, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the completion keys of %s: %v", key, err)
	}
	return keys
}

// A step that failed names the step its task returns to with returnTo, by its place in the
// task, 1 for its first. Only a tester's completion with the verdict fail names one: a pass,
// another role's completion and a place below 1 are refused before anything is recorded, each
// with its reason. A fail naming a step carries it on the fact, and its key names the place, so a
// fail naming another step, after the workflow refused the first, is a fact of its own; a fail
// naming none keeps today's key.
func TestAFailedStepNamesTheStepItReturnsTo(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	written := tester.writeHandoff("test", 1)
	report := func(verdict string, returnTo *int) *httptest.ResponseRecorder {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests " + verdict, Verdict: verdict,
			HandoffID: written, HandoffVersion: 1, ReturnTo: returnTo})
	}
	needsFail := returnToNeedsFail("this completion of phase testing of LEGION-208")

	if message := refused(t, report("pass", new(1)), http.StatusBadRequest, "RETURN_TO_NEEDS_FAIL"); message != needsFail {
		t.Fatalf("a pass naming returnTo was refused with %q, want %q", message, needsFail)
	}
	for _, place := range []int{0, -1} {
		want := fmt.Sprintf("returnTo is %d, and it names a step of the task by its place, 1 for its first; nothing was recorded", place)
		if message := refused(t, report("fail", new(place)), http.StatusBadRequest, "INVALID_RETURN_TO"); message != want {
			t.Fatalf("a fail naming returnTo %d was refused with %q, want %q", place, message, want)
		}
	}
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "c0ffee", ReturnTo: new(1)}),
		http.StatusBadRequest, "RETURN_TO_NEEDS_FAIL"); message != needsFail {
		t.Fatalf("the implementer's completion naming returnTo was refused with %q, want %q", message, needsFail)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusals = %#v, want none", got)
	}

	completion(t, report("fail", new(1)))
	completion(t, report("fail", new(2)))
	completion(t, report("fail", nil))
	var named []int
	for _, fact := range facts.recorded() {
		completed, ok := fact.(intake.HandoffComplete)
		if !ok {
			t.Fatalf("the fact recorded is %T, want a completion", fact)
		}
		named = append(named, completed.ReturnTo)
	}
	if want := []int{1, 2, 0}; !reflect.DeepEqual(named, want) {
		t.Fatalf("the completions' ReturnTo = %v, want %v", named, want)
	}
	// The key's second field is the phase entry the tester's row stands at, 0 where no entry was
	// raised, as here: the issue's own, never a tree's generation (R17 of dispatch://INFRA-3320).
	today := "handoff:LEGION-208:0:1:1:tester:testing:0:" + written + ":fail"
	if got, want := completionKeys(t, h, "LEGION-208"), []string{today, today + ":1", today + ":2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the completion keys = %q, want %q", got, want)
	}
}

// A production check's report is no step, so it names no step to return to: a report naming
// returnTo is refused, whatever its verdict, and nothing is recorded. Control: the same report
// without it is taken.
func TestACheckReportNamesNoStepToReturnTo(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
	tester := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
	want := returnToNeedsFail("this report on look 2 of LEGION-208's production check")
	for _, verdict := range []string{"pass", "fail"} {
		if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "looked", Verdict: verdict, ReturnTo: new(1), CheckOpenedAt: &checkOpenedAt}),
			http.StatusBadRequest, "RETURN_TO_NEEDS_FAIL"); message != want {
			t.Fatalf("a %s report naming returnTo was refused with %q, want %q", verdict, message, want)
		}
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusals = %#v, want none", got)
	}
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "looked", Verdict: "pass", CheckOpenedAt: &checkOpenedAt}))
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("facts after the report without returnTo = %#v, want one", got)
	}
}
