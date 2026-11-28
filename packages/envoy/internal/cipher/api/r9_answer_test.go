package api_test

import (
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"
)

// TestARegistrationNamingNoKeyAndNoGenerationIsAnsweredAsBefore (R9): a registrar that sends what
// every registrar sent before the binding existed — a pod token and a session, no thumbprint and
// no generation, as the session watch and dev-broker.sh write it — is answered the fields the merge
// base answered, and no others: pod_uid, session and expires_at.
func TestARegistrationNamingNoKeyAndNoGenerationIsAnsweredAsBefore(t *testing.T) {
	t.Parallel()
	ts := newTestServerWith(t, withExchangeAccounts(t, daemonAccount, workerAccount))
	registrar := ts.accountToken(t, daemonAccount, "", time.Hour)
	podToken := ts.accountToken(t, workerAccount, "pod-uid-watched", time.Hour)
	status, body := ts.req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions",
		map[string]string{"Authorization": "Bearer " + registrar},
		map[string]any{"pod_token": podToken, "session": "watched-session"})
	if status != http.StatusOK {
		t.Fatalf("a registration naming no key and no generation = %d %s, want 200", status, body)
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	got, want := slices.Sorted(maps.Keys(answer)), []string{"expires_at", "pod_uid", "session"}
	if !slices.Equal(got, want) {
		t.Fatalf("a registration naming no key and no generation is answered the fields %v, want the merge base's %v: %s", got, want, body)
	}
}

// TestARegistrationNamingNoKeyAndNoGenerationLogsAsBefore (R9): the same write, of a row that names
// no key and no generation either, logs the line the merge base logged, with the attributes it
// had and no others: no thumbprint or generation sent or stored, and none replaced.
func TestARegistrationNamingNoKeyAndNoGenerationLogsAsBefore(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (api_test.newRegistrationRig, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	rig := newRegistrationRig(t, "pod-uid-watched")
	for _, write := range []string{"the first write", "a renewal"} {
		rig.register(t, map[string]any{"session": "watched-session"})
		rig.loggedOnly(t, write+" of watched-session naming no key and no generation", map[string]any{
			"level": slog.LevelInfo, "msg": registeredMessage, "session": "watched-session",
		})
	}
}
