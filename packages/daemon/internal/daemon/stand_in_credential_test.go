package daemon

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// fromTheDaemon reports whether r carries bearer, the credential the daemon under test presents to
// a stand-in. Any other request is another process's: the loopback port the kernel gives a
// stand-in may be one a process of the machine still sends to. It is answered 404 and logged, and
// fails nothing, so a stand-in's power to fail the test stays with the daemon's own requests.
func fromTheDaemon(t *testing.T, w http.ResponseWriter, r *http.Request, bearer string) bool {
	t.Helper()
	if r.Header.Get("Authorization") == "Bearer "+bearer {
		return true
	}
	t.Logf("a stand-in answered 404 to %s %s, which carries no credential of the daemon's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return false
}

// Each stand-in that holds the daemon to its requests answers a request without the daemon's
// credential 404, and fails nothing: workflowConfig's Dispatch board on a path it fails the test
// on, the dequeue rig's GitHub on a repository it answers, and its Envoy listener on a publish it
// takes. The request is the one a helper of the machine sent to a reused port: a read of its
// metadata server's identity token.
func TestAStandInAnswersARequestWithoutTheDaemonsCredential404(t *testing.T) {
	var cfg config.Config
	newDequeueListener(t, &cfg)
	client := &http.Client{Timeout: 10 * time.Second}
	for _, row := range []struct{ name, method, url, body string }{
		{"workflowConfig's Dispatch board", http.MethodGet,
			workflowDispatch(t, "TEST"+randomSuffix(t)) + "/computeMetadata/v1/instance/service-accounts/default/identity", ""},
		{"the dequeue rig's GitHub", http.MethodGet, newDequeueGitHub(t).url + "/repos/" + startedAgainRepo, ""},
		{"the dequeue rig's Envoy listener", http.MethodPost, cfg.EnvoyURL + "/v1/messages/publish",
			`{"topic":"notifications.legion.standin","message":"m"}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			request, err := http.NewRequest(row.method, row.url, strings.NewReader(row.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("send %s %s without the daemon's credential: %v", row.method, row.url, err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s without the daemon's credential: status %d, want 404", row.method, row.url, response.StatusCode)
			}
		})
	}
}
