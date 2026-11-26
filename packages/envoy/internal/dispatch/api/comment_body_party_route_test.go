package api

import (
	"net/http"
	"strings"
	"testing"
)

// R4 keeps two refusals for the parties a body names: the cap of ten and a target form the
// server does not take, "each answered with the code and the reason the array is answered with
// today". An agent address of the wider body grammar (text.IsSessionID takes `.`) names a
// session no route reaches (model.ParseRoute reads `session:[A-Za-z0-9_-]+`), so a body that
// names one is refused as the array's `session:a.b` is: 400 MENTION_INPUT, the array's reason,
// and no comment stored, with or without a delivery mode. The same address in a code span names
// nobody, so that body is stored as written and delivers to nobody. c-stored-text-named-v2 runs
// this row by name.
func TestCommentBodyTargetOfNoRouteFormIsRefused(t *testing.T) {
	t.Parallel()
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Body target refusal", "before")

	array := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "Please look.",
		"mentions": []map[string]any{{"target": "session:a.b"}},
	})
	if array.Code != http.StatusBadRequest ||
		!strings.Contains(array.Body.String(), `"code":"MENTION_INPUT"`) ||
		!strings.Contains(array.Body.String(), "mention target must be role:") {
		t.Fatalf("the array target session:a.b: status=%d body=%s", array.Code, array.Body.String())
	}

	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"an agent address no route reaches", map[string]any{"body": "[x](dispatch://agent/a.b) please look."}},
		{"the same address beside a delivery mode", map[string]any{
			"body": "[x](dispatch://agent/a.b) please look.", "delivery": "btw",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := postMentionedComment(t, handler, issue.Key, tc.input)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), `"code":"MENTION_INPUT"`) ||
				!strings.Contains(response.Body.String(), "mention target must be role:") {
				t.Fatalf("%s: status=%d body=%s, want the array's 400 MENTION_INPUT and its reason",
					tc.name, response.Code, response.Body.String())
			}
		})
	}
	if rows := countCommentRows(t, handler, issue.Key); rows != 0 {
		t.Fatalf("the refused writes left %d comments, want none", rows)
	}

	coded := map[string]any{"body": "The address is `dispatch://agent/a.b`, which names nobody."}
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, coded))
	if created.Body != coded["body"] || len(created.Mentions) != 0 || len(created.Deliveries) != 0 {
		t.Fatalf("the address in a code span = %q / %#v / %#v, want it stored as written, naming nobody",
			created.Body, created.Mentions, created.Deliveries)
	}
	if len(sends()) != 0 {
		t.Fatalf("sends = %#v, want none", sends())
	}
}
