package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// waitRequestID is the one request waitBroker serves, and waitValue the value its grant releases
// for WAIT_ME.
const (
	waitRequestID = "req-wait"
	waitValue     = "wait-value"
)

// waitBroker is a fake broker with one request, WAIT_ME's waitRequestID, which turns granted
// grantAfter after POST /v1/requests made it. With honoursWait it answers a status read as the
// broker does a GET /v1/requests/{id}?wait=N: held until the request is granted or N seconds
// pass, whichever is first; without it, it answers at once, as a broker from before the
// parameter does. It verifies every proof as the broker does, with the htu rebuilt from the
// path alone, so a client that signed its query would be refused.
type waitBroker struct {
	*httptest.Server
	mu        sync.Mutex
	created   time.Time // when POST /v1/requests made the request
	released  time.Time // when POST /v1/grants/{id}/values released its value
	waits     []string  // each status read's wait parameter, "" for none
	badProofs int
}

func newWaitBroker(t *testing.T, key *proofKey, honoursWait bool, grantAfter time.Duration) *waitBroker {
	t.Helper()
	b := &waitBroker{}
	verifier := key.verifier()
	var publicURL string
	verified := func(w http.ResponseWriter, r *http.Request) bool {
		if _, err := verifier.Verify(r.Context(), r.Header.Get("Proof"), r.Method, publicURL+r.URL.Path, time.Now()); err != nil {
			b.mu.Lock()
			b.badProofs++
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"PROOF_INVALID","error":"proof invalid: htm/htu do not match the request"}`))
			return false
		}
		return true
	}
	state := func(granted bool) map[string]any {
		answer := map[string]any{"state": "pending", "grant_id": nil, "record_id": "rec-wait", "approver": "ada@example.com", "decided_at": nil, "decision": nil}
		if granted {
			answer["state"], answer["grant_id"] = "granted", "grant-wait"
		}
		return answer
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		if !verified(w, r) {
			return
		}
		b.mu.Lock()
		b.created = time.Now()
		b.mu.Unlock()
		writeJSON(w, map[string]any{
			"request_id": waitRequestID, "state": "pending", "grant_id": nil, "record_id": "rec-wait",
			"approver": "ada@example.com", "secrets": []map[string]string{{"name": "WAIT_ME", "decision": "approval"}},
		})
	})
	mux.HandleFunc("GET /v1/requests/"+waitRequestID, func(w http.ResponseWriter, r *http.Request) {
		if !verified(w, r) {
			return
		}
		wait := r.URL.Query().Get("wait")
		b.mu.Lock()
		b.waits = append(b.waits, wait)
		grantAt := b.created.Add(grantAfter)
		b.mu.Unlock()
		if seconds, err := strconv.Atoi(wait); honoursWait && err == nil && seconds >= 1 && seconds <= 20 {
			until := min(time.Until(grantAt), time.Duration(seconds)*time.Second)
			select {
			case <-time.After(until):
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, state(!time.Now().Before(grantAt)))
	})
	mux.HandleFunc("POST /v1/grants/grant-wait/values", func(w http.ResponseWriter, r *http.Request) {
		if !verified(w, r) {
			return
		}
		b.mu.Lock()
		b.released = time.Now()
		b.mu.Unlock()
		writeJSON(w, map[string]any{"values": map[string]string{"WAIT_ME": waitValue}, "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, fakeSettings)
	})
	b.Server = httptest.NewServer(mux)
	t.Cleanup(b.Close)
	publicURL = b.URL
	return b
}

// proofKey is a session key with the enrollment id it signs as, and the key dir that holds both.
type proofKey struct {
	thumbprint string
	keyDir     string
}

func newProofKey(t *testing.T) *proofKey {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &proofKey{thumbprint: thumbprint, keyDir: writeKeyDir(t, key, testEnrollmentID)}
}

// verifier checks a proof as the broker's proof.Verifier does, for this key's enrollment alone.
func (k *proofKey) verifier() *proof.Verifier {
	return &proof.Verifier{Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			return k.thumbprint, id == testEnrollmentID, nil
		},
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil }}
}

// runWaiting runs `agent-secrets WAIT_ME --wait 1m -- sh -c …` against b, failing t unless the
// child ran with the granted value.
func runWaiting(t *testing.T, b *waitBroker, key *proofKey) {
	t.Helper()
	stdout, stderr, exit := runAgentSecrets(t, buildAgentSecrets(t), b.URL, key.keyDir, nil,
		"WAIT_ME", "--wait", "1m", "--", "sh", "-c", `test "$WAIT_ME" = `+waitValue+` && echo ran-the-child`)
	b.mu.Lock()
	badProofs := b.badProofs
	b.mu.Unlock()
	if badProofs != 0 {
		t.Fatalf("the broker refused %d proofs: a proof signs the path alone, never the query (stderr %q)", badProofs, stderr)
	}
	if exit != 0 || !strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the child run with the granted value", exit, stdout, stderr)
	}
}

// TestExecLearnsOfTheGrantWithinASecond: against a broker that holds each `wait` read, a request
// granted 3 s after it was made has its value released, the last broker call before the child
// runs, by 3.5 s, in at most 2 status reads, each asking a wait of 1 to 20 seconds. The loop
// before this change read at 2 s and 6 s, so it released at 6 s.
func TestExecLearnsOfTheGrantWithinASecond(t *testing.T) {
	key := newProofKey(t)
	b := newWaitBroker(t, key, true, 3*time.Second)
	runWaiting(t, b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	if took := b.released.Sub(b.created); took > 3500*time.Millisecond {
		t.Errorf("the grant was released %s after the request was made, want by 3.5 s: the decision came at 3 s", took)
	}
	if len(b.waits) > 2 {
		t.Errorf("the broker served %d status reads (waits %q), want at most 2", len(b.waits), b.waits)
	}
	for _, wait := range b.waits {
		if seconds, err := strconv.Atoi(wait); err != nil || seconds < 1 || seconds > 20 {
			t.Errorf("a status read asked wait=%q, want a whole number of seconds from 1 to 20 (waits %q)", wait, b.waits)
		}
	}
}

// TestExecBacksOffAgainstABrokerThatIgnoresWait: a broker from before the parameter answers each
// read at once, pending until 9 s after the request was made. The loop does not read it in a
// tight loop: an answer that is still pending well before the wait it asked has run out makes it
// sleep its old backoff, so the child runs after at most 6 reads.
func TestExecBacksOffAgainstABrokerThatIgnoresWait(t *testing.T) {
	key := newProofKey(t)
	b := newWaitBroker(t, key, false, 9*time.Second)
	runWaiting(t, b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.waits) > 6 {
		t.Errorf("the broker served %d status reads, want at most 6: the loop must back off from a broker that answers at once", len(b.waits))
	}
}
