//go:build e2e

// The rig of dispatch://INFRA-4425's checks K2, K3 and K4: every Dispatch client a Legion
// deployment runs, writing with a credential the secrets broker issued, against a real broker, a
// real Dispatch and the real clients. scripts/e2e/dispatch-broker.sh runs it.
//
// Nothing of the credential path is stood in for. The broker is packages/envoy's own
// (ciphertest.NewRig, its real handlers on a real Postgres), with the exchange accounts that rig
// lists. Dispatch is the real server this checkout builds (startScratchDispatchOn), on a database of
// its own, reading each broker token back through that broker's introspection with the token of
// the account the rig lists for Dispatch (DISPATCH_AGENT_SECRETS_URL and
// DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE). As in production, the clients call Dispatch at one
// address and Dispatch's DISPATCH_SERVER_URL names another: every client calls a recording proxy
// in front of Dispatch, DISPATCH_SERVER_URL is a public-looking origin nothing dials, and
// DISPATCH_PROOF_ORIGINS names the proxy's address, so a proof's htu names an origin Dispatch was
// told it is reached at. The proxy is the run's request log: each request's time, route,
// Authorization scheme, a digest of its token and the answer's status and code. Each broker token
// it sees is read back once at the broker's introspection (ciphertest.Rig.Introspect), for the
// expiry the tables print beside the write.
//
// What is stood in for lies outside the credential path, each as the real one with nothing to
// report answers. The Envoy listener Dispatch delivers a message through knows no session
// (rigListener), so a person's direct message to a role's session is recorded for that session,
// which is what lets the role reply to it with a picture. GitHub knows no installation of
// Dispatch's GitHub App (rigGitHub), whose key is a throwaway made for the run, so the daemon's
// read of whether the App is installed is answered false rather than refused for want of an App.
//
// The clients are the real ones. The daemon's is t-4's boot (dispatchCredential, with the pod
// identity of the rig's DaemonExchangeAccount in a token file of its own) and the client it builds,
// internal/dispatch's HTTPClient. A role is an ExchangeRolesAccount pod whose session the
// daemon's own account registered at the broker with the thumbprint of the role's key, a key.pem
// written as `agent-secrets keygen` writes one; its environment is the DISPATCH_ and AGENT_SECRETS_
// variables the launch golden pod-with-dispatch-broker.json names, read from that file and filled
// with the rig's values, so the rig and the launch cannot drift apart. The `dispatch` command is
// packages/envoy-client/bin/dispatch.ts under bun in that environment, and the long-lived clients
// are scripts/e2e/dispatch-broker-clients.ts: one process holding the client the `dispatch`
// command builds and the client the pi-envoy plugin builds.
//
// Each test writes its tables to TMPDIR and logs the file's path.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/cipherclient"
	"github.com/sjawhar/envoy/ciphertest"

	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
)

// The rig's two projects: P, which every account of the rig's exchange names but one, and Q, the
// project of that one, OtherProjectAccount. Both are Dispatch projects the rig makes.
const (
	rigP = ciphertest.ExchangeProject
	rigQ = ciphertest.OtherProject
)

// rigServerURL is the rig's Dispatch's DISPATCH_SERVER_URL: a public-looking origin nothing
// dials, as a deployed Dispatch's public origin is one no pod calls.
const rigServerURL = "https://dispatch.example"

// rigUnlistedAccount is a ServiceAccount no entry of the rig's exchange lists, whose subject token
// the broker refuses for good: K3's refusal.
const rigUnlistedAccount = "system:serviceaccount:example-ns:not-listed"

// rigCheck is the check of P's issue's Verification document a role records its result on, and
// rigCommit the commit it records.
const (
	rigCheck  = "k4-rig"
	rigCommit = "4425f00d4425f00d4425f00d4425f00d4425f00d"
)

// dispatchBrokerRig is one run of the rig: the broker, Dispatch behind the recording proxy, and the
// issues of P and Q the writes address.
type dispatchBrokerRig struct {
	t    *testing.T
	root string
	dir  string
	// broker is the real broker. dispatch is the real Dispatch, which only the setup below calls
	// directly; every client calls it through proxy.
	broker   *ciphertest.Rig
	dispatch *scratchDispatch
	proxy    *recordingProxy
	// url is the proxy's address, the one every client calls and DISPATCH_PROOF_ORIGINS names;
	// outside is a second address of the same proxy, which no origin of Dispatch's names.
	url, outside string
	// registrar is the file holding DaemonAccount's login token, which registers a role's session.
	registrar string
	// pIssue is the issue of P every role write addresses, pDaemon the one the daemon's own
	// writes address, and qIssue an issue of Q.
	pIssue, pDaemon, qIssue string
	// launch is the DISPATCH_ and AGENT_SECRETS_ variables the launch golden names, with its
	// values.
	launch map[string]string
	// tokens is what the broker's introspection answered for each broker token the proxy saw, by
	// its digest, read while that token was live.
	tokens map[string]rigToken
	// documentsOfP is the id of every document and file of P's issues, which names reads a
	// request of /api/v1/artifacts/{id} by; K4 fills it once the run with P's tokens is over.
	documentsOfP map[string]bool
	// direct is the HTTP client of the rig's own setup, outside the proxy.
	direct *http.Client
}

// rigToken is what the broker's introspection answers for one of its access tokens
// (ciphertest.Introspection).
type rigToken ciphertest.Introspection

// expiry is when the token stops being live, the zero time where the broker named none.
func (k rigToken) expiry() time.Time {
	if k.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(k.ExpiresAt, 0)
}

// newDispatchBrokerRig starts the broker with access tokens that live lifetime, Dispatch behind
// the proxy, and the projects and issues. A prerequisite that is missing fails the test, never
// skips it: ciphertest.NewRig skips without BROKER_TEST_DATABASE_URL and testdb without
// LEGION_TEST_PG_DSN, and a skip reads as a pass.
func newDispatchBrokerRig(t *testing.T, lifetime time.Duration) *dispatchBrokerRig {
	t.Helper()
	for _, name := range []string{"BROKER_TEST_DATABASE_URL", "LEGION_TEST_PG_DSN"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is unset: the rig fails rather than skips without it (scripts/e2e/dispatch-broker.sh sets BROKER_TEST_DATABASE_URL to LEGION_TEST_PG_DSN)", name)
		}
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Fatalf("bun is not on PATH, and the dispatch command and the clients' driver run under it: %v", err)
	}
	// testbuild.RootEnv names another checkout to build Dispatch from and run the clients out of,
	// as the handoff records proof reads it; unset, the checkout is four directories up.
	root := os.Getenv(testbuild.RootEnv)
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "node_modules")); err != nil || !info.IsDir() {
		t.Fatalf("%s is missing: run `bun install` at the checkout's root (scripts/e2e/dispatch-broker.sh does)", filepath.Join(root, "node_modules"))
	}
	r := &dispatchBrokerRig{
		t: t, root: root, dir: t.TempDir(), launch: launchVariables(t), tokens: map[string]rigToken{},
		direct: &http.Client{Timeout: 30 * time.Second},
	}
	r.broker = ciphertest.NewRig(t, ciphertest.WithAccessLifetime(lifetime))
	r.proxy = newRecordingProxy()
	at := httptest.NewServer(r.proxy.at("proxy"))
	t.Cleanup(at.Close)
	outside := httptest.NewServer(r.proxy.at("outside"))
	t.Cleanup(outside.Close)
	r.url, r.outside = at.URL, outside.URL
	listener := httptest.NewServer(http.HandlerFunc(rigListener))
	t.Cleanup(listener.Close)
	github := httptest.NewServer(http.HandlerFunc(rigGitHub))
	t.Cleanup(github.Close)
	r.dispatch = startScratchDispatchOn(t, scratchDispatchOptions{root: root, env: []string{
		"DISPATCH_SERVER_URL=" + rigServerURL,
		"DISPATCH_AGENT_SECRETS_URL=" + r.broker.URL,
		// UIToken is the token of the account the rig's BROKER_API_ACCOUNTS lists as the caller
		// "dispatch" (ciphertest's APIAccount): the account Dispatch presents to the broker.
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=" + r.file("dispatch-account-token", r.broker.UIToken),
		"DISPATCH_PROOF_ORIGINS=" + r.url,
		"ENVOY_URL=" + listener.URL,
		"DISPATCH_APP_CLIENT_ID=Iv1.rig",
		"DISPATCH_APP_PEM_B64=" + rigAppKey(t),
		"DISPATCH_GITHUB_API_BASE=" + github.URL,
	}})
	r.proxy.forward(r.dispatch.base)
	r.registrar = r.file("daemon-account-token", r.broker.MintAccountToken(t, ciphertest.DaemonAccount, time.Hour))
	r.setUp()
	t.Logf("broker %s; Dispatch %s (DISPATCH_SERVER_URL %s), reached through the proxy %s, which DISPATCH_PROOF_ORIGINS names, and %s, which it does not; issues %s and %s of %s, %s of %s",
		r.broker.URL, r.dispatch.base, rigServerURL, r.url, r.outside, r.pIssue, r.pDaemon, rigP, r.qIssue, rigQ)
	return r
}

// launchVariables is every DISPATCH_ and AGENT_SECRETS_ variable the launch golden
// pod-with-dispatch-broker.json (t-5) names, with its value there: the environment a role starts
// with under dispatch_token_broker, read from that file so the rig cannot drift from the launch.
func launchVariables(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "runtime", "sandbox", "testdata", "golden", "pod-with-dispatch-broker.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the launch golden: %v", err)
	}
	var golden struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("decode the launch golden %s: %v", path, err)
	}
	launch := map[string]string{}
	for name, value := range golden.Env {
		if strings.HasPrefix(name, "DISPATCH_") || strings.HasPrefix(name, "AGENT_SECRETS_") {
			launch[name] = value
		}
	}
	if len(launch) == 0 {
		t.Fatalf("the launch golden %s names no DISPATCH_ or AGENT_SECRETS_ variable", path)
	}
	return launch
}

// rigListener stands in for the Envoy listener Dispatch delivers a message through, answering as
// one that knows no session: its two list reads (GET /v1/sessions, GET /v1/interests/) are
// empty, and every other route, the per-session read among them, is 404. Dispatch reads that 404
// as "no live session <id>" and records the attempt for the session its route names
// (resolveDeliveryTarget, internal/dispatch/api/envoy_resolve.go), so that session may reply to
// it (replyMessage), where a listener nothing answers records the attempt for no session at all.
func rigListener(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/sessions" || r.URL.Path == "/v1/interests/") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
		return
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

// rigGitHub stands in for GitHub's API under Dispatch's GitHub App, answering as GitHub answers an
// App installed on no repository: 404 for every route, the installation lookup among them, which
// Dispatch reads as an App that is not installed (githubapp.Client.Installed).
func rigGitHub(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

// rigAppKey is a throwaway GitHub App private key made for the run, as the base64 of its PEM
// (DISPATCH_APP_PEM_B64): every App call Dispatch makes is signed with it and goes to rigGitHub.
func rigAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// file writes content to a file of the run's own named name, 0600, and answers its path.
func (r *dispatchBrokerRig) file(name, content string) string {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return path
}

// rigSetupActor is the session the rig's own setup names as its actor under the shared token.
var rigSetupActor = map[string]string{"kind": "session", "id": "rig-setup"}

// setUp makes the projects and the issues with the scratch server's shared agent token, after
// the person every token of the rig acts for has signed in once, since a project's default
// assignee must be a person who has. P's issue carries a spec with one requirement and a
// Verification document with the check a role records its result on.
func (r *dispatchBrokerRig) setUp() {
	r.t.Helper()
	r.call(http.MethodGet, "/api/v1/whoami", true, nil, http.StatusOK, nil)
	for _, project := range []struct{ key, name string }{{rigP, "The rig's project P"}, {rigQ, "The rig's project Q"}} {
		r.call(http.MethodPost, "/api/v1/projects", false, map[string]any{
			"key": project.key, "name": project.name, "default_assignee": r.broker.Operator, "actor": rigSetupActor,
		}, http.StatusCreated, nil)
	}
	r.pIssue = r.createIssue(rigP, "Every write a role makes on its own issue",
		"The issue every role write of the rig addresses.\n\n:::requirement{#r1}\nA role writes every kind with its own token.\n:::\n")
	r.pDaemon = r.createIssue(rigP, "The daemon's own reads and writes on an issue of its project", "")
	r.qIssue = r.createIssue(rigQ, "An issue of the other project", "")
	r.call(http.MethodPost, "/api/v1/issues/"+r.pIssue+"/artifacts", false, map[string]any{
		"name": "verification.md", "actor": rigSetupActor,
		"content": fmt.Sprintf("How %s is proved.\n\n:::check{#%s serves=\"dispatch://%s/spec#b-r1\" surface=\"api\" run=\"bash scripts/e2e/dispatch-broker.sh\" negative_control=\"A token of another project is refused 403 PROJECT_SCOPE.\"}\nA role records a result with its own token.\n:::\n",
			r.pIssue, rigCheck, r.pIssue),
	}, http.StatusCreated, nil)
}

// createIssue creates an issue of project titled title, with spec when it is not empty, and
// answers its key.
func (r *dispatchBrokerRig) createIssue(project, title, spec string) string {
	r.t.Helper()
	body := map[string]any{"project": project, "title": title, "actor": rigSetupActor}
	if spec != "" {
		body["spec"] = spec
	}
	var created struct {
		Key string `json:"key"`
	}
	r.call(http.MethodPost, "/api/v1/issues", false, body, http.StatusCreated, &created)
	if created.Key == "" {
		r.t.Fatalf("Dispatch created the issue %q with no key", title)
	}
	return created.Key
}

// call sends one request to Dispatch directly, outside the proxy, as the person every token of
// the rig acts for (person) or with the shared agent token, and requires want.
func (r *dispatchBrokerRig) call(method, path string, person bool, body any, want int, into any) {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, r.dispatch.base+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if person {
		request.Header.Set("X-Dispatch-User", r.broker.Operator)
	} else {
		request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	}
	response, err := r.direct.Do(request)
	if err != nil {
		r.t.Fatalf("Dispatch %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatalf("read Dispatch's answer to %s %s: %v", method, path, err)
	}
	if response.StatusCode != want {
		r.t.Fatalf("Dispatch %s %s = %d %s, want %d", method, path, response.StatusCode, answer, want)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			r.t.Fatalf("decode Dispatch's answer to %s %s: %v", method, path, err)
		}
	}
}

// askID is the id of the ask on issue whose question starts with question.
func (r *dispatchBrokerRig) askID(issue, question string) string {
	r.t.Helper()
	var asks []struct {
		ID       string `json:"id"`
		Question string `json:"question"`
	}
	r.call(http.MethodGet, "/api/v1/issues/"+issue+"/asks?state=all", false, nil, http.StatusOK, &asks)
	for _, ask := range asks {
		if strings.HasPrefix(ask.Question, question) {
			return ask.ID
		}
	}
	r.t.Errorf("%s holds no ask whose question starts %q", issue, question)
	return ""
}

// artifacts is the id of every document and file of issue, by its name and by its slug.
func (r *dispatchBrokerRig) artifacts(issue string) map[string]string {
	r.t.Helper()
	var listed []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	r.call(http.MethodGet, "/api/v1/issues/"+issue+"/artifacts", false, nil, http.StatusOK, &listed)
	ids := map[string]string{}
	for _, artifact := range listed {
		ids[artifact.Name], ids[artifact.Slug] = artifact.ID, artifact.ID
	}
	return ids
}

// directMessage is a message the person sends session in its own conversation, which belongs to
// no issue, as the Agents page sends one; a role's reply to it carries its pictures into that
// conversation. The rig's listener knows no session (rigListener), so the delivery attempt fails
// "no live session" for that session, which a reply of that session does not read.
func (r *dispatchBrokerRig) directMessage(session string) string {
	r.t.Helper()
	var message struct {
		ID string `json:"id"`
	}
	r.call(http.MethodPost, "/api/v1/agents/"+session+"/messages", true, map[string]any{
		"body": "Send me a picture of what you see.", "delivery": "btw",
	}, http.StatusCreated, &message)
	if message.ID == "" {
		r.t.Fatalf("Dispatch answered the message to %s with no id", session)
	}
	return message.ID
}

// picture is a PNG of the run's own, which a conversation's upload carries.
func (r *dispatchBrokerRig) picture() string {
	r.t.Helper()
	path := filepath.Join(r.dir, "picture.png")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	canvas := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			canvas.Set(x, y, color.RGBA{R: uint8(60 * x), G: uint8(60 * y), B: 160, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return path
}

// during runs do and answers every request the proxy recorded while it ran, reading each broker
// token those requests carried back at the broker's introspection while the token is live.
func (r *dispatchBrokerRig) during(do func()) []proxyRecord {
	r.t.Helper()
	mark := r.proxy.count()
	do()
	records := r.proxy.since(mark)
	for _, record := range records {
		if record.Scheme == "DPoP" {
			r.introspect(record.Token)
		}
	}
	return records
}

// introspect asks the broker what the token of digest is (ciphertest.Rig.Introspect). A token
// already read is not read again.
func (r *dispatchBrokerRig) introspect(digest string) rigToken {
	r.t.Helper()
	if known, ok := r.tokens[digest]; ok {
		return known
	}
	token := rigToken(r.broker.Introspect(r.t, r.proxy.token(digest)))
	r.tokens[digest] = token
	return token
}

// rigRole is one role in an issue pod: its session, its pod, its home, the agent-secrets key
// directory holding its key.pem, and the file holding its pod's subject token for the exchange.
type rigRole struct {
	name, session, pod    string
	home, keyDir, subject string
	key                   *ecdsa.PrivateKey
}

// newRole makes a role's directories and its key, written as `agent-secrets keygen` writes one: a
// PKCS#8 P-256 PEM named key.pem, 0600, in a 0700 directory.
func (r *dispatchBrokerRig) newRole(name, session, pod string) *rigRole {
	r.t.Helper()
	dir := filepath.Join(r.dir, "roles", name)
	role := &rigRole{
		name: name, session: session, pod: pod, home: filepath.Join(dir, "home"),
		keyDir: filepath.Join(dir, "agent-secrets"), subject: filepath.Join(dir, "exchange-token"),
	}
	for _, made := range []string{role.home, role.keyDir} {
		if err := os.MkdirAll(made, 0o700); err != nil {
			r.t.Fatal(err)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		r.t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(role.keyDir, cipherclient.RoleKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		r.t.Fatal(err)
	}
	role.key = key
	return role
}

// writeSubject writes token as the role's pod's subject token, where the kubelet projects it.
func (role *rigRole) writeSubject(t *testing.T, token string) {
	t.Helper()
	if err := os.WriteFile(role.subject, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// registeredRole is a role of an ExchangeRolesAccount pod of P, whose session the daemon's own
// account registered for that pod at the broker with the thumbprint of the role's key
// (RegisterPodSession), as internal/cipher/contract_test.go registers one.
func (r *dispatchBrokerRig) registeredRole(name string) *rigRole {
	r.t.Helper()
	suffix := strings.ToLower(randomSuffix(r.t)[:6])
	role := r.newRole(name, "rig-"+name+"-"+suffix, "rig-pod-"+name+"-"+suffix)
	thumbprint := keyThumbprint(r.t, role.key)
	registrar := &cipher.Client{URL: r.broker.URL, TokenFile: r.registrar}
	podToken := r.broker.MintAccountPodToken(r.t, ciphertest.ExchangeRolesAccount, role.pod, time.Hour)
	if _, err := registrar.RegisterPodSession(context.Background(), role.pod, podToken, role.session, thumbprint, 1); err != nil {
		r.t.Fatalf("the daemon's registration of %s for %s: %v", role.session, role.pod, err)
	}
	role.writeSubject(r.t, r.broker.MintSubjectToken(r.t, ciphertest.ExchangeRolesAccount, role.pod))
	return role
}

// otherProjectRole is a role of an OtherProjectAccount pod: the session that entry fixes, of
// project Q, with a key of its own.
func (r *dispatchBrokerRig) otherProjectRole() *rigRole {
	r.t.Helper()
	role := r.newRole("other", ciphertest.OtherProjectSession, "rig-pod-other")
	role.writeSubject(r.t, r.broker.MintSubjectToken(r.t, ciphertest.OtherProjectAccount, role.pod))
	return role
}

// environment is a role's whole environment: exactly the DISPATCH_ and AGENT_SECRETS_ variables
// the launch golden names, with the rig's values, the two the plugin sets in a role's shell
// (DISPATCH_SESSION_ID, its session, and DISPATCH_HOST, packages/envoy-client/src/dispatch-cli.ts),
// and HOME and PATH. A golden variable the rig has no value for fails the test: the launch moved
// and the rig must move with it. With sharedTokenFile set it is the launch of a deployment
// without dispatch_token_broker instead, whose role reads the shared token through
// DISPATCH_TOKEN_FILE where DISPATCH_TOKEN_BROKER stands under the setting
// (internal/runtime/sandbox/manifest.go, brokerOwned).
func (r *dispatchBrokerRig) environment(role *rigRole, sharedTokenFile string) []string {
	r.t.Helper()
	values := map[string]string{
		"DISPATCH_URL":                   r.url,
		"DISPATCH_TOKEN_BROKER":          r.launch["DISPATCH_TOKEN_BROKER"],
		cipherclient.BrokerURLVar:        r.broker.URL,
		cipherclient.IdentityVar:         r.launch[cipherclient.IdentityVar],
		cipherclient.MachineTokenFileVar: role.subject,
		cipherclient.KeyDirVar:           role.keyDir,
	}
	env := []string{
		"DISPATCH_SESSION_ID=" + role.session, "DISPATCH_HOST=omp", "HOME=" + role.home, "PATH=" + os.Getenv("PATH"),
	}
	for name := range r.launch {
		value, ok := values[name]
		if !ok {
			r.t.Fatalf("the launch golden names %s, which the rig has no value for", name)
		}
		if name == "DISPATCH_TOKEN_BROKER" && sharedTokenFile != "" {
			name, value = "DISPATCH_TOKEN_FILE", sharedTokenFile
		}
		env = append(env, name+"="+value)
	}
	slices.Sort(env)
	return env
}

// dispatchNames is the name of every variable of env whose name holds DISPATCH, without a value.
func dispatchNames(env []string) []string {
	var names []string
	for _, entry := range env {
		if name, _, _ := strings.Cut(entry, "="); strings.Contains(name, "DISPATCH") {
			names = append(names, name)
		}
	}
	return names
}

// rigRun is what one run of the dispatch command printed and its exit status.
type rigRun struct {
	stdout, stderr string
	code           int
}

// outcome is the run in one line: its exit status and the first line it printed.
func (run rigRun) outcome() string {
	printed := strings.TrimSpace(run.stdout + "\n" + run.stderr)
	line, _, _ := strings.Cut(printed, "\n")
	return fmt.Sprintf("exit %d: %s", run.code, line)
}

// command runs `bun packages/envoy-client/bin/dispatch.ts args...` in env, from the role's home,
// as an Oh My Pi session's shell runs `dispatch` (handoff_records_live_test.go's cliOut).
func (r *dispatchBrokerRig) command(role *rigRole, env []string, args ...string) rigRun {
	r.t.Helper()
	command := exec.Command("bun", append([]string{filepath.Join(r.root, "packages", "envoy-client", "bin", "dispatch.ts")}, args...)...)
	command.Dir = role.home
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	run := rigRun{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			r.t.Fatalf("run dispatch %s under bun: %v", strings.Join(args, " "), err)
		}
		run.code = exit.ExitCode()
	}
	return run
}

// daemonSubject writes a subject token of a DaemonExchangeAccount pod, the daemon's own pod
// identity, to a file of its own and answers the file and the pod.
func (r *dispatchBrokerRig) daemonSubject() (string, string) {
	r.t.Helper()
	pod := "rig-daemon-pod-" + strings.ToLower(randomSuffix(r.t)[:6])
	return r.file("daemon-exchange-token-"+pod, r.broker.MintSubjectToken(r.t, ciphertest.DaemonExchangeAccount, pod)), pod
}

// daemonClient is the daemon's Dispatch client of project P as boot builds it under
// dispatch_token_broker (dispatchCredential): its pod identity, the subject token in the file
// subject, traded at the rig's broker, read back by Dispatch's whoami through the proxy and
// checked, and the transport that client presents its token through.
func (r *dispatchBrokerRig) daemonClient(subject string) (*dispatch.HTTPClient, http.RoundTripper) {
	r.t.Helper()
	cfg := config.Config{Project: rigP, DispatchURL: r.url, DispatchTokenBroker: true}
	transport, err := dispatchCredential(context.Background(), cfg, "", getenvOf(map[string]string{
		cipherclient.BrokerURLVar: r.broker.URL, cipherclient.IdentityVar: cipherclient.IdentityKubernetes,
		cipherclient.MachineTokenFileVar: subject,
	}), quietLogger())
	if err != nil {
		r.t.Fatalf("the daemon's boot under dispatch_token_broker: %v", err)
	}
	return dispatch.New(cfg.DispatchURL, transport), transport
}

// sharedDaemonClient is the daemon's Dispatch client of project P as boot builds it without
// dispatch_token_broker: the bearer dispatch_token_file names, the shared agent token.
func (r *dispatchBrokerRig) sharedDaemonClient(sharedTokenFile string) *dispatch.HTTPClient {
	r.t.Helper()
	cfg := config.Config{Project: rigP, DispatchURL: r.url, DispatchTokenFile: sharedTokenFile}
	transport, err := dispatchCredential(context.Background(), cfg, r.dispatch.token, getenvOf(nil), quietLogger())
	if err != nil {
		r.t.Fatalf("the daemon's boot without dispatch_token_broker: %v", err)
	}
	return dispatch.New(cfg.DispatchURL, transport)
}

// otherProjectDaemonClient is the daemon's client built from the same pieces (cipherclient's
// client and token transport, dispatch.New) over OtherProjectAccount's pod identity, whose entry
// fixes the session OtherProjectSession of project Q: K4's control, a token of another project.
func (r *dispatchBrokerRig) otherProjectDaemonClient() *dispatch.HTTPClient {
	r.t.Helper()
	subject := r.file("other-project-daemon-token", r.broker.MintSubjectToken(r.t, ciphertest.OtherProjectAccount, "rig-pod-other-daemon"))
	getenv := getenvOf(map[string]string{cipherclient.IdentityVar: cipherclient.IdentityKubernetes, cipherclient.MachineTokenFileVar: subject})
	identity, err := cipherclient.MachineIdentity(getenv)
	if err != nil {
		r.t.Fatal(err)
	}
	key, err := cipherclient.MachineKey(getenv)
	if err != nil {
		r.t.Fatal(err)
	}
	client, err := cipherclient.New(cipherclient.Config{
		BrokerURL: cipheraddr.New(r.broker.URL), Key: key, Subject: identity, Session: ciphertest.OtherProjectSession,
		NoLauncherCredential: true, HTTP: cipherclient.HTTPClient(), Log: quietLogger(),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return dispatch.New(r.url, cipherclient.NewTokenTransport(client, http.DefaultTransport))
}

// rigClients is scripts/e2e/dispatch-broker-clients.ts running in a role's environment: the
// client the dispatch command builds and the client the pi-envoy plugin builds, in one process
// that lives as long as the test.
type rigClients struct {
	t      *testing.T
	stdin  io.WriteCloser
	lines  chan string
	stderr *syncBuffer
	next   int
}

// clientAnswer is the driver's answer to one write.
type clientAnswer struct {
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Answer  json.RawMessage `json:"answer"`
	Message string          `json:"message"`
	Status  *int            `json:"status"`
	Code    *string         `json:"code"`
}

// field is the string at path in the write's answer, "" where there is none.
func (a clientAnswer) field(path ...string) string {
	var value any
	if json.Unmarshal(a.Answer, &value) != nil {
		return ""
	}
	for _, name := range path {
		fields, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = fields[name]
	}
	text, _ := value.(string)
	return text
}

// outcome is the answer in one line: ok, or the failed write's message.
func (a clientAnswer) outcome() string {
	if a.OK {
		return "ok"
	}
	status := "no answer from Dispatch"
	if a.Status != nil {
		status = strconv.Itoa(*a.Status)
	}
	return fmt.Sprintf("failed (%s): %s", status, a.Message)
}

// startClients starts the driver in env, from the role's home, and waits for it to say both
// clients' settings resolved.
func (r *dispatchBrokerRig) startClients(role *rigRole, env []string) *rigClients {
	t := r.t
	t.Helper()
	command := exec.Command("bun", filepath.Join(r.root, "scripts", "e2e", "dispatch-broker-clients.ts"))
	command.Dir = role.home
	command.Env = env
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &rigClients{t: t, stdin: stdin, lines: make(chan string, 16), stderr: &syncBuffer{}}
	command.Stderr = c.stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start the clients' driver: %v", err)
	}
	read := make(chan struct{})
	go func() {
		defer close(read)
		defer close(c.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for scanner.Scan() {
			c.lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-read:
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-read
		}
		_ = command.Wait()
	})
	var ready struct {
		Ready   bool   `json:"ready"`
		Session string `json:"session"`
	}
	if line := c.read(2 * time.Minute); json.Unmarshal([]byte(line), &ready) != nil || !ready.Ready || ready.Session != role.session {
		t.Fatalf("the clients' driver started with %q, want it ready as %s\nits stderr:\n%s", line, role.session, c.stderr.String())
	}
	return c
}

// read is the driver's next line, failing the test when none comes within wait.
func (c *rigClients) read(wait time.Duration) string {
	c.t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatalf("the clients' driver ended\nits stderr:\n%s", c.stderr.String())
		}
		return line
	case <-time.After(wait):
		c.t.Fatalf("the clients' driver answered nothing in %s\nits stderr:\n%s", wait, c.stderr.String())
	}
	return ""
}

// write asks client ("cli" or "plugin") to make one write, its method called with args, and
// answers the driver's answer.
func (c *rigClients) write(client, method string, args ...any) clientAnswer {
	c.t.Helper()
	c.next++
	id := strconv.Itoa(c.next)
	if args == nil {
		args = []any{}
	}
	request, err := json.Marshal(map[string]any{"id": id, "client": client, "method": method, "args": args})
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.stdin.Write(append(request, '\n')); err != nil {
		c.t.Fatalf("ask the clients' driver: %v\nits stderr:\n%s", err, c.stderr.String())
	}
	line := c.read(3 * time.Minute)
	var answer clientAnswer
	if err := json.Unmarshal([]byte(line), &answer); err != nil || answer.ID != id {
		c.t.Fatalf("the clients' driver answered %q to request %s (%v)", line, id, err)
	}
	return answer
}

// proxyRecord is one request through the proxy: when it came, at which of the proxy's two
// addresses, its method and route, its Authorization scheme, the digest of the token it presented,
// and the status and code Dispatch answered.
type proxyRecord struct {
	At      time.Time
	Address string
	Method  string
	Route   string
	Scheme  string
	Token   string
	Status  int
	Code    string
}

// recordingProxy is the proxy in front of Dispatch: it forwards each request as it came and keeps
// a record of it. The tokens themselves are kept apart, by digest, for the broker's
// introspection alone; no table prints one.
//
// A request's record takes its place when the request arrives, and its status and code only once
// the handler is done with Dispatch's answer, gone out whole or broken off, which can be after the
// client has read that answer and returned: the client stops reading at the body's last byte, while
// the handler is still writing.
// So a reader that marks the records, has a client write and then reads them (since) waits for
// every request it may have read in flight to land, rather than reading before its record does.
type recordingProxy struct {
	mu      sync.Mutex
	landed  *sync.Cond // signalled each time a request's record lands
	target  *url.URL
	records []proxyRecord
	// inFlight is the index in records of each request whose handler has not finished.
	inFlight map[int]struct{}
	tokens   map[string]string
}

// newRecordingProxy is a proxy with no record, pointed at no Dispatch yet (forward).
func newRecordingProxy() *recordingProxy {
	p := &recordingProxy{inFlight: map[int]struct{}{}, tokens: map[string]string{}}
	p.landed = sync.NewCond(&p.mu)
	return p
}

// forward points the proxy at Dispatch.
func (p *recordingProxy) forward(base string) {
	target, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

// at is the proxy's handler at the address it names.
func (p *recordingProxy) at(address string) http.Handler {
	forward := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			p.mu.Lock()
			target := p.target
			p.mu.Unlock()
			out.SetURL(target)
		},
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		record := proxyRecord{At: time.Now(), Address: address, Method: request.Method, Route: request.URL.RequestURI()}
		record.Scheme, record.Token = p.credential(request.Header.Get("Authorization"))
		p.mu.Lock()
		ready := p.target != nil
		index := len(p.records)
		p.records = append(p.records, record)
		p.inFlight[index] = struct{}{}
		p.mu.Unlock()
		answer := &answerRecorder{ResponseWriter: w}
		// The record lands on every way out of this handler. ReverseProxy, under an HTTP server,
		// aborts the handler with panic(http.ErrAbortHandler) when an answer breaks off part way,
		// so a landing after ServeHTTP would never run and every since() from its mark would wait
		// for good. The deferred landing records what the proxy saw, and the panic goes on to the
		// server as before.
		defer func() {
			record.Status = answer.status
			if record.Status == 0 {
				record.Status = http.StatusOK
			}
			if record.Status/100 != 2 {
				var refusal struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(answer.body.Bytes(), &refusal) == nil {
					record.Code = refusal.Code
				}
			}
			p.mu.Lock()
			p.records[index] = record
			delete(p.inFlight, index)
			p.landed.Broadcast()
			p.mu.Unlock()
		}()
		if ready {
			forward.ServeHTTP(answer, request)
		} else {
			http.Error(answer, "the rig's proxy has no Dispatch yet", http.StatusServiceUnavailable)
		}
	})
}

// credential is the scheme of an Authorization header and the digest of its token, keeping the
// token for introspection: "none" and no digest where the request carried no header.
func (p *recordingProxy) credential(header string) (string, string) {
	if header == "" {
		return "none", ""
	}
	scheme, token, _ := strings.Cut(header, " ")
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])[:12]
	p.mu.Lock()
	p.tokens[digest] = token
	p.mu.Unlock()
	return scheme, digest
}

// token is the token whose digest is digest.
func (p *recordingProxy) token(digest string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokens[digest]
}

// count is how many requests have reached the proxy, those still in flight included.
func (p *recordingProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

// since is every request recorded after the first mark, once each of them has landed. A caller
// reads it after the writes it made returned, so each request of those writes has reached the
// proxy and taken its place; only its answer can still be going out, and that ends.
func (p *recordingProxy) since(mark int) []proxyRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.inFlightFrom(mark) {
		p.landed.Wait()
	}
	return append([]proxyRecord(nil), p.records[mark:]...)
}

// inFlightFrom is whether a request recorded at mark or after it is still in flight. The caller
// holds p.mu.
func (p *recordingProxy) inFlightFrom(mark int) bool {
	for index := range p.inFlight {
		if index >= mark {
			return true
		}
	}
	return false
}

// answerRecorder is a ResponseWriter that keeps the status written and the first bytes of the
// body, where a refusal's code is.
type answerRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (a *answerRecorder) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
	a.ResponseWriter.WriteHeader(status)
}

func (a *answerRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if room := 8<<10 - a.body.Len(); room > 0 {
		a.body.Write(b[:min(len(b), room)])
	}
	return a.ResponseWriter.Write(b)
}

func (a *answerRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// rigWrite is one write a test asked of a client, or one read of the daemon's, with the requests
// the proxy recorded while it ran and how the client ended it.
type rigWrite struct {
	write   string
	client  string
	read    bool
	at      time.Duration
	ok      bool
	outcome string
	err     error
	records []proxyRecord
}

// mutations is every request of the write but a GET: the write itself, past the reads a client
// makes on its way.
func (w rigWrite) mutations() []proxyRecord {
	var written []proxyRecord
	for _, record := range w.records {
		if record.Method != http.MethodGet {
			written = append(written, record)
		}
	}
	return written
}

// daemonCall runs one method of the daemon's client and answers it as a rigWrite.
func (r *dispatchBrokerRig) daemonCall(name string, read bool, do func() error) rigWrite {
	r.t.Helper()
	var err error
	records := r.during(func() { err = do() })
	outcome := "ok"
	if err != nil {
		outcome = "failed: " + err.Error()
	}
	return rigWrite{write: name, client: "the daemon's client", read: read, ok: err == nil, outcome: outcome, err: err, records: records}
}

// refusal is the status and code of Dispatch's refusal err carries, 0 and "" for none.
func refusal(err error) (int, string) {
	var refused *dispatch.Error
	if errors.As(err, &refused) {
		return refused.Status, refused.Code
	}
	return 0, ""
}

// rigReport is a test's result, written to TMPDIR as Markdown.
type rigReport struct {
	r    *dispatchBrokerRig
	text strings.Builder
}

func (r *dispatchBrokerRig) report(title string) *rigReport {
	report := &rigReport{r: r}
	fmt.Fprintf(&report.text, "# %s\n\n", title)
	return report
}

// say adds a paragraph.
func (p *rigReport) say(format string, args ...any) {
	fmt.Fprintf(&p.text, format+"\n\n", args...)
}

// quote adds text as written, in a fenced block.
func (p *rigReport) quote(text string) {
	fmt.Fprintf(&p.text, "```\n%s\n```\n\n", strings.TrimRight(text, "\n"))
}

// writes adds a table of writes under heading: one row for each request, beside the write that
// made it; a write that sent nothing is one row naming no route. start, when it is not the zero
// time, adds the column of each write's time since it.
func (p *rigReport) writes(heading string, writes []rigWrite, start time.Time) {
	fmt.Fprintf(&p.text, "## %s\n\n", heading)
	columns := []string{"write", "client", "outcome", "address", "route", "Authorization", "token", "status", "code", "names", "token expiry"}
	if !start.IsZero() {
		columns = append([]string{"at"}, columns...)
	}
	fmt.Fprintf(&p.text, "| %s |\n|%s\n", strings.Join(columns, " | "), strings.Repeat(" --- |", len(columns)))
	for _, write := range writes {
		label := write.write
		if write.read {
			label += " (read)"
		}
		records := write.records
		if len(records) == 0 {
			records = []proxyRecord{{Route: "(no request)"}}
		}
		for i, record := range records {
			row := []string{label, write.client, write.outcome}
			if i > 0 {
				row = []string{"", "", ""}
			}
			status, expiry := "", ""
			if record.Status != 0 {
				status = strconv.Itoa(record.Status)
			}
			if record.Scheme == "DPoP" {
				token := p.r.tokens[record.Token]
				expiry = fmt.Sprintf("%s (session %s, projects %s, active when read %t)",
					token.expiry().UTC().Format(time.RFC3339), token.Session, strings.Join(token.Projects, " "), token.Active)
			}
			names := ""
			if record.Route != "(no request)" {
				names = p.r.names(record)
			}
			row = append(row, record.Address, strings.TrimSpace(record.Method+" "+record.Route), record.Scheme, record.Token, status, record.Code, names, expiry)
			if !start.IsZero() {
				at := ""
				if i == 0 {
					at = fmt.Sprintf("%.0f s", write.at.Seconds())
				}
				row = append([]string{at}, row...)
			}
			for j, cell := range row {
				row[j] = strings.ReplaceAll(strings.ReplaceAll(cell, "|", `\|`), "\n", " ")
			}
			fmt.Fprintf(&p.text, "| %s |\n", strings.Join(row, " | "))
		}
	}
	p.text.WriteString("\n")
}

// write puts the report in a fresh directory under TMPDIR (os.MkdirTemp with no base, the
// placement testtmp's check allows) under name, and logs its path. The directory outlives the
// test, since the person reads the table after the run.
func (p *rigReport) write(name string) string {
	p.r.t.Helper()
	dir, err := os.MkdirTemp("", "dispatch-broker-"+name+"-")
	if err != nil {
		p.r.t.Fatalf("make a directory for the %s table: %v", name, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("dispatch-broker-%s-%s.md", name, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(path, []byte(p.text.String()), 0o644); err != nil {
		p.r.t.Fatalf("write the %s table: %v", name, err)
	}
	p.r.t.Logf("the %s table: %s", name, path)
	return path
}

// names says what a request addresses: an issue, a project or a document of P or Q, or something
// that belongs to no issue — a session's own conversation, a direct message, the caller itself.
func (r *dispatchBrokerRig) names(record proxyRecord) string {
	route, _, _ := strings.Cut(record.Route, "?")
	parts := strings.Split(strings.TrimPrefix(route, "/api/v1/"), "/")
	switch {
	case len(parts) >= 2 && parts[0] == "issues":
		project, _, _ := strings.Cut(parts[1], "-")
		return "issue " + parts[1] + " of " + project
	case len(parts) >= 2 && parts[0] == "projects":
		return "project " + parts[1]
	case len(parts) >= 2 && parts[0] == "artifacts":
		if r.documentsOfP[parts[1]] {
			return "a document of " + rigP
		}
		return "a document outside " + rigP
	case len(parts) >= 2 && parts[0] == "agents":
		return "no issue: session " + parts[1] + "'s own conversation"
	case len(parts) >= 2 && parts[0] == "messages":
		return "no issue: a direct message"
	case len(parts) >= 2 && parts[0] == "handoffs":
		return "a handoff record"
	case len(parts) >= 2 && parts[0] == "asks":
		return "an ask"
	}
	return "no issue"
}

// namesP is whether a request addresses an issue, a project or a document of P.
func (r *dispatchBrokerRig) namesP(record proxyRecord) bool {
	names := r.names(record)
	return strings.HasSuffix(names, " of "+rigP) || names == "project "+rigP
}

// TestTheDaemonWritesAsItselfOnlyInItsProject is K2: under dispatch_token_broker the daemon's own
// client, with the identity of P's daemon account, posts an issue message (PostMessage, its one
// conversation write) on an issue of P and then on an issue of Q, and `dispatch whoami` runs in a
// role's environment of P; the result names each request's Authorization scheme and each answer's
// status, whoami's output, and the names of the role's variables that hold DISPATCH.
//
// Controls: the write to Q is refused 403 PROJECT_SCOPE and the write to P is accepted, so the
// refusal is the token's scope and not a broken client; a write of the same client whose proof
// names an origin outside DISPATCH_PROOF_ORIGINS is refused 401 BROKER_TOKEN_PROOF_INVALID; and
// with the setting off the same two writes carry Bearer and are both accepted, and whoami says it
// runs under the shared token.
func TestTheDaemonWritesAsItselfOnlyInItsProject(t *testing.T) {
	r := newDispatchBrokerRig(t, 15*time.Minute)
	ctx := context.Background()
	report := r.report("K2: the daemon writes as itself and only in its project")

	subject, _ := r.daemonSubject()
	var client *dispatch.HTTPClient
	var transport http.RoundTripper
	boot := rigWrite{write: "boot: the trade and the whoami check of the daemon's own token", client: "the daemon's client", ok: true, outcome: "ok"}
	boot.records = r.during(func() { client, transport = r.daemonClient(subject) })
	onP := r.daemonCall("PostMessage on "+r.pIssue, false, func() error {
		return client.PostMessage(ctx, r.pIssue, "The daemon's notice on an issue of its own project.")
	})
	onQ := r.daemonCall("PostMessage on "+r.qIssue, false, func() error {
		return client.PostMessage(ctx, r.qIssue, "The daemon's notice on an issue of another project.")
	})
	outside := r.daemonCall("PostMessage on "+r.pIssue+", sent to an origin DISPATCH_PROOF_ORIGINS does not name", false, func() error {
		return dispatch.New(r.outside, transport).PostMessage(ctx, r.pIssue, "The daemon's notice at an address Dispatch was not told of.")
	})
	role := r.registeredRole("k2")
	on := r.environment(role, "")
	whoamiOn := r.command(role, on, "whoami")

	shared := r.file("dispatch-shared-token", r.dispatch.token)
	bearer := r.sharedDaemonClient(shared)
	offP := r.daemonCall("PostMessage on "+r.pIssue, false, func() error {
		return bearer.PostMessage(ctx, r.pIssue, "The daemon's notice under the shared token, on an issue of its project.")
	})
	offQ := r.daemonCall("PostMessage on "+r.qIssue, false, func() error {
		return bearer.PostMessage(ctx, r.qIssue, "The daemon's notice under the shared token, on an issue of another project.")
	})
	off := r.environment(role, shared)
	whoamiOff := r.command(role, off, "whoami")

	report.say("The daemon's client of project %s, with dispatch_token_broker on: its pod identity is the rig's DaemonExchangeAccount, traded at the broker %s; Dispatch is reached through the proxy %s, which DISPATCH_PROOF_ORIGINS names, and %s, which it does not.", rigP, r.broker.URL, r.url, r.outside)
	report.writes("The setting on", []rigWrite{boot, onP, onQ, outside}, time.Time{})
	report.say("`dispatch whoami` in the environment of the role %s, a registered session of an ExchangeRolesAccount pod of %s (%s):", role.session, rigP, whoamiOn.outcome())
	report.quote(whoamiOn.stdout + whoamiOn.stderr)
	report.say("The variables of that environment whose name holds DISPATCH: %s.", strings.Join(dispatchNames(on), ", "))
	report.writes("The setting off: the shared token from dispatch_token_file", []rigWrite{offP, offQ}, time.Time{})
	report.say("`dispatch whoami` in the same role's environment with the setting off (%s):", whoamiOff.outcome())
	report.quote(whoamiOff.stdout + whoamiOff.stderr)
	report.say("The variables of that environment whose name holds DISPATCH: %s.", strings.Join(dispatchNames(off), ", "))
	report.write("k2")

	only := func(write rigWrite) proxyRecord {
		t.Helper()
		if len(write.records) != 1 {
			t.Fatalf("%s: the proxy recorded %d requests, want 1: %+v", write.write, len(write.records), write.records)
		}
		return write.records[0]
	}
	if record := only(onP); onP.err != nil || record.Scheme != "DPoP" || record.Status != http.StatusCreated {
		t.Errorf("under the setting the daemon's write to its own project %s went %s and was answered %d %s (%v), want DPoP and 201", r.pIssue, record.Scheme, record.Status, record.Code, onP.err)
	}
	if status, code := refusal(onQ.err); only(onQ).Scheme != "DPoP" || status != http.StatusForbidden || code != "PROJECT_SCOPE" {
		t.Errorf("under the setting the daemon's write to %s of %s was answered %d %s (%v), want 403 PROJECT_SCOPE", r.qIssue, rigQ, status, code, onQ.err)
	}
	if status, code := refusal(outside.err); only(outside).Address != "outside" || status != http.StatusUnauthorized || code != "BROKER_TOKEN_PROOF_INVALID" {
		t.Errorf("a write whose proof names %s, an origin DISPATCH_PROOF_ORIGINS does not name, was answered %d %s (%v), want 401 BROKER_TOKEN_PROOF_INVALID", r.outside, status, code, outside.err)
	}
	for _, write := range []rigWrite{offP, offQ} {
		if record := only(write); write.err != nil || record.Scheme != "Bearer" || record.Status != http.StatusCreated {
			t.Errorf("without the setting %s went %s and was answered %d %s (%v), want Bearer and 201", write.write, record.Scheme, record.Status, record.Code, write.err)
		}
	}
	if whoamiOn.code != 0 || !strings.Contains(whoamiOn.stdout, "acts for "+r.broker.Operator) || !strings.Contains(whoamiOn.stdout, "may write project "+rigP+" and no other") {
		t.Errorf("dispatch whoami in the role's environment under the setting = %s, want it acting for %s and writing project %s alone", whoamiOn.outcome(), r.broker.Operator, rigP)
	}
	for _, name := range dispatchNames(on) {
		if name == "DISPATCH_TOKEN" || name == "DISPATCH_TOKEN_FILE" {
			t.Errorf("the role's environment under the setting names %s: a role gets no Dispatch token at its start", name)
		}
	}
	if whoamiOff.code != 0 || !strings.Contains(whoamiOff.stdout, "runs under the shared token") {
		t.Errorf("dispatch whoami in the role's environment without the setting = %s, want it running under the shared token", whoamiOff.outcome())
	}
}

// k3Lifetime is how long an access token of K3's broker lives.
const k3Lifetime = 420 * time.Second

// k3Writes are the times after its start each client of K3 writes at: twice inside the first
// token's renewal window (renewed 120 s after it is traded, k3Lifetime less
// cipherclient.DefaultRenewBefore, token-client.ts's DEFAULT_RENEW_BEFORE_MS), once past it, and
// once past the first token's expiry.
var k3Writes = []time.Duration{0, 60 * time.Second, 200 * time.Second, 450 * time.Second}

// TestEveryClientRenewsAndStopsOnARefusal is K3: with the broker's access tokens living 420 s,
// three long-lived clients - the dispatch command's client and the pi-envoy plugin's, held in
// scripts/e2e/dispatch-broker-clients.ts, each writing a comment, and the daemon's, writing an
// issue message - each write at their start, after 60 s, after 200 s and after 450 s; the result
// holds each write's time, status and the expiry of the token it carried. Then each subject token
// file is rewritten with a token for an account the rig does not list, so the exchange is refused
// for good; each client tries one more write once the token it holds has expired, and the
// `dispatch` command runs one write in a process of its own; the result holds each client's
// message, that command's exit status and the proxy's record from the refusal on.
//
// Controls: for each client the writes at the start and at 60 s carry the same token, so a client
// that trades for every write fails; its writes carry at least two tokens, and its write at 450 s
// comes after the expiry of the token its first carried, so renewal is told from one trade per
// write; after the refusal each message names the broker's refusal, the command's exit status is
// not 0, and the proxy records no request at all from the refusal on.
//
// It takes about fifteen minutes, so it runs only when LEGION_E2E_TOKEN_RENEWAL=1.
func TestEveryClientRenewsAndStopsOnARefusal(t *testing.T) {
	if os.Getenv("LEGION_E2E_TOKEN_RENEWAL") != "1" {
		t.Skip("LEGION_E2E_TOKEN_RENEWAL is not 1: this check waits out three token lifetimes, about fifteen minutes, so it runs only when that variable asks for it")
	}
	r := newDispatchBrokerRig(t, k3Lifetime)
	ctx := context.Background()
	role := r.registeredRole("k3")
	env := r.environment(role, "")
	clients := r.startClients(role, env)
	subject, daemonPod := r.daemonSubject()
	daemonClient, _ := r.daemonClient(subject)

	type client struct {
		name  string
		write func() (bool, string, error)
	}
	comment := func(via string) func() (bool, string, error) {
		return func() (bool, string, error) {
			answer := clients.write(via, "comment", r.pIssue, map[string]any{
				"body":  "A comment of the " + via + " client, made as long after its start as the rig asked.",
				"actor": map[string]string{"kind": "session", "id": role.session},
			})
			return answer.OK, answer.outcome(), nil
		}
	}
	three := []client{
		{"the dispatch command's client", comment("cli")},
		{"the pi-envoy plugin's client", comment("plugin")},
		{"the daemon's client", func() (bool, string, error) {
			err := daemonClient.PostMessage(ctx, r.pIssue, "An issue message of the daemon's, made as long after its start as the rig asked.")
			if err != nil {
				return false, "failed: " + err.Error(), err
			}
			return true, "ok", nil
		}},
	}
	start := time.Now()
	written := map[string][]rigWrite{}
	var timed []rigWrite
	for _, after := range k3Writes {
		time.Sleep(time.Until(start.Add(after)))
		for _, c := range three {
			write := rigWrite{write: fmt.Sprintf("write after %.0f s", after.Seconds()), client: c.name, at: time.Since(start)}
			write.records = r.during(func() { write.ok, write.outcome, write.err = c.write() })
			written[c.name] = append(written[c.name], write)
			timed = append(timed, write)
		}
	}

	// The refusal: each subject token file now holds a token of an account the rig's exchange does
	// not list, and each client writes once the token it holds has expired. The proxy's record is
	// read from the rewrite on.
	mark := r.proxy.count()
	rewritten := time.Now()
	role.writeSubject(t, r.broker.MintSubjectToken(t, rigUnlistedAccount, role.pod))
	if err := os.WriteFile(subject, []byte(r.broker.MintSubjectToken(t, rigUnlistedAccount, daemonPod)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var held time.Time
	for _, writes := range written {
		last := writes[len(writes)-1]
		for _, record := range last.records {
			if expiry := r.tokens[record.Token].expiry(); expiry.After(held) {
				held = expiry
			}
		}
	}
	time.Sleep(time.Until(held.Add(5 * time.Second)))
	refusedFrom := time.Now()
	var refused []rigWrite
	for _, c := range three {
		write := rigWrite{write: "write once the token held has expired", client: c.name, at: time.Since(start)}
		write.ok, write.outcome, write.err = c.write()
		refused = append(refused, write)
	}
	command := r.command(role, env, "comment", "--issue", r.pIssue, "--body", "A comment of the dispatch command's own process, after the refusal.")
	refused = append(refused, rigWrite{write: "dispatch comment, a process of its own", client: "the dispatch command", at: time.Since(start), ok: command.code == 0, outcome: command.outcome()})
	after := r.proxy.since(mark)

	report := r.report("K3: every client renews its token, and a refused renewal stops its writes")
	var offsets []string
	for _, after := range k3Writes[1:] {
		offsets = append(offsets, fmt.Sprintf("%.0f s", after.Seconds()))
	}
	report.say("The broker's access tokens live %.0f s. The clients started at %s; each wrote at its start and after %s.", k3Lifetime.Seconds(), start.UTC().Format(time.RFC3339), strings.Join(offsets, ", "))
	report.writes("The timed writes", timed, start)
	report.say("At %s each subject token file was rewritten with a token of %s, which the rig's exchange does not list; the writes below came at %s, once the latest token held had expired (%s).",
		rewritten.UTC().Format(time.RFC3339), rigUnlistedAccount, refusedFrom.UTC().Format(time.RFC3339), held.UTC().Format(time.RFC3339))
	report.writes("The writes after the refusal", refused, start)
	report.say("The dispatch command's own process printed (%s):", command.outcome())
	report.quote(command.stdout + command.stderr)
	report.say("The proxy recorded %d requests from the rewrite on.", len(after))
	if len(after) > 0 {
		report.writes("The proxy's record from the rewrite on", []rigWrite{{write: "any", client: "any", records: after}}, start)
	}
	report.write("k3")

	for _, c := range three {
		writes := written[c.name]
		token := func(write rigWrite) string {
			if len(write.records) != 1 || write.records[0].Status/100 != 2 {
				t.Errorf("%s's %s = %s, the proxy recorded %+v; want one request, answered 2xx", c.name, write.write, write.outcome, write.records)
				return ""
			}
			return write.records[0].Token
		}
		tokens := make([]string, len(writes))
		for i, write := range writes {
			tokens[i] = token(write)
		}
		if tokens[0] == "" || tokens[0] != tokens[1] {
			t.Errorf("%s carried %q at its start and %q after 60 s, want the same token: a client that trades for every write fails here", c.name, tokens[0], tokens[1])
		}
		if distinct := slices.Compact(slices.Sorted(slices.Values(tokens))); len(distinct) < 2 {
			t.Errorf("%s carried one token throughout (%v), want it renewed", c.name, tokens)
		}
		if first := r.tokens[tokens[0]].expiry(); first.IsZero() || !start.Add(writes[3].at).After(first) {
			t.Errorf("%s's write after 450 s came at %s, want it after the expiry of the token its first write carried, %s", c.name, start.Add(writes[3].at).UTC().Format(time.RFC3339), first.UTC().Format(time.RFC3339))
		}
	}
	for _, write := range refused {
		if write.ok {
			t.Errorf("%s's %s succeeded (%s), want it stopped by the broker's refusal", write.client, write.write, write.outcome)
			continue
		}
		if write.client != "the dispatch command" && !strings.Contains(write.outcome, "the secrets broker refused") {
			t.Errorf("%s's %s failed with %q, want the message to name the broker's refusal", write.client, write.write, write.outcome)
		}
	}
	if command.code == 0 || !strings.Contains(command.stdout+command.stderr, "the secrets broker refused") {
		t.Errorf("the dispatch command's own process after the refusal = %s, want a non-zero exit naming the broker's refusal", command.outcome())
	}
	if len(after) != 0 {
		t.Errorf("the proxy recorded %d requests from the refusal on, want none: %+v", len(after), after)
	}
}

// roleRefs are the objects of P the role script's run with a token of P made, which the
// control's run addresses again: each client's ask and document, and the plugin's handoff
// record, which the daemon's script reads.
type roleRefs struct {
	asks      map[string]string
	documents map[string]string
	handoff   string
}

// commandScript is K4's script through the dispatch command: one write of each kind R8 names on
// P's issue, each a process of its own in env. inbox is the person's direct message to the
// role's session, which the conversation file's reply answers.
func (r *dispatchBrokerRig) commandScript(role *rigRole, env []string, run string, refs *roleRefs, inbox string) []rigWrite {
	r.t.Helper()
	var writes []rigWrite
	command := func(write string, args ...string) {
		r.t.Helper()
		var out rigRun
		records := r.during(func() { out = r.command(role, env, args...) })
		writes = append(writes, rigWrite{write: write, client: "the dispatch command", ok: out.code == 0, outcome: out.outcome(), records: records})
	}
	issue := r.pIssue
	body := r.file("handoff-"+run+".json", fmt.Sprintf(`{"schemaVersion": 1, "phase": "implement", "issue": %q, "summary": "the %s run's handoff through the dispatch command"}`, issue, run))
	command("handoff-write", "handoff-write", "--issue", issue, "--phase", "implement", "--body-file", body)
	command("result", "result", "--issue", issue, "--check", rigCheck, "--role", "tester", "--verdict", "pass", "--commit", rigCommit,
		"--surface", "api", "--run", "bash scripts/e2e/dispatch-broker.sh", "--observed", "the "+run+" run's result through the dispatch command",
		"--negative-control", "a token of another project is refused 403 PROJECT_SCOPE")
	command("comment", "comment", "--issue", issue, "--body", "The "+run+" run's comment through the dispatch command.")
	command("message", "message", "--issue", issue, "--body", "The "+run+" run's message through the dispatch command.")
	question := "The " + run + " run asks through the dispatch command: does a person read this ask on " + issue + "?"
	command("ask", "ask", "--issue", issue, "--question", question,
		"--option", "Yes: the ask reached the person's Inbox.", "--option", "No: the ask reached nobody.")
	if refs.asks["cli"] == "" {
		refs.asks["cli"] = r.askID(issue, question)
	}
	command("comment --reply-to-ask", "comment", "--issue", issue, "--reply-to-ask", refs.asks["cli"],
		"--body", "The "+run+" run's reply to the ask through the dispatch command.")
	name := "rig-" + run + "-command.md"
	command("artifact", "artifact", "--issue", issue, "--name", name, "--content", "# The "+run+" run's document through the dispatch command\n")
	if refs.documents["cli"] == "" {
		refs.documents["cli"] = name
	}
	command("doc-edit", "doc-edit", "--issue", issue, "--artifact", refs.documents["cli"],
		"--ops-json", fmt.Sprintf(`[{"op":"insert","after":"end","markdown":"The %s run's edit through the dispatch command."}]`, run))
	command("claim", "claim", "--issue", issue)
	command("claim --release", "claim", "--issue", issue, "--release")
	command("the session's own conversation file", "message", "--in-reply-to", inbox,
		"--body", "The "+run+" run's picture through the dispatch command.", "--image", r.picture())
	return writes
}

// pluginScript is K4's script through the pi-envoy plugin's client, held in clients: the same
// writes as commandScript's, each the client's own method with the body the command sends.
// session is the session whose conversation the file is uploaded to.
func (r *dispatchBrokerRig) pluginScript(clients *rigClients, role *rigRole, run string, refs *roleRefs, session string) []rigWrite {
	r.t.Helper()
	var writes []rigWrite
	actor := map[string]string{"kind": "session", "id": role.session}
	call := func(write, method string, args ...any) clientAnswer {
		r.t.Helper()
		var answer clientAnswer
		records := r.during(func() { answer = clients.write("plugin", method, args...) })
		writes = append(writes, rigWrite{write: write, client: "the plugin's client", ok: answer.OK, outcome: answer.outcome(), records: records})
		return answer
	}
	issue := r.pIssue
	handoff := call("handoff-write", "writeHandoff", issue, "implement", map[string]any{
		"actor": actor, "body": fmt.Sprintf(`{"schemaVersion": 1, "phase": "implement", "issue": %q, "summary": "the %s run's handoff through the plugin's client"}`, issue, run),
	})
	if refs.handoff == "" {
		refs.handoff = handoff.field("id")
	}
	call("result", "recordResult", issue, map[string]any{
		"actor": actor, "check": rigCheck, "role": "tester", "verdict": "pass", "commit": rigCommit, "surface": "api",
		"run": "bash scripts/e2e/dispatch-broker.sh", "observed": "the " + run + " run's result through the plugin's client",
		"negative_control": "a token of another project is refused 403 PROJECT_SCOPE",
	})
	call("comment", "comment", issue, map[string]any{"actor": actor, "body": "The " + run + " run's comment through the plugin's client."})
	call("message", "message", issue, map[string]any{"actor": actor, "body": "The " + run + " run's message through the plugin's client."})
	ask := call("ask", "ask", issue, map[string]any{
		"actor": actor, "question": "The " + run + " run asks through the plugin's client: does a person read this ask on " + issue + "?",
		"options": []map[string]string{
			{"label": "Yes", "description": "The ask reached the person's Inbox."},
			{"label": "No", "description": "The ask reached nobody."},
		},
	})
	if refs.asks["plugin"] == "" {
		refs.asks["plugin"] = ask.field("id")
	}
	call("comment --reply-to-ask", "comment", issue, map[string]any{
		"actor": actor, "ask_id": refs.asks["plugin"], "body": "The " + run + " run's reply to the ask through the plugin's client.",
	})
	document := call("artifact", "artifact", issue, map[string]any{
		"actor": actor, "name": "rig-" + run + "-plugin.md", "content": "# The " + run + " run's document through the plugin's client\n",
	})
	if refs.documents["plugin"] == "" {
		refs.documents["plugin"] = document.field("artifact", "id")
	}
	call("doc-edit", "docEdit", refs.documents["plugin"], map[string]any{
		"actor": actor, "ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "The " + run + " run's edit through the plugin's client."}},
	})
	call("claim", "claimIssue", issue, map[string]any{"actor": actor})
	call("claim --release", "releaseIssueClaim", issue, map[string]any{"actor": actor})
	call("the session's own conversation file", "agentArtifact", session, map[string]any{
		"actor": actor, "name": "rig-" + run + "-picture.png", "file": map[string]string{"$picture": r.picture()},
	})
	return writes
}

// daemonRefs are the objects the daemon's script reads that its run with a token of P made, or
// that the role's did: the document it uploads, read back as an approval, the role's handoff
// record, the role's document that its plugin's doc-edit gave a second version, whose two
// versions the script reads the changes between, and the ask the role's plugin opened, which the
// script reads by its id.
type daemonRefs struct {
	document, handoff, edited, ask string
}

// daemonScript is K4's script through the daemon's client: one call of each method of
// internal/dispatch's HTTPClient, its thirteen writes and every read, Whoami among them, on the
// daemon's issue of P. The step record PutStep writes is a fresh one at each run of the script,
// the first write of its run, so it names the run's whole identity; PutStepRating rates that same
// record, a retro's, PutStepExperiment writes on it the experiment and the variant of its issue,
// and PutPlan writes a fresh default plan of the run.
func (r *dispatchBrokerRig) daemonScript(client *dispatch.HTTPClient, refs *daemonRefs, since time.Time) []rigWrite {
	r.t.Helper()
	ctx := context.Background()
	issueKey := r.pDaemon
	var writes []rigWrite
	call := func(name string, read bool, do func() error) {
		r.t.Helper()
		writes = append(writes, r.daemonCall(name, read, do))
	}
	var issue dispatch.Issue
	readIssue := func() error {
		var err error
		issue, err = client.GetIssue(ctx, issueKey)
		return err
	}
	call("Whoami", true, func() error { _, err := client.Whoami(ctx); return err })
	call("ListIssues", true, func() error { _, err := client.ListIssues(ctx, rigP, nil); return err })
	call("GetIssue", true, readIssue)
	call("WebhookAppInstalled", true, func() error {
		_, err := client.WebhookAppInstalled(ctx, "example-org/example-repo")
		return err
	})
	call("SetStatus", false, func() error { _, err := client.SetStatus(ctx, issueKey, "todo"); return err })
	call("GetIssue", true, readIssue)
	call("SetStatusAt", false, func() error { _, err := client.SetStatusAt(ctx, issue, "in_progress"); return err })
	call("GetIssue", true, readIssue)
	call("HandOver", false, func() error { return client.HandOver(ctx, issue, "todo") })
	call("PostMessage", false, func() error { return client.PostMessage(ctx, issueKey, "The daemon's notice on its own issue.") })
	slug := ""
	call("UploadArtifact", false, func() error {
		var err error
		slug, err = client.UploadArtifact(ctx, issueKey, "rig-daemon.md", "# The daemon's own document\n")
		return err
	})
	if refs.document == "" && slug != "" {
		refs.document = r.artifacts(issueKey)[slug]
	}
	call("PutGatedSteps", false, func() error { return client.PutGatedSteps(ctx, rigP, []string{}) })
	call("PutDesignGates", false, func() error { return client.PutDesignGates(ctx, issueKey, []dispatch.DesignGateDocument{}) })
	call("PutRoleHolders", false, func() error { return client.PutRoleHolders(ctx, issueKey, []dispatch.RoleHolder{}) })
	stepID := uuid.NewString()
	started := time.Now().UTC()
	step := dispatch.StepWrite{
		Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
		Agent: "legion-example-" + strings.ToLower(issueKey) + "-implementer", Generation: 1, StartedAt: &started,
	}
	call("PutStep", false, func() error { return client.PutStep(ctx, issueKey, stepID, step) })
	call("PutStepRating", false, func() error {
		return client.PutStepRating(ctx, issueKey, stepID, step,
			dispatch.PlanRating{Rating: 4, Reason: "The rig's retro rates the plan its issue ran."})
	})
	call("PutStepExperiment", false, func() error {
		return client.PutStepExperiment(ctx, issueKey, stepID, step,
			dispatch.StepExperiment{Experiment: "planning", Variant: "b"})
	})
	call("PutPlan", false, func() error {
		return client.PutPlan(ctx, issueKey, uuid.NewString(), dispatch.PlanWrite{Generation: 1, Source: "default",
			Tasks: []dispatch.PlanTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}})
	})
	call("ReleaseClaim", false, func() error { return client.ReleaseClaim(ctx, issueKey) })
	call("Artifacts", true, func() error { _, err := client.Artifacts(ctx, issueKey); return err })
	call("Ask", true, func() error { _, err := client.Ask(ctx, refs.ask); return err })
	call("Reviews", true, func() error { _, err := client.Reviews(ctx, refs.document); return err })
	call("DocumentVersion", true, func() error { _, err := client.DocumentVersion(ctx, refs.edited, 2); return err })
	call("EventsAfter", true, func() error { _, err := client.EventsAfter(ctx, issueKey, 0); return err })
	call("MessageBodiesSince", true, func() error { _, err := client.MessageBodiesSince(ctx, issueKey, since); return err })
	call("Approval", true, func() error { _, err := client.Approval(ctx, refs.document); return err })
	call("ApprovedVersionBefore", true, func() error { _, err := client.ApprovedVersionBefore(ctx, refs.document, 2); return err })
	call("ArtifactChanges", true, func() error { _, err := client.ArtifactChanges(ctx, refs.edited, 1, 2); return err })
	call("PlanDocument", true, func() error { _, err := client.PlanDocument(ctx, refs.document, 1); return err })
	call("Handoff", true, func() error { _, err := client.Handoff(ctx, refs.handoff); return err })
	return writes
}

// TestARoleWritesEveryKindWithItsOwnToken is K4: with a role's broker token for P, one write of
// each kind R8 names, through the dispatch command and through the pi-envoy plugin's client, on
// an issue of P - handoff-write, result, comment, message, comment --reply-to-ask on an ask the
// script opens with ask, artifact and doc-edit, claim and claim --release, and the session's own
// conversation file, a picture uploaded to /agents/<session>/artifacts through the client's
// agentArtifact (the call `dispatch message --in-reply-to` makes for a picture); and with the
// daemon's token for P, one call of each method of internal/dispatch's client. The result is a
// table: the write, the route, the status. A row of it that is not 2xx fails the test, and is a
// defect to report.
//
// Control: the same script with a token of OtherProjectAccount (project Q) is refused 403
// PROJECT_SCOPE on each write that names an issue or a document of P. The table names each request
// that names no issue, which this control cannot tell apart: the conversation file's upload to the
// caller's own session among them, whose upload to the role of P's session a token of another
// session is refused 403 SESSION_SCOPE instead, which the control shows too.
func TestARoleWritesEveryKindWithItsOwnToken(t *testing.T) {
	r := newDispatchBrokerRig(t, 15*time.Minute)
	since := time.Now()
	role := r.registeredRole("k4")
	env := r.environment(role, "")
	clients := r.startClients(role, env)
	refs := &roleRefs{asks: map[string]string{}, documents: map[string]string{}}
	subject, _ := r.daemonSubject()
	var daemonClient *dispatch.HTTPClient
	r.during(func() { daemonClient, _ = r.daemonClient(subject) })

	var withP []rigWrite
	withP = append(withP, r.commandScript(role, env, "P", refs, r.directMessage(role.session))...)
	withP = append(withP, r.pluginScript(clients, role, "P", refs, role.session)...)
	daemon := &daemonRefs{handoff: refs.handoff, edited: refs.documents["plugin"], ask: refs.asks["plugin"]}
	withP = append(withP, r.daemonScript(daemonClient, daemon, since)...)
	// K4's words: with the daemon's token for P the script makes one of each kind of write the
	// daemon's Dispatch client has a method for. A method the client gains and the script does not
	// call is a write no row of the table proves under the broker, so each exported method of
	// internal/dispatch's HTTPClient must name a row of the daemon's client.
	daemonRows := map[string]bool{}
	for _, write := range withP {
		if write.client == "the daemon's client" {
			daemonRows[write.write] = true
		}
	}
	methods := reflect.TypeOf(daemonClient)
	for i := range methods.NumMethod() {
		if name := methods.Method(i).Name; !daemonRows[name] {
			t.Errorf("K4: the daemon's script makes no call of (*dispatch.HTTPClient).%s, so the table proves no %s under the broker; add it to daemonScript", name, name)
		}
	}

	// The documents of P, for the control's table to name what each of its requests addresses.
	r.documentsOfP = map[string]bool{}
	for _, issue := range []string{r.pIssue, r.pDaemon} {
		for _, id := range r.artifacts(issue) {
			r.documentsOfP[id] = true
		}
	}

	other := r.otherProjectRole()
	otherEnv := r.environment(other, "")
	otherClients := r.startClients(other, otherEnv)
	var withQ []rigWrite
	withQ = append(withQ, r.commandScript(other, otherEnv, "Q", refs, r.directMessage(other.session))...)
	withQ = append(withQ, r.pluginScript(otherClients, other, "Q", refs, other.session)...)
	withQ = append(withQ, r.daemonScript(r.otherProjectDaemonClient(), daemon, since)...)
	var intoP clientAnswer
	scoped := rigWrite{write: "the conversation file, uploaded to the session " + role.session + " of P's role", client: "the plugin's client"}
	scoped.records = r.during(func() {
		intoP = otherClients.write("plugin", "agentArtifact", role.session, map[string]any{
			"actor": map[string]string{"kind": "session", "id": other.session}, "name": "rig-Q-into-P.png",
			"file": map[string]string{"$picture": r.picture()},
		})
	})
	scoped.ok, scoped.outcome = intoP.OK, intoP.outcome()

	report := r.report("K4: a role writes every kind with its own token")
	report.say("Every write on %s, the issue of %s the role %s's session addresses, and on %s, the daemon's issue of %s. The role is a registered session of an ExchangeRolesAccount pod of %s; the daemon's identity is the rig's DaemonExchangeAccount; the control's is OtherProjectAccount, whose entry fixes the session %s of %s. A row whose names column says no issue is one this control cannot tell apart.", r.pIssue, rigP, role.session, r.pDaemon, rigP, rigP, other.session, rigQ)
	report.writes("With the tokens of "+rigP, withP, time.Time{})
	report.writes("The control: the same script with tokens of "+rigQ, withQ, time.Time{})
	report.writes("The control's conversation file in another session's conversation", []rigWrite{scoped}, time.Time{})
	report.write("k4")

	for _, write := range withP {
		bad := !write.ok
		for _, record := range write.records {
			bad = bad || record.Status/100 != 2
		}
		if bad {
			t.Errorf("with the token of %s, %s's %s = %s; the proxy recorded %+v: every row of the P table must be 2xx, and this one is a defect to report", rigP, write.client, write.write, write.outcome, write.records)
		}
	}
	for _, write := range withQ {
		if write.read {
			continue
		}
		var addressed []proxyRecord
		for _, record := range write.mutations() {
			if r.namesP(record) {
				addressed = append(addressed, record)
			}
		}
		if strings.HasPrefix(write.write, "the session's own conversation file") {
			for _, record := range write.records {
				if r.namesP(record) {
					t.Errorf("the control's %s through %s addressed %s %s", write.write, write.client, record.Method, record.Route)
				}
			}
			continue
		}
		if len(addressed) == 0 {
			t.Errorf("the control's %s through %s wrote nothing that names %s (%s; the proxy recorded %+v), want it refused 403 PROJECT_SCOPE", write.write, write.client, rigP, write.outcome, write.records)
		}
		for _, record := range addressed {
			if record.Status != http.StatusForbidden || record.Code != "PROJECT_SCOPE" {
				t.Errorf("the control's %s through %s: %s %s was answered %d %s, want 403 PROJECT_SCOPE", write.write, write.client, record.Method, record.Route, record.Status, record.Code)
			}
		}
	}
	if len(scoped.records) != 1 || scoped.records[0].Status != http.StatusForbidden || scoped.records[0].Code != "SESSION_SCOPE" {
		t.Errorf("a token of the session %s uploading to the conversation of %s = %s, the proxy recorded %+v; want 403 SESSION_SCOPE", other.session, role.session, scoped.outcome, scoped.records)
	}
}

// The rig's proxy lands the record of a request whose answer breaks off part way through its body.
// httputil.ReverseProxy, running under an HTTP server, aborts the handler with
// http.ErrAbortHandler when it cannot copy the whole body, so a record landed only after
// forward.ServeHTTP returns never lands, its request stays in flight, and since() waits for it
// until the test's own -timeout. Dispatch whose connection drops mid-answer, or a client that
// gives up while the answer is still going out, is that case. A table must name such a request
// with what the proxy saw of it, and since() must return once the client's call has.
func TestTheRigsProxyLandsARecordWhoseAnswerBreaksOff(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // the connection closes with 93 bytes of the body unsent
	}))
	t.Cleanup(upstream.Close)
	proxy := newRecordingProxy()
	proxy.forward(upstream.URL)
	front := httptest.NewServer(proxy.at("proxy"))
	t.Cleanup(front.Close)

	mark := proxy.count()
	answer, err := http.Get(front.URL + "/api/v1/whoami")
	if err == nil {
		_, _ = io.Copy(io.Discard, answer.Body)
		_ = answer.Body.Close()
	}
	read := make(chan []proxyRecord, 1)
	go func() { read <- proxy.since(mark) }()
	select {
	case records := <-read:
		if len(records) != 1 || records[0].Route != "/api/v1/whoami" {
			t.Fatalf("since(%d) = %+v, want the one request to /api/v1/whoami", mark, records)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("since(%d) still waits 10 s after the client's call returned: the record of a request whose answer broke off never lands, so every since() from that mark on waits until the test times out", mark)
	}
}
