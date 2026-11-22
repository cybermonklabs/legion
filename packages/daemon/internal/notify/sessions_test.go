package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The daemon reads Envoy's whole session list once a minute, unresponsive sessions included (R5):
// each session's state and its last registration, which is what the 20 minutes before a claim is
// out of reach are counted from.
func TestListSessionsReadsEverySessionWithItsLastRegistration(t *testing.T) {
	var method, path, query, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `[
			{"session_id":"ses_live","state":"live","last_seen":1762962661000,"roles":["legion-bet-bet-836-architect"],"topics":["a"]},
			{"session_id":"ses_stalled","state":"unresponsive","last_seen":1762962541000,"roles":[]}
		]`)
	}))
	defer server.Close()

	sessions, err := New(server.URL, "listener-token").ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if method != http.MethodGet || path != "/v1/sessions" || query != "include=unresponsive" || bearer != "Bearer listener-token" {
		t.Fatalf("asked %s %s?%s with %q, want GET /v1/sessions?include=unresponsive with the listener's bearer", method, path, query, bearer)
	}
	want := []ListedSession{
		{Session: "ses_live", State: "live", LastSeen: time.UnixMilli(1762962661000)},
		{Session: "ses_stalled", State: StateUnresponsive, LastSeen: time.UnixMilli(1762962541000)},
	}
	if len(sessions) != len(want) {
		t.Fatalf("ListSessions = %+v, want %+v", sessions, want)
	}
	for i := range want {
		if sessions[i].Session != want[i].Session || sessions[i].State != want[i].State || !sessions[i].LastSeen.Equal(want[i].LastSeen) {
			t.Fatalf("session %d = %+v, want %+v", i, sessions[i], want[i])
		}
	}
	if !sessions[0].Live() || sessions[1].Live() {
		t.Fatalf("Live = %t, %t, want the live session alone live", sessions[0].Live(), sessions[1].Live())
	}
}

// Each row names the Envoy roles its session holds, which is how the daemon reads whether its own
// relaunched controller holds the controller's role again (R6).
func TestListSessionsReadsTheRolesEachSessionHolds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `[
			{"session_id":"ses_controller","state":"live","last_seen":1762962661000,"roles":["legion-bet-controller","other"]},
			{"session_id":"ses_none","state":"live","last_seen":1762962661000,"roles":[]},
			{"session_id":"ses_old","state":"live","last_seen":1762962661000}
		]`)
	}))
	defer server.Close()

	sessions, err := New(server.URL, "").ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("ListSessions = %+v, want three sessions", sessions)
	}
	if got := sessions[0].Roles; !slices.Equal(got, []string{"legion-bet-controller", "other"}) {
		t.Errorf("the controller's session holds the roles %v, want [legion-bet-controller other]", got)
	}
	if !sessions[0].HoldsRole("legion-bet-controller") || sessions[1].HoldsRole("legion-bet-controller") || sessions[2].HoldsRole("legion-bet-controller") {
		t.Errorf("HoldsRole = %t, %t, %t, want the controller's session alone to hold its role",
			sessions[0].HoldsRole("legion-bet-controller"), sessions[1].HoldsRole("legion-bet-controller"), sessions[2].HoldsRole("legion-bet-controller"))
	}
}

// A list the listener does not answer with is a read that failed, never an empty list: an empty
// list would read as Envoy hearing nobody, which counts the 20 minutes again for every claim.
func TestListSessionsTellsARefusalFromAList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "refilling", status: http.StatusServiceUnavailable, body: `{"error":"refilling","code":"refilling"}`},
		{name: "not a list", status: http.StatusOK, body: `{"error":"x"}`},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			sessions, err := New(server.URL, "").ListSessions(context.Background())
			if err == nil {
				t.Fatalf("ListSessions = %+v, want an error", sessions)
			}
			if !strings.Contains(err.Error(), "session list") {
				t.Fatalf("the error %q does not name the session list", err)
			}
		})
	}
}
