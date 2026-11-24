package daemon

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
)

// TestAKeptStopsLinePrintsTheListenersWordsAsTheRuleReadsThem: the Envoy listener, or whatever
// answers in its place, names the dead session's registration with a client instance id, and
// keeps it at the delete with a reason, each of which quotes a URL with a credential in its user
// information and the request's own bearer in its query. The kept line names both as the rule
// reads them: none of the three credentials, and the rest of the listener's words. Both printed as
// they came. The control is the kept line itself, which names the listener's words where they go.
func TestAKeptStopsLinePrintsTheListenersWordsAsTheRuleReadsThem(t *testing.T) {
	bearer := githubtest.InstallationToken("session stop")
	said := "https://not-a-secret-user:not-a-secret-password@issuer.example/kept?access_token=" + bearer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodDelete {
			_, _ = io.WriteString(w, `{"deleted":false,"reason":"kept, see `+said+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"session_id":"ses_dead","client_instance_id":"inst see `+said+`","last_seen":1}`)
	}))
	t.Cleanup(server.Close)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, bearer), log: bufferedLogger(&logged)}

	stop.send("ses_dead", time.Now())

	line := logged.String()
	if !strings.Contains(line, "the Envoy listener kept the registration of a dead role's session") || strings.Count(line, "issuer.example/kept") != 2 {
		t.Fatalf("logged %q, want the kept line naming the listener's client instance and reason", line)
	}
	for _, secret := range []string{bearer, "not-a-secret-user", "not-a-secret-password"} {
		if strings.Contains(line, secret) {
			t.Errorf("the kept line prints %q: %s", secret, line)
		}
	}
}
