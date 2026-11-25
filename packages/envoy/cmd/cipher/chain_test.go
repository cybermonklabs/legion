// packages/envoy/cmd/cipher/chain_test.go
//
// The spec's R4: `agent-secrets NAME -- command` takes its broker identity from the
// first of four sources that has one — a launcher's key directory, a person's sign-in, the helper
// for a caller inside a session it registered, and the machine's own cloud identity found with no
// setting — skipping a source that has nothing for the caller and stopping on one that is present
// but broken. These tests run the built binary in an environment of their own (chainRig), never
// the one the test inherited, against a broker (chainBroker) and a metadata server
// (startMetadataServer) of their own, so what they read is what the binary did: which exchange
// the broker saw, which enrollment signed the request, and what is on disk afterwards.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testport"
)

// chainValue is the value chainBroker releases for TEST_KEY. The use command's child prints its
// length, so no test output carries it.
const chainValue = "chain-test-value"

// chainBroker is the broker routes a use command calls: its settings, the token exchange a
// machine trades its identity at, the enrollment it then registers, the request and the release.
// It records every exchange's form, counts the enrollments and keeps the key thumbprint each was
// registered for, and keeps the enrollment each request's session proof named. As the real broker
// does, it refuses a request whose enrollment it ended (401 ENROLLMENT_NOT_LIVE) and one whose
// signed request object is not that enrollment's own key (400 REQUEST_INVALID).
type chainBroker struct {
	server *httptest.Server

	mu            sync.Mutex
	settings      map[string]any
	settingsDelay time.Duration
	refuse        bool
	exchanges     []url.Values
	enrollments   int
	thumbprints   map[string]string
	ended         map[string]bool
	signedBy      []string
	calls         int
	// proofInvalid makes an ended enrollment's refusal the legacy `PROOF_INVALID` "proof
	// invalid: not live" the production broker answered on 2026-11-22, in place of
	// ENROLLMENT_NOT_LIVE.
	proofInvalid bool
	// selfReads counts GET /v1/enrollments/self, the read a sign-in this machine cannot renew is
	// checked by.
	selfReads int
}

// writeNotLive answers the refusal of a proof that names an enrollment b ended.
func (b *chainBroker) writeNotLive(w http.ResponseWriter) {
	b.mu.Lock()
	legacy := b.proofInvalid
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	if legacy {
		_, _ = w.Write([]byte(`{"code":"PROOF_INVALID","error":"proof invalid: not live"}`))
		return
	}
	_, _ = w.Write([]byte(`{"code":"ENROLLMENT_NOT_LIVE","error":"this enrollment is not live"}`))
}

// answerNotLiveAsProofInvalid makes every refusal of an ended enrollment from now on the legacy
// PROOF_INVALID one (chainBroker.proofInvalid).
func (b *chainBroker) answerNotLiveAsProofInvalid() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.proofInvalid = true
}

// pinKey records thumbprint as the key enrollmentID was registered for, as the broker records a
// sign-in's box key: GET /v1/enrollments/self then verifies its proof against that key, and POST
// /v1/requests its request object, as the real broker does.
func (b *chainBroker) pinKey(enrollmentID, thumbprint string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.thumbprints[enrollmentID] = thumbprint
}

// selfReadCount is how many GET /v1/enrollments/self the broker answered.
func (b *chainBroker) selfReadCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selfReads
}

// storeSettings is GET /v1/settings of a broker on store ("aws" or "gcp"), naming google as its
// Google exchange audience where it is not empty.
func storeSettings(store, google string) map[string]any {
	settings := map[string]any{}
	switch store {
	case "aws":
		settings = map[string]any{"store": secrets.StoreAWS, "secrets_prefix": policytest.Prefix, "kms_key_arn": policytest.KeyARN,
			"aws_account_id": testAccount, "aws_region": "us-east-1"}
	case "gcp":
		settings = map[string]any{"store": secrets.StoreGCP, "secrets_prefix": testGCPPrefix, "kms_key_name": testKMSKeyName,
			"gcp_project": testGCPProject}
	}
	if google != "" {
		settings["google_exchange_audience"] = google
	}
	return settings
}

func newChainBroker(t *testing.T, settings map[string]any) *chainBroker {
	t.Helper()
	b := &chainBroker{settings: settings, thumbprints: map[string]string{}, ended: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		settings, delay := maps.Clone(b.settings), b.settingsDelay
		b.mu.Unlock()
		time.Sleep(delay)
		writeJSON(w, settings)
	})
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "the exchange did not parse", http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		b.exchanges = append(b.exchanges, r.PostForm)
		n, refuse := len(b.exchanges), b.refuse
		b.mu.Unlock()
		if refuse {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"no exchange entry lists this machine"}`))
			return
		}
		writeJSON(w, map[string]any{
			"access_token": fmt.Sprintf("access-token-%d", n), "token_type": "DPoP", "expires_in": 3600,
			"credential_id": fmt.Sprintf("credential-%d", n), "credential_expires_at": "2099-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Thumbprint string `json:"thumbprint"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.mu.Lock()
		b.enrollments++
		id := fmt.Sprintf("machine-enrollment-%d", b.enrollments)
		b.thumbprints[id] = body.Thumbprint
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"enrollment_id": id})
	})
	mux.HandleFunc("GET /v1/enrollments/self", func(w http.ResponseWriter, r *http.Request) {
		eid := proofEID(r)
		b.mu.Lock()
		b.selfReads++
		ended, thumbprint := b.ended[eid], b.thumbprints[eid]
		b.mu.Unlock()
		// An enrollment whose key is on record is held to it, signature, method and URL, as
		// proof.Verifier holds it; a fixture's enrollment with no key on record is not.
		if thumbprint != "" {
			verifier := &proof.Verifier{Skew: time.Minute,
				Lookup: func(context.Context, string) (string, bool, error) { return thumbprint, true, nil },
				Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil }}
			if _, err := verifier.Verify(r.Context(), r.Header.Get("Proof"), http.MethodGet, b.URL()+"/v1/enrollments/self", time.Now()); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "PROOF_INVALID", "error": err.Error()})
				return
			}
		}
		if ended {
			b.writeNotLive(w)
			return
		}
		writeJSON(w, map[string]any{"enrollment_id": eid, "kind": "session", "lease_expires_at": time.Now().Add(time.Hour), "grants": []any{}})
	})
	mux.HandleFunc("POST /v1/enrollments/{id}/renew", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		ended := b.ended[r.PathValue("id")]
		b.mu.Unlock()
		if ended {
			b.writeNotLive(w)
			return
		}
		writeJSON(w, map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		eid := proofEID(r)
		var body struct {
			Request string `json:"request"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.mu.Lock()
		b.signedBy = append(b.signedBy, eid)
		ended, thumbprint := b.ended[eid], b.thumbprints[eid]
		b.mu.Unlock()
		if ended {
			b.writeNotLive(w)
			return
		}
		// An enrollment registered here has its key on record, and the request object must be
		// signed by it (requests.Machine.Create's check); a fixture's enrollment has none.
		if thumbprint != "" {
			object, err := record.VerifyRequestObject(body.Request, b.URL(), time.Minute, time.Now())
			if err != nil || object.Thumbprint != thumbprint {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":"REQUEST_INVALID","error":"iss is not this enrollment's own key"}`))
				return
			}
		}
		writeJSON(w, map[string]any{
			"request_id": "req-chain", "state": "granted",
			"secrets":  []map[string]string{{"name": "TEST_KEY", "decision": "automatic"}},
			"grant_id": "grant-chain", "record_id": nil, "approver": nil,
		})
	})
	mux.HandleFunc("POST /v1/grants/grant-chain/values", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"values": map[string]string{"TEST_KEY": chainValue}, "expires_at": time.Now().Add(time.Hour)})
	})
	// The handlers read b.server (b.URL) on the server's goroutines, and the request that reaches
	// them comes from another process, which orders nothing in this one. So b.server is set before
	// the server serves: built unstarted, assigned, then started (Start sets its URL before it
	// serves).
	b.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls++
		b.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	b.server.Start()
	t.Cleanup(b.server.Close)
	return b
}

func (b *chainBroker) URL() string { return b.server.URL }

// refuseExchanges makes every token exchange from now on answer 400 invalid_grant, as a broker
// whose entries list no such machine does.
func (b *chainBroker) refuseExchanges() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuse = true
}

// setSettings changes what GET /v1/settings answers from now on.
func (b *chainBroker) setSettings(settings map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settings = settings
}

// delaySettings makes every GET /v1/settings from now on answer after delay.
func (b *chainBroker) delaySettings(delay time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settingsDelay = delay
}

// endEnrollment makes every request signed by enrollmentID from now on answer 401
// ENROLLMENT_NOT_LIVE, as the broker answers once a session's lease lapsed.
func (b *chainBroker) endEnrollment(enrollmentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ended[enrollmentID] = true
}

// thumbprintOf is the key thumbprint enrollmentID was registered for here, "" for one it never
// registered.
func (b *chainBroker) thumbprintOf(enrollmentID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.thumbprints[enrollmentID]
}

func (b *chainBroker) exchanged() []url.Values {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.exchanges)
}

func (b *chainBroker) enrolled() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enrollments
}

func (b *chainBroker) requestsSignedBy() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.signedBy)
}

func (b *chainBroker) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// proofEID is the enrollment a request's session proof names (its `eid` claim), "" where it
// carries none.
func proofEID(r *http.Request) string {
	parts := strings.Split(r.Header.Get("Proof"), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		EnrollmentID string `json:"eid"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.EnrollmentID
}

// totalRequests is every request the metadata server answered, on any path.
func (m *metadataServer) totalRequests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, count := range m.requests {
		n += count
	}
	return n
}

// metadataIdentityRequests is how many identity tokens the metadata server minted.
func (m *metadataServer) metadataIdentityRequests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests["/computeMetadata/v1/instance/service-accounts/default/identity"]
}

// chainBinary builds agent-secrets.
func chainBinary(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "agent-secrets"})
}

// writeProjectedToken writes a projected service-account token minted for aud at path.
func writeProjectedToken(t *testing.T, path string, aud ...string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"aud": aud, "sub": "system:serviceaccount:legion:worker"})
	if err != nil {
		t.Fatal(err)
	}
	jwt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
	if err := os.WriteFile(path, []byte(jwt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// chainRig is one process's world: a home, a runtime directory and a state directory of its own,
// PATH, the metadata server's address (nothing listening, unless a row starts one), and the broker
// it is told of. Nothing of the environment the test inherited reaches the binary.
type chainRig struct {
	t                    *testing.T
	binary               string
	home, runtime, state string
	env                  map[string]string
}

func newChainRig(t *testing.T, binary string, broker *chainBroker) *chainRig {
	t.Helper()
	r := &chainRig{t: t, binary: binary, home: t.TempDir(), runtime: shortTempDir(t), state: t.TempDir()}
	r.env = map[string]string{
		"HOME": r.home, "XDG_RUNTIME_DIR": r.runtime, "XDG_STATE_HOME": r.state, "PATH": "/usr/bin:/bin",
		// A metadata server that is not there: a port the test holds and nothing listens on
		// (testport.Refused). The held socket is close-on-exec, so the binary the rig runs does not
		// inherit it, and no other process on the machine can take the port and answer.
		"GCE_METADATA_HOST": testport.Refused(t),
	}
	if broker != nil {
		r.env["AGENT_SECRETS_URL"] = broker.URL()
	}
	return r
}

// withMetadata starts a metadata server for this rig and points GCE_METADATA_HOST at it.
func (r *chainRig) withMetadata() *metadataServer {
	r.t.Helper()
	m := startMetadataServer(r.t, "machine@example.iam.gserviceaccount.com")
	r.env["GCE_METADATA_HOST"] = os.Getenv("GCE_METADATA_HOST")
	return m
}

func (r *chainRig) keyDir() string       { return filepath.Join(r.runtime, "agent-secrets") }
func (r *chainRig) machineCache() string { return filepath.Join(r.state, "agent-secrets", "machine") }

// getenv reads the rig's environment as os.Getenv reads the process's, for the helpers that name
// a default path from one (helper.DefaultSocket, helper.UnitPath).
func (r *chainRig) getenv(name string) string { return r.env[name] }

// command is the binary with args, in the rig's environment and no other.
func (r *chainRig) command(args ...string) *exec.Cmd {
	cmd := exec.Command(r.binary, args...)
	cmd.Env = nil
	for _, name := range slices.Sorted(maps.Keys(r.env)) {
		cmd.Env = append(cmd.Env, name+"="+r.env[name])
	}
	return cmd
}

// run runs the binary with args in the rig's environment.
func (r *chainRig) run(args ...string) (code int, stdout, stderr string) {
	r.t.Helper()
	cmd := r.command(args...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			r.t.Fatalf("run agent-secrets %v: %v", args, err)
		}
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// useArgs is the use command whose child prints the length of the value it was given.
var useArgs = []string{"TEST_KEY", "--wait", "0s", "--", "sh", "-c", "echo ${#TEST_KEY}"}

// use runs useArgs.
func (r *chainRig) use() (code int, stdout, stderr string) {
	r.t.Helper()
	return r.run(useArgs...)
}

// mustUse is use, failing the test unless the child ran with the released value, and answers the
// command's stderr.
func (r *chainRig) mustUse() string {
	r.t.Helper()
	code, stdout, stderr := r.use()
	if code != 0 || strings.TrimSpace(stdout) != fmt.Sprint(len(chainValue)) {
		r.t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want exit 0 and the value's length %d", code, stdout, stderr, len(chainValue))
	}
	return stderr
}

// assertInjectedFromStore fails t unless stderr holds the use command's injection line naming the
// kind of store a broker on store ("aws" or "gcp") keeps its secrets in. That line is the use
// path's one per-store branch (source.withSettings), so a counted `aws` or `gcp` row that checks
// it runs its own backend's code rather than the other's.
func assertInjectedFromStore(t *testing.T, store, stderr string) {
	t.Helper()
	kind := map[string]string{"aws": "AWS Secrets Manager", "gcp": "GCP Secret Manager"}[store]
	if want := "agent-secrets: TEST_KEY from the broker AGENT_SECRETS_URL names (" + kind + ")\n"; kind == "" || !strings.Contains(stderr, want) {
		t.Fatalf("stderr %q; want the injection line %q", stderr, want)
	}
}

// writeMachineCacheFixture keeps a machine session for enrollmentID in the rig's machine cache,
// as an earlier use would have, and answers its key.pem bytes.
func (r *chainRig) writeMachineCacheFixture(enrollmentID string) []byte {
	r.t.Helper()
	return writeSessionFiles(r.t, r.machineCache(), enrollmentID)
}

// writeSessionFiles writes a fresh key as key.pem and enrollmentID as enrollment into dir, in
// the modes this client keeps them in, and answers the key.pem bytes.
func writeSessionFiles(t *testing.T, dir, enrollmentID string) []byte {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := encodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enrollment"), []byte(enrollmentID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pemBytes
}

// assertMachineCache fails the test unless the rig's machine cache holds key.pem and enrollment
// in mode 0600 in a 0700 directory.
func (r *chainRig) assertMachineCache() {
	r.t.Helper()
	for path, want := range map[string]os.FileMode{
		r.machineCache(): 0o700,
		filepath.Join(r.machineCache(), "key.pem"):    0o600,
		filepath.Join(r.machineCache(), "enrollment"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			r.t.Fatalf("the machine cache: %v", err)
		}
		if info.Mode().Perm() != want {
			r.t.Fatalf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

// machineExchanges is every exchange broker saw that traded a machine's own Google identity token
// as its subject.
func machineExchanges(broker *chainBroker) []url.Values {
	var out []url.Values
	for _, form := range broker.exchanged() {
		if form.Get("subject_token_type") == oauthwire.SubjectTokenTypeGoogle {
			out = append(out, form)
		}
	}
	return out
}

// TestAUnitWithNoEnvironmentReadsTheConfigFile is Review Focus 3: a systemd unit or a cron job
// starts with no AGENT_SECRETS_* variable at all, and the use command still finds the broker in
// ~/.config/agent-secrets/config and trades the machine's own identity there. A variable that is
// set wins over the file.
func TestAUnitWithNoEnvironmentReadsTheConfigFile(t *testing.T) {
	binary := chainBinary(t)
	fromFile := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	r := newChainRig(t, binary, nil)
	r.withMetadata()
	config := filepath.Join(r.home, ".config", "agent-secrets", "config")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("# this machine's broker\nAGENT_SECRETS_URL="+fromFile.URL()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.mustUse()
	if got := machineExchanges(fromFile); len(got) != 1 || got[0].Get("subject_token") != "metadata-identity-token" {
		t.Fatalf("the file's broker saw the machine exchanges %v; want one, trading the metadata server's identity token", got)
	}

	fromEnvironment := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	second := newChainRig(t, binary, fromEnvironment)
	second.env["GCE_METADATA_HOST"] = r.env["GCE_METADATA_HOST"]
	second.home = r.home
	second.env["HOME"] = r.home
	calls := fromFile.callCount()
	second.mustUse()
	if fromEnvironment.callCount() == 0 || len(machineExchanges(fromEnvironment)) != 1 {
		t.Fatalf("the broker AGENT_SECRETS_URL names saw %d calls and %d machine exchanges; want the use command's calls", fromEnvironment.callCount(), len(machineExchanges(fromEnvironment)))
	}
	if got := fromFile.callCount(); got != calls {
		t.Fatalf("the file's broker saw %d more calls; the variable wins over the file", got-calls)
	}
}

// TestUseCommandOutsideASessionReachesTheMachineSource is Review Focus 2: a process that inherited
// AGENT_SECRETS_HELPER_SOCK but is in no session the helper registered is skipped by the helper
// and reaches the machine, which trades its own identity, registers one enrollment and keeps it in
// the machine cache. Each backend's row reads the injection line its own store names.
func TestUseCommandOutsideASessionReachesTheMachineSource(t *testing.T) {
	binary := chainBinary(t)
	for _, store := range []string{"aws", "gcp"} {
		t.Run(store, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings(store, "agent-secrets-vm"))
			r := newChainRig(t, binary, broker)
			r.withMetadata()
			sock, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock

			assertInjectedFromStore(t, store, r.mustUse())
			if got := machineExchanges(broker); len(got) != 1 {
				t.Fatalf("the broker saw the machine exchanges %v; want one", got)
			}
			if broker.enrolled() != 1 {
				t.Fatalf("the broker saw %d enrollments; want one", broker.enrolled())
			}
			if signed := broker.requestsSignedBy(); len(signed) != 1 || signed[0] != "machine-enrollment-1" {
				t.Fatalf("the request was signed by %v; want the machine's enrollment", signed)
			}
			r.assertMachineCache()
		})
	}
}

// TestAnOlderBrokerWithoutExchangeAudiencesSkipsTheMachineSource: a broker that names no exchange
// audience accepts no machine identity, so the machine source is skipped with that reason and the
// metadata server is never asked.
func TestAnOlderBrokerWithoutExchangeAudiencesSkipsTheMachineSource(t *testing.T) {
	binary := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", ""))
	r := newChainRig(t, binary, broker)
	metadata := r.withMetadata()
	code, stdout, stderr := r.use()
	if code != 1 || stdout != "" {
		t.Fatalf("exit %d, stdout %q; want exit 1 and nothing run (stderr %q)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "machine: the broker at") || !strings.Contains(stderr, "names no exchange audience") {
		t.Fatalf("stderr %q; want the machine source skipped because the broker names no exchange audience", stderr)
	}
	if n := metadata.totalRequests(); n != 0 {
		t.Fatalf("the metadata server saw %d requests; want none", n)
	}
}

// writeConfigFile writes settings into the rig's ~/.config/agent-secrets/config, one KEY=value
// line each.
func (r *chainRig) writeConfigFile(settings map[string]string) {
	r.t.Helper()
	config := filepath.Join(r.home, ".config", "agent-secrets", "config")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		r.t.Fatal(err)
	}
	var lines string
	for _, name := range slices.Sorted(maps.Keys(settings)) {
		lines += name + "=" + settings[name] + "\n"
	}
	if err := os.WriteFile(config, []byte(lines), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// TestAForcedMachineIdentityIsTheOnlyMachineSource: AGENT_SECRETS_MACHINE_IDENTITY names the one
// identity the machine source trades, from the environment or the config file, and detection is
// not tried: a broker that names no exchange audience, which detection would skip, still sees the
// forced identity's trade. A machine session already in the cache signs before either.
func TestAForcedMachineIdentityIsTheOnlyMachineSource(t *testing.T) {
	binary := chainBinary(t)
	for _, row := range []string{"the environment", "the config file"} {
		t.Run(row, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings("gcp", ""))
			r := newChainRig(t, binary, broker)
			metadata := r.withMetadata()
			settings := map[string]string{"AGENT_SECRETS_MACHINE_IDENTITY": "metadata", "AGENT_SECRETS_MACHINE_AUDIENCE": "forced-audience"}
			if row == "the environment" {
				maps.Copy(r.env, settings)
			} else {
				r.writeConfigFile(settings)
			}

			r.mustUse()
			exchanges := broker.exchanged()
			if len(exchanges) != 1 || exchanges[0].Get("subject_token_type") != oauthwire.SubjectTokenTypeGoogle {
				t.Fatalf("the broker saw the exchanges %v; want one trading the identity token the setting names", exchanges)
			}
			if n := metadata.metadataIdentityRequests(); n != 1 {
				t.Fatalf("the metadata server minted %d identity tokens; want the forced identity's one", n)
			}
		})
	}

	t.Run("a cached machine session", func(t *testing.T) {
		broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
		r := newChainRig(t, binary, broker)
		r.writeMachineCacheFixture("cached-enrollment")
		r.env["AGENT_SECRETS_MACHINE_IDENTITY"] = "metadata"
		r.env["AGENT_SECRETS_MACHINE_AUDIENCE"] = "forced-audience"
		r.mustUse()
		if got := broker.exchanged(); len(got) != 0 {
			t.Fatalf("the broker saw the exchanges %v; a cached session signs and trades nothing", got)
		}
		if signed := broker.requestsSignedBy(); len(signed) != 1 || signed[0] != "cached-enrollment" {
			t.Fatalf("the request was signed by %v; want the cached enrollment", signed)
		}
	})
}

// TestAKubernetesMachineIdentityIsRefusedBeforeAnyTrade: the broker mints no launcher credential
// for a Kubernetes identity, so AGENT_SECRETS_MACHINE_IDENTITY=kubernetes, from the environment or
// the config file, is a usage error naming the values agent-secrets takes, and nothing is asked of
// the broker or the metadata server.
func TestAKubernetesMachineIdentityIsRefusedBeforeAnyTrade(t *testing.T) {
	binary := chainBinary(t)
	for _, row := range []string{"the environment", "the config file"} {
		t.Run(row, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
			r := newChainRig(t, binary, broker)
			metadata := r.withMetadata()
			token := filepath.Join(t.TempDir(), "token")
			writeProjectedToken(t, token, "agent-secrets-exchange")
			settings := map[string]string{"AGENT_SECRETS_MACHINE_IDENTITY": "kubernetes", "AGENT_SECRETS_MACHINE_TOKEN_FILE": token}
			if row == "the environment" {
				maps.Copy(r.env, settings)
			} else {
				r.writeConfigFile(settings)
			}

			code, stdout, stderr := r.use()
			if code != exitUsageError || stdout != "" || !strings.Contains(stderr, "AGENT_SECRETS_MACHINE_IDENTITY=kubernetes") ||
				!strings.Contains(stderr, "metadata") || !strings.Contains(stderr, "pool") {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d refusing kubernetes and naming metadata and pool", code, stdout, stderr, exitUsageError)
			}
			if calls, n := broker.callCount(), metadata.totalRequests(); calls != 0 || n != 0 {
				t.Fatalf("the broker saw %d calls and the metadata server %d requests; want none before the refusal", calls, n)
			}
		})
	}
}

// TestSignInTakesEffectAfterAMachineCache is c5's unit half: a machine that has already used its
// own identity, and so holds a machine session in its cache, acts as a person from the first use
// after they sign in, and the cache stays on disk for the next process with no sign-in.
func TestSignInTakesEffectAfterAMachineCache(t *testing.T) {
	binary := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	r := newChainRig(t, binary, broker)
	r.withMetadata()
	r.mustUse()
	if signed := broker.requestsSignedBy(); len(signed) != 1 || signed[0] != "machine-enrollment-1" {
		t.Fatalf("the first use was signed by %v; want the machine's enrollment", signed)
	}

	signInBroker := newLoginBroker(t)
	issuer := loginIssuer(t, "ada@example.com")
	t.Setenv("XDG_STATE_HOME", r.state)
	t.Setenv(cipherclient.SignInDirVar, "")
	t.Setenv(cipherclient.BrokerURLVar, signInBroker.server.URL)
	t.Setenv(cipherclient.SessionVar, "")
	forceMetadataMachine(t)
	if err := login(context.Background(), io.Discard, &approvingWriter{t: t, issuer: issuer}); err != nil {
		t.Fatalf("login: %v", err)
	}
	signIn := signInBroker.liveEnrollment()

	r.mustUse()
	if signed := broker.requestsSignedBy(); len(signed) != 2 || signed[1] != signIn {
		t.Fatalf("the uses were signed by %v; want the second by %s, the sign-in's enrollment", signed, signIn)
	}
	for _, name := range []string{"key.pem", "enrollment"} {
		if _, err := os.Stat(filepath.Join(r.machineCache(), name)); err != nil {
			t.Fatalf("the machine cache after the sign-in: %v", err)
		}
	}
}

// TestAnOldKeyDirMachineSessionMovesToTheMachineCacheOnce: before #4143 a machine kept its own
// session in the launcher's key directory, marked machine-session. The first use moves it into the
// machine cache, so the key directory is a launcher's alone again, and signs with it; a cache that
// already holds a session keeps its own and the key directory's is removed.
func TestAnOldKeyDirMachineSessionMovesToTheMachineCacheOnce(t *testing.T) {
	binary := chainBinary(t)
	t.Run("an empty cache", func(t *testing.T) {
		broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
		r := newChainRig(t, binary, broker)
		r.withMetadata()
		key := writeSessionFiles(t, r.keyDir(), "old-machine-enrollment")
		if err := os.WriteFile(filepath.Join(r.keyDir(), "machine-session"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		r.mustUse()
		r.mustUse()
		cached, err := os.ReadFile(filepath.Join(r.machineCache(), "key.pem"))
		if err != nil || string(cached) != string(key) {
			t.Fatalf("the machine cache's key.pem = %d bytes, %v; want the key dir's key, moved", len(cached), err)
		}
		if id, err := readTrimmed(filepath.Join(r.machineCache(), "enrollment")); err != nil || id != "old-machine-enrollment" {
			t.Fatalf("the machine cache's enrollment = %q, %v; want old-machine-enrollment", id, err)
		}
		for _, name := range []string{"key.pem", "enrollment", "machine-session"} {
			if _, err := os.Stat(filepath.Join(r.keyDir(), name)); !os.IsNotExist(err) {
				t.Fatalf("the key dir still holds %s (%v)", name, err)
			}
		}
		if got := broker.exchanged(); len(got) != 0 {
			t.Fatalf("the broker saw the exchanges %v; the moved session signs and nothing is traded", got)
		}
		if signed := broker.requestsSignedBy(); !slices.Equal(signed, []string{"old-machine-enrollment", "old-machine-enrollment"}) {
			t.Fatalf("the uses were signed by %v; want both by the moved session", signed)
		}
		r.assertMachineCache()
	})

	t.Run("a cache that holds a session", func(t *testing.T) {
		broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
		r := newChainRig(t, binary, broker)
		r.withMetadata()
		cached := r.writeMachineCacheFixture("cached-enrollment")
		writeSessionFiles(t, r.keyDir(), "old-machine-enrollment")
		if err := os.WriteFile(filepath.Join(r.keyDir(), "machine-session"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		r.mustUse()
		for _, name := range []string{"key.pem", "enrollment", "machine-session"} {
			if _, err := os.Stat(filepath.Join(r.keyDir(), name)); !os.IsNotExist(err) {
				t.Fatalf("the key dir still holds %s (%v)", name, err)
			}
		}
		if got, err := os.ReadFile(filepath.Join(r.machineCache(), "key.pem")); err != nil || string(got) != string(cached) {
			t.Fatalf("the machine cache's key changed (%v); want the session it already held", err)
		}
		if signed := broker.requestsSignedBy(); !slices.Equal(signed, []string{"cached-enrollment"}) {
			t.Fatalf("the use was signed by %v; want the cache's session", signed)
		}
	})
}

// TestTheChainSkipsAbsenceAndStopsOnABrokenSource is c6: a source that has nothing for the caller
// is skipped, and one that is present but broken, or a broker refusal, stops the command with its
// own error rather than falling through to the next source. A helper this machine expects (its
// socket named, or its unit installed) that is not running is present but broken, and so is a
// launcher still enrolling the key directory; a plain-http broker off loopback is refused before
// its settings are read, since those choose the audience the machine mints its token for. A
// machine with no identity keeps no audience pin, so a later audience or broker still gets the
// no-source refusal. Each backend's rows that run the command read the injection line its own
// store names.
func TestTheChainSkipsAbsenceAndStopsOnABrokenSource(t *testing.T) {
	binary := chainBinary(t)
	// stoppedHelper is the check of a row whose helper is expected and not running: the command
	// stops naming the helper's socket, and nothing asks the metadata server or the exchange.
	stoppedHelper := func(t *testing.T, r *chainRig, broker *chainBroker, metadata *metadataServer, code int, stdout, stderr string) {
		sock := helper.DefaultSocket(r.getenv)
		if named := r.env["AGENT_SECRETS_HELPER_SOCK"]; named != "" {
			sock = named
		}
		if code != 1 || stdout != "" || !strings.Contains(stderr, "agent-secrets-helper at "+sock) || strings.Contains(stderr, "no broker identity for this process") {
			t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming the helper at %s, and no fall through to the next source", code, stdout, stderr, sock)
		}
		if n, exchanges := metadata.totalRequests(), broker.exchanged(); n != 0 || len(exchanges) != 0 {
			t.Fatalf("the metadata server saw %d requests and the broker the exchanges %v; a stopped helper stops before the machine source", n, exchanges)
		}
	}
	// noSource is the check of a row where no source has an identity: exit 1 and the one refusal,
	// each source with its reason, the machine's saying there is no metadata server.
	noSource := func(t *testing.T, _ *chainRig, _ *chainBroker, _ *metadataServer, code int, stdout, stderr string) {
		t.Helper()
		if code != 1 || stdout != "" || !strings.Contains(stderr, "no broker identity for this process") {
			t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 and the one refusal", code, stdout, stderr)
		}
		for _, source := range []string{"key-dir: ", "sign-in: ", "helper: ", "machine: "} {
			_, reason, found := strings.Cut(stderr, source)
			if !found || strings.HasPrefix(reason, ";") || strings.TrimSpace(reason) == "" {
				t.Fatalf("stderr %q; want %q with its reason", stderr, source)
			}
		}
		if !strings.Contains(stderr, "no Google metadata server") {
			t.Fatalf("stderr %q; want the machine's reason to say there is no metadata server", stderr)
		}
	}
	// noSourceAndNoPin is noSource for a machine that has already run once with no identity: it
	// minted nothing, so it kept no audience pin and is not stopped by one.
	noSourceAndNoPin := func(t *testing.T, r *chainRig, broker *chainBroker, metadata *metadataServer, code int, stdout, stderr string) {
		t.Helper()
		noSource(t, r, broker, metadata, code, stdout, stderr)
		pin := filepath.Join(r.machineCache(), "google-exchange-audience")
		if _, err := os.Stat(pin); !os.IsNotExist(err) {
			t.Fatalf("%s is there (%v); a machine that minted nothing keeps no audience", pin, err)
		}
	}
	for _, store := range []string{"aws", "gcp"} {
		t.Run(store, func(t *testing.T) {
			rows := []struct {
				name  string
				setup func(t *testing.T, r *chainRig, broker *chainBroker) *metadataServer
				check func(t *testing.T, r *chainRig, broker *chainBroker, metadata *metadataServer, code int, stdout, stderr string)
			}{
				{
					name: "helper says not a session",
					setup: func(t *testing.T, r *chainRig, _ *chainBroker) *metadataServer {
						sock, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
						r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
						return r.withMetadata()
					},
					check: func(t *testing.T, _ *chainRig, broker *chainBroker, _ *metadataServer, code int, stdout, stderr string) {
						if code != 0 || strings.TrimSpace(stdout) != fmt.Sprint(len(chainValue)) || len(machineExchanges(broker)) != 1 {
							t.Fatalf("exit %d, stdout %q, stderr %q, %d machine exchanges; want the machine source to sign", code, stdout, stderr, len(machineExchanges(broker)))
						}
						assertInjectedFromStore(t, store, stderr)
					},
				},
				{
					name: "a named helper socket that is not there",
					setup: func(t *testing.T, r *chainRig, _ *chainBroker) *metadataServer {
						r.env["AGENT_SECRETS_HELPER_SOCK"] = filepath.Join(r.runtime, "stopped-helper.sock")
						return r.withMetadata()
					},
					check: stoppedHelper,
				},
				{
					name: "an installed helper that is stopped",
					setup: func(t *testing.T, r *chainRig, _ *chainBroker) *metadataServer {
						unit := helper.UnitPath(r.getenv)
						if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(unit, []byte("[Service]\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						return r.withMetadata()
					},
					check: stoppedHelper,
				},
				{
					name: "a launcher still enrolling the key directory",
					setup: func(t *testing.T, r *chainRig, _ *chainBroker) *metadataServer {
						if err := os.MkdirAll(r.keyDir(), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(r.keyDir(), "enrollment.pending"), nil, 0o600); err != nil {
							t.Fatal(err)
						}
						r.env["AGENT_SECRETS_ENROLL_WAIT"] = "200ms"
						return r.withMetadata()
					},
					check: func(t *testing.T, _ *chainRig, broker *chainBroker, metadata *metadataServer, code int, stdout, stderr string) {
						if code != 1 || stdout != "" || !strings.Contains(stderr, "enrollment.pending") || strings.Contains(stderr, "no broker identity for this process") {
							t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming the launcher's enrollment.pending, and no fall through to the next source", code, stdout, stderr)
						}
						if n, exchanges := metadata.totalRequests(), broker.exchanged(); n != 0 || len(exchanges) != 0 {
							t.Fatalf("the metadata server saw %d requests and the broker the exchanges %v; a launcher still enrolling stops before the machine source", n, exchanges)
						}
					},
				},
				{
					name: "a plain-http broker off loopback",
					setup: func(t *testing.T, r *chainRig, broker *chainBroker) *metadataServer {
						// The broker answers as the proxy of a host no resolver knows, so a client
						// that read the settings of http://broker.example.test reaches it.
						r.env["AGENT_SECRETS_URL"] = "http://broker.example.test"
						r.env["HTTP_PROXY"] = broker.URL()
						return r.withMetadata()
					},
					check: func(t *testing.T, _ *chainRig, broker *chainBroker, metadata *metadataServer, code int, stdout, stderr string) {
						if code != exitUsageError || stdout != "" || !strings.Contains(stderr, "must be https") {
							t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d refusing a plain-http broker off loopback", code, stdout, stderr, exitUsageError)
						}
						if n, calls := metadata.totalRequests(), broker.callCount(); n != 0 || calls != 0 {
							t.Fatalf("the metadata server saw %d requests and the broker %d calls; the refusal comes before the settings are read", n, calls)
						}
					},
				},
				{
					name: "key.pem does not parse",
					setup: func(t *testing.T, r *chainRig, _ *chainBroker) *metadataServer {
						if err := os.MkdirAll(r.keyDir(), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(r.keyDir(), "key.pem"), []byte("not a pem"), 0o600); err != nil {
							t.Fatal(err)
						}
						return r.withMetadata()
					},
					check: func(t *testing.T, _ *chainRig, _ *chainBroker, metadata *metadataServer, code int, _, stderr string) {
						if code != 1 || !strings.Contains(stderr, "key.pem") || strings.Contains(stderr, "no broker identity for this process") {
							t.Fatalf("exit %d, stderr %q; want exit 1 naming key.pem, and no fall through to the next source", code, stderr)
						}
						if n := metadata.totalRequests(); n != 0 {
							t.Fatalf("the metadata server saw %d requests; a broken key dir stops before the machine source", n)
						}
					},
				},
				{
					name: "broker refuses the machine exchange",
					setup: func(t *testing.T, r *chainRig, broker *chainBroker) *metadataServer {
						broker.refuseExchanges()
						return r.withMetadata()
					},
					check: func(t *testing.T, _ *chainRig, _ *chainBroker, _ *metadataServer, code int, _, stderr string) {
						if code != 1 || !strings.Contains(stderr, "invalid_grant") || strings.Contains(stderr, "no broker identity for this process") {
							t.Fatalf("exit %d, stderr %q; want exit 1 naming the broker's refusal, as the refusal itself", code, stderr)
						}
					},
				},
				{
					name:  "no source",
					setup: func(*testing.T, *chainRig, *chainBroker) *metadataServer { return nil },
					check: noSource,
				},
				{
					// A laptop with no identity of its own ran a use command once, and then the
					// broker's operator rotated its Google audience. The first run kept no pin,
					// since nothing was minted, so this one gives the same refusal.
					name: "a machine with no identity, after an audience rotation",
					setup: func(t *testing.T, r *chainRig, broker *chainBroker) *metadataServer {
						code, stdout, stderr := r.use()
						noSource(t, r, broker, nil, code, stdout, stderr)
						broker.setSettings(storeSettings(store, "rotated-audience"))
						return nil
					},
					check: noSourceAndNoPin,
				},
				{
					name: "a machine with no identity, after a switch to another broker",
					setup: func(t *testing.T, r *chainRig, broker *chainBroker) *metadataServer {
						code, stdout, stderr := r.use()
						noSource(t, r, broker, nil, code, stdout, stderr)
						other := newChainBroker(t, storeSettings(store, "another-brokers-audience"))
						r.env["AGENT_SECRETS_URL"] = other.URL()
						return nil
					},
					check: noSourceAndNoPin,
				},
				{
					// A broker of #4143's first head still names the Kubernetes exchange audience
					// a pod's projected token is minted for; the machine source no longer reads
					// such a token at all, so this machine trades its own Google identity.
					name: "a broker that still names a Kubernetes exchange audience",
					setup: func(t *testing.T, r *chainRig, broker *chainBroker) *metadataServer {
						settings := storeSettings(store, "agent-secrets-vm")
						settings["kubernetes_exchange_audience"] = "agent-secrets-exchange"
						broker.setSettings(settings)
						return r.withMetadata()
					},
					check: func(t *testing.T, _ *chainRig, broker *chainBroker, metadata *metadataServer, code int, stdout, stderr string) {
						if code != 0 || strings.TrimSpace(stdout) != fmt.Sprint(len(chainValue)) {
							t.Fatalf("exit %d, stdout %q, stderr %q; want the metadata source to sign", code, stdout, stderr)
						}
						exchanges := broker.exchanged()
						if len(exchanges) != 1 || exchanges[0].Get("subject_token_type") != oauthwire.SubjectTokenTypeGoogle || metadata.metadataIdentityRequests() != 1 {
							t.Fatalf("the broker saw the exchanges %v; want one trading the metadata server's identity token, not the projected token", exchanges)
						}
						assertInjectedFromStore(t, store, stderr)
					},
				},
			}
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					broker := newChainBroker(t, storeSettings(store, "agent-secrets-vm"))
					r := newChainRig(t, binary, broker)
					metadata := row.setup(t, r, broker)
					code, stdout, stderr := r.use()
					row.check(t, r, broker, metadata, code, stdout, stderr)
				})
			}
		})
	}
}

// TestTheMachineMintsOnlyForTheGoogleAudienceItFirstRead: the broker's unauthenticated settings
// choose the audience the machine mints its Google identity token for, so the first audience the
// machine reads is kept beside its session, and a later read naming another is refused before any
// token is minted for it, naming both audiences and the file that keeps the first, by where the
// machine cache is and never by its path, since the cache holds the machine session's key.pem.
func TestTheMachineMintsOnlyForTheGoogleAudienceItFirstRead(t *testing.T) {
	binary := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	r := newChainRig(t, binary, broker)
	metadata := r.withMetadata()
	r.mustUse()
	kept := filepath.Join(r.machineCache(), "google-exchange-audience")
	if got, err := readTrimmed(kept); err != nil || got != "agent-secrets-vm" {
		t.Fatalf("the kept audience = %q, %v; want agent-secrets-vm, the one the first use read", got, err)
	}

	// The broker now names someone else's service as the audience, and the machine's session
	// lapsed, so the next use registers it again and reads the settings again.
	broker.setSettings(storeSettings("gcp", "https://victim.example"))
	broker.endEnrollment("machine-enrollment-1")
	code, stdout, stderr := r.use()
	named := "google-exchange-audience in the machine cache (${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine)"
	if code != 1 || stdout != "" || !strings.Contains(stderr, `"https://victim.example"`) || !strings.Contains(stderr, `"agent-secrets-vm"`) || !strings.Contains(stderr, named) || strings.Contains(stderr, kept) {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming both audiences and %s, never %s", code, stdout, stderr, named, kept)
	}
	if n := metadata.metadataIdentityRequests(); n != 1 {
		t.Fatalf("the metadata server minted %d identity tokens; want only the first use's", n)
	}
	if got := machineExchanges(broker); len(got) != 1 {
		t.Fatalf("the broker saw the machine exchanges %v; want only the first use's", got)
	}
}

// TestAMachineRegistersAgainUnderAFreshKey: when the broker says the machine's cached session is
// not live, the machine registers again under a key it makes for that registration, so a copy of
// the old key.pem (a home backup, a disk kept across a reboot) signs for no later enrollment. The
// call that met the refusal is signed again by the new key, its request object included, which the
// broker holds to the enrollment's own key.
func TestAMachineRegistersAgainUnderAFreshKey(t *testing.T) {
	binary := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	r := newChainRig(t, binary, broker)
	r.withMetadata()
	r.mustUse()
	keyPath := filepath.Join(r.machineCache(), "key.pem")
	first, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	broker.endEnrollment("machine-enrollment-1")
	r.mustUse()
	second, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("the machine registered again under the key it already held: a copy of that key.pem signs for every later enrollment")
	}
	if got, want := pemThumbprint(t, second), broker.thumbprintOf("machine-enrollment-2"); got != want {
		t.Fatalf("the cached key's thumbprint %s is not the one machine-enrollment-2 was registered for (%s)", got, want)
	}
	if id, err := readTrimmed(filepath.Join(r.machineCache(), "enrollment")); err != nil || id != "machine-enrollment-2" {
		t.Fatalf("the cached enrollment = %q, %v; want machine-enrollment-2", id, err)
	}
	if signed := broker.requestsSignedBy(); !slices.Equal(signed, []string{"machine-enrollment-1", "machine-enrollment-1", "machine-enrollment-2"}) {
		t.Fatalf("the requests were signed by %v; want the first use's, the lapsed one's refused, and the new enrollment's", signed)
	}
	if broker.enrolled() != 2 {
		t.Fatalf("the broker saw %d enrollments; want two", broker.enrolled())
	}
	r.assertMachineCache()
}

// TestMachineRegistrationsAtOnceKeepOneSession: processes that each find the machine cache empty
// at once register one session between them, and every one signs with it: the cache's key.pem is
// always the key its enrollment was registered for.
func TestMachineRegistrationsAtOnceKeepOneSession(t *testing.T) {
	binary := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	// Each settings read waits, so every process has found the cache empty before the first
	// registration ends: with nothing between them, each would register a session of its own.
	broker.delaySettings(500 * time.Millisecond)
	r := newChainRig(t, binary, broker)
	r.withMetadata()
	// The processes run side by side, each on one P: one left to size GOMAXPROCS to the host starts
	// a thread for each P its collector's idle mark workers take, about 80 on a 288-CPU host.
	r.env["GOMAXPROCS"] = "1"

	const processes = 4
	failures := make(chan string, processes)
	var wg sync.WaitGroup
	for range processes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := r.command(useArgs...)
			var out, errOut strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &errOut
			if err := cmd.Run(); err != nil || strings.TrimSpace(out.String()) != fmt.Sprint(len(chainValue)) {
				failures <- fmt.Sprintf("%v: stdout %q, stderr %q", err, out.String(), errOut.String())
			}
		}()
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("a use command: %s", failure)
	}
	if n, e := len(machineExchanges(broker)), broker.enrolled(); n != 1 || e != 1 {
		t.Fatalf("the broker saw %d machine exchanges and %d enrollments; want one of each", n, e)
	}
	key, err := os.ReadFile(filepath.Join(r.machineCache(), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := readTrimmed(filepath.Join(r.machineCache(), "enrollment"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pemThumbprint(t, key), broker.thumbprintOf(id); got != want {
		t.Fatalf("the cache pairs a key of thumbprint %s with %s, which was registered for %s", got, id, want)
	}
}

// TestAUnitWithNoHomeKeepsItsStateUnderItsUsersHome: systemd starts a system unit that names no
// User= with no HOME at all. The client then reads its settings file, and keeps its machine
// session, under the home the password database gives its user, so the unit's second command
// signs with the session its first registered rather than trading and enrolling again.
func TestAUnitWithNoHomeKeepsItsStateUnderItsUsersHome(t *testing.T) {
	isolateChain(t)
	home := t.TempDir()
	original := passwdHome
	passwdHome = func() string { return home }
	t.Cleanup(func() { passwdHome = original })
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
	config := filepath.Join(home, ".config", "agent-secrets", "config")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("AGENT_SECRETS_URL="+broker.URL()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientConfig = nil })
	if err := loadClientConfig(); err != nil {
		t.Fatal(err)
	}

	for run := range 2 {
		if _, _, err := buildSigner(io.Discard); err != nil {
			t.Fatalf("command %d with no HOME: %v", run+1, err)
		}
	}
	if n, e := len(machineExchanges(broker)), broker.enrolled(); n != 1 || e != 1 {
		t.Fatalf("two commands traded %d times and enrolled %d times; want once each, the second signing with the session the first kept", n, e)
	}
	for _, name := range []string{"key.pem", "enrollment"} {
		if _, err := os.Stat(filepath.Join(home, ".local", "state", "agent-secrets", "machine", name)); err != nil {
			t.Fatalf("the machine cache under the user's home: %v", err)
		}
	}
}

// pemThumbprint is the thumbprint of the key a key.pem holds.
func pemThumbprint(t *testing.T, data []byte) string {
	t.Helper()
	key, err := parseKeyPEM(data, "key.pem")
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return thumbprint
}

// TestIdentitySourceNamesTheSourceItWouldUse: `identity --source` prints the source a use command
// would take its identity from, registering nothing, and the helper's socket on a line of its own
// whenever the helper answered and is not that source; with none it prints nothing on stdout and
// the chain's refusal on stderr.
func TestIdentitySourceNamesTheSourceItWouldUse(t *testing.T) {
	binary := chainBinary(t)
	rows := []struct {
		name  string
		setup func(t *testing.T, r *chainRig) string // the stdout the row wants
		exit  int
	}{
		{name: "key dir", setup: func(t *testing.T, r *chainRig) string {
			writeSessionFiles(t, r.keyDir(), "launcher-enrollment")
			return "key-dir: $XDG_RUNTIME_DIR/agent-secrets (AGENT_SECRETS_KEY_DIR is unset)\n"
		}},
		{name: "sign-in", setup: func(t *testing.T, r *chainRig) string {
			writeSignInFixture(t, r, "ada@example.com")
			return "sign-in: ada@example.com\n"
		}},
		{name: "helper session", setup: func(t *testing.T, r *chainRig) string {
			sock, _ := fakeHelper(t, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
			// fakeHelper answers one canned line to any op, so it does not speak op `version`:
			// identity --source names its binary by the pid serving the socket (this test
			// process), the older-helper line. TestIdentitySourceNamesTheHelpersVersion covers a
			// helper that does report a version.
			return "helper: " + sock + fmt.Sprintf(" (a helper from before this release; its binary is /proc/%d/exe)\n", os.Getpid())
		}},
		{name: "metadata", setup: func(t *testing.T, r *chainRig) string {
			r.withMetadata()
			return "machine: " + cipherclient.NewMetadataIdentity("agent-secrets-vm").Describe() + "\n"
		}},
		{name: "machine cache", setup: func(t *testing.T, r *chainRig) string {
			r.writeMachineCacheFixture("cached-enrollment")
			return "machine: the cached enrollment in the machine cache (${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine)\n"
		}},
		{name: "sign-in and a helper that says not a session", setup: func(t *testing.T, r *chainRig) string {
			writeSignInFixture(t, r, "ada@example.com")
			sock, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
			return "sign-in: ada@example.com\nhelper: " + sock + fmt.Sprintf(" (a helper from before this release; its binary is /proc/%d/exe)\n", os.Getpid())
		}},
		{name: "none", setup: func(*testing.T, *chainRig) string { return "" }, exit: 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
			r := newChainRig(t, binary, broker)
			want := row.setup(t, r)
			code, stdout, stderr := r.run("identity", "--source")
			if code != row.exit || stdout != want {
				t.Fatalf("identity --source: exit %d, stdout %q, stderr %q; want exit %d and stdout %q", code, stdout, stderr, row.exit, want)
			}
			if row.exit == 1 {
				for _, source := range []string{"key-dir: ", "sign-in: ", "helper: ", "machine: "} {
					if !strings.Contains(stderr, source) {
						t.Fatalf("stderr %q; want the refusal naming %q", stderr, source)
					}
				}
			}
			if broker.enrolled() != 0 || len(broker.exchanged()) != 0 {
				t.Fatalf("identity --source made %d enrollments and %d exchanges; it registers nothing", broker.enrolled(), len(broker.exchanged()))
			}
		})
	}
}

// writeSignInFixture keeps a sign-in for person in the rig's state directory, as `agent-secrets
// login` would, holding an enrollment already made, and answers its box key's thumbprint, the
// key a broker holds that enrollment to (chainBroker.pinKey).
func writeSignInFixture(t *testing.T, r *chainRig, person string) string {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	store := cipherclient.SignIns(func(name string) string {
		return map[string]string{"XDG_STATE_HOME": r.state, "HOME": r.home}[name]
	})
	if err := store.Write(cipherclient.SignInRecord{
		Person: person, Issuer: "https://accounts.google.com", ClientID: "device-client", RefreshToken: "not-a-secret-refresh-token",
		BoxKey: string(keyPEM(key)), Thumbprint: thumbprint, EnrollmentID: "signin-enrollment", SignedInAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return thumbprint
}
