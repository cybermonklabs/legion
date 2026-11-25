package daemon

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
)

// TestAKeptStopsLinePrintsTheListenersWordsAsTheRuleReadsThem: the Envoy listener, or whatever
// answers in its place, names the dead session's registration with a client instance id and keeps
// it at the delete with a reason; the words it can write quote a URL with a credential in its user
// information and the request's own bearer in its query. The kept line names the instance and the
// reason as the rule reads them: none of the three credentials, and the rest of the listener's
// words (kept). An instance id that quotes them is one the rule would change, which the delete
// cannot send back, so no delete is sent and the line that says so holds none of the three
// either (echoed-instance). The control is the kept line itself, which names the listener's words
// where they go.
func TestAKeptStopsLinePrintsTheListenersWordsAsTheRuleReadsThem(t *testing.T) {
	bearer := githubtest.InstallationToken("session stop")
	said := "https://not-a-secret-user:not-a-secret-password@issuer.example/kept?access_token=" + bearer
	stop := func(t *testing.T, instance string) (line string, deletes int64) {
		t.Helper()
		var deleted atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodDelete {
				deleted.Add(1)
				_, _ = io.WriteString(w, `{"deleted":false,"reason":"kept, see `+said+`"}`)
				return
			}
			_, _ = io.WriteString(w, `{"session_id":"ses_dead","client_instance_id":"`+instance+`","last_seen":1}`)
		}))
		t.Cleanup(server.Close)
		var logged bytes.Buffer
		sessionStop{sessions: notify.New(server.URL, bearer), log: bufferedLogger(&logged)}.send("ses_dead", time.Now())
		line = logged.String()
		for _, secret := range []string{bearer, "not-a-secret-user", "not-a-secret-password"} {
			if strings.Contains(line, secret) {
				t.Errorf("the stop's line prints %q: %s", secret, line)
			}
		}
		return line, deleted.Load()
	}
	t.Run("kept", func(t *testing.T) {
		line, _ := stop(t, "inst_dead")
		if !strings.Contains(line, "the Envoy listener kept the registration of a dead role's session") ||
			!strings.Contains(line, "inst_dead") || strings.Count(line, "issuer.example/kept") != 1 {
			t.Fatalf("logged %q, want the kept line naming the listener's client instance and reason", line)
		}
	})
	t.Run("echoed-instance", func(t *testing.T) {
		line, deletes := stop(t, "inst see "+said)
		if deletes != 0 {
			t.Errorf("%d deletes reached the listener; want none for an instance id the delete cannot send back", deletes)
		}
		if !strings.Contains(line, "the Envoy listener did not confirm the clean stop of a dead role's session") ||
			!strings.Contains(line, "no delete was sent") {
			t.Fatalf("logged %q, want the stop's refusal, which names no delete sent", line)
		}
	})
}
