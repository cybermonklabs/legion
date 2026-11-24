package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim whose refused launch's fact the store has not taken (supervise.Claim.UnrecordedRefusal)
// is served on the operator routes as `unrecordedRefusal`: when the first failed try failed, how
// many tries have failed, and the last try's error. A claim with no such fact carries no key. The
// store never writes it, so only these routes, which read the live machine, carry it.
func TestTheOperatorClaimServesARefusalTheStoreHasNotRecorded(t *testing.T) {
	t.Parallel()
	refused := supervise.Claim{Token: "legion-legion-legion-208-tester", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleTester,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon,
		UnrecordedRefusal: &supervise.UnrecordedRefusal{Since: time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC), Attempts: 3,
			Error: "apply fact supervise/pod-refused:legion-legion-legion-208-tester:1: the database did not answer"}}
	recorded := refused
	recorded.UnrecordedRefusal = nil
	for _, tc := range []struct {
		name  string
		claim supervise.Claim
		want  any
	}{
		{name: "a refusal the store has not recorded", claim: refused, want: map[string]any{
			"since": "2026-11-23T17:00:00Z", "attempts": float64(3), "error": refused.UnrecordedRefusal.Error}},
		{name: "no such refusal", claim: recorded, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(operatorView(tc.claim, supervise.StepRecord{}))
			if err != nil {
				t.Fatalf("marshal the operator claim: %v", err)
			}
			var served map[string]any
			if err := json.Unmarshal(encoded, &served); err != nil {
				t.Fatal(err)
			}
			if got := served["unrecordedRefusal"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the operator claim serves unrecordedRefusal %v, want %v: %s", got, tc.want, encoded)
			}
		})
	}
}
