package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// waitRequestID is the one request waitBroker serves, and waitValue the value its grant releases
// for WAIT_ME.
const (
	waitRequestID = "req-wait"
	waitValue     = "wait-value"
)

// waitBroker is a fake broker with one request, WAIT_ME's waitRequestID, which turns granted
// grantAfter after its start: the request's creation by POST /v1/requests, or, with
// afterFirstHold, the moment the first status read it held for its whole wait answers pending.
// With honoursWait it answers a status read as the broker does a GET /v1/requests/{id}?wait=N:
// held until the request is granted or N seconds pass, whichever is first; without it, it answers
// at once, as a broker from before the parameter does. It checks every proof as the broker does
// (proofKey.verify), so a client that signed its query would be refused.
type waitBroker struct {
	*httptest.Server
	mu        sync.Mutex
	created   time.Time // when POST /v1/requests made the request
	grantAt   time.Time // when the request turns granted; zero until afterFirstHold's hold ends
	released  time.Time // when POST /v1/grants/{id}/values released its value
	waits     []string  // each status read's wait parameter, "" for none
	badProofs int
}

func newWaitBroker(t *testing.T, key *proofKey, honoursWait, afterFirstHold bool, grantAfter time.Duration) *waitBroker {
	t.Helper()
	b := &waitBroker{}
	var publicURL string
	verified := func(w http.ResponseWriter, r *http.Request) bool {
		if err := key.verify(r, publicURL); err != nil {
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
		if !afterFirstHold {
			b.grantAt = b.created.Add(grantAfter)
		}
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
		grantAt := b.grantAt
		b.mu.Unlock()
		if seconds, err := strconv.Atoi(wait); honoursWait && err == nil && seconds >= 1 && seconds <= record.MaxRequestWaitSeconds {
			hold := time.Duration(seconds) * time.Second
			if !grantAt.IsZero() {
				hold = min(time.Until(grantAt), hold)
			}
			select {
			case <-time.After(hold):
			case <-r.Context().Done():
				return
			}
		}
		b.mu.Lock()
		if b.grantAt.IsZero() {
			// afterFirstHold: this read is the first, and its hold has just ended undecided.
			b.grantAt = time.Now().Add(grantAfter)
		}
		granted := !time.Now().Before(b.grantAt)
		b.mu.Unlock()
		writeJSON(w, state(granted))
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

// buildNotingBackoff builds agent-secrets with its test-only build seam execWaitBackoffNote set,
// so the exec form's wait prints execWaitBackoffNoteLine on stderr each time it sleeps its
// backoff. The form ends in syscall.Exec, so a test drives it as a process of its own, which no
// test clock reaches; seen from the broker, a read sent after that sleep looks the same as a read
// a loaded machine sent late, and the line tells the two apart without timing either.
func buildNotingBackoff(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "agent-secrets-noting-backoff",
		Flags: []string{"-ldflags", "-X main.execWaitBackoffNote=1"}})
}

// runWaiting runs `agent-secrets WAIT_ME --wait 1m -- sh -c …`, the build at binary, against b,
// failing t unless the child ran with the granted value, and returns its stderr.
func runWaiting(t *testing.T, binary string, b *waitBroker, key *proofKey) string {
	t.Helper()
	stdout, stderr, exit := runAgentSecrets(t, binary, b.URL, key.keyDir, nil,
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
	return stderr
}

// requireNoBackoffSleep fails t when stderr, from a buildNotingBackoff build, says the exec form's
// wait slept its backoff: after a read the broker held, the next read goes at once.
func requireNoBackoffSleep(t *testing.T, stderr string) {
	t.Helper()
	if strings.Contains(stderr, execWaitBackoffNoteLine) {
		t.Errorf("the wait slept its backoff (stderr %q); want each read sent at once after one the broker held", stderr)
	}
}

// TestExecLearnsOfTheGrantWithinASecond: against a broker that holds each `wait` read, a request
// granted 3 s after it was made has its value released, the last broker call before the child
// runs, as soon as the client learns of the grant (R1): every status read asks a wait of 1 to
// record.MaxRequestWaitSeconds seconds, so the read in flight when the grant comes carries it,
// there are at most 2 of them, and the wait never sleeps its backoff. The loop before this change
// asked no wait and slept its backoff between reads, at 2 s and 6 s, so it released 3 s after the
// grant. No check reads a clock: a loaded machine that is slow to send or act on a read changes
// none of these facts, where a bound on the time from grant to release failed it.
func TestExecLearnsOfTheGrantWithinASecond(t *testing.T) {
	const grantAfter = 3 * time.Second
	key := newProofKey(t)
	b := newWaitBroker(t, key, true, false, grantAfter)
	stderr := runWaiting(t, buildNotingBackoff(t), b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	t.Logf("the grant was released %s after the request was granted, after %d status reads (waits %q)", b.released.Sub(b.grantAt), len(b.waits), b.waits)
	requireNoBackoffSleep(t, stderr)
	if len(b.waits) > 2 {
		t.Errorf("the broker served %d status reads (waits %q), want at most 2", len(b.waits), b.waits)
	}
	for _, wait := range b.waits {
		if seconds, err := strconv.Atoi(wait); err != nil || seconds < 1 || seconds > record.MaxRequestWaitSeconds {
			t.Errorf("a status read asked wait=%q, want a whole number of seconds from 1 to %d (waits %q)", wait, record.MaxRequestWaitSeconds, b.waits)
		}
	}
}

// TestExecLearnsOfAGrantAfterAFullHold: R1's usual case, a person who decides after the first
// held read has run its whole wait. The fake holds that read for all record.MaxRequestWaitSeconds
// and grants 300 ms after it answers pending; the next read, sent with no sleep, carries the
// grant, so the value is released after 2 status reads. A loop that slept its backoff after every
// pending answer, a held one too, would sleep 2 s before that read. Seen from the broker that
// sleep looks the same as a loaded machine's slow read, so the test runs a build that says each
// backoff sleep on stderr (buildNotingBackoff) and requires none, rather than timing the release.
func TestExecLearnsOfAGrantAfterAFullHold(t *testing.T) {
	key := newProofKey(t)
	b := newWaitBroker(t, key, true, true, 300*time.Millisecond)
	stderr := runWaiting(t, buildNotingBackoff(t), b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	full := strconv.Itoa(record.MaxRequestWaitSeconds)
	if len(b.waits) == 0 || b.waits[0] != full {
		t.Fatalf("status reads asked waits %q, want the first to ask %s, the whole wait", b.waits, full)
	}
	t.Logf("the grant was released %s after the request was granted, after %d status reads (waits %q)", b.released.Sub(b.grantAt), len(b.waits), b.waits)
	requireNoBackoffSleep(t, stderr)
	if len(b.waits) != 2 {
		t.Errorf("the broker served %d status reads (waits %q), want 2: the full hold, then the read the grant ends", len(b.waits), b.waits)
	}
}

// TestTheWaitLimitEndsInsideTheClientsCallTimeout: a held read of the longest wait the broker
// takes ends before the whole-call timeout of the client every call goes out on (newClient's
// http.Client) cuts it. A timeout that fell to the limit or under it would cut every undecided
// held read, which the exec form reads as a failed read and answers with its backoff, so R1's
// second would become up to ten.
func TestTheWaitLimitEndsInsideTheClientsCallTimeout(t *testing.T) {
	timeout := newClient(brokeraddr.New("http://broker.invalid")).http.Timeout
	if limit := time.Duration(record.MaxRequestWaitSeconds) * time.Second; timeout <= limit {
		t.Fatalf("record.MaxRequestWaitSeconds is %s, want under the client's whole-call timeout (%s)", limit, timeout)
	}
}

// TestExecBacksOffAgainstABrokerThatIgnoresWait: a broker from before the parameter answers each
// read at once, pending until 9 s after the request was made. The loop does not read it in a
// tight loop: an answer that is still pending well before the wait it asked has run out makes it
// sleep its old backoff, so the child runs after at most 6 reads. The build here is made as a
// release makes it, with no -X of the test-only seam execWaitBackoffNote, so the backoff sleeps
// it takes pass without the line a build that sets the seam prints.
func TestExecBacksOffAgainstABrokerThatIgnoresWait(t *testing.T) {
	key := newProofKey(t)
	b := newWaitBroker(t, key, false, false, 9*time.Second)
	stderr := runWaiting(t, buildAgentSecrets(t), b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.waits) > 6 {
		t.Errorf("the broker served %d status reads, want at most 6: the loop must back off from a broker that answers at once", len(b.waits))
	}
	switch {
	case strings.Contains(stderr, execWaitBackoffNoteLine):
		t.Errorf("a build without -X main.execWaitBackoffNote printed %q (stderr %q); want the seam empty, as in every shipped build", execWaitBackoffNoteLine, stderr)
	case len(b.waits) < 2:
		// Each pending answer here is followed by a backoff sleep; with one read, granted, the
		// loop slept none, so the line's absence shows nothing in this run.
		t.Logf("UNKNOWN whether this build is silent: the loop read once (waits %q) and slept no backoff", b.waits)
	}
}
