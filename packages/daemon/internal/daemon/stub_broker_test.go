package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// stubBroker is the runtime.kubernetes.agent_secrets block of a test that builds a Kubernetes
// configuration by hand, as a loaded one always names it (the loader's checkKubernetesKeys), over
// a secrets broker double that answers every route the daemon calls at a broker. To the daemon's
// own agent-secrets client (internal/cipher) it answers the token login a credential an hour long,
// an enrollment an id of its own, a revoke and a carry check nothing to keep, and a release every
// distinct name it was sent; a pod-session registration it answers 404, which the machine only
// logs. To the trade of the daemon's own pod identity (/v1/oauth/token) it answers
// daemonBrokerToken, the credential the daemon presents its Envoy listener. It also gives the test
// process the daemon pod's identity at that broker (podIdentity), as a daemon pod's environment
// carries it, so a boot under runtime: kubernetes trades it (settleCredentials). The test's listener
// must answer the boot's role lookup (answerRoleLookup). The token file holds a placeholder the
// double never reads.
func stubBroker(t *testing.T) *config.AgentSecretsConfig {
	t.Helper()
	var enrollments atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth/token":
			_, _ = io.Copy(io.Discard, r.Body)
			_, body := mintsTheDaemonsToken(0)
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/launcher-credentials/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"credential_id": "cred-stub", "expires_at": expires})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/enrollments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"enrollment_id": fmt.Sprintf("enr-%d", enrollments.Add(1)), "lease_expires_at": expires})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/enrollments/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/enrollments/carried":
			_, _ = w.Write([]byte(`{"carried":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth/exchange/pod-sessions/release":
			var body struct {
				Sessions []string `json:"sessions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]int{"sessions": len(slices.Compact(slices.Sorted(slices.Values(body.Sessions))))})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	for name, value := range podIdentity(t, server.URL) {
		t.Setenv(name, value)
	}
	tokenFile := filepath.Join(t.TempDir(), "agent-secrets-token")
	if err := os.WriteFile(tokenFile, []byte("placeholder-daemon-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &config.AgentSecretsConfig{
		URL: server.URL, TokenFile: tokenFile, Audience: "agent-secrets", TokenExpirySeconds: 3600, ExchangeAudience: "legion-broker-exchange",
	}
}

// answerRoleLookup answers r when it is a lookup of a project's controller role (GET
// /v1/roles/<project>-controller), the role the boot looks up under the daemon's broker token
// (listenerCredentialOf), as the Envoy listener answers a role no session holds: 404 naming the
// role unclaimed, the answer the boot goes on from. A lookup of any other role is left to the test's
// own listener. It reports whether it answered.
func answerRoleLookup(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/roles/") || !strings.HasSuffix(r.URL.Path, "-controller") {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"error":"no claim","reason":"unclaimed"}`)
	return true
}

// roleListener is an Envoy listener that answers the boot's role lookup (answerRoleLookup) and
// every other call 200, for a test whose daemon calls its listener for nothing else it reads.
func roleListener(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if !answerRoleLookup(w, r) {
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}
