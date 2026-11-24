package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestADispatchRefusalIsReadFromItsCodeAsItCame: Dispatch refuses a read with a coded refusal
// whose error names a URL right before the JSON quote that ends it, which the rule cannot read as
// clean, so it prints the whole body as not readable. The refusal is still Dispatch's code: the
// client decodes the code as it came (oauth2call.CodedRefusal, Error.Code), so the refusal is the
// permanent one Dispatch made and not a codeless answer from whatever sits in front of it, and its
// message prints as the rule reads it, which names no URL.
func TestADispatchRefusalIsReadFromItsCodeAsItCame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"code":"INVALID_TRANSITION","error":"read the workflow at https://dispatch.example/docs#transitions"}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, dispatchToken).GetIssue(context.Background(), "an-issue")
	var refusal *Error
	if !errors.As(err, &refusal) || !refusal.Code.Equal("INVALID_TRANSITION") {
		t.Fatalf("a refusal coded INVALID_TRANSITION beside a URL = %#v, want Dispatch's code", err)
	}
	if _, permanent := PermanentRefusal(err); !permanent {
		t.Errorf("PermanentRefusal(%v) = false, want the coded 422 Dispatch made to be permanent", err)
	}
	if strings.Contains(err.Error(), "#transitions") {
		t.Errorf("the refusal prints %q, want Dispatch's words as the rule reads them", err)
	}
}

// TestADispatchRefusalsCodeIsReadAsItCameBesideARequestThatQuotesIt: Dispatch refuses a message
// post with one of its codes, and the message the daemon posts quotes that code, as a note about an
// earlier refusal does. The message is a value the request carried, so the rule cuts the run of it
// the code holds from the code's reading. What the refusal is - permanent for ISSUE_NOT_FOUND
// (PermanentRefusal), the holder's 409s (Claimed, Contended), the router's 404 (NoRoute) - is still
// decided on Dispatch's code as it came, and the error prints the code as the rule reads it, which
// holds no run of the message. In the control the message names no code.
func TestADispatchRefusalsCodeIsReadAsItCameBesideARequestThatQuotesIt(t *testing.T) {
	for _, c := range []struct {
		code, sentence string
		status         int
		decided        func(error) bool
	}{
		{IssueNotFound, "issue LEGION-9 not found", http.StatusNotFound, func(err error) bool { _, permanent := PermanentRefusal(err); return permanent }},
		{IssueClaimed, "the claim is held by another session", http.StatusConflict, Claimed},
		{ClaimContended, "the claim changed twice while the request ran", http.StatusConflict, Contended},
		{noRouteCode, "no route for POST /api/v1/issues/LEGION-9/messages", http.StatusNotFound, NoRoute},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"code":"` + c.code + `","error":"` + c.sentence + `"}`))
		}))
		t.Cleanup(server.Close)
		for _, row := range []struct{ name, message string }{
			{"control", "the step finished; nothing was refused"},
			{"quoted", "the last write was refused: " + c.code + ", so it is posted again"},
		} {
			t.Run(c.code+"/"+row.name, func(t *testing.T) {
				err := New(server.URL, dispatchToken).PostMessage(context.Background(), "LEGION-9", row.message)
				if !c.decided(err) {
					t.Errorf("Dispatch's %d %s to a message %q = %v, want it decided on Dispatch's code as it came", c.status, c.code, row.message, err)
				}
				if row.name == "quoted" && err != nil && strings.Contains(err.Error(), c.code) {
					t.Errorf("the refusal prints %q, want its code as the rule reads it, which holds no run of the message", err)
				}
			})
		}
	}
}

// TestADispatchRefusalsSentenceIsReadAsItCame: Dispatch's refusals whose sentence a decision reads
// - the 400 INVALID_JSON naming a field a Dispatch from before it refuses (PermanentRefusal), and a
// router's 404 NOT_FOUND "no route for" (NoRoute) - are decided on the sentence as it came (Error's
// Said), never on the rule's reading of it (Error's Message): a sentence whose URL holds a quote,
// which the rule prints as not readable (url), and one that repeats a run of the token the request
// carried, which the rule cuts from it (token). Each decision still holds, and the error prints
// as the rule reads the sentence, which names neither the URL's query nor the token's run.
func TestADispatchRefusalsSentenceIsReadAsItCame(t *testing.T) {
	for _, c := range []struct {
		name, token, body, hidden string
		status                    int
		decided                   func(error) bool
	}{
		{"newer-field-url", dispatchToken, `{"code":"INVALID_JSON","error":"invalid JSON body: json: unknown field \"labels_add\"; see https://dispatch.example/fields?f=\"labels_add\""}`, `f="labels_add"`,
			http.StatusBadRequest, func(err error) bool { _, permanent := PermanentRefusal(err); return !permanent }},
		{"newer-field-token", "dsp_labels_add_9f3c1e7a", `{"code":"INVALID_JSON","error":"invalid JSON body: json: unknown field \"labels_add\""}`, "labels_add",
			http.StatusBadRequest, func(err error) bool { _, permanent := PermanentRefusal(err); return !permanent }},
		{"no-route-url", dispatchToken, `{"code":"NOT_FOUND","error":"no route for GET /api/v1/issues/an-issue; see https://dispatch.example/routes?r=\"issues\""}`, `r="issues"`,
			http.StatusNotFound, NoRoute},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, c.token).GetIssue(context.Background(), "an-issue")
			if !c.decided(err) {
				t.Errorf("Dispatch's %d %s = %v, want it decided on its sentence as it came", c.status, c.body, err)
			}
			if err != nil && strings.Contains(err.Error(), c.hidden) {
				t.Errorf("the refusal prints %q, want its sentence as the rule reads it", err)
			}
		})
	}
}
