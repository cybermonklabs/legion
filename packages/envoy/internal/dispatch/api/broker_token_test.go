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
	"net/http"
	"net/http/httptest"
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
	// brokerTestReader is the service a read-only token names: the one a read entry of the
	// broker's configuration names for the account its token is exchanged from.
	brokerTestReader = "example-reader"
	// brokerTestProject is the one project a named-scope token may write.
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
			Account:   ciphertoken.NewAccountToken(accountTokenFile(t, credentialTestAccountToken)),
			Origin:    testServerURL,
			Replay:    store.NewDPoPProofs(database.Pool),
			// What Dispatch itself passes: the check keys its bucket on the address
			// Dispatch's own proxy-trust rule reads, never one of its own, and serves a
			// read-only token on the routes its read list names and a results token on the
			// routes its results list names (cmd/dispatch, brokerTokenVerifier).
			Address:        auth.ForwardedAddress,
			ServesReadOnly: true,
			ServesResults:  true,
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
	Kind    string  `json:"kind"`
	Login   string  `json:"login"`
	Owner   *string `json:"owner"`
	Service *string `json:"service"`
	Session *string `json:"session"`
	Project *string `json:"project"`
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
// project, so the caller carries none at all rather than an empty one.
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
// projects, and Dispatch writes its project limit only for one; such an answer is refused before
// any write rather than read as a caller no project limits.
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

// TestANamedBrokerTokenLimitsWritesToItsProject: a token an operator's entry mapped to one
// project writes that project and no other, as it always has.
func TestANamedBrokerTokenLimitsWritesToItsProject(t *testing.T) {
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
	if refusal := decodeBody[refusalAnswer](t, other); other.Code != http.StatusForbidden || refusal.Code != codeProjectScope {
		t.Fatalf("a named token's write into another project: status=%d body=%s, want 403 %s", other.Code, other.Body.String(), codeProjectScope)
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
// answers — and only its writes are refused (the test below).
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
