package cipher

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestASessionCallRunsUnderItsOwnContext: a session call goes through the shared broker exchange
// under the context it is given, as a forwarder's stop relies on when it hands each revocation pass
// a time limit of its own. A Revoke under a context that has ended reaches no broker and answers
// that context's error; the same Revoke under a live context reaches the broker with its session
// proof, on the session's own client.
//
// The named revert: have call build its request without its ctx, and the ended context's Revoke
// reaches the broker.
func TestASessionCallRunsUnderItsOwnContext(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" proof="+map[bool]string{true: "yes", false: "no"}[r.Header.Get("Proof") != ""])
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"revoked"}`))
	}))
	t.Cleanup(server.Close)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{URL: server.URL, Key: key, EnrollmentID: "enrollment-1", HTTP: server.Client()}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Revoke(ended, "g1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Revoke under an ended context = %v, want context.Canceled", err)
	}
	mu.Lock()
	early := len(seen)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("the broker saw %d calls from a Revoke whose context had ended; want none", early)
	}
	if err := session.Revoke(context.Background(), "g1"); err != nil {
		t.Fatalf("Revoke under a live context = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "POST /v1/grants/g1/revoke proof=yes" {
		t.Fatalf("the broker saw %v; want one POST /v1/grants/g1/revoke with a proof", seen)
	}
}
