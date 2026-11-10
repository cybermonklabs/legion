package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A mention is a reference (INFRA-1702 R4): the parties a comment names are the ones its body
// writes as party references, beside any the request's own `mentions` array still carries. These
// rows cover that set - what it holds, what it refuses, and what a person in it does.

// partyListener is a listener that knows one session, s1, and records every send it is given.
func partyListener(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mutex sync.Mutex
	sent := []map[string]any{}
	const session = `{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_, _ = w.Write([]byte(session))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte("[" + session + "]"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s1","title":"planner","capabilities":["aside","btw","steer"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode send: %v", err)
			}
			mutex.Lock()
			sent = append(sent, input)
			mutex.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"party-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return server, func() []map[string]any {
		mutex.Lock()
		defer mutex.Unlock()
		return append([]map[string]any(nil), sent...)
	}
}

// The body names the party, and the comment is delivered to it in the mode the request asks for:
// nothing but the text said who it was for. The send is the mention sender's, behind the create's
// own answer, so the attempt is read once it has settled (settledCommentDeliveries) rather than
// off the 201, which carries it pending.
func TestCommentBodyMentionDelivers(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Body mention", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "[planner](dispatch://agent/s1) please look.", "delivery": "btw",
	}))
	if len(created.Mentions) != 1 || created.Mentions[0].Target != "session:s1" ||
		created.Mentions[0].Delivery != "btw" {
		t.Fatalf("body mention = %#v, want one btw mention of session:s1", created.Mentions)
	}
	attempts := settledCommentDeliveries(t, database, created.ID)
	if len(attempts) != 1 || attempts[0].Target != "session:s1" || attempts[0].State != "sent" ||
		attempts[0].Delivery != "btw" {
		t.Fatalf("body delivery = %#v, want one sent btw attempt of session:s1", attempts)
	}
	if len(sends()) != 1 {
		t.Fatalf("sends = %#v, want one", sends())
	}
}

// A body that names one party twice names one party: the repeat is dropped before any rule reads
// the set, so the comment is delivered once and nothing is refused.
func TestCommentBodyMentionRepeatDeliversOnce(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Repeated party", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "[planner](dispatch://agent/s1) and [planner](dispatch://agent/s1) again.",
	}))
	if len(created.Mentions) != 1 || created.Mentions[0].Target != "session:s1" {
		t.Fatalf("repeated party = %#v, want one mention", created.Mentions)
	}
	if attempts := settledCommentDeliveries(t, database, created.ID); len(attempts) != 1 {
		t.Fatalf("repeated party deliveries = %#v, want one", attempts)
	}
	if len(sends()) != 1 {
		t.Fatalf("sends = %#v, want one", sends())
	}
}

// The request's array and the body name the same party: it is one party, and no "must not repeat
// a target" refusal, which is about an array that repeats one of its own.
func TestCommentArrayAndBodyNameOneParty(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Array and body", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "[planner](dispatch://agent/s1) please look.",
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	attempts := settledCommentDeliveries(t, database, created.ID)
	if len(created.Mentions) != 1 || len(attempts) != 1 || len(sends()) != 1 {
		t.Fatalf("array and body = %#v / %#v / %#v, want one of each",
			created.Mentions, attempts, sends())
	}
}

// A caller that still sends an array alone is served exactly as before: the field is not going
// away, and nothing but the dashboard composer ever sent one.
func TestCommentMentionsArrayStillServed(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Array caller", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "Nothing in these words names anybody.",
		"mentions": []map[string]any{{"target": "session:s1"}},
		"delivery": "aside",
	}))
	attempts := settledCommentDeliveries(t, database, created.ID)
	if len(created.Mentions) != 1 || created.Mentions[0].Delivery != "aside" ||
		len(attempts) != 1 || attempts[0].State != "sent" {
		t.Fatalf("array caller = %#v / %#v", created.Mentions, attempts)
	}
	if len(sends()) != 1 {
		t.Fatalf("sends = %#v, want one", sends())
	}
}

// A person is a reader of the record, never a delivering party (R5): naming one is naming
// somebody, so a delivery mode is not refused, but nothing is resolved, counted or sent for it.
func TestCommentBodyPersonIsNeverDelivered(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Person mention", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "[Alice](dispatch://person/alice@example.com) should see this.", "delivery": "btw",
	}))
	if len(created.Mentions) != 0 || len(created.Deliveries) != 0 {
		t.Fatalf("person mention = %#v / %#v, want neither", created.Mentions, created.Deliveries)
	}
	if len(sends()) != 0 {
		t.Fatalf("sends = %#v, want none", sends())
	}
}

// A person does not count toward the cap either: ten delivering parties and a person is ten.
func TestCommentBodyPersonDoesNotCountTowardTheCap(t *testing.T) {
	listener, _ := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Person and the cap", "before")

	words := []string{"[Alice](dispatch://person/alice@example.com)"}
	for index := range 10 {
		words = append(words, fmt.Sprintf("[r%d](dispatch://role/r%d)", index, index))
	}
	response := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": strings.Join(words, " "),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("ten parties and a person: status=%d body=%s", response.Code, response.Body.String())
	}
}

// Each refusal keyed on the party set, with the code and the reason it has always answered.
func TestCommentPartySetRefusals(t *testing.T) {
	listener, _ := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Party refusals", "before")

	eleven := make([]string, 0, 11)
	for index := range 11 {
		eleven = append(eleven, fmt.Sprintf("[r%d](dispatch://role/r%d)", index, index))
	}
	for _, tc := range []struct {
		name   string
		input  map[string]any
		reason string
	}{
		{
			"a delivery mode with no party named anywhere",
			map[string]any{"body": "Nobody at all.", "delivery": "btw"},
			"delivery requires mentions",
		},
		{
			"more delivering parties in the body than the cap",
			map[string]any{"body": strings.Join(eleven, " ")},
			"mentions allow at most 10 targets",
		},
		{
			"a delivery mode outside the three, on a party the body names",
			map[string]any{"body": "[planner](dispatch://agent/s1)", "delivery": "interrupt"},
			"delivery must be one of btw, aside, steer",
		},
		{
			"an array target of no known form, beside a party the body names",
			map[string]any{
				"body":     "[planner](dispatch://agent/s1)",
				"mentions": []map[string]any{{"target": "agent:s1"}},
			},
			// The rest of the reason names the two forms in angle brackets, which JSON escapes.
			"mention target must be role:",
		},
		{
			"an array that repeats one of its own targets",
			map[string]any{
				"body":     "[planner](dispatch://agent/s1)",
				"mentions": []map[string]any{{"target": "role:reviewer"}, {"target": "role:reviewer"}},
			},
			"mentions must not repeat a target",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := postMentionedComment(t, handler, issue.Key, tc.input)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), `"code":"MENTION_INPUT"`) ||
				!strings.Contains(response.Body.String(), tc.reason) {
				t.Fatalf("%s: status=%d body=%s", tc.name, response.Code, response.Body.String())
			}
		})
	}
}

// An edit changes who the record names, because the record's parties are in its text, and sends
// nothing: comment_mentions is the delivery ledger, and an edit writes no row in it.
func TestCommentEditChangesWhoItNamesAndSendsNothing(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Edited mention", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Nobody yet.",
	}))
	if len(sends()) != 0 {
		t.Fatalf("sends before the edit = %#v, want none", sends())
	}
	edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+created.ID,
		map[string]any{"body": "[planner](dispatch://agent/s1) please look."}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	read := decodeBody[mentionedCommentRead](t, edited)
	if read.Body != "[planner](dispatch://agent/s1) please look." {
		t.Fatalf("edited body = %q", read.Body)
	}
	if len(read.Mentions) != 0 || len(read.Deliveries) != 0 {
		t.Fatalf("edited ledger = %#v / %#v, want neither", read.Mentions, read.Deliveries)
	}
	if len(sends()) != 0 {
		t.Fatalf("sends after the edit = %#v, want none", sends())
	}
}

// A party reference a code context holds names nobody: the body is showing the address rather
// than naming its party, which is how every renderer of the record draws it. The three contexts
// are a fenced code block, an indented code block and a code span; nothing is listed, nothing is
// resolved and nothing is sent for any of them.
func TestCommentBodyMentionInCodeIsNeverDelivered(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Party in code", "before")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"a fenced code block", "Run this:\n\n```\ndispatch://agent/s1\n```"},
		{"an indented code block", "Run this:\n\n    dispatch://agent/s1\n"},
		{"a code span", "The address `dispatch://agent/s1` names the planner."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key,
				map[string]any{"body": tc.body}))
			if len(created.Mentions) != 0 || len(created.Deliveries) != 0 {
				t.Fatalf("%s = %#v / %#v, want neither", tc.name, created.Mentions, created.Deliveries)
			}
			if read := decodeBody[mentionedCommentRead](t, dispatchRequest(t, handler,
				http.MethodGet, "/api/v1/comments/"+created.ID, nil, "alice")); len(read.Deliveries) != 0 {
				t.Fatalf("%s deliveries = %#v, want none", tc.name, read.Deliveries)
			}
		})
	}
	if len(sends()) != 0 {
		t.Fatalf("sends = %#v, want none", sends())
	}
}

// A body whose only party is in code names nobody at all, so a delivery mode beside it is the
// refusal a body naming nobody has always earned - for a person written there as well, a person
// in prose being somebody named.
func TestCommentBodyMentionInCodeNamesNobodyForDelivery(t *testing.T) {
	listener, _ := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Code and delivery", "before")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"an agent in a fenced block", "```\ndispatch://agent/s1\n```"},
		{"a role in a code span", "Write `dispatch://role/reviewer` here."},
		{"a person in a code span", "Write `dispatch://person/alice@example.com` here."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := postMentionedComment(t, handler, issue.Key,
				map[string]any{"body": tc.body, "delivery": "btw"})
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), `"code":"MENTION_INPUT"`) ||
				!strings.Contains(response.Body.String(), "delivery requires mentions") {
				t.Fatalf("%s: status=%d body=%s", tc.name, response.Code, response.Body.String())
			}
		})
	}
}

// A party in code does not count toward the cap either: eleven party references with one of them
// in a code span is ten delivering parties.
func TestCommentBodyMentionInCodeDoesNotCountTowardTheCap(t *testing.T) {
	listener, _ := partyListener(t)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Code and the cap", "before")

	words := []string{"`dispatch://role/spelled-out`"}
	for index := range 10 {
		words = append(words, fmt.Sprintf("[r%d](dispatch://role/r%d)", index, index))
	}
	response := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": strings.Join(words, " "),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("ten parties and one in code: status=%d body=%s", response.Code, response.Body.String())
	}
}

// The same party written in code and outside it is named by the occurrence outside: the code one
// decides nothing but its own delivery, and the worded link is the ordinary mention it always was.
func TestCommentBodyMentionOutsideCodeStillDelivers(t *testing.T) {
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Code and prose", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "The address is `dispatch://agent/s1`, so [planner](dispatch://agent/s1) please look.",
	}))
	if len(created.Mentions) != 1 || created.Mentions[0].Target != "session:s1" {
		t.Fatalf("code and prose = %#v, want one mention of session:s1", created.Mentions)
	}
	attempts := settledCommentDeliveries(t, database, created.ID)
	if len(attempts) != 1 || attempts[0].Target != "session:s1" || attempts[0].State != "sent" {
		t.Fatalf("code and prose deliveries = %#v, want one sent attempt of session:s1", attempts)
	}
	if len(sends()) != 1 {
		t.Fatalf("sends = %#v, want one", sends())
	}
}
