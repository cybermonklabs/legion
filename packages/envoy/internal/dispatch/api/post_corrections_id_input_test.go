// post_corrections_id_input_test.go is what the five correction routes answer a path id that is
// no id at all. Each one refuses it with the 400 its kind's name carries (requireUUIDPath), as
// every other route that takes a comment id or a message id does: a caller that typed the id
// wrong is told so, and a value that reaches a `::uuid` column as written would answer 500,
// which says the server broke where the request was.
package api

import (
	"net/http"
	"testing"
)

// The two correction routes of a comment and the three of a message each refuse a path id that
// is no uuid and no id written short. The read routes beside them are the control: they are the
// answer this test holds the five to, and they carry the same refusal today.
func TestEveryCorrectionRouteRefusesAPathIDThatIsNoID(t *testing.T) {
	handler := newTestHandler(t)
	for name, route := range map[string]struct {
		method string
		target string
		body   any
		code   string
	}{
		"edit a comment": {
			http.MethodPatch, "/api/v1/comments/not-a-uuid",
			map[string]any{"actor": sessionActor(), "body": "a corrected body"}, "COMMENT_ID_INPUT",
		},
		"delete a comment": {
			http.MethodDelete, "/api/v1/comments/not-a-uuid",
			map[string]any{"actor": sessionActor()}, "COMMENT_ID_INPUT",
		},
		"read a comment": {http.MethodGet, "/api/v1/comments/not-a-uuid", nil, "COMMENT_ID_INPUT"},
		"read a comment's history": {
			http.MethodGet, "/api/v1/comments/not-a-uuid/history", nil, "COMMENT_ID_INPUT",
		},
		"edit a message": {
			http.MethodPatch, "/api/v1/messages/not-a-uuid",
			map[string]any{"actor": sessionActor(), "body": "a corrected body"}, "MESSAGE_ID_INPUT",
		},
		"delete a message": {
			http.MethodDelete, "/api/v1/messages/not-a-uuid",
			map[string]any{"actor": sessionActor()}, "MESSAGE_ID_INPUT",
		},
		"read a message's history": {
			http.MethodGet, "/api/v1/messages/not-a-uuid/history", nil, "MESSAGE_ID_INPUT",
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := sessionRequest(t, handler, route.method, route.target, route.body)
			code, _ := refusalOf(t, response)
			if response.Code != http.StatusBadRequest || code != route.code {
				t.Fatalf("%s with a path id that is no id: status=%d body=%s, want 400 %s",
					name, response.Code, response.Body.String(), route.code)
			}
		})
	}
}
