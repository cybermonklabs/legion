package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The operator claim view of a claim that holds a look's task writes the task's text with
// `<look id withheld>` where the text gives the look id, keeps the rest of the text as it is, and
// has no field that carries the id, before the task is sent and after (R3 of
// dispatch://INFRA-3518, D8 of its plan). The listing and the answer of a route about the one
// claim (its suspension) are both read: every operator route renders a claim through
// operatorView. A task that gives the id more than once has each of them withheld.
func TestTheOperatorClaimViewWithholdsTheLookID(t *testing.T) {
	for _, sent := range []bool{false, true} {
		name := "before the send"
		if sent {
			name = "after the send"
		}
		t.Run(name, func(t *testing.T) {
			f := newLookIDFixture(t, sent)
			want := strings.ReplaceAll(f.task, hiddenLookID, lookIDWithheld)
			if !strings.Contains(want, "Look id: `"+lookIDWithheld+"`.") || strings.Count(f.task, hiddenLookID) != 1 {
				t.Fatalf("the fixture's task %q does not give the look id once on its own line", f.task)
			}
			withheld := func(label string, rec *httptest.ResponseRecorder, view OperatorClaim) {
				t.Helper()
				if strings.Contains(rec.Body.String(), hiddenLookID) {
					t.Fatalf("%s carries the look id %q: %s", label, hiddenLookID, rec.Body)
				}
				if view.Pending == nil || view.Pending.ID != "outbox:look-1" {
					t.Fatalf("%s shows the pending task %+v, want the look's task outbox:look-1", label, view.Pending)
				}
				if view.Pending.Task != want {
					t.Fatalf("%s shows the task\n%q\nwant the look's task with its look id withheld and the rest of it whole\n%q", label, view.Pending.Task, want)
				}
			}

			listed := f.h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
			if listed.Code != http.StatusOK {
				t.Fatalf("the listing = %d, want 200: %s", listed.Code, listed.Body)
			}
			var claims OperatorClaims
			if err := json.Unmarshal(listed.Body.Bytes(), &claims); err != nil {
				t.Fatalf("decode the listing: %v", err)
			}
			var view *OperatorClaim
			for i := range claims.Claims {
				if claims.Claims[i].Token == f.token {
					view = &claims.Claims[i]
				}
			}
			if view == nil {
				t.Fatalf("the listing has no claim %s: %s", f.token, listed.Body)
			}
			withheld("the listing", listed, *view)

			suspended := f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/suspend", nil)
			if suspended.Code != http.StatusOK && suspended.Code != http.StatusAccepted {
				t.Fatalf("the suspension = %d, want 200 or 202: %s", suspended.Code, suspended.Body)
			}
			var answered OperatorClaim
			decodeInto(t, suspended, &answered)
			withheld("the suspension's answer", suspended, answered)
		})
	}

	t.Run("a task that gives the look id twice", func(t *testing.T) {
		task := "Look id: `" + hiddenLookID + "`. Report with look_id `" + hiddenLookID + "`."
		view := operatorView(supervise.Claim{Token: "legion-legion-legion-209-tester", Pending: &supervise.Delivery{ID: "outbox:look-1", Task: task, LookID: hiddenLookID}},
			supervise.StepRecord{})
		encoded, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("encode the view: %v", err)
		}
		if strings.Contains(string(encoded), hiddenLookID) {
			t.Fatalf("the view carries the look id: %s", encoded)
		}
		if want := "Look id: `" + lookIDWithheld + "`. Report with look_id `" + lookIDWithheld + "`."; view.Pending.Task != want {
			t.Fatalf("the view shows the task %q, want %q", view.Pending.Task, want)
		}
	})

	// A look's task whose delivery holds no look id of its own — one the release before wrote,
	// whose upsert names no look id and keeps the id of the delivery it replaced — still gives the
	// look's id on its look-id line (intake.LookIDLine), and that id is withheld by the line's form.
	for _, tc := range []struct{ name, lookID string }{
		{name: "a look's task whose delivery holds no look id"},
		{name: "a look's task whose delivery holds another look's id", lookID: "ANOTHERLOOKIDOFLOOKONEABCD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := func(id string) string {
				return "Production check of LEGION-209: child.\n" + intake.LookIDLine + "`" + id +
					"`. Your report gives check_opened_at and look_id from this task.\nReport the look."
			}
			view := operatorView(supervise.Claim{Token: "legion-legion-legion-209-tester", Pending: &supervise.Delivery{ID: "outbox:look-2",
				Task: line(hiddenLookID), CheckLook: supervise.CheckLook{Look: 2}, LookID: tc.lookID}}, supervise.StepRecord{})
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatalf("encode the view: %v", err)
			}
			if strings.Contains(string(encoded), hiddenLookID) {
				t.Fatalf("the view carries the look id its task's line gives: %s", encoded)
			}
			if want := line(lookIDWithheld); view.Pending.Task != want {
				t.Fatalf("the view shows the task %q, want %q", view.Pending.Task, want)
			}
		})
	}
}
