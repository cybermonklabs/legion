package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAReadBackPrintsNoAnswerThatEchoesItsRequest: a first version of a reference target that
// landed with no answer to give is read back by its name from the issue's documents
// (refReadBack). Whatever answers that read in Dispatch's place echoes the bearer the read
// carried, in a refusal (refused) or in a listing that names no document of the target's name
// (unnamed). What the read back's error prints of the answer holds no run of the bearer, where it
// printed the answer's bytes as they came, while the answer as it came holds it whole (twin).
// The control: each error still names the read back and the answer's own words beside the
// bearer. Captures loadtest.answer-members/read-back-<case>.
func TestAReadBackPrintsNoAnswerThatEchoesItsRequest(t *testing.T) {
	issue := seedProjects[0].key + "-1"
	documents := "/api/v1/issues/" + issue + "/artifacts"
	for _, tc := range []struct {
		name, words string
		status      int
		answer      func(bearer string) any
	}{
		{"refused", "the gateway refused", http.StatusForbidden, func(bearer string) any {
			return map[string]string{"error": "the gateway refused " + bearer, "code": "FORBIDDEN"}
		}},
		{"unnamed", "a document of another name", http.StatusOK, func(bearer string) any {
			return []map[string]string{{"id": memberUUID, "name": "a document of another name " + bearer}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != documents {
					writeJSONAnswer(w, http.StatusNotFound, map[string]string{"error": "no such route", "code": "NOT_FOUND"})
					return
				}
				writeJSONAnswer(w, tc.status, tc.answer(r.Header.Get("Authorization")))
			}))
			t.Cleanup(stand.Close)
			a := newAPI(stand.URL, redacttest.Token, 1)
			t.Run("twin", func(t *testing.T) {
				res := a.call(context.Background(), http.MethodGet, documents, who{}, nil)
				if !strings.Contains(string(res.body), redacttest.Token) {
					t.Errorf("the answer as it came is %q; want the bearer in it, so the row can see it printed", res.body)
				}
			})
			id, err := refReadBack(context.Background(), a, issue, "ref-target-1.md")
			said := errorText(err)
			redacttest.RequireNoPiece(t, "loadtest.answer-members/read-back-"+tc.name, said, redacttest.Token)
			if id != "" || !strings.Contains(said, "read back ref-target-1.md on "+issue) || !strings.Contains(said, tc.words) {
				t.Errorf("refReadBack = %q, %q; want a refusal naming the read back and the answer's own words %q", id, said, tc.words)
			}
		})
	}
}
