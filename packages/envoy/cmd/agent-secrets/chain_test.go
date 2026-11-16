// packages/envoy/cmd/agent-secrets/chain_test.go
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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
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

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// chainValue is the value chainBroker releases for TEST_KEY. The use command's child prints its
// length, so no test output carries it.
const chainValue = "chain-test-value"

// chainBroker is the broker routes a use command calls: its settings, the token exchange a
// machine trades its identity at, the enrollment it then registers, the request and the release.
// It records every exchange's form, counts the enrollments, and keeps the enrollment each
// request's session proof named.
type chainBroker struct {
	server *httptest.Server

	mu          sync.Mutex
	settings    map[string]any
	refuse      bool
	exchanges   []url.Values
	enrollments int
	signedBy    []string
	calls       int
}

// storeSettings is GET /v1/settings of a broker on store ("aws" or "gcp"), naming google and
// kubernetes as its exchange audiences where they are not empty.
func storeSettings(store, google, kubernetes string) map[string]any {
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
	if kubernetes != "" {
		settings["kubernetes_exchange_audience"] = kubernetes
	}
	return settings
}

func newChainBroker(t *testing.T, settings map[string]any) *chainBroker {
	t.Helper()
	b := &chainBroker{settings: settings}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		settings := maps.Clone(b.settings)
		b.mu.Unlock()
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
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		b.enrollments++
		id := fmt.Sprintf("machine-enrollment-%d", b.enrollments)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"enrollment_id": id})
	})
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.signedBy = append(b.signedBy, proofEID(r))
		b.mu.Unlock()
		writeJSON(w, map[string]any{
			"request_id": "req-chain", "state": "granted",
			"secrets":  []map[string]string{{"name": "TEST_KEY", "decision": "automatic"}},
			"grant_id": "grant-chain", "record_id": nil, "approver": nil,
		})
	})
	mux.HandleFunc("POST /v1/grants/grant-chain/values", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"values": map[string]string{"TEST_KEY": chainValue}, "expires_at": time.Now().Add(time.Hour)})
	})
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls++
		b.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
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

// closedAddress is a loopback address nothing listens on: a metadata server that is not there.
func closedAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// chainBinary builds agent-secrets with the projected token it looks for at a path of the test's
// own (brokerclient.DefaultProjectedTokenFile, set at link time), absent until a row writes it,
// so no row reads a token the machine running the test holds.
func chainBinary(t *testing.T) (binary, token string) {
	t.Helper()
	token = filepath.Join(t.TempDir(), "projected-token")
	binary = cmdtest.BuildCommand(t, cmdtest.Command{Name: "agent-secrets", Flags: []string{
		"-ldflags", "-X github.com/sjawhar/envoy/internal/brokerclient.DefaultProjectedTokenFile=" + token,
	}})
	return binary, token
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
		"GCE_METADATA_HOST": closedAddress(t),
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

// run runs the binary with args in the rig's environment.
func (r *chainRig) run(args ...string) (code int, stdout, stderr string) {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = nil
	for _, name := range slices.Sorted(maps.Keys(r.env)) {
		cmd.Env = append(cmd.Env, name+"="+r.env[name])
	}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			r.t.Fatalf("run agent-secrets %v: %v", args, err)
		}
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// use is the use command whose child prints the length of the value it was given.
func (r *chainRig) use() (code int, stdout, stderr string) {
	r.t.Helper()
	return r.run("TEST_KEY", "--wait", "0s", "--", "sh", "-c", "echo ${#TEST_KEY}")
}

// mustUse is use, failing the test unless the child ran with the released value.
func (r *chainRig) mustUse() {
	r.t.Helper()
	code, stdout, stderr := r.use()
	if code != 0 || strings.TrimSpace(stdout) != fmt.Sprint(len(chainValue)) {
		r.t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want exit 0 and the value's length %d", code, stdout, stderr, len(chainValue))
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
	binary, _ := chainBinary(t)
	fromFile := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", ""))
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

	fromEnvironment := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", ""))
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
// the machine cache.
func TestUseCommandOutsideASessionReachesTheMachineSource(t *testing.T) {
	binary, _ := chainBinary(t)
	for _, store := range []string{"aws", "gcp"} {
		t.Run(store, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings(store, "agent-secrets-vm", ""))
			r := newChainRig(t, binary, broker)
			r.withMetadata()
			sock, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock

			r.mustUse()
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
	binary, _ := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", "", ""))
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

// TestAForcedMachineIdentityIsTheOnlyMachineSource: AGENT_SECRETS_MACHINE_IDENTITY names the one
// identity the machine source trades, from the environment or the config file, and detection is
// not tried; a machine session already in the cache signs before either.
func TestAForcedMachineIdentityIsTheOnlyMachineSource(t *testing.T) {
	binary, _ := chainBinary(t)
	for _, row := range []string{"the environment", "the config file"} {
		t.Run(row, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", "agent-secrets-exchange"))
			r := newChainRig(t, binary, broker)
			metadata := r.withMetadata()
			token := filepath.Join(t.TempDir(), "token")
			writeProjectedToken(t, token, "agent-secrets-exchange")
			settings := map[string]string{"AGENT_SECRETS_MACHINE_IDENTITY": "kubernetes", "AGENT_SECRETS_MACHINE_TOKEN_FILE": token}
			if row == "the environment" {
				maps.Copy(r.env, settings)
			} else {
				config := filepath.Join(r.home, ".config", "agent-secrets", "config")
				if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
					t.Fatal(err)
				}
				var lines string
				for _, name := range slices.Sorted(maps.Keys(settings)) {
					lines += name + "=" + settings[name] + "\n"
				}
				if err := os.WriteFile(config, []byte(lines), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			r.mustUse()
			exchanges := broker.exchanged()
			if len(exchanges) != 1 || exchanges[0].Get("subject_token_type") != oauthwire.SubjectTokenTypeKubernetes {
				t.Fatalf("the broker saw the exchanges %v; want one trading the projected Kubernetes token the setting names", exchanges)
			}
			if n := metadata.totalRequests(); n != 0 {
				t.Fatalf("the metadata server saw %d requests; a forced identity is the only machine source", n)
			}
		})
	}

	t.Run("a cached machine session", func(t *testing.T) {
		broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", ""))
		r := newChainRig(t, binary, broker)
		r.writeMachineCacheFixture("cached-enrollment")
		r.env["AGENT_SECRETS_MACHINE_IDENTITY"] = "kubernetes"
		r.env["AGENT_SECRETS_MACHINE_TOKEN_FILE"] = filepath.Join(t.TempDir(), "no-such-token")
		r.mustUse()
		if got := broker.exchanged(); len(got) != 0 {
			t.Fatalf("the broker saw the exchanges %v; a cached session signs and trades nothing", got)
		}
		if signed := broker.requestsSignedBy(); len(signed) != 1 || signed[0] != "cached-enrollment" {
			t.Fatalf("the request was signed by %v; want the cached enrollment", signed)
		}
	})
}

// TestSignInTakesEffectAfterAMachineCache is c5's unit half: a machine that has already used its
// own identity, and so holds a machine session in its cache, acts as a person from the first use
// after they sign in, and the cache stays on disk for the next process with no sign-in.
func TestSignInTakesEffectAfterAMachineCache(t *testing.T) {
	binary, _ := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", ""))
	r := newChainRig(t, binary, broker)
	r.withMetadata()
	r.mustUse()
	if signed := broker.requestsSignedBy(); len(signed) != 1 || signed[0] != "machine-enrollment-1" {
		t.Fatalf("the first use was signed by %v; want the machine's enrollment", signed)
	}

	signInBroker := newLoginBroker(t)
	issuer := loginIssuer(t, "ada@example.com")
	t.Setenv("XDG_STATE_HOME", r.state)
	t.Setenv(brokerclient.SignInDirVar, "")
	t.Setenv(brokerclient.BrokerURLVar, signInBroker.server.URL)
	t.Setenv(brokerclient.SessionVar, "")
	t.Setenv(brokerclient.IdentityVar, brokerclient.IdentityKubernetes)
	t.Setenv(brokerclient.MachineTokenFileVar, writeLoginFile(t, "machine-token", "projected.service.account.token"))
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
	binary, _ := chainBinary(t)
	t.Run("an empty cache", func(t *testing.T) {
		broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", ""))
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
		broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", ""))
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
// own error rather than falling through to the next source.
func TestTheChainSkipsAbsenceAndStopsOnABrokenSource(t *testing.T) {
	binary, token := chainBinary(t)
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
					name: "no source",
					setup: func(*testing.T, *chainRig, *chainBroker) *metadataServer {
						return nil
					},
					check: func(t *testing.T, _ *chainRig, _ *chainBroker, _ *metadataServer, code int, stdout, stderr string) {
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
					},
				},
				{
					name: "projected token for another audience",
					setup: func(t *testing.T, r *chainRig, _ *chainBroker) *metadataServer {
						writeProjectedToken(t, token, "agent-secrets")
						t.Cleanup(func() { os.Remove(token) })
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
					},
				},
			}
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					broker := newChainBroker(t, storeSettings(store, "agent-secrets-vm", "agent-secrets-exchange"))
					r := newChainRig(t, binary, broker)
					metadata := row.setup(t, r, broker)
					code, stdout, stderr := r.use()
					row.check(t, r, broker, metadata, code, stdout, stderr)
				})
			}
		})
	}
}

// TestIdentitySourceNamesTheSourceItWouldUse: `identity --source` prints the source a use command
// would take its identity from, registering nothing, and the helper's socket on a line of its own
// whenever the helper answered and is not that source; with none it prints nothing on stdout and
// the chain's refusal on stderr.
func TestIdentitySourceNamesTheSourceItWouldUse(t *testing.T) {
	binary, _ := chainBinary(t)
	rows := []struct {
		name  string
		setup func(t *testing.T, r *chainRig) string // the stdout the row wants
		exit  int
	}{
		{name: "key dir", setup: func(t *testing.T, r *chainRig) string {
			writeSessionFiles(t, r.keyDir(), "launcher-enrollment")
			return "key-dir: " + r.keyDir() + "\n"
		}},
		{name: "sign-in", setup: func(t *testing.T, r *chainRig) string {
			writeSignInFixture(t, r, "ada@example.com")
			return "sign-in: ada@example.com\n"
		}},
		{name: "helper session", setup: func(t *testing.T, r *chainRig) string {
			sock, _ := fakeHelper(t, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
			return "helper: " + sock + "\n"
		}},
		{name: "metadata", setup: func(t *testing.T, r *chainRig) string {
			r.withMetadata()
			return "machine: " + brokerclient.NewMetadataIdentity("agent-secrets-vm").Describe() + "\n"
		}},
		{name: "machine cache", setup: func(t *testing.T, r *chainRig) string {
			r.writeMachineCacheFixture("cached-enrollment")
			return "machine: the cached enrollment in " + r.machineCache() + "\n"
		}},
		{name: "sign-in and a helper that says not a session", setup: func(t *testing.T, r *chainRig) string {
			writeSignInFixture(t, r, "ada@example.com")
			sock, _ := fakeHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
			return "sign-in: ada@example.com\nhelper: " + sock + "\n"
		}},
		{name: "none", setup: func(*testing.T, *chainRig) string { return "" }, exit: 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm", ""))
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
// login` would, holding an enrollment already made.
func writeSignInFixture(t *testing.T, r *chainRig, person string) {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	store := brokerclient.SignIns(func(name string) string {
		return map[string]string{"XDG_STATE_HOME": r.state, "HOME": r.home}[name]
	})
	if err := store.Write(brokerclient.SignInRecord{
		Person: person, Issuer: "https://accounts.google.com", ClientID: "device-client", RefreshToken: "not-a-secret-refresh-token",
		BoxKey: string(keyPEM(key)), Thumbprint: thumbprint, EnrollmentID: "signin-enrollment", SignedInAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}
