package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// routeTour is the server the route tour (TestV2RouteTour) and the tests of the /api/v2 routes'
// authorities and retries (TestV2DeclaredAuthority, TestV2Retries) drive, on a database of its own,
// with every service Dispatch calls faked as the package's other tests fake it. A step's setup
// makes what its request names through it (project, issue, ask, comment), and tells a fake what to
// answer where its route calls one.
type routeTour struct {
	handler  http.Handler
	database *store.Store
	served   *server
	// routes are the /api/v2 routes the rules of /api/v2 bind (apiRoute.v2Bound), by "<method>
	// <pattern>": every route but the rows of the INFRA-979 tree and the test hooks.
	routes map[string]apiRoute
	// listener is the Envoy listener Dispatch asks about sessions (newFakeListener), behind the
	// two delivery routes tourDeliveryListener answers for tourRecipient.
	listener *fakeListener
	// github is GitHub, behind an App key Dispatch holds.
	github *tourGitHub
	// secrets is the secrets broker the credential pages relay to.
	secrets *tourSecretsBroker
	// brokerTokens decides a broker-issued token a step presents (ciphertokentest).
	brokerTokens *ciphertokentest.StubBroker
	// personToken and scopelessToken are alice's personal tokens, one holding the person scope
	// and one holding no scope: an agent of alice's acting with and without her authority.
	personToken, scopelessToken string
	// keys numbers the projects the fixtures make, so each is a project of its own.
	keys atomic.Int32
}

// newRouteTour builds the tour's server: the package's own test server (newRegisteredTestServer)
// on a database of its own, its test hooks in, with the listener, GitHub, the secrets broker and
// the broker that decides a broker-issued token faked.
func newRouteTour(t *testing.T) *routeTour {
	t.Helper()
	fakeURL, listener := newFakeListener(t, nil, nil)
	listenerURL := tourDeliveryListener(t, fakeURL)
	github := &tourGitHub{answers: map[string]http.HandlerFunc{}}
	githubURL := githubtest.Server(t, githubtest.Spec{Handle: github.serve})
	secrets := newTourSecretsBroker(t)
	brokerTokens := ciphertokentest.NewStubBroker(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		testHooks:       true,
		envoyURL:        listenerURL,
		app:             &auth.AppConfig{ClientID: "Iv1.tour", PEM: pemText},
		githubAPIBase:   githubURL,
		agentSecretsURL: secrets.server.URL,
		brokerTokens:    stubBrokerTokens(t, brokerTokens),
	})
	routes := map[string]apiRoute{}
	for _, route := range served.routes() {
		if route.v2Bound() {
			routes[route.Method+" "+route.Pattern] = route
		}
	}
	return &routeTour{
		handler: handler, database: database, served: served, routes: routes,
		listener: listener, github: github, secrets: secrets, brokerTokens: brokerTokens,
		personToken:    personalTokenOwnedBy(t, database, "alice", model.ScopePerson),
		scopelessToken: personalTokenOwnedBy(t, database, "alice"),
	}
}

// requireMounted is the route /api/v2 serves at method and pattern (version-free), and fails t for
// one it does not walk: a route the server does not mount, a row of the INFRA-979 tree, or a test
// hook. Every test of the tour looks its routes up here, so a renamed route fails the test that
// names it.
func (tour *routeTour) requireMounted(t *testing.T, method, pattern string) apiRoute {
	t.Helper()
	name := method + " /api/v2" + pattern
	route, mounted := tour.routes[name]
	if !mounted {
		t.Fatalf("the test calls %s, which is no /api/v2 route the tour walks: a route the server does not mount, "+
			"a row of the INFRA-979 tree, or a test hook", name)
	}
	return route
}

// project makes a project of the tour's own, answering its key.
func (tour *routeTour) project(t *testing.T) string {
	t.Helper()
	key := fmt.Sprintf("TOUR%d", tour.keys.Add(1))
	if created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create project %s: %d %s", key, created.Code, created.Body.String())
	}
	return key
}

// tourIssue is an issue a fixture made: its key and its spec's document.
type tourIssue struct {
	Key      string `json:"key"`
	Document string `json:"primary_artifact_id"`
}

// issue makes an issue with a spec in a project of its own, answering it once the spec owes no
// settlement: the settlement writes after the create answers, and a step's write that lands
// between the two fingerprints of unknown-keys would read as the refused request's.
func (tour *routeTour) issue(t *testing.T) tourIssue {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": tour.project(t), "title": "A toured issue", "spec": "# Spec\n\nA passage the tour reads.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: %d %s", created.Code, created.Body.String())
	}
	issue := decodeBody[tourIssue](t, created)
	awaitSettled(t, tour.database, issue.Document)
	return issue
}

// tourSession is the session the fixtures' agent writes as.
const tourSession = "session-tour-0123456789"

// ask makes an open ask on issue, asked by tourSession under the shared agent token, answering its
// id.
func (tour *routeTour) ask(t *testing.T, issue tourIssue) string {
	t.Helper()
	created := sessionRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which way does the tour go?",
		"actor":    map[string]any{"kind": "session", "id": tourSession},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: %d %s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// comment makes a comment on issue by alice, answering its id.
func (tour *routeTour) comment(t *testing.T, issue tourIssue) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A toured comment.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// eventCount is how many events the issue's log holds of eventType, or of every type where
// eventType is "". Where issue is "" it counts the whole event log: every issue's events, and those
// of no issue, so a caller that compares two counts sees a write anywhere.
func (tour *routeTour) eventCount(t *testing.T, issue, eventType string) int {
	t.Helper()
	awaitBackgroundWrites(t, tour.served)
	var count int
	if err := tour.database.Pool.QueryRow(t.Context(), `
		select count(*) from events where ($1 = '' or issue_key = $1) and ($2 = '' or type = $2)`,
		issue, eventType).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// tourRecipient is the one session the tour's listener delivers to (tourDeliveryListener): live,
// advertising every mode, so a message, a mention or a broadcast sent to it settles sent, and the
// replies and the accept it writes as a bearer answer the attempts it was sent.
const tourRecipient = "session-tour-recipient"

// tourDeliveryListener is the Envoy listener the tour's server is pointed at: the fake listener at
// upstream (newFakeListener), which answers the interests and the session list, with the two
// routes a delivery takes answered in front of it - GET /v1/registry/<id>, which holds
// tourRecipient alone and answers every other session 404 "no live session", and POST
// /v1/messages/send, which takes every send.
func tourDeliveryListener(t *testing.T, upstream string) string {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("read the fake listener's URL: %v", err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	recipients := `[{"session_id":"` + tourRecipient + `","title":"tour recipient","dir":"/w/tour","machine_id":"m1",` +
		`"roles":[],"capabilities":["aside","btw","steer"]}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, recipients, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			WriteJSON(w, http.StatusOK, map[string]string{"event_id": "tour-envelope-" + uuid.NewString(), "recipient": tourRecipient})
		default:
			forward.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// tourGitHub is GitHub as the tour fakes it: each method and path a step's setup tells it to
// answer, and 404 for every other, as GitHub answers a repository it does not show the App.
type tourGitHub struct {
	mu      sync.Mutex
	answers map[string]http.HandlerFunc
}

// answer tells GitHub to answer method and path with handler from now on.
func (g *tourGitHub) answer(method, path string, handler http.HandlerFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answers[method+" "+path] = handler
}

func (g *tourGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	handler, ok := g.answers[r.Method+" "+r.URL.Path]
	g.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		return
	}
	handler(w, r)
}

// tourSecretsBroker is the secrets broker the credential pages relay to: each method and path
// answered as a step's setup tells it, 200 {"ok":true} otherwise, and every call counted by method
// and path, so a test can tell one relay from two.
type tourSecretsBroker struct {
	server  *httptest.Server
	mu      sync.Mutex
	answers map[string]tourBrokerAnswer
	calls   map[string]int
}

// tourBrokerAnswer is a status and a JSON body the fake broker answers with.
type tourBrokerAnswer struct {
	Status int
	Body   string
}

func newTourSecretsBroker(t *testing.T) *tourSecretsBroker {
	t.Helper()
	broker := &tourSecretsBroker{answers: map[string]tourBrokerAnswer{}, calls: map[string]int{}}
	broker.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		broker.mu.Lock()
		broker.calls[route]++
		answer, ok := broker.answers[route]
		broker.mu.Unlock()
		if !ok {
			answer = tourBrokerAnswer{Status: http.StatusOK, Body: `{"ok":true}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.Status)
		_, _ = io.WriteString(w, answer.Body)
	}))
	t.Cleanup(broker.server.Close)
	return broker
}

// answer tells the broker to answer method and path with status and body from now on.
func (b *tourSecretsBroker) answer(method, path string, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answers[method+" "+path] = tourBrokerAnswer{Status: status, Body: string(encoded)}
}

// callsTo is how many times Dispatch has called method and path.
func (b *tourSecretsBroker) callsTo(method, path string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[method+" "+path]
}
