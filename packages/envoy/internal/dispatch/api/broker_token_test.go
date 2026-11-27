// broker_token_test.go drives the real server with the fifth credential kind: an access token the
// secrets broker issued, presented under the RFC 9449 DPoP scheme. The broker is a stub answering
// what each test says a token is, and every proof comes from internal/cipher/helper's DPoP
// transport — the producer a real caller uses — so no second signer of this package's own can
// drift from it. Both stand-ins are internal/ciphertoken/ciphertokentest's, the ones that
// package's own suite drives.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/testport"
)

const (
	// brokerTestSession is the session the broker says its tokens act for, and brokerTestPerson
	// the person it says they belong to: one of the two people newTestServer seeds (alice and
	// bob), so a token this suite presents names someone the server already knows.
	brokerTestSession = "ses_b1c2d3e4f5a6"
	brokerTestPerson  = "alice"
	// brokerTestService is the service a token that names no person names instead: the fleet
	// the machine presenting it belongs to, as the broker's own configuration names it.
	brokerTestService = "example-fleet"
	// brokerTestJob is the service a job's token names instead of a person: the identity its job
	// entry lists, here a pod's Kubernetes service account as the exchange spells its subject.
	brokerTestJob = "system:serviceaccount:example-ns:example-job"
	// brokerTestReader is the service a read-only token names: the one a read entry of the
	// broker's configuration names for the account its token is exchanged from.
	brokerTestReader = "example-reader"
	// brokerTestProject is the project a named-scope token names.
	brokerTestProject = "BRK"
	// brokerTestAddress is the source address the verifier's per-address bucket is keyed on.
	brokerTestAddress = "203.0.113.9:40001"
)

// liveNamedToken is the broker's answer for a live token bound to jkt and scoped to one
// project: one the RFC 8693 exchange issued for acting as a session, which is what every token
// Dispatch acts on is.
func liveNamedToken(jkt string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      brokerTestSession,
		Person:       brokerTestPerson,
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{brokerTestProject},
		Scope:        "dispatch.write",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// personScopedTokenAnswer is the broker's answer for a dev box session's token: live, bound to jkt,
// acting as its person (the project scope person) and naming no project. It is what a person's
// sign-in and a person's machine login both trade for.
func personScopedTokenAnswer(jkt string) ciphertoken.Introspection {
	answer := liveNamedToken(jkt)
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, nil
	return answer
}

// serviceOnlyToken is the broker's answer for a machine nobody has signed in on: live, bound to
// jkt, naming a session and the service its fleet is, and no person. It names no project either.
// The exchange mints this shape for one caller only, a machine whose fleet entry names neither a
// person nor a project (cipher/exchange's Set.Google), and marks it with the person scope, the
// wire's word for a token held to no one project.
func serviceOnlyToken(jkt string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      brokerTestSession,
		Service:      brokerTestService,
		ProjectScope: ciphertoken.ProjectScopePerson,
		Scope:        "dispatch.write",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// liveJobNamedToken is the broker's answer for a job's token whose entry names one project: live,
// bound to jkt, naming a session, the job's identity as its service, no person and no OAuth scope,
// and the project scope named with that one project. The exchange mints this shape from a job
// entry (BROKER_EXCHANGE_JOB_ACCOUNTS, BROKER_EXCHANGE_JOB_WORKFLOWS or
// BROKER_VM_EXCHANGE_JOB_ACCOUNTS), which names no person.
func liveJobNamedToken(jkt string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      brokerTestSession,
		Service:      brokerTestJob,
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{brokerTestProject},
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// liveJobPublicToken is the broker's answer for a job's token whose entry names public: the same
// shape as liveJobNamedToken's, with the project scope public and the entry's list of private
// projects, sorted as the broker writes it, beside which the token reaches every public project.
func liveJobPublicToken(jkt string, projects ...string) ciphertoken.Introspection {
	answer := liveJobNamedToken(jkt)
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePublic, projects
	return answer
}

// brokerTestAccount is the account an account entry naming no person and no project lists, which
// the broker names as the service of every token it mints for that account's pods.
const brokerTestAccount = "system:serviceaccount:example-ns:example-public"

// personlessAccountToken is the broker's answer for a token minted from an account entry that
// names no person and no project (cipher/exchange's Set.Pod): liveJobNamedToken's shape with the
// account as its service and no project, which is the shape the broker's introspection answers for
// such a token (cmd/cipherd's TestTheBinaryExchangesAPersonlessAccountEntrysPodToken).
func personlessAccountToken(jkt string) ciphertoken.Introspection {
	answer := liveJobNamedToken(jkt)
	answer.Service, answer.Projects = brokerTestAccount, nil
	return answer
}

// readOnlyToken is the broker's answer for a read-only token: live, bound to jkt, naming a session
// and the service of the read entry it was exchanged under, and no person and no project. The
// project scope is none and the scope is the one word read, the shape the broker mints from a
// read entry and nothing else (BROKER_EXCHANGE_READ_ACCOUNTS), and the one a verifier built with
// Options.ServesReadOnly marks read-only.
func readOnlyToken(jkt string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      brokerTestSession,
		Service:      brokerTestReader,
		ProjectScope: ciphertoken.ProjectScopeNone,
		Scope:        ciphertoken.ScopeRead,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// liveDaemonToken is the broker's answer for a Legion daemon's own token: liveNamedToken, of the
// session the daemon of brokerTestProject writes as (the daemon spells it legion-daemon:<PROJECT>,
// DaemonSession in packages/daemon/internal/dispatch/client.go), holding legion-status, the scope
// the broker grants to a daemon's token alone.
func liveDaemonToken(jkt string) ciphertoken.Introspection {
	token := liveNamedToken(jkt)
	token.Session, token.Scope = legionDaemonSessionPrefix+brokerTestProject, ciphertoken.ScopeLegionStatus
	return token
}

// machineLoginToken is the broker's answer for a machine login's own access token: the
// credential the agent-secrets helper holds to ask the broker for everything else. It is live
// and bound to the caller's key like any other, and the broker names no session, no person and
// no project scope for it, no exchange having decided one.
func machineLoginToken(jkt string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:     true,
		Scope:      "machine",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
		ClientID:   "agent-secrets-helper",
		Thumbprint: jkt,
	}
}

// brokerCaller is an agent holding one key, producing the proofs its real client produces.
type brokerCaller struct {
	caller *ciphertokentest.Caller
	jkt    string
}

// newBrokerCaller mints a caller's key and the transport that proves it.
func newBrokerCaller(t *testing.T) *brokerCaller {
	t.Helper()
	caller := ciphertokentest.NewCaller(t)
	return &brokerCaller{caller: caller, jkt: caller.JKT()}
}

// presenting is the request a caller sends Dispatch: the server-side request for path, carrying
// token under the DPoP scheme and a proof of this caller's key signed for that method and target.
// Each call signs a fresh proof, as a real client does.
func (c *brokerCaller) presenting(t *testing.T, method, path, token string, body any) *http.Request {
	t.Helper()
	signed := c.caller.Signed(t, method, testServerURL+path, token)
	return brokerRequest(t, method, path, body, signed.Header.Get("Authorization"), signed.Header.Get("DPoP"))
}

// presentingUpload is the multipart upload a caller sends Dispatch, carrying token under the
// DPoP scheme and a fresh proof for that target. A proof names the method and the target URI
// and never the body, so the upload's bytes are the request's own, as they are for a browser.
func (c *brokerCaller) presentingUpload(t *testing.T, path, token, filename, contentType string, content []byte) *http.Request {
	t.Helper()
	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": filename}, filename, contentType, content)
	if err != nil {
		t.Fatalf("build the upload: %v", err)
	}
	signed := c.caller.Signed(t, http.MethodPost, testServerURL+path, token)
	request := httptest.NewRequest(http.MethodPost, path, body)
	request.RemoteAddr = brokerTestAddress
	request.Header.Set("Content-Type", bodyType)
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	return request
}

// brokerRequest is the server-side request for path with the credential headers given, so a test
// can replay one proof, present a token under another scheme, or send a body the proof never
// covered (a proof names the method and the target URI, never the body).
func brokerRequest(t *testing.T, method, path string, body any, authorization, dpop string) *http.Request {
	t.Helper()
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = brokerTestAddress
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if dpop != "" {
		request.Header.Set("DPoP", dpop)
	}
	return request
}

// brokerTokenRig is the server under test, the broker it asks and the store behind it.
type brokerTokenRig struct {
	handler  http.Handler
	deps     Deps
	database *store.Store
	broker   *ciphertokentest.StubBroker
	// served is the server Register mounted, whose own chains a test builds again around a
	// stand-in for a route's handler (routeHandler, liveSocketHandler).
	served *server
}

// newBrokerTokenRig is the package's own test server (newTestServer) with the broker-token check
// wired to a stub broker: the replay store and the rest of the API on one database, as a
// deployment has them. Each option is applied to the verifier's own options before it is built,
// for a test that needs a bucket it can reach.
func newBrokerTokenRig(t *testing.T, options ...func(*ciphertoken.Options)) *brokerTokenRig {
	t.Helper()
	return newBrokerTokenRigOn(t, testServerOptions{}, options...)
}

// newBrokerTokenRigOn is newBrokerTokenRig on a server built from serving, whose brokerTokens is
// the rig's own verifier: a test that needs the test hooks' rows mounted, or a GitHub and a
// traced store of its own, sets them there, and the server is still the one newTestServer call.
func newBrokerTokenRigOn(t *testing.T, serving testServerOptions, options ...func(*ciphertoken.Options)) *brokerTokenRig {
	t.Helper()
	broker := ciphertokentest.NewStubBroker(t)
	serving.brokerTokens = func(database *store.Store) *ciphertoken.Verifier {
		opts := ciphertoken.Options{
			BrokerURL: broker.Server.URL,
			Account:   ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", accountTokenFile(t, credentialTestAccountToken)),
			Origins:   []string{testServerURL},
			Replay:    store.NewDPoPProofs(database.Pool),
			// What Dispatch itself passes: the check keys its bucket on the address
			// Dispatch's own proxy-trust rule reads, never one of its own, and serves a
			// read-only token on the reads of its read class and an approve-only token on the
			// routes its approve list names (cmd/dispatch, brokerTokenVerifier).
			Address:           auth.ForwardedAddress,
			ServesReadOnly:    true,
			ServesApproveOnly: true,
		}
		for _, option := range options {
			option(&opts)
		}
		verifier, err := ciphertoken.New(opts)
		if err != nil {
			t.Fatalf("build the broker token verifier: %v", err)
		}
		return verifier
	}
	handler, database, deps, served := newRegisteredTestServer(t, serving)
	return &brokerTokenRig{handler: handler, deps: deps, database: database, broker: broker, served: served}
}

// serve carries one request through the server under test.
func (r *brokerTokenRig) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response
}

// whoamiAnswer is what GET /api/v1/whoami tells an agent caller.
type whoamiAnswer struct {
	Kind    string   `json:"kind"`
	Login   string   `json:"login"`
	Owner   *string  `json:"owner"`
	Service *string  `json:"service"`
	Session *string  `json:"session"`
	Project *string  `json:"project"`
	Scopes  []string `json:"scopes"`
	// A pointer, so an answer that leaves the field out reads differently from one that names
	// an empty scope.
	ProjectScope *string `json:"project_scope"`
	// SignInRequired is a pointer so an answer that omits the field reads as nil, not false.
	SignInRequired *bool `json:"sign_in_required"`
}

// refusal is the code and message a refused request carries.
type refusalAnswer struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// TestALiveBrokerTokenWithASoundProofIsAccepted: the two checks together admit the caller, and
// whoami reports the session the token proved, the person it acts for and the project it may
// write.
func TestALiveBrokerTokenWithASoundProofIsAccepted(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-the-broker-issued"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("whoami under a broker token: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := decodeBody[whoamiAnswer](t, response)
	if answer.Kind != "agent" || answer.Session == nil || *answer.Session != brokerTestSession {
		t.Fatalf("whoami = %+v, want the session the token names", answer)
	}
	if answer.Owner == nil || *answer.Owner != brokerTestPerson {
		t.Fatalf("whoami owner = %v, want %q", answer.Owner, brokerTestPerson)
	}
	if answer.Project == nil || *answer.Project != brokerTestProject {
		t.Fatalf("whoami project = %v, want %q", answer.Project, brokerTestProject)
	}
	if answer.Service != nil {
		t.Fatalf("whoami service = %v, want null: no service-account token authenticated this", *answer.Service)
	}
}

// TestAPersonScopedBrokerTokenCarriesNoProject: a dev box session's token names no single
// project, so the caller carries none at all rather than an empty one, and whoami answers its
// project scope, person.
func TestAPersonScopedBrokerTokenCarriesNoProject(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-dev-box-session-token"
	rig.broker.Says(token, personScopedTokenAnswer(caller.jkt))

	response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("whoami under a person-scoped token: status=%d body=%s", response.Code, response.Body.String())
	}
	decided := decodeBody[whoamiAnswer](t, response)
	if decided.Project != nil {
		t.Fatalf("whoami project = %q, want null for a person-scoped token", *decided.Project)
	}
	if decided.Session == nil || *decided.Session != brokerTestSession {
		t.Fatalf("whoami = %+v, want the session the token names", decided)
	}
	if decided.ProjectScope == nil || *decided.ProjectScope != ciphertoken.ProjectScopePerson {
		t.Fatalf("whoami project_scope = %v, want %q: %s", decided.ProjectScope, ciphertoken.ProjectScopePerson, response.Body.String())
	}
}

// TestABrokerTokenWritesAsTheSessionItProves: the body's actor does not decide the session, which
// the token carries. An absent one and a matching one are both taken, a matching one's origin is
// kept because nothing else carries it, and the write is recorded as the token's session with the
// person and project the token named. The project the writes go into is made by a person: a
// token scoped to one project may not create another (project_scope.go).
func TestABrokerTokenWritesAsTheSessionItProves(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-writes"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the project as a person: status=%d body=%s", created.Code, created.Body.String())
	}

	issue := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject,
		"title":   "Written by the token's session",
		"spec":    "# Spec",
		"actor": map[string]any{
			"kind":   "session",
			"id":     brokerTestSession,
			"origin": map[string]string{"cwd": "/workspace", "host": "example-host-one", "pane": "%7"},
		},
	}))
	if issue.Code != http.StatusCreated {
		t.Fatalf("create an issue with the matching actor: status=%d body=%s", issue.Code, issue.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key

	noActor := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", token, map[string]any{
		"body": "Written with no actor in the body.",
	}))
	if noActor.Code != http.StatusCreated {
		t.Fatalf("comment with no actor in the body: status=%d body=%s", noActor.Code, noActor.Body.String())
	}

	listed := rig.serve(withPerson(brokerRequest(t, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "", ""), brokerTestPerson))
	if listed.Code != http.StatusOK {
		t.Fatalf("read the issue's events: status=%d body=%s", listed.Code, listed.Body.String())
	}
	log := decodeBody[[]struct {
		Type  string `json:"type"`
		Actor struct {
			Kind    string  `json:"kind"`
			ID      string  `json:"id"`
			Owner   *string `json:"owner"`
			Project *string `json:"project"`
			Origin  *struct {
				Cwd  string `json:"cwd"`
				Host string `json:"host"`
				Pane string `json:"pane"`
			} `json:"origin"`
		} `json:"actor"`
	}](t, listed)
	if len(log) == 0 {
		t.Fatalf("the issue has no events: %s", listed.Body.String())
	}
	actor := log[0].Actor
	if actor.Kind != "session" || actor.ID != brokerTestSession {
		t.Fatalf("the write's actor = %s:%s, want the session the token proved", actor.Kind, actor.ID)
	}
	if actor.Owner == nil || *actor.Owner != brokerTestPerson {
		t.Fatalf("the write's owner = %v, want %q", actor.Owner, brokerTestPerson)
	}
	if actor.Project == nil || *actor.Project != brokerTestProject {
		t.Fatalf("the write's project = %v, want %q", actor.Project, brokerTestProject)
	}
	if actor.Origin == nil || actor.Origin.Cwd != "/workspace" || actor.Origin.Host != "example-host-one" || actor.Origin.Pane != "%7" {
		t.Fatalf("the write's origin = %+v, want the body's cwd, host and pane kept", actor.Origin)
	}
}

// TestABrokerPersonIsStoredCanonicalOnASessionsRow: the broker answers the person of a
// BROKER_EXCHANGE_ACCOUNTS or BROKER_EXCHANGE_WORKFLOWS entry as the entry writes it, and
// Dispatch reads that person in its canonical form where it first reads the broker's answer
// (ciphertoken's introspect, fold.Login), so a row a session writes under the token names its
// owner as every other row and read of Dispatch names that person. The broker here answers a
// person in mixed case with white space around it, the session comments under the token with no
// actor in the body, and the comment's stored author names the canonical login as its owner.
func TestABrokerPersonIsStoredCanonicalOnASessionsRow(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-whose-person-the-broker-spells-as-configured"
	const want = "dana@example.com"
	says := liveNamedToken(caller.jkt)
	says.Person = " Dana@Example.COM "
	rig.broker.Says(token, says)
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project as a person: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "Commented on under a broker token", "spec": "# Spec",
	})
	if issue.Code != http.StatusCreated {
		t.Fatalf("create the issue as a person: status=%d body=%s", issue.Code, issue.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key

	commented := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", token, map[string]any{
		"body": "Written by the session the broker proved.",
	}))
	if commented.Code != http.StatusCreated {
		t.Fatalf("comment under the broker's token: status=%d body=%s", commented.Code, commented.Body.String())
	}
	commentID := decodeBody[struct {
		ID string `json:"id"`
	}](t, commented).ID

	var raw []byte
	if err := rig.database.Pool.QueryRow(t.Context(), `select author from comments where id = $1`, commentID).Scan(&raw); err != nil {
		t.Fatalf("read the comment's author: %v", err)
	}
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		t.Fatalf("decode the comment's author %s: %v", raw, err)
	}
	requireSessionProven(t, "the comment's author", author, true)
	stored := "none"
	if author.Owner != nil {
		stored = fmt.Sprintf("%q", *author.Owner)
	}
	if author.Owner == nil || *author.Owner != want {
		t.Fatalf("the session-authored row stores owner %s, want the canonical login %q: Dispatch takes the broker's person as it reads it", stored, want)
	}
}

// TestAWriteIsProvenOnlyWhereTheCredentialProvedItsSession: each credential below writes an
// issue with a spec, a comment on it and an edit of the spec as one and the same session, and a
// reader tells them apart by the stored actor's proven mark alone. A broker-issued token proved
// the session, so the comment's author, the author of both versions - the spec's first, which
// the issue's create writes, and the edit's, which the document service writes - and the actor
// of both events carry proven: true. The shared agent token, a personal token and a pod's
// service-account token prove no session, so the session their body names is recorded as it
// always was and carries no mark, though it is the very session the broker's token proves.
//
// Each of those three also writes once with a forged body: its actor says proven: true, with an
// act whose session_proven is true. Both are declared fields of model.Actor, so the server's
// strict decode takes the body (every write below must answer its success code) and the mark
// reaches the handler as the caller's own word. The write is still recorded with no mark on the
// comment's author, either version's author or either event's actor: a server that copied the
// body's actor, or its mark, into what it records fails every one of those assertions.
func TestAWriteIsProvenOnlyWhereTheCredentialProvedItsSession(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{oidc: issuer.verifier(t)})
	caller := newBrokerCaller(t)
	const brokerToken = "a-token-whose-session-is-proven"
	rig.broker.Says(brokerToken, liveNamedToken(caller.jkt))
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project as a person: status=%d body=%s", created.Code, created.Body.String())
	}
	minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "laptop"})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a personal token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[agentTokenResponse](t, minted).Token
	service := issuer.mint(t, serviceTokenAudience)
	bearer := func(token string) func(*testing.T, string, string, map[string]any) *httptest.ResponseRecorder {
		return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.serve(brokerRequest(t, method, path, body, "Bearer "+token, ""))
		}
	}

	claimed := map[string]any{"kind": "session", "id": brokerTestSession}
	forged := map[string]any{
		"kind": "session", "id": brokerTestSession, "proven": true,
		"act": map[string]any{"session": brokerTestSession, "session_proven": true},
	}
	// Each row's issue is written in words no other row's shares - its title, its spec's one
	// sentence and the sentence its edit puts there - so the near-copy gate on POST /api/v1/issues
	// reads no row's issue as a copy of an earlier row's (409 POSSIBLE_DUPLICATE).
	for _, credential := range []struct {
		name string
		// title, first and second are the row's issue title, its spec's sentence and the
		// sentence the edit replaces it with.
		title, first, second string
		// actor is the body's actor: none for the token that proves its session, the session
		// claimed, or claimed with a forged mark, for every other.
		actor  map[string]any
		proven bool
		send   func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder
	}{
		{"a broker-issued token", "Harbor lighthouse inventory", "Seagulls circle the pier.", "Ferries dock at dawn.", nil, true,
			func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
				return rig.serve(caller.presenting(t, method, path, brokerToken, body))
			}},
		{"the shared agent token", "Orchard pruning calendar", "Apples ripen in autumn.", "Pears follow in winter.", claimed, false, bearer("agent-token")},
		{"a personal token", "Volcanic basalt survey", "Lava cools into columns.", "Ash settles downhill.", claimed, false, bearer(personal)},
		{"a service-account token", "Violin rehearsal notes", "Strings need fresh rosin.", "Bows rest overnight.", claimed, false, bearer(service)},
		{"the shared agent token with a body that says proven", "Glacier melt telemetry", "Ice retreats each July.", "Meltwater feeds rivers.", forged, false, bearer("agent-token")},
		{"a personal token with a body that says proven", "Bakery sourdough starter", "Flour ferments slowly.", "Loaves rise by noon.", forged, false, bearer(personal)},
		{"a service-account token with a body that says proven", "Comet orbit sketch", "Tails point sunward.", "Perihelion arrives soon.", forged, false, bearer(service)},
	} {
		t.Run(credential.name, func(t *testing.T) {
			write := func(method, path string, body map[string]any) *httptest.ResponseRecorder {
				if credential.actor != nil {
					body["actor"] = credential.actor
				}
				return credential.send(t, method, path, body)
			}
			issue := write(http.MethodPost, "/api/v1/issues", map[string]any{
				"project": brokerTestProject, "title": credential.title, "spec": credential.first,
			})
			if issue.Code != http.StatusCreated {
				t.Fatalf("create the issue: status=%d body=%s", issue.Code, issue.Body.String())
			}
			created := decodeBody[struct {
				Key               string `json:"key"`
				PrimaryArtifactID string `json:"primary_artifact_id"`
			}](t, issue)
			comment := write(http.MethodPost, "/api/v1/issues/"+created.Key+"/comments", map[string]any{"body": "A comment."})
			if comment.Code != http.StatusCreated {
				t.Fatalf("comment: status=%d body=%s", comment.Code, comment.Body.String())
			}
			commentID := decodeBody[struct {
				ID string `json:"id"`
			}](t, comment).ID
			edited := write(http.MethodPost, "/api/v1/artifacts/"+created.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]string{{"op": "replace", "find": credential.first, "with": credential.second}},
			})
			if edited.Code != http.StatusOK {
				t.Fatalf("edit the spec: status=%d body=%s", edited.Code, edited.Body.String())
			}
			waitForArtifactVersion(t, rig.handler, created.PrimaryArtifactID, 2)

			read := rig.asPerson(t, http.MethodGet, "/api/v1/comments/"+commentID, nil)
			if read.Code != http.StatusOK {
				t.Fatalf("read the comment: status=%d body=%s", read.Code, read.Body.String())
			}
			requireSessionProven(t, "the comment's author", decodeBody[struct {
				Comment commentAuthor `json:"comment"`
			}](t, read).Comment.Author, credential.proven)
			for _, number := range []string{"1", "2"} {
				version := rig.asPerson(t, http.MethodGet, "/api/v1/artifacts/"+created.PrimaryArtifactID+"/versions/"+number, nil)
				if version.Code != http.StatusOK {
					t.Fatalf("read version %s: status=%d body=%s", number, version.Code, version.Body.String())
				}
				authors := decodeBody[struct {
					Authors []model.Actor `json:"authors"`
				}](t, version).Authors
				if len(authors) != 1 {
					t.Fatalf("version %s authors = %#v, want the writing session alone", number, authors)
				}
				requireSessionProven(t, "version "+number+"'s author", authors[0], credential.proven)
			}
			events := rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+created.Key+"/events", nil)
			if events.Code != http.StatusOK {
				t.Fatalf("read the issue's events: status=%d body=%s", events.Code, events.Body.String())
			}
			seen := map[string]bool{}
			for _, event := range decodeBody[[]model.Event](t, events) {
				if event.Type == "issue.created" || event.Type == "comment.created" {
					seen[event.Type] = true
					requireSessionProven(t, "the "+event.Type+" event's actor", event.Actor, credential.proven)
				}
			}
			if !seen["issue.created"] || !seen["comment.created"] {
				t.Fatalf("the issue's events hold %v, want issue.created and comment.created", seen)
			}
		})
	}
}

// requireSessionProven fails t unless actor is the broker's test session, carries the proven
// mark exactly when want says the credential proved it, and names an act only where the
// credential proved the session for its person: the broker-issued token names a person
// (brokerTestPerson), so bearerSessionActor records the session it proved as the actor's act,
// proven (R14). A session a body named under any other credential names no act, so no
// session_proven a body sent can ride on it.
func requireSessionProven(t *testing.T, what string, actor model.Actor, want bool) {
	t.Helper()
	if actor.Kind != "session" || actor.ID != brokerTestSession {
		t.Fatalf("%s = %s:%s, want the session %s", what, actor.Kind, actor.ID, brokerTestSession)
	}
	if actor.Proven != want {
		t.Fatalf("%s proven = %t, want %t", what, actor.Proven, want)
	}
	switch {
	case want && (actor.Act == nil || actor.Act.Session != brokerTestSession || !actor.Act.SessionProven ||
		actor.Act.TokenID != "" || actor.Act.TokenName != ""):
		t.Fatalf("%s act = %+v, want the session %s alone, proven", what, actor.Act, brokerTestSession)
	case !want && actor.Act != nil:
		t.Fatalf("%s act = %+v, want none", what, *actor.Act)
	}
}

// TestANamedBrokerTokenNamingTwoProjectsWritesNothing: no exchange mints a named token naming two
// projects, and Dispatch reads a named token's project only where it names one. Such an answer is
// refused before any write rather than read as a caller that names no project.
func TestANamedBrokerTokenNamingTwoProjectsWritesNothing(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-named-token-naming-two-projects"
	answer := liveNamedToken(caller.jkt)
	answer.Projects = []string{brokerTestProject, "OTHER"}
	rig.broker.Says(token, answer)
	seedProject(t, rig, "OTHER", "Other")
	written := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": "OTHER", "title": "Written under a named token naming two projects", "spec": "# Spec",
	}))
	if refusal := decodeBody[refusalAnswer](t, written); written.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_A_SESSION" {
		t.Fatalf("a write under a named token naming two projects: status=%d body=%s, want 401 BROKER_TOKEN_NOT_A_SESSION", written.Code, written.Body.String())
	}
}

// TestAnOlderBrokersNamedAnswerWritesNothing: while an older broker replica answers introspection
// for a named token this release stored (a rolling update, a canary, a rollback), it reads the
// token's one project from the older `project` claim the exchange also writes, and answers
// `"project":"<key>"` with no `projects`. Dispatch refuses that answer before any write, rather
// than reading a named token naming no project, which it would hold to no project at all. This is
// the reader's half and passes whatever the exchange stores; the writer's half, that the exchange
// stores that claim at all, is TestANamedTokenStoresItsProjectUnderTheOlderClaimToo
// (internal/cipher/oauth).
func TestAnOlderBrokersNamedAnswerWritesNothing(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-named-token-an-older-replica-answers-for"
	rig.broker.SaysRaw(token, fmt.Sprintf(`{"active":true,"session":%q,"person":%q,"project_scope":"named","project":%q,"exp":%d,"client_id":%q,"jkt":%q}`,
		brokerTestSession, brokerTestPerson, brokerTestProject, time.Now().Add(time.Hour).Unix(), ciphertoken.ExchangeClientID, caller.jkt))
	seedProject(t, rig, "OTHER", "Other")
	written := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": "OTHER", "title": "Written under an older replica's answer", "spec": "# Spec",
	}))
	if refusal := decodeBody[refusalAnswer](t, written); written.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_A_SESSION" {
		t.Fatalf("a write under an older replica's named answer: status=%d body=%s, want 401 BROKER_TOKEN_NOT_A_SESSION", written.Code, written.Body.String())
	}
}

// TestANamedBrokerTokenWritesAnotherProject: a token an operator's entry mapped to one project
// writes that project, and writes another project as a credential that names no project does
// (INFRA-4799): an issue made in either is made in the project its body names.
func TestANamedBrokerTokenWritesAnotherProject(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-named-token"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	seedProject(t, rig, "OTHER", "Other")

	own := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject, "title": "Written into its own project", "spec": "# Spec",
	}))
	if own.Code != http.StatusCreated {
		t.Fatalf("a named token's write into its own project: status=%d body=%s", own.Code, own.Body.String())
	}
	other := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": "OTHER", "title": "Written into another project", "spec": "# Spec",
	}))
	if other.Code != http.StatusCreated {
		t.Fatalf("a named token's write into another project: status=%d body=%s, want 201", other.Code, other.Body.String())
	}
	if issue := decodeBody[model.Issue](t, other); issue.Project != "OTHER" {
		t.Fatalf("the issue written into another project is %s of project %q, want OTHER", issue.Key, issue.Project)
	}
}

// TestAPersonTokenWithProjectsLimitsNoWriteYet: a token acting as its person carries the list its
// trade asked for, and that list names no project the token writes as its own: whoami names
// none, and the token writes another project its person may write, exactly as a token naming no
// project does. What later reads the list is Dispatch's own project access rule.
func TestAPersonTokenWithProjectsLimitsNoWriteYet(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-person-token-naming-a-project"
	answer := liveNamedToken(caller.jkt)
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, []string{brokerTestProject}
	rig.broker.Says(token, answer)
	seedProject(t, rig, brokerTestProject, "Broker")
	seedProject(t, rig, "OTHER", "Other")

	who := decodeBody[whoamiAnswer](t, rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil)))
	if who.Project != nil {
		t.Fatalf("whoami project = %q, want null: a person token's list names no project it writes as its own", *who.Project)
	}
	other := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": "OTHER", "title": "Written into a project the list does not name", "spec": "# Spec",
	}))
	if other.Code != http.StatusCreated {
		t.Fatalf("a person token's write into a project its list does not name: status=%d body=%s, want it written as before", other.Code, other.Body.String())
	}
}

// withPerson signs a request in as a person through the test header identity.
func withPerson(request *http.Request, login string) *http.Request {
	request.Header.Set("X-Dispatch-User", login)
	return request
}

// TestABrokerTokenRefusesABodyActorNamingAnotherSession: the one thing a body actor may not do is
// name a writer that is not the credential's, and the refusal names the session the token carries
// so the caller can see what it should have sent.
func TestABrokerTokenRefusesABodyActorNamingAnotherSession(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-whose-body-lies"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")

	for name, actor := range map[string]any{
		"another session": map[string]string{"kind": "session", "id": "ses_someone_else"},
		"a person":        map[string]string{"kind": "user", "id": brokerTestPerson},
	} {
		t.Run(name, func(t *testing.T) {
			response := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+own.issue+"/comments", token, map[string]any{
				"body": "Written as someone else.", "actor": actor,
			}))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s, want 403", response.Code, response.Body.String())
			}
			refusal := decodeBody[refusalAnswer](t, response)
			if refusal.Code != "ACTOR_SESSION_MISMATCH" || !strings.Contains(refusal.Error, brokerTestSession) {
				t.Fatalf("refusal = %+v, want ACTOR_SESSION_MISMATCH naming %s", refusal, brokerTestSession)
			}
		})
	}
}

// TestABrokerTokenProvedByAnotherKeyIsRefused: a sound proof signed by a key the token is not
// bound to is what a stolen token looks like, and it gets its own code.
func TestABrokerTokenProvedByAnotherKeyIsRefused(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	owner, thief := newBrokerCaller(t), newBrokerCaller(t)
	const token = "a-token-bound-to-its-owners-key"
	rig.broker.Says(token, liveNamedToken(owner.jkt))

	response := rig.serve(thief.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", response.Code, response.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, response); refusal.Code != "BROKER_TOKEN_KEY_MISMATCH" {
		t.Fatalf("refusal = %+v, want BROKER_TOKEN_KEY_MISMATCH", refusal)
	}
}

// TestBrokerTokenRefusalsEachHaveTheirOwnCode: a token the broker does not hold as live, a proof
// already spent, and a broker that answers nothing are three different things to a caller, and
// only the last is a 503 — a caller told its token was refused would discard a live one.
func TestBrokerTokenRefusalsEachHaveTheirOwnCode(t *testing.T) {
	t.Parallel()
	t.Run("not live", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-the-broker-revoked"
		rig.broker.Says(token, ciphertoken.Introspection{Active: false})

		response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_ACTIVE" {
			t.Fatalf("status=%d refusal=%+v, want 401 BROKER_TOKEN_NOT_ACTIVE", response.Code, refusal)
		}
		if strings.Contains(refusal.Error, token) {
			t.Fatalf("the refusal repeats the token: %q", refusal.Error)
		}
	})
	t.Run("a spent proof", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-whose-proof-is-captured"
		rig.broker.Says(token, liveNamedToken(caller.jkt))

		presented := caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil)
		replay := brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil,
			presented.Header.Get("Authorization"), presented.Header.Get("DPoP"))
		if first := rig.serve(presented); first.Code != http.StatusOK {
			t.Fatalf("the first presentation: status=%d body=%s", first.Code, first.Body.String())
		}
		response := rig.serve(replay)
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_PROOF_INVALID" {
			t.Fatalf("status=%d refusal=%+v, want 401 BROKER_TOKEN_PROOF_INVALID", response.Code, refusal)
		}
	})
	t.Run("the broker unreachable", func(t *testing.T) {
		// The verifier asks a broker at a port the test holds and nothing listens on.
		refused := "http://" + testport.Refused(t)
		rig := newBrokerTokenRig(t, func(o *ciphertoken.Options) { o.BrokerURL = refused })
		caller := newBrokerCaller(t)
		const token = "a-token-nobody-can-be-asked-about"

		response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusServiceUnavailable || refusal.Code != "BROKER_UNAVAILABLE" {
			t.Fatalf("status=%d refusal=%+v, want 503 BROKER_UNAVAILABLE", response.Code, refusal)
		}
	})
}

// TestAMachineLoginsOwnTokenIsRefusedAndNeverFilledInFromTheBody: the broker sets a session and
// a person only from an exchange's claims, so a machine login's own access token — live, bound
// to the caller's key, issued to the agent-secrets helper — answers with neither. Dispatch
// records every write under the session its credential proved, so such a token is refused by
// name: whoami will not say who it is, and a write will not take the session its body names.
func TestAMachineLoginsOwnTokenIsRefusedAndNeverFilledInFromTheBody(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-machine-logins-own-access-token"
	rig.broker.Says(token, machineLoginToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	who := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	refusal := decodeBody[refusalAnswer](t, who)
	if who.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_A_SESSION" {
		t.Fatalf("whoami: status=%d refusal=%+v, want 401 BROKER_TOKEN_NOT_A_SESSION", who.Code, refusal)
	}
	if strings.Contains(refusal.Error, token) {
		t.Fatalf("the refusal repeats the token: %q", refusal.Error)
	}

	const chosen = "ses-chosen-by-the-body"
	written := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject, "title": "Filed by a machine login", "spec": "# Spec",
		"actor": map[string]any{"kind": "session", "id": chosen},
	}))
	refused := decodeBody[refusalAnswer](t, written)
	if written.Code != http.StatusUnauthorized || refused.Code != "BROKER_TOKEN_NOT_A_SESSION" {
		t.Fatalf("a write: status=%d body=%s, want 401 BROKER_TOKEN_NOT_A_SESSION", written.Code, written.Body.String())
	}
	if strings.Contains(written.Body.String(), chosen) {
		t.Fatalf("the refusal carries the session the body named: %s", written.Body.String())
	}
	issues := rig.asPerson(t, http.MethodGet, "/api/v1/issues?project="+brokerTestProject, nil)
	if strings.Contains(issues.Body.String(), "Filed by a machine login") {
		t.Fatalf("the refused write created an issue: %s", issues.Body.String())
	}
}

// TestABrokerTokenUnderTheBearerSchemeIsRefusedAsItAlwaysWas: RFC 9449 Section 7.2 refuses a
// DPoP-bound token presented under any other scheme, and Dispatch's bearer branches are untouched
// — such a presentation is the unknown personal token it has always been, and the broker is never
// asked about it.
func TestABrokerTokenUnderTheBearerSchemeIsRefusedAsItAlwaysWas(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-presented-the-wrong-way"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	response := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+token, ""))
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusUnauthorized || refusal.Code != "UNAUTHORIZED" {
		t.Fatalf("status=%d refusal=%+v, want 401 UNAUTHORIZED", response.Code, refusal)
	}
	if rig.broker.Count() != 0 {
		t.Fatalf("the broker was asked %d times about a bearer presentation, want none", rig.broker.Count())
	}
}

// TestADPoPPresentationWithoutABrokerIsRefusedAsItAlwaysWas: a Dispatch with no broker configured
// has no such credential kind, and the answer is what any Authorization header that is not a
// bearer has always been here.
func TestADPoPPresentationWithoutABrokerIsRefusedAsItAlwaysWas(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	caller := newBrokerCaller(t)
	presented := caller.presenting(t, http.MethodGet, "/api/v1/whoami", "a-token-nobody-here-decides", nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, presented)
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusUnauthorized || refusal.Code != "UNAUTHORIZED" || refusal.Error != "invalid bearer token" {
		t.Fatalf("status=%d refusal=%+v, want 401 UNAUTHORIZED invalid bearer token", response.Code, refusal)
	}
}

// TestTheOtherCredentialsAreUnaffectedByTheBrokerBranch: a cookie caller and a personal-token
// caller resolve exactly as they did, on a server that decides broker tokens too — the personal
// token's write still has to name its own session, since that credential proves none.
func TestTheOtherCredentialsAreUnaffectedByTheBrokerBranch(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)

	person := rig.serve(withPerson(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "", ""), brokerTestPerson))
	if person.Code != http.StatusOK {
		t.Fatalf("whoami as a person: status=%d body=%s", person.Code, person.Body.String())
	}
	if answer := decodeBody[whoamiAnswer](t, person); answer.Kind != "user" || answer.Login != brokerTestPerson {
		t.Fatalf("whoami as a person = %+v, want the signed-in person", answer)
	}

	minted := rig.serve(withPerson(brokerRequest(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "laptop"}, "", ""), brokerTestPerson))
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a personal token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[struct {
		Token string `json:"token"`
	}](t, minted).Token
	if personal == "" {
		t.Fatalf("the minted token is empty: %s", minted.Body.String())
	}

	agent := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+personal, ""))
	if agent.Code != http.StatusOK {
		t.Fatalf("whoami under a personal token: status=%d body=%s", agent.Code, agent.Body.String())
	}
	answer := decodeBody[whoamiAnswer](t, agent)
	if answer.Kind != "agent" || answer.Owner == nil || *answer.Owner != brokerTestPerson {
		t.Fatalf("whoami under a personal token = %+v, want the token's owner", answer)
	}
	if answer.Session != nil || answer.Project != nil {
		t.Fatalf("whoami under a personal token = %+v, want no session and no project: it proves neither", answer)
	}

	unnamed := rig.serve(brokerRequest(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "PRS", "name": "Personal"}, "Bearer "+personal, ""))
	refusal := decodeBody[refusalAnswer](t, unnamed)
	if unnamed.Code != http.StatusBadRequest || refusal.Code != "ACTOR_KIND" {
		t.Fatalf("a personal token writing with no actor: status=%d refusal=%+v, want 400 ACTOR_KIND", unnamed.Code, refusal)
	}
}

// TestABrokerTokensReresolutionIsNotProofCheckedAgain: a connection that outlives its request
// resolves its caller again from a detached copy of that request, whose proof is already spent. A
// second request carrying that proof is refused as the replay it is, while the re-resolution of
// the connection it authenticated asks only whether the token is still live — which is what
// whileCallerResolves calls on every heartbeat (callerStillResolves), driven here directly.
func TestABrokerTokensReresolutionIsNotProofCheckedAgain(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-holding-a-socket-open"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	served := directServer(rig.deps)

	admitted := caller.presenting(t, http.MethodGet, "/ws/live", token, nil)
	actor, human, err := served.optionalActor(admitted)
	if err != nil || human || actor.ID != brokerTestSession {
		t.Fatalf("the upgrade's caller = %+v, human=%v, err=%v; want the token's session", actor, human, err)
	}

	replayed := brokerRequest(t, http.MethodGet, "/ws/live", nil, admitted.Header.Get("Authorization"), admitted.Header.Get("DPoP"))
	if _, _, err := served.optionalActor(replayed); err == nil {
		t.Fatal("a fresh request carrying the spent proof resolved; want it refused as a replay")
	}

	// Two heartbeats of the socket that proof opened. Both carry it, and neither is refused:
	// the re-resolution never looks at it.
	for beat := range 2 {
		if err := served.callerStillResolves(admitted); err != nil {
			t.Fatalf("heartbeat %d: %v, want the caller to keep resolving", beat, err)
		}
	}

	// Once the broker stops holding the token live, the re-resolution refuses and the connection
	// closes. The affirmative answer is remembered for a few seconds, so this reads past it.
	rig.broker.Says(token, ciphertoken.Introspection{Active: false})
	time.Sleep(ciphertoken.AffirmativeCacheTTL + 50*time.Millisecond)
	if err := served.callerStillResolves(admitted); err == nil {
		t.Fatal("a revoked token kept resolving on an open connection")
	}
}

// TestTheRateRefusalCarriesTheWaitInItsHeader: the introspection bucket's refusal tells the
// caller when to come back in Retry-After as well as in its prose, as the broker's own 429s do,
// so a client reads the wait without parsing a sentence. The bucket here holds one request, so
// the second presentation of a token the broker has not been asked about meets it.
func TestTheRateRefusalCarriesTheWaitInItsHeader(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Minute, Burst: 1}
	})
	caller := newBrokerCaller(t)

	spent := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", "the-token-that-spends-the-bucket", nil))
	if refusal := decodeBody[refusalAnswer](t, spent); spent.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_ACTIVE" {
		t.Fatalf("the request that spends the bucket: status=%d refusal=%+v, want 401 BROKER_TOKEN_NOT_ACTIVE", spent.Code, refusal)
	}

	limited := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", "a-second-token", nil))
	refusal := decodeBody[refusalAnswer](t, limited)
	if limited.Code != http.StatusTooManyRequests || refusal.Code != "BROKER_TOKEN_RATE_LIMITED" {
		t.Fatalf("status=%d refusal=%+v, want 429 BROKER_TOKEN_RATE_LIMITED", limited.Code, refusal)
	}
	if got := limited.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want the bucket's minute as whole seconds", got)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the refused request reached the broker: %d introspections, want 1", rig.broker.Count())
	}
}

// TestAServiceOnlyBrokerTokenReadsAndIsNamedByWhoami: a machine nobody has signed in on holds a
// token the broker says names a session and a service, no person and no project. It is a
// session's credential like any other — whoami names the session and the service, and a read
// answers — and only its writes are refused (the test below). whoami answers its project scope,
// person, which is what tells it from a job's token, whose service writes.
func TestAServiceOnlyBrokerTokenReadsAndIsNamedByWhoami(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-naming-no-person"
	rig.broker.Says(token, serviceOnlyToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")

	who := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if who.Code != http.StatusOK {
		t.Fatalf("whoami under a service-only token: status=%d body=%s", who.Code, who.Body.String())
	}
	answer := decodeBody[whoamiAnswer](t, who)
	if answer.Kind != "agent" || answer.Session == nil || *answer.Session != brokerTestSession {
		t.Fatalf("whoami = %+v, want the session the token names", answer)
	}
	if answer.Service == nil || *answer.Service != brokerTestService {
		t.Fatalf("whoami service = %v, want %q", answer.Service, brokerTestService)
	}
	if answer.Owner != nil {
		t.Fatalf("whoami owner = %q, want null: the token names no person", *answer.Owner)
	}
	if answer.Project != nil {
		t.Fatalf("whoami project = %q, want null: the token is held to no one project", *answer.Project)
	}
	if answer.ProjectScope == nil || *answer.ProjectScope != ciphertoken.ProjectScopePerson {
		t.Fatalf("whoami project_scope = %v, want %q: %s", answer.ProjectScope, ciphertoken.ProjectScopePerson, who.Body.String())
	}
	if answer.SignInRequired == nil || !*answer.SignInRequired {
		t.Fatalf("whoami answered %s, want sign_in_required true: Dispatch refuses every write of a machine nobody has signed in on", who.Body.String())
	}

	read := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/issues/"+own.issue, token, nil))
	if read.Code != http.StatusOK {
		t.Fatalf("read an issue under a service-only token: status=%d body=%s", read.Code, read.Body.String())
	}
}

// TestAServiceOnlyBrokerTokenIsRefusedEveryWrite: the refusal is at the one chokepoint register
// wraps every mutating row in, so it covers every write class — a project's object, the
// deployment's own rows, a write whose project its body names, and a session's own rows — and
// the rows below are one of each. Each refusal names the command that makes the write possible,
// and nothing lands.
func TestAServiceOnlyBrokerTokenIsRefusedEveryWrite(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-write"
	rig.broker.Says(token, serviceOnlyToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")

	writes := map[string]struct {
		method, pattern, path string
		body                  any
		class                 writeScope
	}{
		"a comment on its own project's issue": {
			http.MethodPost, "/api/v1/issues/{key}/comments", "/api/v1/issues/" + own.issue + "/comments",
			map[string]any{"body": "Written by nobody."}, scopeProject,
		},
		"an issue whose body names the project": {
			http.MethodPost, "/api/v1/issues", "/api/v1/issues",
			map[string]any{"project": brokerTestProject, "title": "Filed by nobody", "spec": "# Spec"},
			scopeProjectInBody,
		},
		"a project of the deployment's own": {
			http.MethodPost, "/api/v1/projects", "/api/v1/projects",
			map[string]any{"key": "NEW", "name": "New"}, scopeDeployment,
		},
		"an artifact of its own session": {
			http.MethodPost, "/api/v1/agents/{session_id}/artifacts", "/api/v1/agents/" + brokerTestSession + "/artifacts",
			map[string]any{"name": "notes.md", "content": "# Notes"}, scopeSession,
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			if got := writeClasses[write.method+" "+write.pattern]; got != write.class {
				t.Fatalf("%s %s is classed %q, want %q", write.method, write.pattern, got, write.class)
			}
			response := rig.serve(caller.presenting(t, write.method, write.path, token, write.body))
			refusal := decodeBody[refusalAnswer](t, response)
			if response.Code != http.StatusForbidden || refusal.Code != contracts.DispatchSignInRequired {
				t.Fatalf("status=%d refusal=%+v, want 403 %s", response.Code, refusal, contracts.DispatchSignInRequired)
			}
			if !strings.Contains(refusal.Error, "agent-secrets login") {
				t.Fatalf("the refusal does not name the sign-in command: %q", refusal.Error)
			}
			if !strings.Contains(refusal.Error, brokerTestService) {
				t.Fatalf("the refusal does not name the service: %q", refusal.Error)
			}
		})
	}

	issues := rig.asPerson(t, http.MethodGet, "/api/v1/issues?project="+brokerTestProject, nil)
	if strings.Contains(issues.Body.String(), "Filed by nobody") {
		t.Fatalf("a refused write created an issue: %s", issues.Body.String())
	}
	projects := rig.asPerson(t, http.MethodGet, "/api/v1/projects", nil)
	if strings.Contains(projects.Body.String(), `"NEW"`) {
		t.Fatalf("a refused write created a project: %s", projects.Body.String())
	}
}

// TestAPersonlessTokenWritesAsItsAccount: a token an account entry naming no person minted acts as
// that account. whoami names the session and the account as its service, and no person, no
// project and no scope; its writes land, of every class the sign-in check would refuse a
// machine's — a project's object, a write whose body names its project and a session's own
// rows — and each is recorded as the session the token proved, acting for the account and for no
// person, proven. The session check still holds it to its own session's rows: an upload into
// another session's conversation is 403 SESSION_SCOPE and stores nothing. The same writes under a
// machine's token that names a service and no person stay refused
// (TestAServiceOnlyBrokerTokenIsRefusedEveryWrite), and so does a route that needs the person
// scope, which such a token does not hold.
func TestAPersonlessTokenWritesAsItsAccount(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-of-an-account-entry-naming-no-person"
	rig.broker.Says(token, personlessAccountToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")

	who := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if who.Code != http.StatusOK {
		t.Fatalf("whoami under a personless account token: status=%d body=%s", who.Code, who.Body.String())
	}
	answer := decodeBody[whoamiAnswer](t, who)
	if answer.Kind != "agent" || answer.Session == nil || *answer.Session != brokerTestSession ||
		answer.Service == nil || *answer.Service != brokerTestAccount || answer.Owner != nil || answer.Project != nil || len(answer.Scopes) != 0 {
		t.Fatalf("whoami = %+v, want the session %s acting for the service %s, no owner, no project and no scope", answer, brokerTestSession, brokerTestAccount)
	}
	if answer.SignInRequired == nil || *answer.SignInRequired {
		t.Fatalf("whoami answered %s, want sign_in_required false: the account's writes are recorded under it, so no sign-in is needed", who.Body.String())
	}

	comment := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+own.issue+"/comments", token, map[string]any{
		"body": "Written by the account.",
	}))
	if comment.Code != http.StatusCreated {
		t.Fatalf("a comment on a project's issue: status=%d body=%s", comment.Code, comment.Body.String())
	}
	issue := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject, "title": "Filed by the account", "spec": "# Spec",
	}))
	if issue.Code != http.StatusCreated {
		t.Fatalf("an issue whose body names its project: status=%d body=%s", issue.Code, issue.Body.String())
	}
	filed := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key
	type recordedActor struct {
		Kind    string  `json:"kind"`
		ID      string  `json:"id"`
		Owner   *string `json:"owner"`
		Service *string `json:"service"`
		Project *string `json:"project"`
		Proven  bool    `json:"proven"`
	}
	requireTheAccount := func(t *testing.T, what string, actor recordedActor) {
		t.Helper()
		if actor.Kind != "session" || actor.ID != brokerTestSession || !actor.Proven {
			t.Fatalf("%s is recorded as %s:%s (proven %t), want the session %s the token proved", what, actor.Kind, actor.ID, actor.Proven, brokerTestSession)
		}
		if actor.Service == nil || *actor.Service != brokerTestAccount {
			t.Fatalf("%s is recorded under the service %v, want the account %s", what, actor.Service, brokerTestAccount)
		}
		if actor.Owner != nil || actor.Project != nil {
			t.Fatalf("%s is recorded with owner %v and project %v, want neither: the token names no person and no project", what, actor.Owner, actor.Project)
		}
	}
	upload := rig.serve(caller.presentingUpload(t, "/api/v1/agents/"+brokerTestSession+"/artifacts", token,
		"shot.png", "image/png", pngBytes("the account's own conversation")))
	if upload.Code != http.StatusCreated {
		t.Fatalf("an upload into its own session's conversation: status=%d body=%s", upload.Code, upload.Body.String())
	}
	requireTheAccount(t, "the session's upload", decodeBody[struct {
		Artifact struct {
			CreatedBy recordedActor `json:"created_by"`
		} `json:"artifact"`
	}](t, upload).Artifact.CreatedBy)

	// The session check holds the account's token to the session it proved, as it holds every
	// broker token (callerSession): the upload above went into its own conversation, and the same
	// upload into another session's is refused by name before anything is stored there.
	t.Run("another session's rows are refused", func(t *testing.T) {
		const anotherSession = "01a1058e-f14f-7684-87eb-3dc885955552"
		intruding := rig.serve(caller.presentingUpload(t, "/api/v1/agents/"+anotherSession+"/artifacts", token,
			"shot.png", "image/png", pngBytes("another session's conversation")))
		if intruding.Code != http.StatusForbidden {
			t.Fatalf("an upload into another session's conversation: status=%d body=%s, want 403 %s", intruding.Code, intruding.Body.String(), codeSessionScope)
		}
		refusal := decodeBody[refusalAnswer](t, intruding)
		if refusal.Code != codeSessionScope {
			t.Fatalf("an upload into another session's conversation: refusal=%+v, want %s", refusal, codeSessionScope)
		}
		if !strings.Contains(refusal.Error, brokerTestSession) || !strings.Contains(refusal.Error, anotherSession) {
			t.Fatalf("refusal %q names neither the session the token proved (%s) nor the session addressed (%s)",
				refusal.Error, brokerTestSession, anotherSession)
		}
		read := rig.serve(withPerson(brokerRequest(t, http.MethodGet,
			"/api/v1/agents/"+anotherSession+"/artifacts/shot-png", nil, "", ""), brokerTestPerson))
		if read.Code != http.StatusNotFound {
			t.Fatalf("the other session's conversation: status=%d body=%s, want nothing stored in it", read.Code, read.Body.String())
		}
	})

	for _, key := range []string{own.issue, filed} {
		listed := rig.serve(withPerson(brokerRequest(t, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "", ""), brokerTestPerson))
		if listed.Code != http.StatusOK {
			t.Fatalf("read %s's events: status=%d body=%s", key, listed.Code, listed.Body.String())
		}
		var written []recordedActor
		for _, event := range decodeBody[[]struct {
			Type  string        `json:"type"`
			Actor recordedActor `json:"actor"`
		}](t, listed) {
			if event.Actor.ID == brokerTestSession {
				written = append(written, event.Actor)
			}
		}
		if len(written) == 0 {
			t.Fatalf("%s's events name no write of the session %s: %s", key, brokerTestSession, listed.Body.String())
		}
		for _, actor := range written {
			requireTheAccount(t, key+"'s event", actor)
		}
	}

	refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/broadcasts", token, map[string]any{
		"body": "To every session.",
	}))
	if refusal := decodeBody[refusalAnswer](t, refused); refused.Code != http.StatusForbidden || refusal.Code != "OAUTH_SCOPE_REQUIRED" {
		t.Fatalf("a broadcast, which needs the person scope: status=%d refusal=%+v, want 403 OAUTH_SCOPE_REQUIRED: the token holds no scope, and its writes are not refused %s", refused.Code, refusal, contracts.DispatchSignInRequired)
	}
}

// TestWhoamiSaysNoSignInForEveryCredentialThatMayWrite: whoami answers sign_in_required false for
// every credential the sign-in check lets write — a personal token, the deployment's shared
// token, a verified service-account token and a broker token that names a person — so a client
// reading the field tells none of them to run `agent-secrets login`. The broker tokens naming a
// service and no person are tested elsewhere: a machine's answers true, and an account's and a
// job's answer false (TestAPersonlessTokenWritesAsItsAccount, TestAJobBrokerTokenIsNamedByWhoami).
func TestWhoamiSaysNoSignInForEveryCredentialThatMayWrite(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{oidc: issuer.verifier(t)})
	caller := newBrokerCaller(t)
	const brokerToken = "a-token-that-names-a-person"
	rig.broker.Says(brokerToken, personScopedTokenAnswer(caller.jkt))
	for _, row := range []struct {
		name string
		call func() *httptest.ResponseRecorder
	}{
		{"a personal token", func() *httptest.ResponseRecorder {
			return agentRequest(t, rig.handler, http.MethodGet, "/api/v1/whoami", nil, personalTokenHolding(t, rig.database, model.ScopePerson))
		}},
		{"the deployment's shared token", func() *httptest.ResponseRecorder {
			return agentRequest(t, rig.handler, http.MethodGet, "/api/v1/whoami", nil, "agent-token")
		}},
		{"a verified service-account token", func() *httptest.ResponseRecorder {
			return agentRequest(t, rig.handler, http.MethodGet, "/api/v1/whoami", nil, issuer.mint(t, serviceTokenAudience))
		}},
		{"a broker token that names a person", func() *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", brokerToken, nil))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			response := row.call()
			if response.Code != http.StatusOK {
				t.Fatalf("whoami under %s: status=%d body=%s", row.name, response.Code, response.Body.String())
			}
			answer := decodeBody[whoamiAnswer](t, response)
			if answer.Kind != "agent" || answer.SignInRequired == nil || *answer.SignInRequired {
				t.Fatalf("whoami under %s answered %s, want kind agent and sign_in_required false: Dispatch lets it write", row.name, response.Body.String())
			}
		})
	}
}

// TestAServiceOnlyBrokerTokenWithNoProofIsRefused: the new caller kind changes nothing about how
// a token is presented. A DPoP-bound token sent with no proof at all is refused where it always
// was — the broker holds it live and the proof check is what turns it down — so no write
// refusal is ever reached, there being no caller to refuse, and a write under it lands nothing.
func TestAServiceOnlyBrokerTokenWithNoProofIsRefused(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-sent-without-its-proof"
	rig.broker.Says(token, serviceOnlyToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	response := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "DPoP "+token, ""))
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_PROOF_INVALID" {
		t.Fatalf("status=%d refusal=%+v, want 401 BROKER_TOKEN_PROOF_INVALID", response.Code, refusal)
	}

	written := rig.serve(brokerRequest(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "Filed with no proof", "spec": "# Spec",
	}, "DPoP "+token, ""))
	refused := decodeBody[refusalAnswer](t, written)
	if written.Code != http.StatusUnauthorized || refused.Code != "BROKER_TOKEN_PROOF_INVALID" {
		t.Fatalf("a write: status=%d refusal=%+v, want 401 BROKER_TOKEN_PROOF_INVALID", written.Code, refused)
	}
	issues := rig.asPerson(t, http.MethodGet, "/api/v1/issues?project="+brokerTestProject, nil)
	if strings.Contains(issues.Body.String(), "Filed with no proof") {
		t.Fatalf("the unproved write created an issue: %s", issues.Body.String())
	}
}

// TestAJobBrokerTokenWritesAsItsSessionForItsService: a job's token names its identity as its
// service and no person, and its writes land in the one project its entry names: an issue, a
// comment on it and a change of its status. Each write's event names the session the token proved
// as its actor, the job's identity as the service it acted for, and no owner; each write's
// access-log line names that session as its caller and the broker token as its credential, with
// the identity under the `service:` prefix. It captures the default logger, so it runs alone.
func TestAJobBrokerTokenWritesAsItsSessionForItsService(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-job-token-naming-one-project"
	rig.broker.Says(token, liveJobNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	logged := captureLog(t)

	created := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": brokerTestProject, "title": "Filed by a job", "spec": "# Spec",
	}))
	if created.Code != http.StatusCreated {
		t.Fatalf("a job's issue in its own project: status=%d body=%s, want 201", created.Code, created.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	commented := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", token, map[string]any{
		"body": "Written by a job.",
	}))
	if commented.Code != http.StatusCreated {
		t.Fatalf("a job's comment: status=%d body=%s, want 201", commented.Code, commented.Body.String())
	}
	moved := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+key, token, map[string]any{
		"status": "in_progress",
	}))
	if moved.Code != http.StatusOK {
		t.Fatalf("a job's status change: status=%d body=%s, want 200", moved.Code, moved.Body.String())
	}

	lines := accessLines(t, logged)
	if len(lines) != 3 {
		t.Fatalf("three writes wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		for _, want := range []string{`caller_kind=session`, `caller=` + brokerTestSession, `credential=broker-token:service:` + brokerTestJob} {
			if !strings.Contains(line, want) {
				t.Errorf("access-log line %q does not carry %q", line, want)
			}
		}
	}

	listed := rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+key+"/events", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("read the issue's events: status=%d body=%s", listed.Code, listed.Body.String())
	}
	seen := map[string]bool{}
	for _, event := range decodeBody[[]model.Event](t, listed) {
		switch event.Type {
		case "issue.created", "comment.created", "issue.updated":
		default:
			continue
		}
		seen[event.Type] = true
		actor := event.Actor
		if actor.Kind != "session" || actor.ID != brokerTestSession {
			t.Errorf("the %s event's actor = %s:%s, want the session the token proved", event.Type, actor.Kind, actor.ID)
		}
		if actor.Service == nil || *actor.Service != brokerTestJob {
			t.Errorf("the %s event's service = %v, want %q", event.Type, actor.Service, brokerTestJob)
		}
		if actor.Owner != nil {
			t.Errorf("the %s event's owner = %q, want none: the token names no person", event.Type, *actor.Owner)
		}
	}
	if !seen["issue.created"] || !seen["comment.created"] || !seen["issue.updated"] {
		t.Fatalf("the issue's events hold %v, want issue.created, comment.created and issue.updated", seen)
	}
}

// TestAJobBrokerTokenWritesEveryClass: the write rule that refuses a machine nobody signed in on
// lets a job's token through at the same chokepoint, so a job's public token writes every class
// TestAServiceOnlyBrokerTokenIsRefusedEveryWrite is refused: a project's object, a write whose
// project its body names, the deployment's own rows, and its own session's rows.
func TestAJobBrokerTokenWritesEveryClass(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-job-token-naming-public"
	rig.broker.Says(token, liveJobPublicToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")

	writes := map[string]struct {
		method, pattern string
		class           writeScope
		request         func(t *testing.T) *http.Request
	}{
		"a comment on a project's issue": {
			http.MethodPost, "/api/v1/issues/{key}/comments", scopeProject, func(t *testing.T) *http.Request {
				return caller.presenting(t, http.MethodPost, "/api/v1/issues/"+own.issue+"/comments", token,
					map[string]any{"body": "Written by a job."})
			},
		},
		"an issue whose body names the project": {
			http.MethodPost, "/api/v1/issues", scopeProjectInBody, func(t *testing.T) *http.Request {
				return caller.presenting(t, http.MethodPost, "/api/v1/issues", token,
					map[string]any{"project": brokerTestProject, "title": "Filed by a job", "spec": "# Spec"})
			},
		},
		"a project of the deployment's own": {
			http.MethodPost, "/api/v1/projects", scopeDeployment, func(t *testing.T) *http.Request {
				// A credential that acts for nobody names the person the project's unowned work
				// lands on, which a person's create would default to themselves.
				return caller.presenting(t, http.MethodPost, "/api/v1/projects", token,
					map[string]any{"key": "JOB", "name": "Job", "default_assignee": brokerTestPerson})
			},
		},
		"a file in its own session's conversation": {
			http.MethodPost, "/api/v1/agents/{session_id}/artifacts", scopeSession, func(t *testing.T) *http.Request {
				return caller.presentingUpload(t, "/api/v1/agents/"+brokerTestSession+"/artifacts", token,
					"shot.png", "image/png", pngBytes("a job's conversation"))
			},
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			if got := writeClasses[write.method+" "+write.pattern]; got != write.class {
				t.Fatalf("%s %s is classed %q, want %q", write.method, write.pattern, got, write.class)
			}
			response := rig.serve(write.request(t))
			if response.Code < 200 || response.Code > 299 {
				t.Fatalf("status=%d body=%s, want 2xx", response.Code, response.Body.String())
			}
		})
	}
}

// TestAJobsNamedTokenIsJudgedAsAPersonsNamedTokenForTheSameProject: a job's token whose entry
// names one project is judged by the rule that judges a person's named token for that project.
// Each write below (one whose body names another project, one to another project's object, and
// the deployment's own project create) is sent under the job's token and under a person's named
// token for the same project, and the job's answer has the status and the refusal code of the
// person's. That answer is 201 for the issue made in the other project and for the comment on
// the other project's issue, since a token that names one project writes a public project's
// objects as a credential that names none does, and 403 PROJECT_SCOPE_DEPLOYMENT for the project
// create (dispatch://INFRA-264). A person reads each write back: the job's write lands where the
// answer is 201, and lands nothing where it is a refusal. The job's writes into its own project
// are TestAJobBrokerTokenWritesAsItsSessionForItsService's.
func TestAJobsNamedTokenIsJudgedAsAPersonsNamedTokenForTheSameProject(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const jobToken, personToken = "a-job-token-held-to-one-project", "a-persons-named-token-for-the-same-project"
	rig.broker.Says(jobToken, liveJobNamedToken(caller.jkt))
	rig.broker.Says(personToken, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTHER", "Other")

	for name, write := range map[string]struct {
		method, path string
		// job and person are the write's body under each token, each with a text of its own, so
		// the job's write is told from the person's where both land.
		job, person any
		// read is where a person reads the job's write back, and written the text it shows where
		// the write landed, and must not show where it was refused.
		read, written string
		// status and code are the answer both tokens get: its status, and its refusal code, which
		// is empty for a write that lands.
		status int
		code   string
	}{
		"an issue whose body names another project": {
			method: http.MethodPost, path: "/api/v1/issues",
			job:    map[string]any{"project": "OTHER", "title": "Filed by a job elsewhere", "spec": "# Spec"},
			person: map[string]any{"project": "OTHER", "title": "Filed by a person elsewhere", "spec": "# Spec"},
			read:   "/api/v1/issues?project=OTHER", written: "Filed by a job elsewhere",
			status: http.StatusCreated,
		},
		"a comment on another project's issue": {
			method: http.MethodPost, path: "/api/v1/issues/" + other.issue + "/comments",
			job:    map[string]any{"body": "Written by a job elsewhere."},
			person: map[string]any{"body": "Written by a person elsewhere."},
			read:   "/api/v1/issues/" + other.issue + "/comments", written: "Written by a job elsewhere.",
			status: http.StatusCreated,
		},
		"a project of the deployment's own": {
			method: http.MethodPost, path: "/api/v1/projects",
			job:    map[string]any{"key": "JOB", "name": "Job", "default_assignee": brokerTestPerson},
			person: map[string]any{"key": "PERSON", "name": "Person", "default_assignee": brokerTestPerson},
			read:   "/api/v1/projects", written: `"JOB"`,
			status: http.StatusForbidden, code: codeProjectScopeDeployment,
		},
	} {
		t.Run(name, func(t *testing.T) {
			persons := rig.serve(caller.presenting(t, write.method, write.path, personToken, write.person))
			jobs := rig.serve(caller.presenting(t, write.method, write.path, jobToken, write.job))
			personCode := decodeBody[refusalAnswer](t, persons).Code
			if persons.Code != write.status || personCode != write.code {
				t.Fatalf("the person's named token for the same project: status=%d code=%q body=%s, want status=%d code=%q",
					persons.Code, personCode, persons.Body.String(), write.status, write.code)
			}
			if decodeBody[refusalAnswer](t, jobs).Code != personCode || jobs.Code != persons.Code {
				t.Fatalf("the job's token: status=%d body=%s, want the answer of the person's named token for the same project: status=%d body=%s",
					jobs.Code, jobs.Body.String(), persons.Code, persons.Body.String())
			}
			read := rig.asPerson(t, http.MethodGet, write.read, nil)
			if read.Code != http.StatusOK {
				t.Fatalf("read %s back: status=%d body=%s", write.read, read.Code, read.Body.String())
			}
			if landed, want := strings.Contains(read.Body.String(), write.written), write.status < http.StatusBadRequest; landed != want {
				t.Fatalf("the job's write landed: %t, want %t: %s", landed, want, read.Body.String())
			}
		})
	}
}

// TestAJobsPublicTokenWritesEveryProjectAndTheDeployment: a job's token whose entry names public
// writes wherever a token that names no project may, projects its list does not name included,
// and its writes that belong to the deployment follow that rule too: the project check lets them
// through.
func TestAJobsPublicTokenWritesEveryProjectAndTheDeployment(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-job-token-writing-every-project"
	rig.broker.Says(token, liveJobPublicToken(caller.jkt, "PRIV"))
	seedProject(t, rig, brokerTestProject, "Broker")
	seedProject(t, rig, "OTHER", "Other")

	for _, project := range []string{brokerTestProject, "OTHER"} {
		written := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
			"project": project, "title": "Filed by a public job", "spec": "# Spec",
		}))
		if written.Code != http.StatusCreated {
			t.Fatalf("a public job's issue in %s: status=%d body=%s, want 201", project, written.Code, written.Body.String())
		}
		if key := decodeBody[struct {
			Key string `json:"key"`
		}](t, written).Key; !strings.HasPrefix(key, project+"-") {
			t.Fatalf("a public job's issue in %s was filed as %q", project, key)
		}
	}
	created := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/projects", token, map[string]any{
		"key": "JOB", "name": "Job", "default_assignee": brokerTestPerson,
	}))
	if created.Code != http.StatusCreated {
		t.Fatalf("a public job's project: status=%d body=%s, want 201", created.Code, created.Body.String())
	}
}

// TestAJobBrokerTokenIsNamedByWhoami: whoami names a job's token by the session it proved and the
// identity its entry lists as its service, with no owner and no OAuth scope; the project is the
// one a named token's entry names, and none for a public token, which no one project limits; and
// the project scope is the token's own, named or public, and sign_in_required is false, which is
// what tells a job's token, whose service writes, from a machine's, which is refused every write.
func TestAJobBrokerTokenIsNamedByWhoami(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	for _, row := range []struct {
		name, token string
		answer      ciphertoken.Introspection
		project     *string
		scope       string
	}{
		{"a job's named token", "a-job-named-token-asking-who-it-is", liveJobNamedToken(caller.jkt), new(brokerTestProject), ciphertoken.ProjectScopeNamed},
		{"a job's public token", "a-job-public-token-asking-who-it-is", liveJobPublicToken(caller.jkt, "PRIV"), nil, ciphertoken.ProjectScopePublic},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig.broker.Says(row.token, row.answer)
			response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", row.token, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("whoami: status=%d body=%s", response.Code, response.Body.String())
			}
			answer := decodeBody[whoamiAnswer](t, response)
			if answer.Kind != "agent" || answer.Session == nil || *answer.Session != brokerTestSession {
				t.Fatalf("whoami = %s, want the session the token names", response.Body.String())
			}
			if answer.Service == nil || *answer.Service != brokerTestJob {
				t.Fatalf("whoami service = %v, want %q", answer.Service, brokerTestJob)
			}
			if answer.Owner != nil {
				t.Fatalf("whoami owner = %q, want null: the token names no person", *answer.Owner)
			}
			if answer.Scopes == nil || len(answer.Scopes) != 0 {
				t.Fatalf("whoami scopes = %v, want an empty array: a job's token holds no OAuth scope", answer.Scopes)
			}
			switch {
			case row.project == nil && answer.Project != nil:
				t.Fatalf("whoami project = %q, want null", *answer.Project)
			case row.project != nil && (answer.Project == nil || *answer.Project != *row.project):
				t.Fatalf("whoami project = %v, want %q", answer.Project, *row.project)
			}
			if answer.ProjectScope == nil || *answer.ProjectScope != row.scope {
				t.Fatalf("whoami project_scope = %v, want %q: %s", answer.ProjectScope, row.scope, response.Body.String())
			}
			if answer.SignInRequired == nil || *answer.SignInRequired {
				t.Fatalf("whoami answered %s, want sign_in_required false: a job's token writes as its service", response.Body.String())
			}
		})
	}
}

// TestAJobTokenWritesOnlyAsItsOwnSession: a job's token proved one session, and a write whose body
// names another is refused ACTOR_SESSION_MISMATCH naming the session the token proved, as for
// every broker token, so a job writes under no session but its own.
func TestAJobTokenWritesOnlyAsItsOwnSession(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	own := seedProject(t, rig, brokerTestProject, "Broker")
	for _, row := range []struct {
		name, token string
		answer      ciphertoken.Introspection
	}{
		{"a job's named token", "a-job-named-token-naming-another-session", liveJobNamedToken(caller.jkt)},
		{"a job's public token", "a-job-public-token-naming-another-session", liveJobPublicToken(caller.jkt)},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig.broker.Says(row.token, row.answer)
			response := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+own.issue+"/comments", row.token, map[string]any{
				"body": "Written as another session.", "actor": map[string]string{"kind": "session", "id": "ses_someone_else"},
			}))
			refusal := decodeBody[refusalAnswer](t, response)
			if response.Code != http.StatusForbidden || refusal.Code != "ACTOR_SESSION_MISMATCH" || !strings.Contains(refusal.Error, brokerTestSession) {
				t.Fatalf("status=%d refusal=%+v, want 403 ACTOR_SESSION_MISMATCH naming %s", response.Code, refusal, brokerTestSession)
			}
		})
	}
}

// TestANarrowedTokenActsAsItsIdentityAndOnlyReads: a token narrowed to reading acts as the
// identity the same trade without read would have, and holds less. For each answer of the golden
// file the broker's own test holds its narrowed answers to, the actor is the session it names,
// with the person or the service it names; a token narrowed from a named one names its one
// project; a read-person token that names its person holds the person scope alone; every other
// holds no scope. Each is marked read-only with the cut its answer names, and none holds approve
// or carries the word read among its scopes. The control is the machine login's answer with the
// project scope person and no read: the same identity, holding both scopes and not read-only, so
// approve is withheld by the narrowing.
func TestANarrowedTokenActsAsItsIdentityAndOnlyReads(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const session = "ses_narrowed"
	answers := ciphertokentest.NarrowedAnswers(t, caller.jkt, session, time.Now().Add(time.Hour).Unix())
	ada, fleet, reader, account, acme := "ada@example.com", brokerTestService, brokerTestReader, brokerTestAccount, "ACME"
	cut, err := time.Parse(time.RFC3339Nano, answers["machine-login-with-a-cut"].AsOf)
	if err != nil {
		t.Fatalf("the golden machine login's cut: %v", err)
	}
	type identity struct {
		owner, service, project *string
		scopes                  []string
		readOnly                bool
		cut                     time.Time
	}
	personOnly := []string{model.ScopePerson}
	want := map[string]identity{
		"pod-entry":                     {owner: &ada, project: &acme, readOnly: true},
		"pod-entry-no-person":           {service: &account, readOnly: true},
		"read-entry":                    {service: &reader, readOnly: true},
		"workflow-entry":                {owner: &ada, project: &acme, readOnly: true},
		"google-identity-no-person":     {service: &fleet, readOnly: true},
		"google-identity-with-a-person": {owner: &ada, project: &acme, readOnly: true},
		"person-sign-in":                {owner: &ada, scopes: personOnly, readOnly: true},
		"machine-login":                 {owner: &ada, scopes: personOnly, readOnly: true},
		"machine-login-with-a-cut":      {owner: &ada, scopes: personOnly, readOnly: true, cut: cut},
	}
	if kinds := slices.Sorted(maps.Keys(answers)); !slices.Equal(kinds, slices.Sorted(maps.Keys(want))) {
		t.Fatalf("the golden file answers for %v, and this test names the actor of %v", kinds, slices.Sorted(maps.Keys(want)))
	}
	writer := answers["machine-login"]
	writer.ProjectScope, writer.Scope = ciphertoken.ProjectScopePerson, ""
	rows := map[string]ciphertoken.Introspection{"the machine login's token with no read": writer}
	// The same token without read writes: it holds the person scope alone, as every broker token
	// that acts for its person does (brokerPersonScopes), and it is not read-only.
	want["the machine login's token with no read"] = identity{owner: &ada, scopes: personOnly}
	maps.Copy(rows, answers)

	same := func(got, want *string) bool { return (got == nil) == (want == nil) && (got == nil || *got == *want) }
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		t.Run(name, func(t *testing.T) {
			token := "a-token-of-" + strings.ReplaceAll(name, " ", "-")
			rig.broker.Says(token, rows[name])
			actor, human, err := rig.served.brokerTokenActor(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
			if err != nil || human {
				t.Fatalf("brokerTokenActor = %+v, human %v, %v; want the session's actor", actor, human, err)
			}
			expected := want[name]
			if actor.Kind != "session" || actor.ID != session || !actor.Proven {
				t.Fatalf("actor = %s:%s (proven %v), want the proven session %s", actor.Kind, actor.ID, actor.Proven, session)
			}
			if !same(actor.Owner, expected.owner) || !same(actor.Service, expected.service) || !same(actor.Project, expected.project) {
				t.Fatalf("actor owner, service, project = %v, %v, %v; want %v, %v, %v",
					actor.Owner, actor.Service, actor.Project, expected.owner, expected.service, expected.project)
			}
			credential := actor.Credential
			if credential == nil || credential.Session != session {
				t.Fatalf("credential = %+v, want one naming the session %s", credential, session)
			}
			if !slices.Equal(credential.Scopes, expected.scopes) {
				t.Fatalf("credential scopes = %v, want %v", credential.Scopes, expected.scopes)
			}
			if credential.ReadOnly != expected.readOnly {
				t.Fatalf("credential read-only = %v, want %v", credential.ReadOnly, expected.readOnly)
			}
			if !credential.Cut.Equal(expected.cut) || credential.Cut.Location() != time.UTC {
				t.Fatalf("credential cut = %v, want %v in UTC", credential.Cut, expected.cut)
			}
		})
	}
}
