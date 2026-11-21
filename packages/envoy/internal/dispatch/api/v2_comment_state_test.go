package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A decided suggestion's thread is resolved, by whoever accepted or rejected it, so under
// decision 8's no-op rule PUT /api/v2/comments/{id}/state {state: resolved} from that person
// changes nothing - 200, no event, its resolver and its time kept - and from anyone else is 409
// ALREADY_ACTIONED, as an accept or a reject of it is: the decision keeps the person who made it,
// and that person's own repeat of {state: accepted} stays a no-op too, answering lost null, since
// the repeat checked nothing.
func TestResolvingADecidedSuggestionKeepsItsDecision(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodPut, "/comments/{id}/state")
	alice, bob := tourPerson("alice"), tourPerson("bob")
	issue := tour.issue(t)
	id := tour.suggestion(t, issue, true)
	put := func(t *testing.T, state string, as tourCaller) *httptest.ResponseRecorder {
		t.Helper()
		return tour.send(t, route, tourRequest{Path: map[string]string{"id": id}, Body: map[string]any{"state": state}, As: as}, "")
	}
	resolution := func(t *testing.T) (by string, at time.Time) {
		t.Helper()
		if err := tour.database.Pool.QueryRow(t.Context(),
			`select resolved_by->>'id', resolved_at from comments where id = $1`, id).Scan(&by, &at); err != nil {
			t.Fatalf("read the suggestion's resolution: %v", err)
		}
		return by, at
	}
	if accepted := put(t, "accepted", alice); accepted.Code != http.StatusOK {
		t.Fatalf("alice accepts her suggestion: %d %s", accepted.Code, accepted.Body.String())
	}
	events := tour.eventCount(t, issue.Key, "")
	acceptedBy, acceptedAt := resolution(t)

	if again := put(t, "resolved", alice); again.Code != http.StatusOK {
		t.Fatalf("the accepter resolving her accepted suggestion answered %d %s, want 200", again.Code, again.Body.String())
	}
	if by, at := resolution(t); by != acceptedBy || !at.Equal(acceptedAt) {
		t.Errorf("the accepter's resolve rewrote the resolution to %s at %v, want %s at %v kept", by, at, acceptedBy, acceptedAt)
	}
	if after := tour.eventCount(t, issue.Key, ""); after != events {
		t.Errorf("the accepter's resolve of her accepted suggestion appended %d events, want none", after-events)
	}

	other := put(t, "resolved", bob)
	if code, _ := refusalOf(t, other); other.Code != http.StatusConflict || code != "ALREADY_ACTIONED" {
		t.Errorf("bob resolving alice's accepted suggestion answered %d %s, want 409 ALREADY_ACTIONED", other.Code, other.Body.String())
	}
	if by, _ := resolution(t); by != acceptedBy {
		t.Errorf("bob's resolve made %s the suggestion's resolver, want %s kept", by, acceptedBy)
	}

	repeat := put(t, "accepted", alice)
	if repeat.Code != http.StatusOK {
		t.Fatalf("the accepter's repeat of her accept answered %d %s, want 200", repeat.Code, repeat.Body.String())
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(repeat.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the repeat's answer: %v", err)
	}
	if lost, carried := answer["lost"]; !carried || string(lost) != "null" {
		t.Errorf("the repeat of an accept answered lost %s (carried %v), want null", lost, carried)
	}
	if after := tour.eventCount(t, issue.Key, ""); after != events {
		t.Errorf("the decided suggestion's repeats appended %d events, want none", after-events)
	}
}
