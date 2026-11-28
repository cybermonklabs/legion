package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// decodedWhoamiOrigins parses response as a JSON object and returns its origins array (nil when
// absent) and whether the "origins" key appeared at all.
func decodedWhoamiOrigins(t *testing.T, what string, response *httptest.ResponseRecorder) ([]string, bool) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s = %d %s, want 200", what, response.Code, response.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode %s body %s: %v", what, response.Body.String(), err)
	}
	raw, present := fields["origins"]
	if !present {
		return nil, false
	}
	var origins []string
	if err := json.Unmarshal(raw, &origins); err != nil {
		t.Fatalf("decode %s origins %s: %v", what, string(raw), err)
	}
	return origins, true
}

// TestTheAPIWhoamiNamesTheOriginsAuthWhoamiNames (R10, D14): GET /api/v1/whoami writes the list
// DepsInput.WhoamiOrigins holds (wired from the router's origin table in routes; held against GET
// /auth/whoami by routes' TestTheAPIWhoamiMatchesAuthWhoamiThroughTheRouter) at every Host on both
// its user and its agent forms, and no "origins" key where it is unset, as the router leaves it
// with neither list set, so a server at one address answers as it always did.
func TestTheAPIWhoamiNamesTheOriginsAuthWhoamiNames(t *testing.T) {
	t.Parallel()
	const (
		canonical = "https://a.example.test"
		served    = "https://b.example.test"
		former    = "https://old.example.test"
		alice     = "alice@example.test"
		agentKey  = "dispatch-whoami-origins-fixture"
	)
	every := []string{canonical, served, former}

	mount := func(origins []string) http.Handler {
		deps, err := NewDeps(withDocumentService(t, DepsInput{
			Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", People: unrecordedPeople{}},
			AgentTokens:   sharedAgentTokens(t, agentKey),
			ServerURL:     canonical,
			ServedOrigins: []string{served},
			FormerOrigins: []string{former},
			WhoamiOrigins: origins,
		}))
		if err != nil {
			t.Fatalf("NewDeps: %v", err)
		}
		mux := http.NewServeMux()
		background := Register(mux, t.Context(), deps)
		t.Cleanup(background.Wait)
		return mux
	}

	ask := func(handler http.Handler, host, kind string) ([]string, bool) {
		request := httptest.NewRequest(http.MethodGet, "https://"+host+"/api/v1/whoami", nil)
		if kind == "user" {
			request.Header.Set("X-Dispatch-User", alice)
		} else {
			request.Header.Set("Authorization", "Bearer "+agentKey)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return decodedWhoamiOrigins(t, kind+" GET /api/v1/whoami at Host "+host, response)
	}

	configured := mount(every)
	for _, host := range []string{"a.example.test", "b.example.test"} {
		for _, kind := range []string{"user", "agent"} {
			if got, present := ask(configured, host, kind); !present || !slices.Equal(got, every) {
				t.Errorf("%s GET /api/v1/whoami at Host %s = %v (present=%v), want %v", kind, host, got, present, every)
			}
		}
	}

	unset := mount(nil)
	for _, kind := range []string{"user", "agent"} {
		if got, present := ask(unset, "a.example.test", kind); present {
			t.Errorf("%s: GET /api/v1/whoami with WhoamiOrigins unset carried origins=%v, want the key omitted", kind, got)
		}
	}
}
