package envoy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAListenersSessionAnswerKeepsNoWordItEchoes: the listener, or whatever answers in its place,
// answers a role lookup (role), a session read (session) and the session list (sessions) 200, with
// the session id and the title echoing the bearer Dispatch's read carried, or quoting a
// credential's URL. An id the rule does not print as it came is kept nowhere: the read is
// ErrUnavailable, which names the read and the member and none of the id (id-echo, id-url). A
// title is kept as the rule reads it, so no print of the session holds the bearer or the URL's
// secrets, and it still names the title's own words (title-echo). An id and a title the rule
// prints as they came are kept as they came (plain). Captures envoy.answer-words/<read>/<case>.
func TestAListenersSessionAnswerKeepsNoWordItEchoes(t *testing.T) {
	// The bearer is GitHub-shaped, ghs_ and 36 characters.
	bearer := githubtest.InstallationToken(t.Name())
	reads := []struct {
		name, route, member string
		answer              func(id, title string) any
		read                func(*Client) (Session, error)
	}{
		{
			name: "role", route: "GET /v1/roles/legion-planner", member: "holder",
			answer: func(id, title string) any {
				return map[string]any{"holder": id, "title": title, "holder_state": "live", "capabilities": []string{}, "last_seen": 1}
			},
			read: func(c *Client) (Session, error) { return c.Role(context.Background(), "legion-planner") },
		},
		{
			name: "session", route: "GET /v1/registry/ses_asked", member: "session_id",
			answer: func(id, title string) any {
				return map[string]any{"session_id": id, "title": title, "state": "live", "capabilities": []string{}}
			},
			read: func(c *Client) (Session, error) { return c.Session(context.Background(), "ses_asked") },
		},
		{
			name: "sessions", route: "GET /v1/sessions", member: "session_id",
			answer: func(id, title string) any {
				return []map[string]any{{"session_id": id, "title": title, "state": "live", "capabilities": []string{}}}
			},
			read: func(c *Client) (Session, error) {
				sessions, err := c.Sessions(context.Background())
				if err != nil || len(sessions) != 1 {
					return Session{}, errors.Join(err, fmt.Errorf("%d sessions listed, want 1", len(sessions)))
				}
				return sessions[0], nil
			},
		},
	}
	for _, c := range []struct {
		name string
		// id and title are what the stand-in answers, given the bearer the read carried.
		id, title func(echoed string) string
		// kept is false where the read must refuse the id; otherwise the session it reads
		// carries wantID and a title holding wantTitle.
		kept              bool
		wantID, wantTitle string
	}{
		{name: "id-echo", id: func(echoed string) string { return echoed }, title: func(string) string { return "planner" }},
		{name: "id-url", id: func(string) string { return redacttest.Code }, title: func(string) string { return "planner" }},
		{
			name: "title-echo", id: func(string) string { return "ses_holder" },
			title: func(echoed string) string { return "planner " + echoed + "; " + redacttest.Words },
			kept:  true, wantID: "ses_holder", wantTitle: "planner [redacted]; " + redacttest.Shown,
		},
		{
			name: "plain", id: func(string) string { return "ses:holder" }, title: func(string) string { return "planner" },
			kept: true, wantID: "ses:holder", wantTitle: "planner",
		},
	} {
		for _, read := range reads {
			t.Run(read.name+"/"+c.name, func(t *testing.T) {
				listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					echoed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(read.answer(c.id(echoed), c.title(echoed)))
				}))
				t.Cleanup(listener.Close)
				session, err := read.read(New(listener.URL, WithToken(bearer)))
				printed := fmt.Sprintf("%v | %+v | %#v | %+v", session, session, session, err)
				redacttest.RequireNoPiece(t, "envoy.answer-words/"+read.name+"/"+c.name, printed, bearer)
				if !c.kept {
					want := read.route + " answered a " + read.member + " the rule does not print as it came"
					if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), want) {
						t.Fatalf("the %s read answered %+v, %v; want ErrUnavailable naming %q", read.name, session, err, want)
					}
					return
				}
				if err != nil {
					t.Fatalf("the %s read answered %v, want the session", read.name, err)
				}
				if session.SessionID != c.wantID || !strings.Contains(session.Title, c.wantTitle) {
					t.Errorf("the %s read kept session %q titled %q, want %q titled with %q", read.name, session.SessionID, session.Title, c.wantID, c.wantTitle)
				}
			})
		}
	}
}
