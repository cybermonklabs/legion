// packages/envoy/cmd/cipher/main_test.go
//
// These tests build the real agent-secrets binary and run it as a subprocess against a fake
// broker: the "NAME... -- <command>" form ends in syscall.Exec, which replaces the calling
// process image, so it cannot be exercised in-process the way an ordinary function call can.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testtmp"
	"github.com/sjawhar/envoy/internal/testwait"
)

// testEnrollmentID is the enrollment id these tests' key dir fixture "issues" (writes into the
// enrollment file), and the id fakeBroker's GET /v1/enrollments/self echoes back — proof that
// self round-trips whatever id was on disk rather than a hardcoded one.
const testEnrollmentID = "enr-fake-1"

// buildAgentSecrets compiles this package's binary, once for the test process (TestMain), and
// returns its path.
func buildAgentSecrets(t *testing.T) string {
	t.Helper()
	return cmdtest.Build(t, "agent-secrets")
}

// shortTempDir is a directory of the test's own, removed when the test ends, with room under it
// for the longest path these tests put there: the default helper socket a test's XDG_RUNTIME_DIR
// holds, agent-secrets/helper.sock (hostmode_test.go). TMPDIR places it, and the helper falls
// back to /tmp for that length alone, where t.TempDir()'s path carries the whole of the test's
// name and passes the 107 bytes a unix socket path holds (docs/testing.md).
func shortTempDir(t *testing.T) string {
	t.Helper()
	return testtmp.Dir(t, filepath.Join("agent-secrets", "helper.sock"))
}

// brokerCounters records how many times fakeBroker served each route, and every call it answered
// on any route (calls), so a test can assert on call counts (e.g. "no second ask", "never entered
// the polling loop", "asked the broker nothing") instead of only on the final observable outcome,
// plus (guarded by mu) the last session_id POST /v1/requests recorded and the gate a test may hold
// the pending request's status polls behind.
type brokerCounters struct {
	createRequest int32
	getRequest    int32
	calls         int32

	mu            sync.Mutex
	lastSessionID string
	pendingPoll   chan struct{}
	noSettings    bool
	settings      *Settings
	released      *string
}

// releaseValue makes POST /v1/grants/grant-granted/values answer value for GRANT_ME in place of
// the default, so a test can release a value of a shape of its own, such as one that ends in a
// line ending.
func (c *brokerCounters) releaseValue(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = &value
}

// releasedValue is what that route answers with: what releaseValue chose, else the default.
func (c *brokerCounters) releasedValue() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released != nil {
		return *c.released
	}
	return "topsecretvalue123"
}

func (c *brokerCounters) sessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSessionID
}

// holdPendingPolls makes GET /v1/requests/req-pending wait until gate is closed.
func (c *brokerCounters) holdPendingPolls(gate chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingPoll = gate
}

// hideSettings makes GET /v1/settings answer 404, as a broker too old to serve it does: the CLI
// then names the broker alone as its source.
func (c *brokerCounters) hideSettings() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noSettings = true
}

// serveSettings makes GET /v1/settings describe settings in place of fakeSettings.
func (c *brokerCounters) serveSettings(settings Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings = &settings
}

// servedSettings is what GET /v1/settings answers: nil for a 404, else the settings it describes.
func (c *brokerCounters) servedSettings() *Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.noSettings:
		return nil
	case c.settings != nil:
		return c.settings
	default:
		return &fakeSettings
	}
}

// fakePending are fakeBroker's never-resolving pending requests, keyed by the name that asks for
// each: its request id, its record, the approver its answers carry, and whether they carry the
// record's page on the broker as approve_url, <fake broker>/approve/<record>. PENDING_ME_NO_PAGE
// answers as a broker from before that page, which carries none, and PENDING_UNNAMED as one from
// before the approver field, which names none. No real broker answers with the page and no
// approver, since approve_url is the newer field, but the waiting line has a sentence for that
// pair and PENDING_UNNAMED is what checks it. PENDING_JUDGE is a request the model judge holds:
// its answers say judge_deciding and carry no page. PENDING_HANDOVER is one the judge holds when
// it is written and has handed to its person by the first status read, which then carries the
// page.
var fakePending = map[string]struct {
	requestID, recordID, approver string
	page                          bool
	judging, handover             bool
}{
	"PENDING_ME":         {"req-pending", "rec-pending-1", "ada@example.com", true, false, false},
	"PENDING_ANYONE":     {"req-pending-anyone", "rec-pending-anyone", record.AnyoneApprover, true, false, false},
	"PENDING_UNNAMED":    {"req-pending-unnamed", "rec-pending-unnamed", "", true, false, false},
	"PENDING_ME_NO_PAGE": {"req-pending-no-page", "rec-pending-no-page", "ada@example.com", false, false, false},
	"PENDING_JUDGE":      {"req-pending-judge", "rec-pending-judge", "ada@example.com", false, true, false},
	"PENDING_HANDOVER":   {"req-pending-handover", "rec-pending-handover", "ada@example.com", false, true, true},
}

// pendingAnswer is fakeBroker's answer for a fakePending entry, request fields included or not;
// base is the fake broker's own URL, which its approve_url names. A status read (request false) of
// a PENDING_HANDOVER entry answers as the judge's hand-over left it: no longer held, with its page.
func pendingAnswer(name, base string, request bool) map[string]any {
	p := fakePending[name]
	page, judging := p.page, p.judging
	if p.handover && !request {
		page, judging = true, false
	}
	answer := map[string]any{"state": "pending", "grant_id": nil, "record_id": p.recordID}
	if p.approver != "" {
		answer["approver"] = p.approver
	}
	if page {
		answer["approve_url"] = base + "/approve/" + p.recordID
	}
	if judging {
		answer["judge_deciding"] = true
	}
	if request {
		answer["request_id"] = p.requestID
		answer["secrets"] = []map[string]string{{"name": name, "decision": "approval"}}
	} else {
		answer["decided_at"], answer["decision"] = nil, nil
	}
	return answer
}

// approvedStatusJSON is fakeBroker's GET /v1/requests/req-approved, byte for byte: a request a
// person approved, which keeps the record and the approver it waited on, as the broker answers it.
const approvedStatusJSON = `{"state":"granted","grant_id":"grant-approved","record_id":"rec-approved","approver":"ada@example.com","decided_at":"2026-10-09T12:00:00Z","decision":{"by":"ada@example.com","at":"2026-10-09T12:00:00Z"}}`

// refusedDetail is the broker's own sentence about the refusal DETAIL_DENIED gets: the secret
// whose requesters tag does not name this session, and the rule that refused it. DENY_ME keeps
// its own canned answer, which carries no detail at all, so what a denial the broker explained
// prints is told apart from what one it did not still prints.
const refusedDetail = "GCLOUD_DEMO: requester-not-listed"

// judgeRefusedText is the broker's own sentence for JUDGE_REFUSED: the model judge refused the
// request, finally, in the words refusalOf writes.
const judgeRefusedText = "the model judge refused this request: the guidance asks for an approved post. This request will not be granted. Tell whoever gave you this work."

// TestAJudgedRefusalIsFinal: a request the model judge refused is denied, and both the request form
// and the exec form exit 77, as any denial does, printing the judge's sentence and running no
// child. Nothing either prints tells the agent to try again or names a person to ask.
func TestAJudgedRefusalIsFinal(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	broker, _ := fakeBroker(t)
	for _, args := range [][]string{
		{"request", "JUDGE_REFUSED"},
		{"JUDGE_REFUSED", "--wait", "200ms", "--", "sh", "-c", "echo the-child-ran"},
	} {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, args...)
		out := stdout + stderr
		if exit != exitDenied || !strings.Contains(out, judgeRefusedText) || strings.Contains(out, "the-child-ran") ||
			strings.Contains(out, "again") || strings.Contains(out, "retry") || strings.Contains(out, "/approve/") {
			t.Errorf("%v = exit %d stdout %q stderr %q; want exit %d with the judge's sentence, no child and nothing that says to try again", args, exit, stdout, stderr, exitDenied)
		}
	}
}

// TestTheExecFormSaysWhenTheJudgeHandsARequestOver: a request the judge held when it was written
// and handed to its person while the exec form waits prints the judge's line first and then, at
// the first status read that shows the hand-over, the line naming the person and the page.
func TestTheExecFormSaysWhenTheJudgeHandsARequestOver(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	broker, _ := fakeBroker(t)
	_, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"PENDING_HANDOVER", "--wait", "3s", "--", "sh", "-c", "echo the-child-ran")
	judging := "the model judge is deciding it; nobody is asked to approve it unless the judge cannot decide it"
	handed := "the model judge could not decide it; waiting for ada@example.com to approve it on the secrets broker: " + broker.URL + "/approve/rec-pending-handover"
	if exit != exitPending || !strings.Contains(stderr, judging) || !strings.Contains(stderr, handed) || strings.Index(stderr, judging) > strings.Index(stderr, handed) {
		t.Errorf("exec form of PENDING_HANDOVER = exit %d stderr %q; want exit %d, the judge's line and then the hand-over line", exit, stderr, exitPending)
	}
}

// deniedDetailStatusJSON is fakeBroker's GET /v1/requests/req-denied-listed, byte for byte: the
// request DETAIL_DENIED made, read back, carrying the same detail its creation answered.
const deniedDetailStatusJSON = `{"state":"denied","grant_id":null,"record_id":null,"approver":null,"decided_at":"2026-10-09T12:00:00Z","decision":null,"detail":"` + refusedDetail + `"}`

// cannedStatus is every decided request fakeBroker serves GET /v1/requests/{id} for, byte for
// byte, because a test reads one back by id: the approved request and the refused one. Every
// other request the fake decided at once is still a 404 there (see the route below).
var cannedStatus = map[string]string{"req-approved": approvedStatusJSON, "req-denied-listed": deniedDetailStatusJSON}

// fakeBroker serves just enough of the broker's HTTP API for the exec-form and --json
// tests: POST /v1/requests decodes the signed request object CreateRequest posts (verifying it
// with record.VerifyRequestObject against the fake's own URL as audience — a real, non-stubbed
// check, since the wire shape under test IS that signed object) and routes on its first
// authorization_detail's identifier to a canned granted/pending/approved/denied/denied-with-a-
// detail/unreleased/no-trailing-newline response, GET /v1/requests/{id} answers each fakePending
// request's own id with the same never-resolving pending state and each cannedStatus id with its
// own bytes (and 404s any other id, since a granted-or-denied-immediately response a test does
// not read back must never be polled), POST /v1/grants/{id}/values releases one canned value
// (or, for the unreleased case, none at all, and for AIRLOCK_ME none and an airlock object
// instead), and GET /v1/enrollments/self echoes testEnrollmentID. It does not verify the outer
// Proof header at all — proof.Verifier's own behavior is covered by internal/cipher/proof and
// internal/cipher/api's test suites, not this package's.
func fakeBroker(t *testing.T) (*httptest.Server, *brokerCounters) {
	t.Helper()
	counters := &brokerCounters{}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counters.createRequest, 1)
		var body struct {
			Request   string  `json:"request"`
			SessionID *string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Request == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.SessionID != nil {
			counters.mu.Lock()
			counters.lastSessionID = *body.SessionID
			counters.mu.Unlock()
		}
		obj, err := record.VerifyRequestObject(body.Request, srv.URL, time.Minute, time.Now())
		if err != nil || len(obj.Details) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch name := obj.Details[0].Identifier; name {
		case "GRANT_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-granted", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "automatic"}},
				"grant_id": "grant-granted", "record_id": nil, "approver": nil,
			})
		case "PENDING_ME", "PENDING_ANYONE", "PENDING_UNNAMED", "PENDING_ME_NO_PAGE", "PENDING_JUDGE", "PENDING_HANDOVER":
			writeJSON(w, pendingAnswer(name, srv.URL, true))
		case "APPROVED_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-approved", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "approval"}},
				"grant_id": "grant-approved", "record_id": "rec-approved", "approver": "ada@example.com",
			})
		case "DENY_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-denied", "state": "denied",
				"secrets":  []map[string]string{{"name": name, "decision": "deny"}},
				"grant_id": nil, "record_id": nil, "approver": nil,
			})
		case "DETAIL_DENIED":
			writeJSON(w, map[string]any{
				"request_id": "req-denied-listed", "state": "denied",
				"secrets":  []map[string]string{{"name": name, "decision": "deny"}},
				"grant_id": nil, "record_id": nil, "approver": nil, "detail": refusedDetail,
			})
		case "PROJECT_DENIED":
			writeJSON(w, map[string]any{
				"request_id": "req-denied-project", "state": "denied",
				"secrets":  []map[string]string{{"name": name, "decision": "deny"}},
				"grant_id": nil, "record_id": nil, "approver": nil, "detail": name + ": " + policy.ReasonProjectNotNamed,
			})
		case "JUDGE_REFUSED":
			writeJSON(w, map[string]any{
				"request_id": "req-judge-refused", "state": "denied",
				"secrets":  []map[string]string{{"name": name, "decision": "approval"}},
				"grant_id": nil, "record_id": "rec-judge-refused", "approver": "ada@example.com", "detail": judgeRefusedText,
			})
		case "UNRELEASED_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-unreleased", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "automatic"}},
				"grant_id": "grant-unreleased", "record_id": nil, "approver": nil,
			})
		case "AIRLOCK_ME":
			writeJSON(w, map[string]any{
				"request_id": "req-airlock", "state": "granted",
				"secrets":  []map[string]string{{"name": name, "decision": "automatic"}},
				"grant_id": "grant-airlock", "record_id": nil, "approver": nil,
			})
		case "NONEWLINE_ME":
			// Written with http.ResponseWriter.Write directly, with NO trailing newline, unlike
			// every other case (which goes through writeJSON's json.Encoder, always "\n"
			// terminated) — this is the fixture for the writeVerbatim byte-exact test.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"req-nonewline","state":"granted","secrets":[{"name":"NONEWLINE_ME","decision":"automatic"}],"grant_id":"grant-nonewline","record_id":null,"approver":null}`))
		case "WAITING_ME":
			// The broker's refusal of a secret `secret create` made and has not yet filled.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"PENDING_VALUE","error":"WAITING_ME exists and awaits its first value; ask again after its owner stores it"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counters.getRequest, 1)
		if canned, ok := cannedStatus[r.PathValue("id")]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(canned))
			return
		}
		name := ""
		for n, p := range fakePending {
			if p.requestID == r.PathValue("id") {
				name = n
			}
		}
		if name == "" {
			// A request the fake decided immediately (granted or denied) must never be polled;
			// failing loudly here (rather than serving it) is the reuse-transparency test's proof
			// that cmdExec does not enter its pending-wait loop for an already-decided response.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		counters.mu.Lock()
		gate := counters.pendingPoll
		counters.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, pendingAnswer(name, srv.URL, false))
	})
	mux.HandleFunc("POST /v1/grants/grant-granted/values", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"values":     map[string]string{"GRANT_ME": counters.releasedValue()},
			"expires_at": time.Now().Add(time.Hour),
		})
	})
	mux.HandleFunc("POST /v1/grants/grant-unreleased/values", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"values":     map[string]string{},
			"expires_at": time.Now().Add(time.Hour),
		})
	})
	mux.HandleFunc("POST /v1/grants/grant-nonewline/values", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"values":     map[string]string{"NONEWLINE_ME": "irrelevant"},
			"expires_at": time.Now().Add(time.Hour),
		})
	})
	mux.HandleFunc("POST /v1/grants/grant-airlock/values", func(w http.ResponseWriter, r *http.Request) {
		// The airlock's release: nothing in "values" for the name at all, and the proxy, the
		// certificate to trust it by and the name's placeholder in "airlock", exactly as the
		// broker answers one (internal/cipher/api's grantValuesResponse).
		writeJSON(w, map[string]any{
			"values":     map[string]string{},
			"expires_at": time.Now().Add(time.Hour),
			"airlock": map[string]any{
				"proxy_url":      "http://airlock.broker.example:3128",
				"ca_certificate": testAirlockCertificate,
				"proxy_password": testAirlockPlaceholder,
				"names": map[string]any{
					"AIRLOCK_ME": map[string]any{
						"value": testAirlockPlaceholder,
						"slots": map[string]string{"value": testAirlockPlaceholder},
						"hosts": []string{"api.example.com"},
					},
				},
			},
		})
	})
	mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"enrollment_id": testEnrollmentID, "kind": "session", "operator": "ada@example.com",
			"lease_expires_at": time.Now().Add(time.Hour), "grants": []any{},
		})
	})
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		settings := counters.servedSettings()
		if settings == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, settings)
	})
	// The handlers read srv.URL, so srv is assigned before the server serves: Start, which sets
	// the URL, runs after the assignment and before the goroutine that serves.
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&counters.calls, 1)
		mux.ServeHTTP(w, r)
	}))
	srv.Start()
	return srv, counters
}

// fakeSettings is the store GET /v1/settings describes for these tests: a GCP broker, named by a
// placeholder project, so every line that names a source carries a store as a deployed broker's
// would.
var fakeSettings = Settings{
	Store:         secrets.StoreGCP,
	SecretsPrefix: "projects/000000000000/secrets/agent-secrets-",
	KMSKeyName:    "projects/000000000000/locations/global/keyRings/example/cryptoKeys/agent-secrets",
	GCPProject:    "000000000000",
}

// fakeSourceText is the source the CLI names for a line about a secret from this fake broker: its
// host and the store fakeSettings describes. It is built with the CLI's own renderer, because
// what each of these lines must carry is the source, not one spelling of it; source_test.go pins
// what the renderer puts in a source.
func fakeSourceText(broker *httptest.Server) string {
	return brokerSource(cipheraddr.New(broker.URL)).withSettings(fakeSettings).String()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newKeyDir writes a real ECDSA key.pem (proof.Sign needs a genuine key to sign with, even
// though fakeBroker never verifies the signature) and an enrollment file naming
// testEnrollmentID — the two files AGENT_SECRETS_KEY_DIR holds for every session-authenticated
// subcommand.
func newKeyDir(t *testing.T) string {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatalf("proof.NewKey: %v", err)
	}
	return writeKeyDir(t, key, testEnrollmentID)
}

// writeKeyDir writes key as key.pem and enrollmentID as the enrollment file into a new key dir.
func writeKeyDir(t *testing.T, key *ecdsa.PrivateKey, enrollmentID string) string {
	t.Helper()
	dir := t.TempDir()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pemBytes, 0o600); err != nil {
		t.Fatalf("write key.pem: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enrollment"), []byte(enrollmentID+"\n"), 0o600); err != nil {
		t.Fatalf("write enrollment: %v", err)
	}
	return dir
}

// proofKey is a session key that signs as testEnrollmentID, with the key dir that holds both, for
// a fake broker that checks each proof as the broker does.
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

// verify checks r's proof as the broker's proof.Verifier does, for this key's enrollment alone,
// with the htu rebuilt from publicURL and r's path, never its query, as the broker rebuilds it
// from BROKER_PUBLIC_URL. A client that signed anything else is refused.
func (k *proofKey) verify(r *http.Request, publicURL string) error {
	verifier := &proof.Verifier{Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			return k.thumbprint, id == testEnrollmentID, nil
		},
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil }}
	_, err := verifier.Verify(r.Context(), r.Header.Get("Proof"), r.Method, publicURL+r.URL.Path, time.Now())
	return err
}

// runAgentSecrets runs the built binary against broker and keyDir, returning its separate
// stdout/stderr and exit code (0 for a clean exit). extraEnv, when non-nil, is appended after the
// fixed AGENT_SECRETS_* variables, so a test can set an inherited variable the CLI must not let
// leak into (or shadow) a granted secret. The binary runs in a world of its own (hostIsolation),
// so a person's sign-in or settings file on the machine running the tests is not a source.
func runAgentSecrets(t *testing.T, binary, broker, keyDir string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(append(append(os.Environ(), hostIsolation(t)...), "AGENT_SECRETS_URL="+broker, "AGENT_SECRETS_KEY_DIR="+keyDir), extraEnv...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err == nil {
		return out.String(), errOut.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	t.Fatalf("run agent-secrets %v: %v\nstdout: %s\nstderr: %s", args, err, out.String(), errOut.String())
	return "", "", -1
}

// hostIsolation is the environment a binary runAgentSecrets runs gets over the test process's own,
// before the test's extraEnv: a home, a config, a state and a runtime directory of its own, and
// no sign-in store, machine identity or helper socket, as the harness of startCall gives them. So
// a developer's own sign-in (`agent-secrets login`), settings file, machine cache or helper on the
// machine running the tests is no source a test's command takes; a test that wants one names it
// in extraEnv.
func hostIsolation(t *testing.T) []string {
	t.Helper()
	env := []string{"AGENT_SECRETS_SIGNIN_DIR=", "AGENT_SECRETS_MACHINE_IDENTITY=", "AGENT_SECRETS_HELPER_SOCK="}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		env = append(env, name+"="+shortTempDir(t))
	}
	return env
}

// oneJSONObject parses stdout as exactly one JSON object (one line, nothing else) and returns
// its top-level key set, for asserting --json prints "the contract's response body verbatim ...
// one JSON object on stdout and nothing else".
func oneJSONObject(t *testing.T, stdout string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines, want exactly 1: %q", len(lines), stdout)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("stdout is not one JSON object: %v: %q", err, stdout)
	}
	return obj
}

func keySet(m map[string]any) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// redactSecrets replaces the granted value these tests' fake broker hands out with a placeholder
// before it is used in a t.Fatalf diagnostic. Comparisons and control flow always use the real
// value; this project's rule against a secret ever reaching logs, audit, or error messages
// carries no test-only carve-out, so nothing this suite prints on failure may contain one either,
// even a fake, fixture-only value.
func redactSecrets(s string) string {
	return strings.ReplaceAll(s, "topsecretvalue123", "[REDACTED]")
}

func TestExecFormGrantRunsChildWithValueInEnvironment(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"GRANT_ME", "--", "sh", "-c", `echo -n "$GRANT_ME" | sha256sum`)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, redactSecrets(stdout), redactSecrets(stderr))
	}
	sum := sha256.Sum256([]byte("topsecretvalue123"))
	want := hex.EncodeToString(sum[:])
	fields := strings.Fields(stdout)
	if len(fields) == 0 || fields[0] != want {
		t.Fatalf("child sha256 = %q, want %q (stdout=%q)", redactSecrets(strings.Join(fields, " ")), want, redactSecrets(stdout))
	}
	if strings.Contains(stdout, "topsecretvalue123") {
		t.Fatalf("raw secret value leaked into stdout: %q", redactSecrets(stdout))
	}
}

// TestTheExecFormGivesTheChildTheReleasedBytesAndSaysNothingMore: the real child of the real
// binary gets the bytes the broker released, byte for byte — compared as a digest, so no value
// is in this test's output — and standard error carries the one source line and then nothing of
// this command's at all, not even where the released value ends in a line ending. Which bytes a
// value has is the broker's decision now (policy.TrimLineEnding): a value that reaches the child
// ending in a line feed ends that way because the stored value holds a line ending inside it, and the agent
// running the command could not store it differently in any case.
func TestTheExecFormGivesTheChildTheReleasedBytesAndSaysNothingMore(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	const released = "topsecretvalue123\n"
	counters.releaseValue(released)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"GRANT_ME", "--", "sh", "-c", `printf %s "$GRANT_ME" | sha256sum; echo the-child-ran >&2`)
	// Nothing below prints stdout or stderr: the released value is in neither unless the very
	// regression this test exists to catch has happened, and redactSecrets hides only the
	// default fixture. Each failure says what was wrong instead: an exit code, a digest, which
	// assertion about the two streams failed.
	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	want, fields := digest(released), strings.Fields(stdout)
	if len(fields) == 0 {
		t.Fatalf("the child printed no digest at all")
	}
	if fields[0] != want {
		// The token is not printed: a regression that puts the value where the digest belongs
		// would print it. Its own digest tells two failures apart and discloses nothing.
		t.Fatalf("what the child printed first is not the released value's digest %s; sha256 of what it did print: %s", want, digest(fields[0]))
	}
	source, rest, ended := strings.Cut(stderr, "\n")
	if !ended || !strings.HasPrefix(source, "agent-secrets: GRANT_ME from ") {
		t.Fatalf("the first of stderr's %d lines is not the source line", strings.Count(stderr, "\n"))
	}
	if rest != "the-child-ran\n" {
		t.Fatalf("after the source line stderr holds %d bytes (sha256 %s), want the child's own line and nothing else", len(rest), digest(rest))
	}
	// The released value, whole and ending aside, in either stream is the leak.
	for _, part := range []string{released, strings.TrimRight(released, "\r\n")} {
		if strings.Contains(stdout+stderr, part) {
			t.Fatalf("the released value reached this command's own output")
		}
	}
}

// pendingSentences is the sentence each fakePending request's wait names whom it waits on and
// where they decide it with, keyed by the name that asks for it. <broker> stands for the fake
// broker's URL. A request whose answer carries approve_url names that page on the broker; one
// whose answer carries none names its approver and no page at all. Neither sentence depends on
// AGENT_SECRETS_APPROVE_URL, which the two tests below set to Dispatch's origin and to nothing
// around every row. The *_UNNAMED broker names no approver, so its sentence says so. A request the
// model judge holds says the judge is deciding it and that nobody is asked unless the judge
// cannot decide it, and never that its approver must approve it.
var pendingSentences = map[string]string{
	"PENDING_ME":         "waiting for ada@example.com to approve it on the secrets broker: <broker>/approve/rec-pending-1",
	"PENDING_ANYONE":     "waiting for anyone signed in to the secrets broker to approve it: <broker>/approve/rec-pending-anyone",
	"PENDING_UNNAMED":    "waiting for approval on the secrets broker: <broker>/approve/rec-pending-unnamed; the broker does not say whose",
	"PENDING_ME_NO_PAGE": "waiting for ada@example.com to approve it on the secrets broker",
	"PENDING_JUDGE":      "the model judge is deciding it; nobody is asked to approve it unless the judge cannot decide it",
}

// TestAPendingRequestWithoutABrokerPageNamesItsApproverAlone pins all three sentences waitingOn
// writes when the broker's answer carries no approve_url: each names the approver the broker
// named, or says the broker named none, and names no page. PENDING_ME_NO_PAGE runs the third of
// them through the real binary in the two tests below; the other two have no fixture of their
// own, and this is what holds their wording.
func TestAPendingRequestWithoutABrokerPageNamesItsApproverAlone(t *testing.T) {
	t.Parallel()
	person, anyone := "ada@example.com", record.AnyoneApprover
	for _, row := range []struct {
		name     string
		approver *string
		want     string
	}{
		{"no approver", nil, "waiting for approval on the secrets broker; the broker does not say whose"},
		{"anyone signed in", &anyone, "waiting for anyone signed in to the secrets broker to approve it"},
		{"a person", &person, "waiting for ada@example.com to approve it on the secrets broker"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := waitingOn(cipheraddr.New("https://broker.example"), row.approver, "", false); got != row.want {
				t.Errorf("waitingOn = %q, want %q", got, row.want)
			}
		})
	}
}

// TestExecFormPendingExitsSeventyFiveWithNoChild also pins what the wait says, and that it says it
// before it waits: the request id and the bound, then whom it waits on and where they decide it —
// the approver the broker names (a person, or anyone signed in), and the record's page on the
// broker its answer names as approve_url. A broker that names no page leaves the approver alone.
// The fake broker holds the first status poll until the test has read those two lines from the
// command's stderr, so lines printed only once the wait ends never arrive in time.
func TestExecFormPendingExitsSeventyFiveWithNoChild(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for name, sentence := range pendingSentences {
		sentence = strings.ReplaceAll(sentence, "<broker>", broker.URL)
		for _, approveURL := range []string{"", "https://dispatch.example/"} {
			t.Run(name+" "+approveURL, func(t *testing.T) {
				requestID := fakePending[name].requestID
				announced := make(chan struct{})
				counters.holdPendingPolls(announced)
				released := false
				release := func() {
					if !released {
						released = true
						close(announced)
					}
				}
				defer release()

				cmd := exec.Command(binary, name, "--wait", "200ms", "--", "sh", "-c", "echo ran-the-child")
				cmd.Env = append(append(os.Environ(), hostIsolation(t)...), "AGENT_SECRETS_URL="+broker.URL, "AGENT_SECRETS_KEY_DIR="+keyDir,
					"AGENT_SECRETS_APPROVE_URL="+approveURL)
				c := startCall(t, cmd)
				want := "agent-secrets: request " + requestID + " to " + fakeSourceText(broker) + " is waiting for approval; waiting up to 200ms\n" +
					"agent-secrets: " + sentence + "\n"
				// The first two lines arrive while the poll is held, or never: the shared wait only
				// guards against a call that says nothing and does not end.
				testwait.Eventually(t, "the call's first two stderr lines while the status poll was held", func() bool {
					return strings.Count(c.stderr.String(), "\n") >= 2 || c.ended()
				})
				if got := c.stderr.String(); !strings.HasPrefix(got, want) {
					t.Errorf("stderr while the status poll was held began %q, want %q before the wait", got, want)
				}
				release()
				exit := c.end(t)
				stdout, rest := c.stdout.String(), strings.TrimPrefix(c.stderr.String(), want)
				if exit != exitPending {
					t.Fatalf("exit = %d, want %d (pending): stdout=%q stderr=%q", exit, exitPending, stdout, rest)
				}
				if strings.Contains(stdout, "ran-the-child") {
					t.Fatalf("child ran while request was still pending: stdout=%q", stdout)
				}
				if !strings.Contains(rest, "agent-secrets status "+requestID) {
					t.Fatalf("stderr = %q, want the request id and the status command to check it", rest)
				}
			})
		}
	}
}

// TestRequestAndStatusNameWhomAPendingRequestWaitsOn pins the text forms of request and status for
// a pending request: after its id and state, the same sentence the exec form's wait prints, naming
// the approver the broker names and the page on the broker where they decide it, which a broker
// that names none leaves out; request exits 75 and status 0, as before.
func TestRequestAndStatusNameWhomAPendingRequestWaitsOn(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for name, sentence := range pendingSentences {
		sentence = strings.ReplaceAll(sentence, "<broker>", broker.URL)
		for _, approveURL := range []string{"", "https://dispatch.example/"} {
			t.Run(name+" "+approveURL, func(t *testing.T) {
				env := []string{"AGENT_SECRETS_APPROVE_URL=" + approveURL}
				requestID := fakePending[name].requestID
				stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, env, "request", name)
				if want := "request_id: " + requestID + "\nstate: pending\nsource: " + fakeSourceText(broker) + "\n" + sentence + "\n"; exit != exitPending || stdout != want {
					t.Errorf("request %s = exit %d stdout %q (stderr %q); want exit %d stdout %q", name, exit, stdout, stderr, exitPending, want)
				}
				stdout, stderr, exit = runAgentSecrets(t, binary, broker.URL, keyDir, env, "status", requestID)
				if want := "state: pending\nsource: " + fakeSourceText(broker) + "\n" + sentence + "\n"; exit != 0 || stdout != want {
					t.Errorf("status %s = exit %d stdout %q (stderr %q); want exit 0 stdout %q", requestID, exit, stdout, stderr, want)
				}
			})
		}
	}
}

// TestADecidedRequestSaysNothingOfWaiting pins that only a pending request prints the waiting
// line: a request a person approved keeps its record and approver on the broker's answers, and
// request and status still print no `waiting for …` line for it, under any
// AGENT_SECRETS_APPROVE_URL. status --json prints the broker's answer byte for byte, approver
// included.
func TestADecidedRequestSaysNothingOfWaiting(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for _, approveURL := range []string{"", "https://dispatch.example/"} {
		env := []string{"AGENT_SECRETS_APPROVE_URL=" + approveURL}
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, env, "request", "APPROVED_ME")
		if want := "request_id: req-approved\nstate: granted\nsource: " + fakeSourceText(broker) + "\n"; exit != 0 || stdout != want {
			t.Errorf("request APPROVED_ME (approve URL %q) = exit %d stdout %q (stderr %q); want exit 0 stdout %q", approveURL, exit, stdout, stderr, want)
		}
		stdout, stderr, exit = runAgentSecrets(t, binary, broker.URL, keyDir, env, "status", "req-approved")
		if want := "state: granted\nsource: " + fakeSourceText(broker) + "\ngrant_id: grant-approved\ndecided_by: ada@example.com\n"; exit != 0 || stdout != want {
			t.Errorf("status req-approved (approve URL %q) = exit %d stdout %q (stderr %q); want exit 0 stdout %q", approveURL, exit, stdout, stderr, want)
		}
	}

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "status", "req-approved", "--json")
	if exit != 0 || stdout != approvedStatusJSON {
		t.Errorf("status req-approved --json = exit %d stdout %q (stderr %q); want exit 0 and the broker's answer byte for byte %q", exit, stdout, stderr, approvedStatusJSON)
	}
}

func TestExecFormDeniedExitsSeventySevenWithNoChild(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"DENY_ME", "--", "sh", "-c", "echo ran-the-child")
	if exit != exitDenied {
		t.Fatalf("exit = %d, want %d (denied): stdout=%q stderr=%q", exit, exitDenied, stdout, stderr)
	}
	if strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("child ran for a denied request: stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "req-denied") || !strings.Contains(stderr, "was denied") {
		t.Fatalf("stderr = %q, want it to name the denied request", stderr)
	}
	if !strings.Contains(stderr, fakeSourceText(broker)) {
		t.Fatalf("stderr = %q, want the refusal to name %s", stderr, fakeSourceText(broker))
	}
}

func TestExecFormRefusesWhenNoNameIsGiven(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, _, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "--", "sh", "-c", "echo ran-the-child")
	if exit == 0 {
		t.Fatalf("exit = 0, want a nonzero usage error when no NAME is given")
	}
	if strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("child ran with no NAME given: stdout=%q", stdout)
	}
}

// TestExecFormRefusesAMissingCommandBeforeAskingTheBroker: a command that is not on PATH is
// refused with the exit status and the line it always had (1, and `agent-secrets: ` before
// exec.LookPath's own error), and the broker is asked nothing at all: no request is made, so none
// waits on an approver for a command that could never run. PENDING_ME is a secret a person must
// approve; before the command was looked up first, its request reached that person and the run
// exited 75, still waiting.
func TestExecFormRefusesAMissingCommandBeforeAskingTheBroker(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)
	const missing = "agent-secrets-test-command-on-no-path"
	_, lookErr := exec.LookPath(missing)
	if lookErr == nil {
		t.Fatalf("%s is on PATH; this test needs a command that is not", missing)
	}
	for _, name := range []string{"GRANT_ME", "PENDING_ME"} {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, name, "--wait", "0s", "--", missing, "an-argument")
		if exit != 1 {
			t.Fatalf("%s: exit = %d, want 1: stdout=%q stderr=%q", name, exit, stdout, stderr)
		}
		if lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "agent-secrets: ") || !strings.Contains(lines[0], missing) || stdout != "" {
			t.Fatalf("%s: stdout=%q stderr=%q; want nothing on stdout and one agent-secrets line naming %s", name, stdout, stderr, missing)
		}
	}
	if calls := atomic.LoadInt32(&counters.calls); calls != 0 {
		t.Fatalf("the broker answered %d calls for a command that is not on PATH, want none", calls)
	}
}

// TestExecFormASecretWaitingForItsValueExitsOneAndRunsNothing: the broker's PENDING_VALUE refusal,
// for a secret `secret create` made and has not yet filled, ends the exec form with exit 1, its line
// naming the code and saying to ask again, and the command never runs.
func TestExecFormASecretWaitingForItsValueExitsOneAndRunsNothing(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, newKeyDir(t), nil, "WAITING_ME", "--", "sh", "-c", "echo ran-the-child")
	if exit != 1 || !strings.Contains(stderr, "(PENDING_VALUE)") || !strings.Contains(stderr, "ask again") || strings.Contains(stdout+stderr, "ran-the-child") {
		t.Fatalf("exit = %d, stdout=%q stderr=%q; want 1, the PENDING_VALUE refusal saying to ask again, and no command run", exit, stdout, stderr)
	}
}

// TestTopLevelHelpExitsZero pins that a bare top-level "--help" or "-h" — no subcommand — exits
// 0 and prints usage(), the conventional exit code for an explicit help request; only the
// zero-argument form (no NAME, no --help) remains a usage error exiting 2.
func TestTopLevelHelpExitsZero(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for _, flag := range []string{"--help", "-h", "help"} {
		stdout, _, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, flag)
		if exit != 0 {
			t.Fatalf("agent-secrets %s: exit = %d, want 0", flag, exit)
		}
		if !strings.Contains(stdout, "usage:") {
			t.Fatalf("agent-secrets %s: stdout = %q, want it to contain %q", flag, stdout, "usage:")
		}
	}

	if _, _, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil); exit != exitUsageError {
		t.Fatalf("agent-secrets with no arguments: exit = %d, want %d", exit, exitUsageError)
	}
}

// TestEveryFormAnswersHelp pins that each form usage lists answers -h with its own synopsis and
// exit 0, flags or none, before it needs a broker, a key or a helper; the docs site's CLI
// reference is built from these answers.
func TestEveryFormAnswersHelp(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	for _, c := range commands {
		if c.name == "--version" {
			continue
		}
		args := append(strings.Fields(c.name), "-h")
		if c.name == "" {
			args = []string{"NAME", "-h", "--", "true"}
		}
		_, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(), nil, args...)
		if exit != 0 {
			t.Fatalf("agent-secrets %s: exit = %d, want 0: %s", strings.Join(args, " "), exit, stderr)
		}
		if !strings.Contains(stderr, "usage: "+c.synopsis) {
			t.Fatalf("agent-secrets %s: stderr = %q, want its synopsis %q", strings.Join(args, " "), stderr, c.synopsis)
		}
	}
}

// TestExecFormRefusesToRunWhenAGrantedNameHasNoValue is the regression for the review's Important
// finding 3: a granted request whose values come back without one of its names must never exec,
// since silently execing without it would be a silent partial grant.
func TestExecFormRefusesToRunWhenAGrantedNameHasNoValue(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
		"UNRELEASED_ME", "--", "sh", "-c", "echo ran-the-child")
	if exit == 0 {
		t.Fatalf("exit = 0, want a nonzero refusal for a grant with no value: stdout=%q stderr=%q", stdout, stderr)
	}
	if exit == exitPending || exit == exitDenied {
		t.Fatalf("exit = %d, want a plain operational refusal (not 75/76/77): stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if strings.Contains(stdout, "ran-the-child") {
		t.Fatalf("child ran despite an unreleased secret: stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "UNRELEASED_ME") {
		t.Fatalf("stderr should name the missing secret UNRELEASED_ME: %q", stderr)
	}
}

// TestExecFormDoesNotLetInheritedEnvShadowAGrantedValue is the regression for the review's
// Important finding 1: an inherited environment variable sharing a granted secret's exact name
// must not survive into the child's environment alongside (and potentially before) the
// broker-released value.
func TestExecFormDoesNotLetInheritedEnvShadowAGrantedValue(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir,
		[]string{"GRANT_ME=attacker-controlled-stale-value"},
		"GRANT_ME", "--", "sh", "-c", `echo -n "$GRANT_ME" | sha256sum`)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, redactSecrets(stdout), redactSecrets(stderr))
	}
	sum := sha256.Sum256([]byte("topsecretvalue123"))
	want := hex.EncodeToString(sum[:])
	fields := strings.Fields(stdout)
	if len(fields) == 0 || fields[0] != want {
		t.Fatalf("child GRANT_ME sha256 = %q, want %q (granted value, not the shadowing inherited one): stdout=%q", redactSecrets(strings.Join(fields, " ")), want, redactSecrets(stdout))
	}
}

// TestExecFormSecondInvocationReusesGrantTransparently proves cmd/cipher needs no
// client-side change to benefit from Machine.Create's server-side grant reuse (the review's
// Critical finding; requests/machine.go): running the exec form twice for the same
// already-granted secret name must both times grant immediately, and — critically — must never
// poll GET /v1/requests/{id} (proof that neither invocation ever entered the pending-wait loop,
// whether the broker minted the grant fresh or handed back a reused one).
func TestExecFormSecondInvocationReusesGrantTransparently(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	for i := 0; i < 2; i++ {
		stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
			"GRANT_ME", "--", "sh", "-c", `echo -n "$GRANT_ME" | sha256sum`)
		if exit != 0 {
			t.Fatalf("invocation %d: exit = %d, want 0: stdout=%q stderr=%q", i, exit, redactSecrets(stdout), redactSecrets(stderr))
		}
	}
	if got := atomic.LoadInt32(&counters.createRequest); got != 2 {
		t.Fatalf("POST /v1/requests called %d times, want 2 (one per invocation)", got)
	}
	if got := atomic.LoadInt32(&counters.getRequest); got != 0 {
		t.Fatalf("GET /v1/requests/{id} called %d times, want 0 (an immediately granted request must never be polled)", got)
	}
}

// TestExecFormNestedCallKeepsTheSessionIdentity proves a command run under the exec form belongs
// to the same session: its own `agent-secrets` call (a skill that nests a request, gh's token
// helper under `agent-secrets K -- omp`) reaches the broker as that session instead of failing
// with "AGENT_SECRETS_URL is required".
func TestExecFormNestedCallKeepsTheSessionIdentity(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	// No default key dir or helper socket to fall back to: the inner call can reach the broker
	// only through the variables the outer one kept for it.
	env := []string{"BIN=" + binary, "XDG_RUNTIME_DIR=" + t.TempDir(), "AGENT_SECRETS_HELPER_SOCK="}
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, env,
		"GRANT_ME", "--", "sh", "-c", `"$BIN" NONEWLINE_ME -- sh -c "printf %s \"\$NONEWLINE_ME\""`)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, redactSecrets(stdout), redactSecrets(stderr))
	}
	if stdout != "irrelevant" {
		t.Fatalf("the nested call's child printed %q, want the value the broker released to it", redactSecrets(stdout))
	}
}

// testAirlockFiles are the two paths prepareAirlock would have written, as the tests that build
// a child environment pass them: buildChildEnv names them and writes neither.
var testAirlockFiles = airlockFiles{bundle: "/run/user/1000/agent-secrets/airlock/g1/ca-bundle.pem", wgetrc: "/run/user/1000/agent-secrets/airlock/g1/wgetrc"}

// TestBuildChildEnvKeepsOnlyTheSessionIdentity pins which of this CLI's variables the exec'd
// child inherits: exactly the three that say which session it is (the broker's URL, the helper
// socket, the box's key dir), never any other AGENT_SECRETS_* setting, and never an inherited
// copy of a released name beside the released value.
//
// The second row is the same grant with one name behind the airlock, and pins the whole of that
// child's environment too: the name holds its placeholder, the proxy, its NO_PROXY and Node's
// switch are set, every bundle variable and WGETRC name the files, and the three session
// identity variables are kept exactly as in the first row. An inherited copy of anything this
// CLI sets — the stale proxy, the inherited trust file — is gone, as an inherited copy of a
// released name is.
func TestBuildChildEnvKeepsOnlyTheSessionIdentity(t *testing.T) {
	t.Parallel()
	identity := []string{
		"PATH=/usr/bin",
		"AGENT_SECRETS_URL=https://broker.internal.example",
		"AGENT_SECRETS_HELPER_SOCK=/run/user/1000/agent-secrets/helper.sock",
		"AGENT_SECRETS_KEY_DIR=/run/user/1000/agent-secrets",
		"AGENT_SECRETS_WAIT=5m",
		"AGENT_SECRETS_ENROLL_WAIT=3s",
		"AGENT_SECRETS_APPROVE_URL=https://dispatch.internal.example",
		"GRANT_ME=inherited-stale-value",
		"NOT_AN_ASSIGNMENT",
	}
	kept := []string{
		"PATH=/usr/bin",
		"AGENT_SECRETS_URL=https://broker.internal.example",
		"AGENT_SECRETS_HELPER_SOCK=/run/user/1000/agent-secrets/helper.sock",
		"AGENT_SECRETS_KEY_DIR=/run/user/1000/agent-secrets",
	}
	placeholder := "agent-secrets-placeholder-" + strings.Repeat("a", 32)
	for _, row := range []struct {
		name    string
		environ []string
		release *AirlockRelease
		want    []string
	}{
		{
			name:    "no airlock",
			environ: identity,
			want:    append(append([]string{}, kept...), "GRANT_ME=released"),
		},
		{
			name: "one airlock name",
			environ: append(append([]string{}, identity...),
				"SSL_CERT_FILE=/inherited/roots.pem",
				"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH=/inherited/grpc-roots.pem",
				"HTTPS_PROXY=http://stale.example:3128",
				"NO_PROXY=keep.example.com"),
			release: &AirlockRelease{
				ProxyURL:      "http://proxy.broker.example:3128",
				CACertificate: "-----BEGIN CERTIFICATE-----\nproxy\n-----END CERTIFICATE-----\n",
				ProxyPassword: placeholder,
				Names:         map[string]AirlockName{"AIRLOCK_ME": {Value: placeholder, Slots: map[string]string{"value": placeholder}, Hosts: []string{"api.example.com"}}},
			},
			want: append(append([]string{}, kept...),
				"AIRLOCK_ME="+placeholder,
				"AWS_CA_BUNDLE="+testAirlockFiles.bundle,
				"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE="+testAirlockFiles.bundle,
				"CURL_CA_BUNDLE="+testAirlockFiles.bundle,
				"GIT_SSL_CAINFO="+testAirlockFiles.bundle,
				"GRANT_ME=released",
				"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH="+testAirlockFiles.bundle,
				"HTTPS_PROXY="+userinfoURL("http", "agent-secrets", placeholder, "proxy.broker.example:3128"),
				"NODE_EXTRA_CA_CERTS="+testAirlockFiles.bundle,
				"NODE_USE_ENV_PROXY=1",
				"NO_PROXY=keep.example.com,localhost,127.0.0.1,::1,.svc,.cluster.local,.internal,.ts.net,metadata.google.internal,169.254.169.254,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,broker.internal.example",
				"REQUESTS_CA_BUNDLE="+testAirlockFiles.bundle,
				"SSL_CERT_FILE="+testAirlockFiles.bundle,
				"WGETRC="+testAirlockFiles.wgetrc,
				"https_proxy="+userinfoURL("http", "agent-secrets", placeholder, "proxy.broker.example:3128"),
				"no_proxy=keep.example.com,localhost,127.0.0.1,::1,.svc,.cluster.local,.internal,.ts.net,metadata.google.internal,169.254.169.254,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,broker.internal.example",
			),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := buildChildEnv(row.environ, map[string]string{"GRANT_ME": "released"}, row.release, testAirlockFiles)
			if err != nil {
				t.Fatalf("buildChildEnv: %v", err)
			}
			if strings.Join(got, "\n") != strings.Join(row.want, "\n") {
				t.Fatalf("child env:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(row.want, "\n"))
			}
		})
	}
}

// TestReleasedValuesReachTheChildAsTheBrokerSentThem: which bytes a released value has is the
// broker's decision, taken from the stored value itself (policy.TrimLineEnding), so the exec form hands
// the child what the broker sent, byte for byte, line endings and all.
func TestReleasedValuesReachTheChildAsTheBrokerSentThem(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"one trailing newline":   "sk-live-abc\n",
		"trailing CRLF":          "sk-live-abc\r\n",
		"two trailing newlines":  "sk-live-abc\n\n",
		"no trailing newline":    "sk-live-abc",
		"a newline inside":       "first\nsecond",
		"only a newline":         "\n",
		"a lone carriage return": "sk-live-abc\r",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := buildChildEnv(nil, map[string]string{"GRANT_ME": value}, nil, airlockFiles{})
			if err != nil {
				t.Fatalf("buildChildEnv: %v", err)
			}
			// Compared as digests, and reported as digests: a failure here is a value that came
			// out other than it went in, which is the one thing not to print.
			want := []string{"GRANT_ME=" + value}
			gotSum, wantSum := sha256.Sum256([]byte(strings.Join(got, "\n"))), sha256.Sum256([]byte(strings.Join(want, "\n")))
			if gotSum != wantSum {
				t.Fatalf("child env sha256 = %s, want %s: the value must reach the child as the broker sent it", hex.EncodeToString(gotSum[:]), hex.EncodeToString(wantSum[:]))
			}
		})
	}
}

// call is a run of the built binary a test started (startCall), with what it has written to
// stdout and stderr so far, so that the test can act on a line the call prints while it runs.
type call struct {
	cmd            *exec.Cmd
	stdout, stderr *cmdtest.LockedBuffer
	exited         chan struct{}
}

// startCall starts cmd with its stdout and stderr kept apart, and kills it when the test ends if
// it is still running.
func startCall(t *testing.T, cmd *exec.Cmd) *call {
	t.Helper()
	c := &call{cmd: cmd, stdout: &cmdtest.LockedBuffer{}, stderr: &cmdtest.LockedBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = c.stdout, c.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", cmd.Path, err)
	}
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-c.exited
	})
	return c
}

// ended reports whether the call has exited. Once it has, its stdout and stderr are whole: Wait
// returns only after it has copied both.
func (c *call) ended() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// says waits until the call's stderr holds line times times, and fails the test when the call
// ends before that; when names the point the call ended at. The wait is the package's shared one
// (testwait.Eventually): it guards against a call that hangs, and bounds nothing the row decides.
func (c *call) says(t *testing.T, line string, times int, when string) {
	t.Helper()
	testwait.Eventually(t, "the call to say "+line, func() bool {
		return strings.Count(c.stderr.String(), line) >= times || c.ended()
	})
	if strings.Count(c.stderr.String(), line) < times {
		t.Fatalf("the call ended (exit %d) %s, and never said %q (%d times): stderr=%q", c.cmd.ProcessState.ExitCode(), when, line, times, c.stderr.String())
	}
}

// end waits for the call to exit and returns its exit code. The wait is testwait.Receive on the
// call's exit (docs/testing.md): it sets no span of its own, so a call that is merely slow on a
// loaded machine still ends the row, and one that never exits fails it by name 30 s before the
// test binary's deadline. A row whose verdict needs the call to end within a span waits for that
// itself, before it calls end.
func (c *call) end(t *testing.T) int {
	t.Helper()
	testwait.Receive(t, "the call to end", c.exited)
	return c.cmd.ProcessState.ExitCode()
}

// startExecForm starts the exec form of GRANT_ME against broker from keyDir with no helper to
// fall back to (hostIsolation: an empty runtime dir and no AGENT_SECRETS_HELPER_SOCK). extraEnv
// comes last, so it can override any of that. Its home, config and state directories are the
// test's own, so no sign-in or machine cache of the machine running the test answers for a key
// dir a row leaves empty.
func startExecForm(t *testing.T, binary, broker, keyDir string, extraEnv ...string) *call {
	t.Helper()
	cmd := exec.Command(binary, "GRANT_ME", "--", "sh", "-c", "exit 0")
	cmd.Env = append(append(append(os.Environ(), hostIsolation(t)...), "AGENT_SECRETS_URL="+broker, "AGENT_SECRETS_KEY_DIR="+keyDir), extraEnv...)
	return startCall(t, cmd)
}

// runExecForm runs startExecForm's call to its end, returning its exit code and stderr.
func runExecForm(t *testing.T, binary, broker, keyDir string, extraEnv ...string) (int, string) {
	t.Helper()
	c := startExecForm(t, binary, broker, keyDir, extraEnv...)
	return c.end(t), c.stderr.String()
}

// writePendingMarker writes the launcher's enrollment.pending into dir (created if absent), aged
// by age.
func writePendingMarker(t *testing.T, dir string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "enrollment.pending")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(marker, then, then); err != nil {
		t.Fatal(err)
	}
	return marker
}

// launcherWaitStart is how a call's first line about waiting on the launcher starts, whatever it
// names its key dir and its wait: a row in which no call may wait fails on it.
const launcherWaitStart = "agent-secrets: the launcher is still enrolling this process ("

// launcherWaitLine is the line a call prints as it starts to wait on the launcher still enrolling
// its key dir, which the line names as named (keyDirName: the setting, never the path), for wait at
// most (a time.Duration's text), while the key dir holds neither key.pem nor enrollment
// (awaitEnrollment).
func launcherWaitLine(named, wait string) string {
	return launcherWaitStart + named + " holds enrollment.pending); waiting up to " + wait + " for its key.pem and enrollment\n"
}

// enrollmentWaitLine is the line a waiting call prints once key.pem is there and enrollment is
// not yet (awaitEnrollment).
const enrollmentWaitLine = "agent-secrets: still waiting for the launcher's enrollment\n"

// keyGoneWaitLine is the line a waiting call prints when the key.pem it saw is gone again, so it
// waits for both files once more (awaitEnrollment says each change of what it waits for).
const keyGoneWaitLine = "agent-secrets: still waiting for the launcher's key.pem and enrollment\n"

// launcherPlayLines is all a call prints on stderr while playTheLauncher plays its launcher, in
// order, before anything the call prints once it has its identity.
func launcherPlayLines(named, wait string) string {
	return launcherWaitLine(named, wait) + enrollmentWaitLine + keyGoneWaitLine + enrollmentWaitLine
}

// playTheLauncher plays the launcher of keyDir, which is still enrolling the process of c: keyDir
// holds its fresh marker and neither key.pem nor enrollment, the call names it as named
// (keyDirName), and the call's bound is wait. It changes the key dir only once the call says it
// waits for that change, so the call is waiting before each change however slow the machine is:
// it writes fixture's key.pem once the call says it waits for key.pem and enrollment; once the
// call says it still waits for the enrollment, it takes key.pem away; once the call says it waits
// for both again, it puts key.pem back; once the call says a second time that it still waits for
// the enrollment, it writes fixture's enrollment, and then it removes the marker, as the launcher
// does. No real launcher takes key.pem back. The stand-in does so because the call's line about
// it is the one thing that shows, from outside the call, that the call still waits after it said
// it waits for the enrollment: a call that stops waiting once key.pem is there never says it, and
// when it reads, however late, it finds key.pem gone or the enrollment not yet written, and fails.
// Before each change it renews the marker (writePendingMarker), as the launcher does before each
// step of its setup, so a row that a slow machine stretches past the marker's life
// (pendingMarkerLife) still finds it fresh. Each file is written whole (writeFileAtomic), as the
// launcher writes it, so the call never reads one half-written. It fails the test when the call
// ends before any of those lines, or has ended by the time the launcher is about to write the
// enrollment. Once the enrollment is written the call may end at any moment, which is what it
// waited for: a call still running just before that write and exiting 0 afterwards ended after
// the write, since it cannot sign without the enrollment.
func playTheLauncher(t *testing.T, c *call, keyDir, named, fixture, wait string) {
	t.Helper()
	marker := filepath.Join(keyDir, pendingMarker)
	copyFile := func(name string) {
		t.Helper()
		writePendingMarker(t, keyDir, 0)
		data, err := os.ReadFile(filepath.Join(fixture, name))
		if err == nil {
			err = writeFileAtomic(filepath.Join(keyDir, name), data, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	c.says(t, launcherWaitLine(named, wait), 1, "before it waited for the launcher")
	copyFile(keyFile)
	c.says(t, enrollmentWaitLine, 1, "once key.pem was there and before the launcher wrote the enrollment")
	writePendingMarker(t, keyDir, 0)
	if err := os.Remove(filepath.Join(keyDir, keyFile)); err != nil {
		t.Fatal(err)
	}
	c.says(t, keyGoneWaitLine, 1, "once key.pem was gone again and before the launcher wrote the enrollment")
	copyFile(keyFile)
	c.says(t, enrollmentWaitLine, 2, "once key.pem was back and before the launcher wrote the enrollment")
	if c.ended() {
		t.Fatalf("the call ended (exit %d) while it waited for the enrollment, before the launcher wrote it: stderr=%q", c.cmd.ProcessState.ExitCode(), c.stderr.String())
	}
	copyFile(enrollmentFile)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
}

// TestEnrollWaitRidesOutTheLaunchersSetup is the box's enrollment race: omp starts its MCP
// servers, and they call agent-secrets, while the launcher is still generating the box's key and
// enrolling it. While the launcher's enrollment.pending marker is there, a call waits for key.pem
// and enrollment to appear instead of failing at once. The row decides by the order of events,
// never by a span: the launcher changes the key dir only once the call says it waits for that
// change (playTheLauncher), and the call must exit 0 after the enrollment is there, having said
// each line of its wait in order. Its bound is a day, so the bound cannot end the wait within the
// shared test wait; the marker, fresh for 160 s from each renewal before a change and removed only
// after the enrollment is written, cannot either. A call that does not wait for the marker ends
// before it says so, and one that stops waiting once key.pem is there never says that it waits
// for both files again, which fails the row whatever the load.
func TestEnrollWaitRidesOutTheLaunchersSetup(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	fixture := newKeyDir(t)
	keyDir := t.TempDir()
	writePendingMarker(t, keyDir, 0)

	c := startExecForm(t, binary, broker.URL, keyDir, "AGENT_SECRETS_ENROLL_WAIT=24h")
	playTheLauncher(t, c, keyDir, "AGENT_SECRETS_KEY_DIR", fixture, "24h0m0s")
	if exit := c.end(t); exit != 0 {
		t.Fatalf("exit = %d, want 0 once the launcher wrote the enrollment: stderr=%q", exit, c.stderr.String())
	}
	if want := launcherPlayLines("AGENT_SECRETS_KEY_DIR", "24h0m0s"); !strings.HasPrefix(c.stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to start with the wait's lines in order, %q", c.stderr.String(), want)
	}
}

// TestEnrollWaitGivesUpAtItsBound pins the wait's end: a marker nothing ever follows (a launcher
// stuck without removing it) holds a call until its AGENT_SECRETS_ENROLL_WAIT has passed, and the
// call then fails naming the marker and the wait. A fresh marker is a launcher still enrolling,
// which is present: the call stops there rather than go on to a later source of the chain. The
// call says that it waits before it reads its deadline (awaitEnrollment), so the row requires that
// line however late a loaded machine runs the call. The refusal says how long the call waited,
// from the moment it set its deadline, and its bound: "(waited 1.004s, its bound 1s, while ...)".
// The row requires two spans each to be at least the bound, and neither can fail a correct call,
// since load only lengthens them. The call's own time holds only its wait, so a call whose
// deadline came early fails it however late it started. The row's own span, from before it starts
// the call to the call's end, holds the call's start too, so it passes such a call that started
// late; it is there for a call that cuts its wait short and reports its bound as the time it
// waited, which the call's own time cannot see. The row reads no upper limit, which load could
// pass; the call's end (call.end) stands against a call whose bound never ends its wait. The two
// things that end a wait earlier are checked absent: the marker is still fresh, and key.pem never
// came.
func TestEnrollWaitGivesUpAtItsBound(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := t.TempDir()
	marker := writePendingMarker(t, keyDir, 0)

	start := time.Now()
	exit, stderr := runExecForm(t, binary, broker.URL, keyDir, "AGENT_SECRETS_ENROLL_WAIT=1s")
	spanned := time.Since(start)
	if exit != 1 || !strings.Contains(stderr, launcherWaitLine("AGENT_SECRETS_KEY_DIR", "1s")) || !strings.Contains(stderr, "AGENT_SECRETS_KEY_DIR holds enrollment.pending and no key.pem yet") ||
		strings.Contains(stderr, keyDir) || strings.Contains(stderr, "no broker identity for this process") {
		t.Fatalf("exit = %d, stderr = %q, want exit 1 saying it waits, naming the launcher's marker and the wait, and no later source", exit, stderr)
	}
	_, said, found := strings.Cut(stderr, "(waited ")
	said, _, bounded := strings.Cut(said, ", its bound 1s, while AGENT_SECRETS_KEY_DIR held enrollment.pending)")
	waited, err := time.ParseDuration(said)
	if !found || !bounded || err != nil {
		t.Fatalf("stderr = %q, want the refusal to say how long the call waited, \"(waited <time>, its bound 1s, while AGENT_SECRETS_KEY_DIR held enrollment.pending)\"", stderr)
	}
	if waited < 1*time.Second {
		t.Fatalf("the call says it waited %s, under its 1s bound: stderr = %q", waited, stderr)
	}
	if spanned < 1*time.Second {
		t.Fatalf("the call ended %s after the row started it, under its 1s bound, though it says it waited %s: stderr = %q", spanned, waited, stderr)
	}
	if !freshMarker(keyDir) {
		t.Fatalf("%s is no longer fresh, so the wait may have ended on the marker's age rather than its bound", marker)
	}
	if _, err := os.Stat(filepath.Join(keyDir, keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat key.pem: %v; nothing writes it in this row", err)
	}
}

// TestEnrollWaitSaysItWaitsBeforeItReadsItsDeadline pins the order of the call's own lines when
// its bound has already passed at its first look (1ns): the call still says that it waits, then
// refuses. A loaded machine can stall a call for longer than its bound before that look, so a call
// that read its deadline before it said so would give up with no first line; this row decides by
// the order the call prints, which no load changes.
func TestEnrollWaitSaysItWaitsBeforeItReadsItsDeadline(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := t.TempDir()
	writePendingMarker(t, keyDir, 0)

	exit, stderr := runExecForm(t, binary, broker.URL, keyDir, "AGENT_SECRETS_ENROLL_WAIT=1ns")
	line := strings.Index(stderr, launcherWaitLine("AGENT_SECRETS_KEY_DIR", "1ns"))
	refusal := strings.Index(stderr, ", its bound 1ns, while AGENT_SECRETS_KEY_DIR held enrollment.pending)")
	if exit != 1 || line < 0 || refusal < line {
		t.Fatalf("exit = %d, stderr = %q, want exit 1, the first wait line, then the refusal", exit, stderr)
	}
}

// TestEnrollWaitNeedsAFreshMarker pins that only the launcher's live marker makes a call wait: an
// empty key dir (a machine session's, a pod's), a marker older than the launcher's own setup bound
// (a launcher killed outright), and a failed enrollment the launcher has already reported
// (enrollment.error, marker removed) all fail at once, as they did before the wait existed. The
// row fails as soon as a call says that it waits. Each call is given a day to wait in, so a call
// that waited without saying so would not come back within the shared wait, which would fail
// naming it.
func TestEnrollWaitNeedsAFreshMarker(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()

	stale := t.TempDir()
	writePendingMarker(t, stale, 200*time.Second)
	failed := newKeyDir(t)
	if err := os.Remove(filepath.Join(failed, "enrollment")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failed, "enrollment.error"), []byte("broker 503 DATABASE: postgres unreachable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, keyDir, want string
	}{
		{"empty key dir", t.TempDir(), "no broker identity for this process"},
		{"stale marker", stale, "no broker identity for this process"},
		{"reported failure", failed, "postgres unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startExecForm(t, binary, broker.URL, tc.keyDir, "AGENT_SECRETS_ENROLL_WAIT=24h")
			testwait.Eventually(t, "the call to end or say that it waits", func() bool {
				return c.ended() || strings.Contains(c.stderr.String(), launcherWaitStart)
			})
			if stderr := c.stderr.String(); strings.Contains(stderr, launcherWaitStart) {
				t.Fatalf("stderr = %q; with no fresh marker the call must not wait", stderr)
			}
			exit, stderr := c.end(t), c.stderr.String()
			if exit == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d, stderr = %q, want a failure naming %q", exit, stderr, tc.want)
			}
		})
	}
}

// TestEnrollWaitRefusesAnInvalidBound pins that a malformed AGENT_SECRETS_ENROLL_WAIT is an
// error naming the variable, never a silent fall back to the default.
func TestEnrollWaitRefusesAnInvalidBound(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	for _, bad := range []string{"soon", "-1s"} {
		exit, stderr := runExecForm(t, binary, broker.URL, newKeyDir(t), "AGENT_SECRETS_ENROLL_WAIT="+bad)
		if exit == 0 || !strings.Contains(stderr, "AGENT_SECRETS_ENROLL_WAIT") {
			t.Fatalf("AGENT_SECRETS_ENROLL_WAIT=%s: exit = %d, stderr = %q, want a refusal naming the variable", bad, exit, stderr)
		}
	}
}

func TestRequestJSONPrintsExactlyOneContractObject(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "request", "GRANT_ME", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	obj := oneJSONObject(t, stdout)
	want := map[string]bool{"request_id": true, "state": true, "secrets": true, "grant_id": true, "record_id": true, "approver": true}
	if got := keySet(obj); !mapsEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if obj["request_id"] != "req-granted" || obj["state"] != "granted" {
		t.Fatalf("request/state = %v/%v, want req-granted/granted", obj["request_id"], obj["state"])
	}
}

// TestRequestJSONIsByteIdenticalToTheBrokerResponse is the regression for the review's Important
// finding 2: --json must print the broker's exact bytes, never appending a newline (or anything
// else) the response didn't already end with.
func TestRequestJSONIsByteIdenticalToTheBrokerResponse(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	const wantExact = `{"request_id":"req-nonewline","state":"granted","secrets":[{"name":"NONEWLINE_ME","decision":"automatic"}],"grant_id":"grant-nonewline","record_id":null,"approver":null}`
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "request", "NONEWLINE_ME", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout != wantExact {
		t.Fatalf("stdout = %q, want byte-identical to the broker's exact response %q", stdout, wantExact)
	}
}

func TestSelfJSONPrintsExactlyOneContractObjectNamingTheIssuedEnrollment(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil, "self", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	obj := oneJSONObject(t, stdout)
	want := map[string]bool{"enrollment_id": true, "kind": true, "operator": true, "lease_expires_at": true, "grants": true}
	if got := keySet(obj); !mapsEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if obj["enrollment_id"] != testEnrollmentID {
		t.Fatalf("enrollment_id = %v, want %q (the id the fake issued)", obj["enrollment_id"], testEnrollmentID)
	}
}

// TestRequestSignsARequestObject pins the broker's request shape: POST /v1/requests
// posts a signed request object (record.Sign, via Signer.SignRequestObject) instead of plain
// top-level "secrets"/"reason"/"issue" fields. The fake broker asserts the body is exactly
// {"request": <jws>, "session_id": null}, verifies the JWS with record.VerifyRequestObject
// against its own URL as audience, and checks the request object's authorization_details name
// exactly the argv secret names with the reason carried inside the signed object rather than as
// a top-level field.
func TestRequestSignsARequestObject(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)

	var mu sync.Mutex
	var gotBody map[string]any
	var gotObj record.RequestObject
	var verifyErr error
	var audience string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		aud := audience
		mu.Unlock()
		compact, _ := body["request"].(string)
		obj, verr := record.VerifyRequestObject(compact, aud, time.Minute, time.Now())
		mu.Lock()
		gotBody, gotObj, verifyErr = body, obj, verr
		mu.Unlock()
		writeJSON(w, map[string]any{
			"request_id": "req-signed", "state": "granted",
			"secrets":  []map[string]string{{"name": "GRANT_ME", "decision": "automatic"}},
			"grant_id": "grant-signed", "record_id": nil,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mu.Lock()
	audience = srv.URL
	mu.Unlock()

	// Every one of requestSessionID's three env vars is cleared: this test's own process may run
	// inside an omp session, whose id the client would otherwise forward as session_id (the
	// behavior TestRequestAndExecFormSendSessionIDWithFallbackOrder pins), and a Claude Code or
	// Envoy-registered session sets the other two the same way.
	stdout, stderr, exit := runAgentSecrets(t, binary, srv.URL, keyDir,
		[]string{"OMP_SESSION_ID=", "ENVOY_SESSION_ID=", "CLAUDE_CODE_SESSION_ID=", "CODEX_THREAD_ID="},
		"request", "GRANT_ME", "--reason", "need it for the build")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	sessionID, present := gotBody["session_id"]
	if !present || sessionID != nil {
		t.Fatalf(`request body["session_id"] = %v (present=%v), want a present null`, sessionID, present)
	}
	if verifyErr != nil {
		t.Fatalf("record.VerifyRequestObject(request): %v", verifyErr)
	}
	if len(gotObj.Details) != 1 || gotObj.Details[0].Type != "agent_secret" || gotObj.Details[0].Identifier != "GRANT_ME" {
		t.Fatalf("authorization_details = %+v, want exactly one agent_secret detail naming GRANT_ME", gotObj.Details)
	}
	if gotObj.Reason != "need it for the build" {
		t.Fatalf("reason = %q, want the --reason text", gotObj.Reason)
	}
}

// fakeLoginHelper serves just the machine-login socket ops (login, login-status) a bare unix
// listener needs to drive cmdMachineLogin's own poll loop, letting each test's states slice fully
// drive the poll loop through however many pending answers it wants before a terminal state.
func fakeLoginHelper(t *testing.T, code string, states []string) string {
	t.Helper()
	return fakeLoginHelperAt(t, code, "", states)
}

// fakeLoginHelperAt is fakeLoginHelper whose login answer names verificationURI.
func fakeLoginHelperAt(t *testing.T, code, verificationURI string, states []string) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var calls int32
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req helper.Request
			_ = json.Unmarshal(line, &req)
			var resp helper.Response
			switch req.Op {
			case "login":
				resp = helper.Response{OK: true, Code: code, LoginState: "pending", VerificationURI: verificationURI}
			case "login-status":
				idx := int(atomic.AddInt32(&calls, 1)) - 1
				state := states[len(states)-1]
				if idx < len(states) {
					state = states[idx]
				}
				resp = helper.Response{OK: true, Code: code, LoginState: state}
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
			conn.Close()
		}
	}()
	return sock
}

// TestMachineLoginPrintsTheCodeAndWaits pins the socket-based machine login: it prints the
// confirmation code and, since AGENT_SECRETS_APPROVE_URL is unset, the generic Dispatch-page
// line, then polls login-status through two pending answers before exiting 0 on "issued".
func TestMachineLoginPrintsTheCodeAndWaits(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at main_test.go:1745).
	t.Setenv("AGENT_SECRETS_APPROVE_URL", "")
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelper(t, "KQ7M-X4PZ", []string{"pending", "pending", "issued"})
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "machine", "login")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	want := "machine login code: KQ7M-X4PZ\nenter it on the Dispatch credential page\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// TestMachineLoginPrintsTheDispatchURLWhenApproveURLIsSet pins the other half of the two-line
// contract: with AGENT_SECRETS_APPROVE_URL set, the second line names it instead of the generic
// "Dispatch credential page" fallback.
func TestMachineLoginPrintsTheDispatchURLWhenApproveURLIsSet(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelper(t, "KQ7M-X4PZ", []string{"issued"})
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_APPROVE_URL=https://dispatch.example/"},
		"machine", "login")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	want := "machine login code: KQ7M-X4PZ\nenter it at https://dispatch.example/credentials/machine (AGENT_SECRETS_APPROVE_URL names its origin) — approve only if the code matches this terminal\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// TestMachineLoginPrintsTheBrokersVerificationURI: with AGENT_SECRETS_APPROVE_URL unset, the
// second line names the page the broker's device authorization named, which the helper relays;
// with it set, that setting still wins.
func TestMachineLoginPrintsTheBrokersVerificationURI(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelperAt(t, "KQ7M-X4PZ", "https://dispatch.example/credentials/machine", []string{"issued"})
	for _, tc := range []struct{ approve, want string }{
		{"", "machine login code: KQ7M-X4PZ\nenter it at https://dispatch.example/credentials/machine — approve only if the code matches this terminal\n"},
		{"https://other.example", "machine login code: KQ7M-X4PZ\nenter it at https://other.example/credentials/machine (AGENT_SECRETS_APPROVE_URL names its origin) — approve only if the code matches this terminal\n"},
	} {
		stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
			[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_APPROVE_URL=" + tc.approve}, "machine", "login")
		if exit != 0 || stdout != tc.want {
			t.Fatalf("AGENT_SECRETS_APPROVE_URL=%q: exit %d stdout %q stderr %q; want %q", tc.approve, exit, stdout, stderr, tc.want)
		}
	}
}

// TestMachineLoginExitsOneOnDenied pins the terminal-failure half of the poll loop: a
// login-status answer of "denied" exits 1 and names the state, never retrying past it.
func TestMachineLoginExitsOneOnDenied(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelper(t, "KQ7M-X4PZ", []string{"denied"})
	_, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "machine", "login")
	if exit != 1 {
		t.Fatalf("exit = %d, want 1: stderr=%q", exit, stderr)
	}
	if !strings.Contains(stderr, "denied") {
		t.Fatalf("stderr = %q, want it to name the denied state", stderr)
	}
}

// TestMachineLoginStatusExitsZeroOnlyWhileACredentialIsHeld pins login-status's read-only,
// single-shot contract: it prints a bare state on stdout and its exit code is a
// liveness probe — 0, printing "issued", while the helper holds a launcher credential, and 1 for
// every login state with none, "none" when login was never run (empty LoginState) — with a single
// helper call, never login's mint-a-fresh-key-and-poll side effect. A re-login still pending or
// expired unapproved beside a held credential prints "issued" and names that login on stderr; a
// helper from before credential_held sends "issued" alone exactly while it holds one. Every held
// credential's answer ends with when it expires, or that the helper cannot say. Every state with
// no credential and no login in flight (never logged in, denied, or expired, which is also what a
// credential the broker rejected becomes) says on stderr to run the login again, and a login in
// flight outranks a refused credential.
func TestMachineLoginStatusExitsZeroOnlyWhileACredentialIsHeld(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	const held = "the helper still holds the launcher credential an earlier login issued"
	const prefix = "agent-secrets machine login-status: "
	const unknown = prefix + "the helper does not know when the launcher credential expires (it, or its broker, is older than this client)\n"
	expiresAt := time.Now().Add(50 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name   string
		resp   helper.Response
		want   string
		exit   int
		remedy bool
		stderr string // the whole of stderr, when the case pins it
	}{
		{name: "issued", resp: helper.Response{LoginState: "issued"}, want: "issued\n", stderr: unknown},
		{name: "issued with its expiry", resp: helper.Response{LoginState: "issued", CredentialHeld: true, CredentialExpiresAt: expiresAt}, want: "issued\n",
			stderr: prefix + "the machine login renews itself; without a refresh it lapses at " + expiresAt + " (in 2d1h59m)\n"},
		{name: "pending", resp: helper.Response{LoginState: "pending"}, want: "pending\n", exit: 1},
		{name: "denied", resp: helper.Response{LoginState: "denied"}, want: "denied\n", exit: 1, remedy: true},
		{name: "expired", resp: helper.Response{LoginState: "expired"}, want: "expired\n", exit: 1, remedy: true},
		{name: "none", resp: helper.Response{}, want: "none\n", exit: 1, remedy: true,
			stderr: prefix + "no machine login since the helper started, and none kept on this machine; run: agent-secrets machine login, and have a human approve it\n"},
		{name: "none, in identity mode", resp: helper.Response{MachineIdentity: true}, want: "none\n", exit: 1,
			stderr: prefix + "no machine login since the helper started, and none kept on this machine; the helper's trade loop is trading this machine's identity for a new credential\n"},
		{name: "dropped at its expiry, in identity mode", resp: helper.Response{LoginState: "expired", LoginRefused: true, CredentialDropped: "the launcher credential reached its expiry", MachineIdentity: true},
			want: "expired\n", exit: 1,
			stderr: prefix + "the launcher credential reached its expiry; the helper's trade loop is trading this machine's identity for a new credential\n"},
		{name: "a live credential, the trade loop stopped", resp: helper.Response{LoginState: "issued", CredentialHeld: true, CredentialExpiresAt: expiresAt, MachineIdentity: true, IdentityStopped: true},
			want: "issued\n",
			stderr: prefix + "nothing renews the launcher credential, which lapses at " + expiresAt + " (in 2d1h59m), and no session enrols under this machine's credential after that; " +
				"the helper stopped trading this machine's identity, and its log gives the reason; restart agent-secrets-helper once that is fixed\n"},
		{name: "a live credential, the trade loop running", resp: helper.Response{LoginState: "issued", CredentialHeld: true, CredentialExpiresAt: expiresAt, MachineIdentity: true},
			want:   "issued\n",
			stderr: prefix + "the helper's trade loop renews the launcher credential; without a trade it lapses at " + expiresAt + " (in 2d1h59m)\n"},
		{name: "dropped, the trade loop stopped", resp: helper.Response{LoginState: "expired", LoginRefused: true, CredentialDropped: "the launcher credential reached its expiry", MachineIdentity: true, IdentityStopped: true},
			want: "expired\n", exit: 1,
			stderr: prefix + "the launcher credential reached its expiry; the helper stopped trading this machine's identity, and its log gives the reason; restart agent-secrets-helper once that is fixed\n"},
		{name: "pending beside a refused credential", resp: helper.Response{LoginState: "pending", LoginRefused: true}, want: "pending\n", exit: 1},
		{name: "pending beside a held credential", resp: helper.Response{LoginState: "pending", CredentialHeld: true}, want: "issued\n",
			stderr: prefix + "a machine login is waiting for approval (code KQ7M-X4PZ); " + held + "\n" + unknown},
		{name: "expired beside a held credential", resp: helper.Response{LoginState: "expired", CredentialHeld: true}, want: "issued\n",
			stderr: prefix + "the most recent machine login (code KQ7M-X4PZ) expired before anyone approved it; " + held + "\n" + unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.resp.OK, tc.resp.Code = true, "KQ7M-X4PZ"
			sock, reqs := fakeHelper(t, tc.resp)
			stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
				[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "machine", "login-status")
			if exit != tc.exit || stdout != tc.want {
				t.Fatalf("exit = %d stdout = %q, want exit %d stdout %q (stderr=%q)", exit, stdout, tc.exit, tc.want, stderr)
			}
			if got := strings.Contains(stderr, "run: agent-secrets machine login"); got != tc.remedy {
				t.Fatalf("stderr = %q, want the login remedy: %v", stderr, tc.remedy)
			}
			if tc.stderr != "" && stderr != tc.stderr {
				t.Fatalf("stderr = %q, want %q", stderr, tc.stderr)
			}
			if req := <-reqs; req.Op != "login-status" || len(reqs) != 0 {
				t.Fatalf("helper calls: first %q, %d more; want one login-status", req.Op, len(reqs))
			}
		})
	}
}

// TestCredentialExpiryLine: login-status's expiry line rounds to the minute with days, names a
// credential already past its expiry (the helper has not dropped it yet) with the hint of the
// helper's mode, and says plainly when the helper's answer carries no expiry or one it cannot read.
func TestCredentialExpiryLine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	machineLogin, identity := cipherclient.FailureNoMachineLogin, cipherclient.FailureIdentityLapsed
	for _, tc := range []struct {
		expiresAt string
		failure   cipherclient.Failure
		want      string
	}{
		{"2026-10-10T11:59:30Z", machineLogin, "the machine login renews itself; without a refresh it lapses at 2026-10-10T11:59:30Z (in 6d23h59m)"},
		{"2026-10-10T11:59:30Z", identity, "the helper's trade loop renews the launcher credential; without a trade it lapses at 2026-10-10T11:59:30Z (in 6d23h59m)"},
		{"2026-10-10T11:59:30Z", cipherclient.FailureIdentityStopped, "nothing renews the launcher credential, which lapses at 2026-10-10T11:59:30Z (in 6d23h59m), and no session enrols under this machine's credential after that; the helper stopped trading this machine's identity, and its log gives the reason; restart agent-secrets-helper once that is fixed"},
		{"2026-10-03T12:00:00Z", cipherclient.FailureIdentityStopped, "the launcher credential expired at 2026-10-03T12:00:00Z; the helper stopped trading this machine's identity, and its log gives the reason; restart agent-secrets-helper once that is fixed"},
		{"2026-10-03T15:05:00Z", machineLogin, "the machine login renews itself; without a refresh it lapses at 2026-10-03T15:05:00Z (in 3h5m)"},
		{"2026-10-03T12:00:40Z", machineLogin, "the machine login renews itself; without a refresh it lapses at 2026-10-03T12:00:40Z (in 0m)"},
		{"2026-10-03T12:00:00Z", machineLogin, "the launcher credential expired at 2026-10-03T12:00:00Z; run: agent-secrets machine login, and have a human approve it"},
		{"2026-10-03T12:00:00Z", identity, "the launcher credential expired at 2026-10-03T12:00:00Z; the helper's trade loop is trading this machine's identity for a new credential"},
		{"", machineLogin, "the helper does not know when the launcher credential expires (it, or its broker, is older than this client)"},
		{"next tuesday", machineLogin, `the helper reported an unreadable launcher credential expiry "next tuesday"`},
	} {
		if got := credentialExpiry(tc.expiresAt, now, tc.failure); got != tc.want {
			t.Errorf("credentialExpiry(%q, %v) = %q, want %q", tc.expiresAt, tc.failure, got, tc.want)
		}
	}
}

// TestEnrollHelperPrintsEnrollmentID pins enroll --helper's box contract: it asks the local
// agent-secrets-helper over its unix socket (never the broker directly), writes the returned
// enrollment id into AGENT_SECRETS_KEY_DIR/enrollment exactly as file mode's own buildSigner
// reads it back, and prints the enrollment id alone on stdout — the contract scripts/agentbox
// parses.
func TestEnrollHelperPrintsEnrollmentID(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, EnrollmentID: "enr-box-1", LeaseExpires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	keyDir := t.TempDir()
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir,
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
		"enroll", "--helper", "--runtime-id", "box-1", "--thumbprint", "tp-box")
	if exit != 0 {
		t.Fatalf("enroll --helper exit = %d: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout != "enr-box-1\n" {
		t.Fatalf("stdout = %q, want the bare enrollment id", stdout)
	}
	data, err := os.ReadFile(filepath.Join(keyDir, "enrollment"))
	if err != nil || strings.TrimSpace(string(data)) != "enr-box-1" {
		t.Fatalf("enrollment file: err=%v content=%q", err, string(data))
	}
}

// TestUnenrollHelperSucceeds pins unenroll --helper's exit-code-only contract: it asks the local
// helper to revoke the enrollment over its unix socket and prints nothing on success.
func TestUnenrollHelperSucceeds(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true})
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
		[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, "unenroll", "--helper", "--enrollment", "enr-box-1")
	if exit != 0 {
		t.Fatalf("unenroll --helper exit = %d: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing on success", stdout)
	}
}

// TestEnrollHelperFlagValidation pins enroll/unenroll's remaining validation now that contract
// v9 dropped launcher bearer tokens entirely: --helper is the only enrollment path left, so it
// is required (not merely one of two mutually exclusive options).
func TestEnrollHelperFlagValidation(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	keyDir := t.TempDir()

	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, nil,
		"enroll", "--runtime-id", "box-1", "--thumbprint", "tp-box"); exit != exitUsageError {
		t.Fatalf("enroll without --helper exit = %d, want a usage error: %s", exit, stderr)
	}
	if _, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, nil,
		"unenroll", "--enrollment", "enr-1"); exit != exitUsageError {
		t.Fatalf("unenroll without --helper exit = %d, want a usage error: %s", exit, stderr)
	}
}

// TestRequestAndExecFormSendSessionIDWithFallbackOrder is the regression for the review's finding
// I3: POST /v1/requests carries session_id so the broker can wake this machine session directly
// once its request is decided (machine session enrollments carry no session_id of their own) — both call
// sites, cmdRequest and cmdExec, must forward requestSessionID()'s answer rather than leaving it
// "" — and for LEGION-587's fallback order: with OMP_SESSION_ID unset, the request form still
// sends ENVOY_SESSION_ID. requestSessionID's own three-way fallback order is pinned at the unit
// level by TestRequestSessionIDFallbackOrder, below; this test is the proof that both real call
// sites actually call it rather than os.Getenv("OMP_SESSION_ID") directly.
func TestRequestAndExecFormSendSessionIDWithFallbackOrder(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	broker, counters := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)
	clearFallbacks := []string{"ENVOY_SESSION_ID=", "CLAUDE_CODE_SESSION_ID=", "CODEX_THREAD_ID="}

	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir,
		append([]string{"OMP_SESSION_ID=sess-request-123"}, clearFallbacks...), "request", "GRANT_ME")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if got := counters.sessionID(); got != "sess-request-123" {
		t.Fatalf("request form session_id = %q, want %q", got, "sess-request-123")
	}

	stdout, stderr, exit = runAgentSecrets(t, binary, broker.URL, keyDir,
		append([]string{"OMP_SESSION_ID=sess-exec-456"}, clearFallbacks...), "GRANT_ME", "--", "true")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if got := counters.sessionID(); got != "sess-exec-456" {
		t.Fatalf("exec form session_id = %q, want %q", got, "sess-exec-456")
	}

	// OMP_SESSION_ID unset: the request form falls back to ENVOY_SESSION_ID.
	stdout, stderr, exit = runAgentSecrets(t, binary, broker.URL, keyDir,
		[]string{"OMP_SESSION_ID=", "ENVOY_SESSION_ID=sess-envoy-789", "CLAUDE_CODE_SESSION_ID=", "CODEX_THREAD_ID="},
		"request", "GRANT_ME")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if got := counters.sessionID(); got != "sess-envoy-789" {
		t.Fatalf("request form fallback session_id = %q, want %q", got, "sess-envoy-789")
	}
}

// TestRequestSessionIDFallbackOrder pins requestSessionID's own fallback order (LEGION-587):
// OMP_SESSION_ID first, then ENVOY_SESSION_ID, then CLAUDE_CODE_SESSION_ID, empty when none is
// set. This process's own environment may already carry one of the three (a Pi session sets its
// own OMP_SESSION_ID), so every case sets all three explicitly rather than relying on any being
// unset already.
// TestRequestSessionIDCodex pins where CODEX_THREAD_ID stands: in Claude Code's place,
// and, when both are set, the nearer of the two harness processes decides.
func TestRequestSessionIDCodex(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at main_test.go:2048); it sets a package variable the package's other tests read (nearestHarness set at main_test.go:2053).
	cases := []struct {
		name          string
		claude, codex string
		nearest       string
		omp           string
		want          string
	}{
		{"CODEX_THREAD_ID alone", "", "thread-codex", "", "", "thread-codex"},
		{"Codex nearer than Claude Code", "sess-claude", "thread-codex", "codex", "", "thread-codex"},
		{"Claude Code nearer than Codex", "sess-claude", "thread-codex", "claude", "", "sess-claude"},
		{"neither found: Claude Code, as before", "sess-claude", "thread-codex", "", "", "sess-claude"},
		{"OMP_SESSION_ID when Oh My Pi is nearer", "", "thread-codex", "omp", "sess-omp", "sess-omp"},
		{"CODEX_THREAD_ID when Codex is nearer than Oh My Pi", "", "thread-codex", "codex", "sess-omp", "thread-codex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("OMP_SESSION_ID", c.omp)
			t.Setenv("ENVOY_SESSION_ID", "")
			t.Setenv("CLAUDE_CODE_SESSION_ID", c.claude)
			t.Setenv("CODEX_THREAD_ID", c.codex)
			saved := nearestHarness
			nearestHarness = func(...string) string { return c.nearest }
			t.Cleanup(func() { nearestHarness = saved })
			if got := requestSessionID(); got != c.want {
				t.Fatalf("requestSessionID() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRequestSessionIDFallbackOrder(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at main_test.go:2077).
	cases := []struct {
		name               string
		omp, envoy, claude string
		want               string
	}{
		{"OMP_SESSION_ID alone", "sess-omp", "", "", "sess-omp"},
		{"OMP_SESSION_ID wins over the others", "sess-omp", "sess-envoy", "sess-claude", "sess-omp"},
		{"ENVOY_SESSION_ID when OMP_SESSION_ID is unset", "", "sess-envoy", "sess-claude", "sess-envoy"},
		{"CLAUDE_CODE_SESSION_ID when the first two are unset", "", "", "sess-claude", "sess-claude"},
		{"empty when none is set", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("OMP_SESSION_ID", c.omp)
			t.Setenv("ENVOY_SESSION_ID", c.envoy)
			t.Setenv("CLAUDE_CODE_SESSION_ID", c.claude)
			t.Setenv("CODEX_THREAD_ID", "")
			if got := requestSessionID(); got != c.want {
				t.Fatalf("requestSessionID() = %q, want %q", got, c.want)
			}
		})
	}
}
