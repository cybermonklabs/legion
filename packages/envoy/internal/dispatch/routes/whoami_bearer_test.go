package routes

// GET /auth/whoami for a request with no browser credential whose bearer the API takes for an
// agent (authWhoami): the dashboard is drawn for that agent, by the label its credential gives it,
// and a browser credential still decides alone wherever a request carries one.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	// bearerOrigin is the origin the bearer rows send their requests to, which a broker-issued
	// token's proof names and the router's verifier checks it against.
	bearerOrigin = "http://dispatch.test"
	// bearerSession is the session the stub broker says its tokens act for.
	bearerSession = "ses_a1b2c3d4e5f6"
	// sharedToken is the deployment's shared agent token the rows configure, the e2e stack's own.
	sharedToken = "e2e-token"
)

// bearerRouter is storeRouter with the shared agent token and a secrets broker's tokens checked
// against broker, as cmd/dispatch configures both.
func bearerRouter(t *testing.T, database *store.Store, broker *ciphertokentest.StubBroker) http.Handler {
	t.Helper()
	account := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(account, []byte("dispatch-account-token"), 0o600); err != nil {
		t.Fatalf("write the account token: %v", err)
	}
	verifier, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL:      broker.Server.URL,
		Account:        ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", account),
		Origins:        []string{bearerOrigin},
		Replay:         store.NewDPoPProofs(database.Pool),
		Address:        auth.ForwardedAddress,
		ServesReadOnly: true,
	})
	if err != nil {
		t.Fatalf("build the broker token verifier: %v", err)
	}
	return storeRouterWith(t, database, func(options *AppContextOptions) {
		options.AgentTokens = sharedAgentTokens(t, sharedToken)
		options.BrokerTokens = verifier
	})
}

// brokerToken is the stub broker's answer for a live token bound to jkt acting for bearerSession,
// for person where it names one and otherwise for a service. A read-only token is the shape the
// broker mints from a read entry: a service, the scope read and the project scope none.
func brokerToken(jkt, person string, readOnly bool) ciphertoken.Introspection {
	answer := ciphertoken.Introspection{
		Active:       true,
		Session:      bearerSession,
		Person:       person,
		ProjectScope: ciphertoken.ProjectScopePerson,
		Scope:        "dispatch.write",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
	if person == "" {
		answer.Service = "example-fleet"
	}
	if readOnly {
		answer.Service, answer.ProjectScope, answer.Scope = "example-reader", ciphertoken.ProjectScopeNone, ciphertoken.ScopeRead
	}
	return answer
}

// credential is how one row's request authenticates: the headers it carries for a GET of path,
// a broker-issued token's proof signed afresh for each request, since a proof is single use.
type credential func(t *testing.T, path string) http.Header

// bearer is the credential of a plain bearer token.
func bearer(token string) credential {
	return func(*testing.T, string) http.Header {
		return http.Header{"Authorization": {"Bearer " + token}}
	}
}

// dpop is the credential of a broker-issued token, presented under the DPoP scheme by caller.
func dpop(caller *ciphertokentest.Caller, token string) credential {
	return func(t *testing.T, path string) http.Header {
		signed := caller.Signed(t, http.MethodGet, bearerOrigin+path, token)
		return http.Header{"Authorization": {signed.Header.Get("Authorization")}, "DPoP": {signed.Header.Get("DPoP")}}
	}
}

// serveGet serves a GET of path at bearerOrigin carrying headers and no browser credential.
func serveGet(handler http.Handler, path string, headers http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, bearerOrigin+path, nil)
	request.RemoteAddr = "203.0.113.9:40001"
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// agentAnswerOf decodes an agent's whoami answer, failing the test unless it is a 200.
func agentAnswerOf(t *testing.T, response *httptest.ResponseRecorder) whoamiAgentAnswer {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("whoami: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	var answer whoamiAgentAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode whoami %s: %v", response.Body.String(), err)
	}
	return answer
}

// callerFields is what an access-log line says of the caller it served: the kind, the caller and
// the credential behind it.
var callerFields = regexp.MustCompile(`caller_kind=\S+ caller=\S+|credential=\S+`)

// lastCaller is what the last access-log line of route says of its caller.
func lastCaller(t *testing.T, logged *logBuffer, route string) []string {
	t.Helper()
	lines := routeLines(logged, route)
	if len(lines) == 0 {
		t.Fatalf("no access-log line for %s in %q", route, logged.String())
	}
	return callerFields.FindAllString(lines[len(lines)-1], -1)
}

// mintPersonalToken mints a personal agent token of alice@d.example's through the router's API,
// as a person mints one on the Settings page.
func mintPersonalToken(t *testing.T, handler http.Handler) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, bearerOrigin+"/api/v1/me/agent-tokens", strings.NewReader(`{"name":"dashboard"}`))
	request.Header.Set("X-Dispatch-User", "alice@d.example")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("mint a personal token: status=%d body=%s", response.Code, response.Body.String())
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &minted); err != nil || minted.Token == "" {
		t.Fatalf("decode the minted token %s: %v", response.Body.String(), err)
	}
	return minted.Token
}

// TestAuthWhoamiAnswersEachAgentByTheLabelRule: a bearer the API takes for an agent is answered
// kind agent, the session its credential proved (null where it proved none) and event_head as a
// person's is, under the login the rule in authWhoami gives it: the owner its credential acts for,
// else agent:<session>, else agent. Its access-log line names the caller exactly as GET
// /api/v1/whoami's line names the same credential's.
func TestAuthWhoamiAnswersEachAgentByTheLabelRule(t *testing.T) {
	database := storetest.Open(t)
	broker := ciphertokentest.NewStubBroker(t)
	handler := bearerRouter(t, database, broker)
	personal := mintPersonalToken(t, handler)
	serviceCaller, personCaller := ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t)
	broker.Says("service-token", brokerToken(serviceCaller.JKT(), "", false))
	broker.Says("person-token", brokerToken(personCaller.JKT(), "Alice@D.example", false))
	session := bearerSession

	for _, row := range []struct {
		name       string
		credential credential
		login      string
		session    *string
	}{
		{"the shared token names no owner and proves no session", bearer(sharedToken), "agent", nil},
		{"a personal token names its owner", bearer(personal), "alice@d.example", nil},
		{"a broker token naming a service proves its session", dpop(serviceCaller, "service-token"), "agent:" + bearerSession, &session},
		{"a broker token naming a person is that person's, its session beside", dpop(personCaller, "person-token"), "alice@d.example", &session},
	} {
		t.Run(row.name, func(t *testing.T) {
			logged := captureAccessLog(t)
			answer := agentAnswerOf(t, serveGet(handler, "/auth/whoami", row.credential(t, "/auth/whoami")))
			if answer.Kind != "agent" || answer.Login != row.login {
				t.Fatalf("whoami = %+v, want kind agent with login %q", answer, row.login)
			}
			if (answer.Session == nil) != (row.session == nil) || answer.Session != nil && *answer.Session != *row.session {
				t.Fatalf("whoami session = %v, want %v", answer.Session, row.session)
			}
			if head := strconv.FormatInt(newestEventID(t, database), 10); answer.EventHead != head {
				t.Fatalf("whoami event_head = %q, want the log's newest id %s, as a person is answered", answer.EventHead, head)
			}
			dashboard := lastCaller(t, logged, "/auth/whoami")
			if apiWhoami := serveGet(handler, "/api/v1/whoami", row.credential(t, "/api/v1/whoami")); apiWhoami.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/whoami: status=%d body=%s", apiWhoami.Code, apiWhoami.Body.String())
			}
			if api := lastCaller(t, logged, "/api/v1/whoami"); len(dashboard) == 0 || !slices.Equal(dashboard, api) {
				t.Fatalf("GET /auth/whoami's line names %v, GET /api/v1/whoami's %v: want the same caller", dashboard, api)
			}
		})
	}

	// The whole answer, in the order its keys have always had, with session last.
	response := serveGet(handler, "/auth/whoami", bearer(sharedToken)(t, "/auth/whoami"))
	want := `{"event_head":"` + strconv.FormatInt(newestEventID(t, database), 10) + `","kind":"agent","login":"agent","session":null}`
	if got := response.Body.String(); response.Code != http.StatusOK || got != want+"\n" {
		t.Fatalf("whoami under the shared token: %d %q, want 200 %s", response.Code, got, want)
	}
}

// TestAuthWhoamiAnswersABearerTheAPIRefusesAsItAnswersNone: a bearer the API refuses - a token it
// does not know, or a broker token the broker does not hold live - and a read-only token, which
// GET /api/v1/whoami answers 200 (main #4557) but which can save nothing the dashboard draws
// (dashboardAgent), are each answered exactly as a request carrying no bearer: 401 NO_IDENTITY,
// which sends a page to sign in. The shared token on the same router is answered as an agent, so
// the refusal is the bearer's own.
func TestAuthWhoamiAnswersABearerTheAPIRefusesAsItAnswersNone(t *testing.T) {
	database := storetest.Open(t)
	broker := ciphertokentest.NewStubBroker(t)
	handler := bearerRouter(t, database, broker)
	reader, lapsed := ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t)
	broker.Says("read-only-token", brokerToken(reader.JKT(), "", true))
	broker.Says("lapsed-token", ciphertoken.Introspection{Active: false})

	none := serveGet(handler, "/auth/whoami", nil)
	if none.Code != http.StatusUnauthorized || errorCodeOf(t, none) != "NO_IDENTITY" {
		t.Fatalf("whoami with no credential: %d %s, want 401 NO_IDENTITY", none.Code, none.Body.String())
	}
	if answer := agentAnswerOf(t, serveGet(handler, "/auth/whoami", bearer(sharedToken)(t, "/auth/whoami"))); answer.Login != "agent" {
		t.Fatalf("whoami under the shared token = %+v, want the agent", answer)
	}

	for _, row := range []struct {
		name       string
		credential credential
		apiStatus  int
	}{
		{"a token the API does not know", bearer("dsp_not-a-token-anyone-minted"), http.StatusUnauthorized},
		{"a broker token the broker does not hold live", dpop(lapsed, "lapsed-token"), http.StatusUnauthorized},
		{"a read-only token", dpop(reader, "read-only-token"), http.StatusOK},
	} {
		t.Run(row.name, func(t *testing.T) {
			if refused := serveGet(handler, "/api/v1/whoami", row.credential(t, "/api/v1/whoami")); refused.Code != row.apiStatus {
				t.Fatalf("GET /api/v1/whoami: status=%d body=%s, want %d", refused.Code, refused.Body.String(), row.apiStatus)
			}
			response := serveGet(handler, "/auth/whoami", row.credential(t, "/auth/whoami"))
			if response.Code != none.Code || response.Body.String() != none.Body.String() {
				t.Fatalf("whoami: %d %s, want what a request with no bearer is answered: %d %s",
					response.Code, response.Body.String(), none.Code, none.Body.String())
			}
		})
	}
}

// TestAuthWhoamiAnswersTheBrowserCredentialWhateverBearerItCarries: a request carrying a session
// cookie is answered for the cookie's person, exactly as before, beside a bearer the API takes for
// an agent; a cookie that names nobody is refused as it is refused alone, never answered for the
// bearer; and so is a request carrying an IAP assertion, the other browser credential. The same
// bearer alone is answered as the agent.
func TestAuthWhoamiAnswersTheBrowserCredentialWhateverBearerItCarries(t *testing.T) {
	handler := newDevSignInRouter(t, devSignInOrigin, &memorySessionStore{generations: map[string]int64{}})
	withBearer := func(request *http.Request) *httptest.ResponseRecorder {
		request.Header.Set("Authorization", "Bearer "+sharedToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	alone := withBearer(devSignInRequest(t, devSignInOrigin+"/auth/whoami"))
	if got := alone.Body.String(); alone.Code != http.StatusOK || got != `{"event_head":"0","kind":"agent","login":"agent","session":null}`+"\n" {
		t.Fatalf("whoami under the bearer alone: %d %q, want the agent", alone.Code, got)
	}

	signIn := httptest.NewRecorder()
	handler.ServeHTTP(signIn, devSignInRequest(t, devSignInOrigin+"/auth/_dev/signin?login=alice@d.example"))
	cookie := sessionCookieOf(signIn)
	if cookie == nil {
		t.Fatalf("the dev sign-in set no cookie: %d %s", signIn.Code, signIn.Body.String())
	}
	signedIn := devSignInRequest(t, devSignInOrigin+"/auth/whoami")
	signedIn.AddCookie(cookie)
	if person := withBearer(signedIn); person.Code != http.StatusOK || person.Body.String() != `{"event_head":"0","kind":"user","login":"alice@d.example"}`+"\n" {
		t.Fatalf("whoami with the cookie and the bearer: %d %s, want the cookie's person as before", person.Code, person.Body.String())
	}

	stale := devSignInRequest(t, devSignInOrigin+"/auth/whoami")
	stale.AddCookie(&http.Cookie{Name: cookie.Name, Value: "not-a-session"})
	if refused := withBearer(stale); refused.Code != http.StatusUnauthorized || errorCodeOf(t, refused) != "NO_IDENTITY" {
		t.Fatalf("whoami with a cookie naming nobody and the bearer: %d %s, want the cookie's 401 NO_IDENTITY", refused.Code, refused.Body.String())
	}

	asserted := devSignInRequest(t, devSignInOrigin+"/auth/whoami")
	asserted.Header.Set(identity.IAPAssertionHeader, "an-assertion")
	if refused := withBearer(asserted); refused.Code != http.StatusUnauthorized || errorCodeOf(t, refused) != "NO_IDENTITY" {
		t.Fatalf("whoami with an IAP assertion and the bearer: %d %s, want the browser credential's own 401 NO_IDENTITY", refused.Code, refused.Body.String())
	}
}
