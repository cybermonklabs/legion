package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// TestForwarderRefusesAnIncompleteCall: every flag the forwarder needs, and the role's broker and
// key directory, is required, and a call missing one exits 2 naming it before anything listens.
func TestForwarderRefusesAnIncompleteCall(t *testing.T) {
	full := []string{"--role", "implementer", "--listen", "127.0.0.1:0", "--secret", "ROLE_APP", "--reason", "the implementer of ACME-1", "--github-host", "api.github.com", "--gh-dir", t.TempDir()}
	without := func(flag string) []string {
		at := slices.Index(full, flag)
		return slices.Concat(full[:at], full[at+2:])
	}
	t.Setenv("AGENT_SECRETS_URL", "http://127.0.0.1:1")
	t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
	for _, flag := range []string{"--role", "--listen", "--secret", "--reason", "--github-host", "--gh-dir"} {
		code, _, stderr := legionRun(t, append([]string{"forwarder"}, without(flag)...)...)
		if code != 2 || !strings.Contains(stderr, flag+" is required") {
			t.Errorf("legion forwarder without %s = %d, %q; want 2 naming it", flag, code, stderr)
		}
	}
	for _, bad := range [][]string{{"--rename", "acme/a"}, {"--default-owner", "Acme Org"}} {
		code, _, stderr := legionRun(t, append([]string{"forwarder"}, append(slices.Clone(full), bad...)...)...)
		if code != 2 || !strings.Contains(stderr, bad[0]) {
			t.Errorf("legion forwarder %v = %d, %q; want 2 naming %s", bad, code, stderr, bad[0])
		}
	}
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	if code, _, stderr := legionRun(t, append([]string{"forwarder"}, full...)...); code != 2 || !strings.Contains(stderr, "AGENT_SECRETS_KEY_DIR") {
		t.Errorf("legion forwarder with no key directory = %d, %q; want 2 naming AGENT_SECRETS_KEY_DIR", code, stderr)
	}
	if code, _, stderr := legionRun(t, "forwarder", "stray"); code != 2 || stderr == "" {
		t.Errorf("legion forwarder stray = %d, %q; want a usage error", code, stderr)
	}
}

// forwarderBroker answers the three session routes a forwarder calls: a request for the role's names is
// granted at once, the grant's release holds a placeholder for each of two accounts behind a proxy,
// and a revocation is recorded. It records each request object's claims as the forwarder signed
// them, and each call's proof.
type forwarderBroker struct {
	server *httptest.Server
	mu     sync.Mutex
	claims []map[string]any
	proofs int
	ended  []string
}

func newForwarderBroker(t *testing.T) *forwarderBroker {
	t.Helper()
	b := &forwarderBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		b.proof(r)
		var body struct {
			Request string `json:"request"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		parts := strings.Split(body.Request, ".")
		payload, _ := base64.RawURLEncoding.DecodeString(parts[min(1, len(parts)-1)])
		var claims map[string]any
		_ = json.Unmarshal(payload, &claims)
		b.mu.Lock()
		b.claims = append(b.claims, claims)
		id := fmt.Sprintf("grant-%d", len(b.claims))
		b.mu.Unlock()
		fmt.Fprintf(w, `{"request_id":"req-1","state":"granted","grant_id":%q,"lifetime_seconds":3600}`, id)
	})
	mux.HandleFunc("POST /v1/grants/{id}/values", func(w http.ResponseWriter, r *http.Request) {
		b.proof(r)
		id := r.PathValue("id")
		fmt.Fprintf(w, `{"expires_at":%q,"values":{},"airlock":{"proxy_url":"http://127.0.0.1:1","ca_certificate":"","proxy_password":"pw-%s",
			"names":{"ROLE_APP":{"value":"{}","hosts":["api.github.com","github.com"],"slots":{"github:acme-a":"ph-a-%s","github:acme-b":"ph-b-%s"}}}}}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339), id, id, id)
	})
	mux.HandleFunc("POST /v1/grants/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		b.proof(r)
		b.mu.Lock()
		b.ended = append(b.ended, r.PathValue("id"))
		b.mu.Unlock()
		fmt.Fprint(w, `{"state":"revoked"}`)
	})
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *forwarderBroker) proof(r *http.Request) {
	if r.Header.Get("Proof") != "" {
		b.mu.Lock()
		b.proofs++
		b.mu.Unlock()
	}
}

// roleKeyDir is a role's key directory as its shim and the daemon leave it: key.pem and, once the
// daemon has enrolled the key, the enrollment id beside it.
func roleKeyDir(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestForwarderAsksWithTheRolesKeyWritesTheBundleAndRevokesAtItsStop runs `legion forwarder` in
// this process against a broker that answers its session routes. It waits for the daemon's
// enrollment, then asks with the role's key, by proof, for --secret's names for 3600 seconds
// under --reason and no target; writes each account's placeholder into --gh-dir's account
// directory, beside --rename's pair; and revokes the grant when it stops.
func TestForwarderAsksWithTheRolesKeyWritesTheBundleAndRevokesAtItsStop(t *testing.T) {
	broker := newForwarderBroker(t)
	keyDir := roleKeyDir(t)
	ghDir := filepath.Join(t.TempDir(), "gh")
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listen.Addr().String()
	listen.Close()

	getenv := func(name string) string {
		return map[string]string{"AGENT_SECRETS_URL": broker.server.URL, "AGENT_SECRETS_KEY_DIR": keyDir}[name]
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runForwarder(ctx, forwarderFlags{role: "implementer", listen: address, secrets: []string{"ROLE_APP"}, githubHosts: []string{"api.github.com", "github.com"},
			reason: "the implementer of ACME-1", ghDir: ghDir, defaultOwner: "acme-a", renames: []string{"acme-b/old=acme-a/new"}}, getenv, &stderr)
	}()

	time.Sleep(300 * time.Millisecond)
	broker.mu.Lock()
	early := len(broker.claims)
	broker.mu.Unlock()
	if early != 0 {
		t.Fatalf("the forwarder asked %d times before the key was enrolled; want none", early)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "enrollment"), []byte("enrollment-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(ghDir, "acme-b", ghconfig.HostsFile)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(hosts); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for account, want := range map[string]string{"acme-a": "ph-a-grant-1", "acme-b": "ph-b-grant-1"} {
		data, err := os.ReadFile(filepath.Join(ghDir, account, ghconfig.HostsFile))
		if err != nil {
			t.Fatalf("%s's hosts.yml: %v (stderr %s)", account, err, stderr.String())
		}
		if token, err := ghconfig.TokenFromHosts(data); err != nil || token != want {
			t.Fatalf("%s's hosts.yml holds %q, %v; want %q", account, token, err, want)
		}
	}
	if pairs, err := ghconfig.ReadPairs(ghDir); err != nil || len(pairs) != 1 || pairs[0].From.String() != "acme-b/old" {
		t.Fatalf("the bundle's renames = %v, %v; want acme-b/old moved to acme-a/new", pairs, err)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("the forwarder exited %d at its stop: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the forwarder did not stop")
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.claims) != 1 {
		t.Fatalf("the forwarder asked %d times; want once", len(broker.claims))
	}
	claims := broker.claims[0]
	details, _ := claims["authorization_details"].([]any)
	if len(details) != 1 || claims["reason"] != "the implementer of ACME-1" || claims["lifetime_seconds"] != float64(3600) || claims["aud"] != broker.server.URL {
		t.Fatalf("the request object = %v; want ROLE_APP for 3600 s under the reason, for the broker's URL", claims)
	}
	detail, _ := details[0].(map[string]any)
	if detail["type"] != "agent_secret" || detail["identifier"] != "ROLE_APP" || detail["locations"] != nil {
		t.Fatalf("the request's detail = %v; want ROLE_APP to inject with no target", detail)
	}
	if broker.proofs < 3 {
		t.Fatalf("%d of the forwarder's calls carried a proof; want every one: the request, the release and the revocation", broker.proofs)
	}
	if !slices.Equal(broker.ended, []string{"grant-1"}) {
		t.Fatalf("the broker saw revocations %v; want grant-1 at the stop", broker.ended)
	}
}
