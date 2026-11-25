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

// A tester's completion names the step its task returns to with returnTo, by its place in the
// task, 1 for its first, beside the head it tested and with no verdict: the check states at that
// head decide whether the step failed, and the step named is the return of a fail they decide.
// Another role's completion and a place below 1 are refused before anything is recorded, each with
// its code. A completion naming a step carries it on the fact, and its key names the place, so
// one naming another step, after the workflow refused the first, is a fact of its own; one naming
// none keeps today's key, whose last segment, where a verdict stood, is empty.
func TestAFailedStepNamesTheStepItReturnsTo(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	written := tester.writeHandoff("test", 1)
	report := func(returnTo *int) *httptest.ResponseRecorder {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests ran", Commit: headSHA,
			HandoffID: written, HandoffVersion: 1, ReturnTo: returnTo})
	}

	for _, place := range []int{0, -1} {
		want := fmt.Sprintf("returnTo is %d, and it names a step of the task by its place, 1 for its first; nothing was recorded", place)
		if message := refused(t, report(new(place)), http.StatusBadRequest, "INVALID_RETURN_TO"); message != want {
			t.Fatalf("a completion naming returnTo %d was refused with %q, want %q", place, message, want)
		}
	}
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "c0ffee", ReturnTo: new(1)}),
		http.StatusBadRequest, "RETURN_TO_NEEDS_FAIL")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusals = %#v, want none", got)
	}

	completion(t, report(new(1)))
	completion(t, report(new(2)))
	completion(t, report(nil))
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
	today := "handoff:LEGION-208:1:1:1:tester:testing:0:" + written + ":"
	if got, want := completionKeys(t, h, "LEGION-208"), []string{today, today + ":1", today + ":2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the completion keys = %q, want %q", got, want)
	}
}

// A tester's completion names returnTo with no verdict, and a verdict is refused on it as on every
// phase completion: one naming fail or pass beside returnTo is refused VERDICT_NOT_TAKEN before
// anything is recorded, not taken as a fail that names its step, and the same completion sent
// again with no verdict is taken, the step it names on the fact.
func TestATestersVerdictIsRefusedAndItsReturnToTakenWithNone(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	written := tester.writeHandoff("test", 1)
	report := func(verdict string) *httptest.ResponseRecorder {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests ran", Verdict: verdict, Commit: headSHA,
			HandoffID: written, HandoffVersion: 1, ReturnTo: new(1)})
	}
	for _, verdict := range []string{"fail", "pass"} {
		refused(t, report(verdict), http.StatusBadRequest, "VERDICT_NOT_TAKEN")
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusals = %#v, want none", got)
	}

	completion(t, report(""))
	recorded := facts.recorded()
	if len(recorded) != 1 {
		t.Fatalf("facts after the completion with no verdict = %#v, want one", recorded)
	}
	if completed, ok := recorded[0].(intake.HandoffComplete); !ok || completed.Role != claim.RoleTester || completed.ReturnTo != 1 {
		t.Fatalf("the fact recorded = %#v, want the tester's completion naming returnTo 1", recorded[0])
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
	for _, verdict := range []string{"pass", "fail"} {
		refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "looked", Verdict: verdict, ReturnTo: new(1)}),
			http.StatusBadRequest, "RETURN_TO_NEEDS_FAIL")
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusals = %#v, want none", got)
	}
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "looked", Verdict: "pass"}))
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("facts after the report without returnTo = %#v, want one", got)
	}
}
