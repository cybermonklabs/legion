package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A text every upload refuses alike, a replacement included - each row of textRefusalsFile whose
// scope is upload - is refused by a replacement upload over a held document, naming that
// document's token, inline and as a file as the row's sent allows, with the status, code and words
// the round trip asked without new_document answers, the most permissive replacement's answer. So
// a row the list scopes upload that a replacement answers otherwise fails here. `dispatch
// artifact --force` asks that round trip first, in its dry run and in its call, and the call
// throws the round trip's refusal before it uploads, so the upload's own words reach only a
// client that skips that check: an older `dispatch`, a call whose check got no answer, or a caller
// of the API itself. Those words are the round trip's. The tester found at 5a574bff, on real
// servers, that the upload of such a replacement refused block HTML and markdown no reader reads
// back as "INVALID_MARKDOWN: replace live document: markdown is not a Proof document: ...", where
// the file round trip answered "INVALID_MARKDOWN: markdown is not a Proof document: ...", and the
// call then printed the upload's refusal, so the dry run and the call printed two refusals
// (INFRA-4224 tester round 16).
func TestARoundTripWithoutNewDocumentRefusesInTheReplacementUploadsOwnWords(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	uploads := "/api/v1/issues/" + issue.Key + "/artifacts"
	for index, test := range roundTripRefusals(t) {
		if test.scope != uploadScope {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			for _, kind := range test.sent {
				name := fmt.Sprintf("held-%d-%s.md", index, kind)
				held := dispatchRequest(t, handler, http.MethodPost, uploads, map[string]string{"name": name, "content": "Held.\n"}, "alice")
				if held.Code != http.StatusCreated {
					t.Fatalf("upload %s: status=%d body=%.300s, want 201", name, held.Code, held.Body.String())
				}
				id := artifactIDNamed(t, handler, uploads, name)
				var replaced *httptest.ResponseRecorder
				if kind == "file" {
					replaced = multipartRequest(t, handler, uploads, map[string]string{"name": name, "precondition": uploadBaseField(t, handler, id)},
						name, "text/markdown", []byte(test.text), "alice")
				} else {
					replaced = dispatchRequest(t, handler, http.MethodPost, uploads, map[string]any{
						"name": name, "content": test.text, "precondition": uploadBase(t, handler, id),
					}, "alice")
				}
				roundTrip := roundTripOf(t, handler, kind, test.text, false)
				code, message := refusalOf(t, replaced)
				tripCode, tripMessage := refusalOf(t, roundTrip)
				if replaced.Code == http.StatusCreated || replaced.Code == http.StatusOK {
					t.Fatalf("the %s replacement of %s was stored (%d), want it refused", kind, name, replaced.Code)
				}
				if roundTrip.Code != replaced.Code || tripCode != code || tripMessage != message {
					t.Errorf("the %s round trip without new_document answers %d %s %.200q, want the replacement upload's %d %s %.200q",
						kind, roundTrip.Code, tripCode, tripMessage, replaced.Code, code, message)
				}
			}
		})
	}
}
