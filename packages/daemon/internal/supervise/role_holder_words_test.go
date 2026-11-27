package supervise

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
)

// roleLookupBearer is the stand-in Envoy token the role lookups below carry, GitHub-shaped so no
// run of it is an English word the rule would cut out of the listener's own words.
var roleLookupBearer = githubtest.InstallationToken("role lookup")

// forgetAgainst is a claim suspended over session, every name of which another claim holds, whose
// role lookup is the real notify client asking a stand-in listener that answers status and body.
func forgetAgainst(t *testing.T, status int, body string) error {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(listener.Close)
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
	h.wantState(StateRetired)
	for _, name := range RecordedHolds(session, sessionFile) {
		h.store.setHold(name, "legion-legion-LEGION-209-tester")
	}
	h.deps.RoleHolder = notify.New(listener.URL, roleLookupBearer).RoleHolder
	h.restart()
	return h.handle(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByOperator})
}

// TestAForgetRefusalPrintsTheListenersHolderStateAsTheRuleReadsIt: the listener, or whatever
// answers in its place, names the claim's own session as the role's holder, and a holder_state
// that quotes a URL with a credential in its user information and the request's own bearer in its
// query. The forget is refused naming that state, which prints as the rule reads it: none of the
// three credentials, and the rest of the listener's words. The 200 printed the state as it came;
// the 404 of a lapsed holder, the same words, printed "lapsed" in their place, a state the
// listener did not give.
func TestAForgetRefusalPrintsTheListenersHolderStateAsTheRuleReadsIt(t *testing.T) {
	state := "see https://not-a-secret-user:not-a-secret-password@issuer.example/?access_token=" + roleLookupBearer
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"live holder", http.StatusOK, `{"holder":"` + session + `","holder_state":"` + state + `","last_seen":1}`},
		{"lapsed holder", http.StatusNotFound, `{"reason":"holder_lapsed","holder":"` + session + `","holder_state":"` + state + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := forgetAgainst(t, tc.status, tc.body)
			var refused *RefusedError
			if !errors.As(err, &refused) || !errors.Is(err, ErrForgetWaitsForRole) {
				t.Fatalf("the clear = %v, want it refused while the listener names %s", err, session)
			}
			// Control: the refusal is the one that names the holder's state, where the words go.
			if !strings.Contains(err.Error(), "names "+session+" (") || !strings.Contains(err.Error(), "issuer.example") {
				t.Fatalf("the refusal = %v, want it to name %s and the state the listener gave", err, session)
			}
			for _, secret := range []string{roleLookupBearer, "not-a-secret-user", "not-a-secret-password"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal prints %q: %v", secret, err)
				}
			}
		})
	}
}

// TestAForgetDecidesOnTheListenersHolderAsItCame: the listener names as the role's holder another
// session, whose id shares sixteen characters with the request's bearer, so the rule prints it
// changed. Whether that holder is the claim's own session is decided on the id as it came, so the
// clear goes on, on the 200 and on the 404 of a lapsed holder alike; the 404 refused it, as a
// lookup the listener refuses, since its holder was read as the rule prints it. The control names
// the claim's own session on both, which refuses the clear as a wait.
func TestAForgetDecidesOnTheListenersHolderAsItCame(t *testing.T) {
	another := "ses_" + roleLookupBearer[4:20]
	for _, tc := range []struct {
		name, holder string
		status       int
		admitted     bool
	}{
		{"another live holder", another, http.StatusOK, true},
		{"another lapsed holder", another, http.StatusNotFound, true},
		{"control: the claim's own session live", session, http.StatusOK, false},
		{"control: the claim's own session lapsed", session, http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"holder":"` + tc.holder + `","holder_state":"unresponsive","last_seen":1}`
			if tc.status == http.StatusNotFound {
				body = `{"reason":"holder_lapsed","holder":"` + tc.holder + `","holder_state":"unresponsive"}`
			}
			err := forgetAgainst(t, tc.status, body)
			if tc.admitted {
				if err != nil {
					t.Fatalf("the clear = %v, want it admitted: the listener names another session than %s", err, session)
				}
				return
			}
			if !errors.Is(err, ErrForgetWaitsForRole) {
				t.Fatalf("the clear = %v, want it refused as a wait while the listener names %s", err, session)
			}
		})
	}
}
