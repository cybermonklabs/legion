// project_repositories_test.go drives GET /v1/project-repositories, the broker's relay to an edge
// listener of Dispatch's answer to "which repositories do a project's issues resolve to", against
// a stand-in of Dispatch's route (projectrepostest) and the real broker otherwise: the edge
// listeners are the ones natscallout.ParseServices reads from a BROKER_NATS_SERVICES value, as
// cmd/cipherd hands them on.
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/projectrepos"
	"github.com/sjawhar/envoy/internal/cipher/projectrepos/projectrepostest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// edgeAccount is the edge listener of PRIV, and otherEdgeAccount the edge listener of OTHER.
	edgeAccount      = "system:serviceaccount:example-ns:example-edge-listener"
	otherEdgeAccount = "system:serviceaccount:example-ns:example-other-edge-listener"
	// edgeServices is the BROKER_NATS_SERVICES these tests' broker reads: the two edge listeners,
	// and the Envoy listener, whose grant reaches every project.
	edgeServices = "edge-listener/edge-a/PRIV=" + edgeAccount + " edge-listener/edge-b/OTHER=" + otherEdgeAccount +
		" listener/host-a/*=" + listenerAccount
	repositoriesPath = "/v1/project-repositories"
)

// repositoriesDispatch is the stand-in of Dispatch every test here starts from: PRIV's issues
// resolve to acme/pub and acme/two, OTHER's to acme/other.
func repositoriesDispatch(t *testing.T) *projectrepostest.Server {
	t.Helper()
	return projectrepostest.NewServer(t, map[string][]string{"PRIV": {"acme/pub", "acme/two"}, "OTHER": {"acme/other"}})
}

// newRepositoriesServer is a broker reading Dispatch's answers from dispatch, nil for a broker
// with no BROKER_PROJECT_ACCESS_URL, whose edge listeners are edgeServices' and which lists the
// Envoy listener in BROKER_API_ACCOUNTS too, as a deployment does.
func newRepositoriesServer(t *testing.T, dispatch *projectrepostest.Server) *testServer {
	t.Helper()
	services, err := natscallout.ParseServices(edgeServices)
	if err != nil {
		t.Fatal(err)
	}
	return newTestServerWith(t, func(d *api.Deps) {
		d.APIAccounts[listenerAccount] = ciphertest.Caller(t, listenerCaller)
		d.EdgeListeners = natscallout.EdgeListeners(services)
		if dispatch != nil {
			d.ProjectRepositories = projectrepos.New(dispatch.URL, dispatch.Account, nil)
		}
	})
}

// askRepositories is GET /v1/project-repositories?project=<project> with bearer, safe to call
// from a goroutine: it fails nothing and answers what went wrong.
func askRepositories(base, bearer, project string) (int, []byte, error) {
	return askRepositoriesQuery(base, bearer, "project="+url.QueryEscape(project))
}

// askRepositoriesQuery is askRepositories with the query written as given.
func askRepositoriesQuery(base, bearer, query string) (int, []byte, error) {
	request, err := http.NewRequest(http.MethodGet, base+repositoriesPath+"?"+query, nil)
	if err != nil {
		return 0, nil, err
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

// repositories asks for project's repositories as account, presenting that account's own
// projected ServiceAccount token.
func (ts *testServer) repositories(t *testing.T, account, project string) (int, []byte) {
	t.Helper()
	status, body, err := askRepositories(ts.URL, ts.accountToken(t, account, "", time.Hour), project)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

// requireRepositories fails unless the answer is 200 with project's list want, in the wire's own
// fields, an empty list written as [].
func requireRepositories(t *testing.T, status int, body []byte, project string, want []string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s; want 200 with %s's repositories %q", status, body, project, want)
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer %s is no JSON object: %v", body, err)
	}
	var got []string
	if err := json.Unmarshal(answer["repositories"], &got); err != nil || got == nil {
		t.Fatalf("the answer %s carries no list of repositories", body)
	}
	if string(answer["project"]) != `"`+project+`"` || !reflect.DeepEqual(got, want) || len(answer) != 2 {
		t.Fatalf("the answer %s, want exactly project %s and repositories %q", body, project, want)
	}
}

// requireRefused fails unless the answer is status with code, naming no repository.
func requireRefused(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	var refusal struct{ Code, Error string }
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("status %d, body %s: no refusal object: %v", status, body, err)
	}
	if status != wantStatus || refusal.Code != wantCode {
		t.Fatalf("status %d code %q (body %s), want %d %s", status, refusal.Code, body, wantStatus, wantCode)
	}
	if strings.Contains(string(body), "acme/") || strings.Contains(string(body), `"repositories"`) {
		t.Fatalf("the refusal %s names a repository", body)
	}
}

// TestTheBrokerRelaysAFreshRepositoryAnswer: the edge listener whose entry lists PRIV is answered
// PRIV's list as Dispatch answers it, read from Dispatch on every call: a list Dispatch changed
// between two calls is the second call's answer, one call each, and a project none of whose
// issues resolves to a repository is [].
func TestTheBrokerRelaysAFreshRepositoryAnswer(t *testing.T) {
	dispatch := repositoriesDispatch(t)
	ts := newRepositoriesServer(t, dispatch)
	status, body := ts.repositories(t, edgeAccount, "PRIV")
	requireRepositories(t, status, body, "PRIV", []string{"acme/pub", "acme/two"})
	dispatch.SetRepositories("PRIV", []string{"acme/pub"})
	status, body = ts.repositories(t, edgeAccount, "PRIV")
	requireRepositories(t, status, body, "PRIV", []string{"acme/pub"})
	dispatch.SetRepositories("PRIV", nil)
	status, body = ts.repositories(t, edgeAccount, "PRIV")
	requireRepositories(t, status, body, "PRIV", []string{})
	if calls := dispatch.Calls(); calls != 3 {
		t.Fatalf("three relays asked Dispatch %d times, want 3: no answer is kept", calls)
	}
}

// TestTheRepositoryAnswerIsOnlyForTheEdgeOfThatProject: a project is answered only to an edge
// listener whose entry lists it. The edge listener of OTHER asking for PRIV, the Envoy listener
// (whose own grant reaches every project), Dispatch's caller, an account no list names and a call
// with no token are refused, and none of them reaches Dispatch; the edge listener of OTHER is
// answered OTHER's list, so the refusal is the project's and not the caller's.
func TestTheRepositoryAnswerIsOnlyForTheEdgeOfThatProject(t *testing.T) {
	dispatch := repositoriesDispatch(t)
	ts := newRepositoriesServer(t, dispatch)
	for _, row := range []struct {
		name, account, project string
		status                 int
		code                   string
	}{
		{"the edge listener of OTHER asking for PRIV", otherEdgeAccount, "PRIV", http.StatusForbidden, "PROJECT_NOT_LISTED"},
		{"the edge listener of PRIV asking for a project it does not list", edgeAccount, "NOPE", http.StatusForbidden, "PROJECT_NOT_LISTED"},
		{"the Envoy listener", listenerAccount, "PRIV", http.StatusForbidden, "NOT_AN_EDGE_LISTENER"},
		{"Dispatch's caller", dispatchAccount, "PRIV", http.StatusForbidden, "NOT_AN_EDGE_LISTENER"},
		{"an account no list names", unlistedAccount, "PRIV", http.StatusForbidden, "NOT_AN_EDGE_LISTENER"},
		{"an edge listener naming no project", edgeAccount, "", http.StatusBadRequest, "PROJECT_REQUIRED"},
	} {
		t.Run(row.name, func(t *testing.T) {
			status, body := ts.repositories(t, row.account, row.project)
			requireRefused(t, status, body, row.status, row.code)
		})
	}
	t.Run("no token", func(t *testing.T) {
		status, body, err := askRepositories(ts.URL, "", "PRIV")
		if err != nil {
			t.Fatal(err)
		}
		requireRefused(t, status, body, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID")
	})
	t.Run("two projects", func(t *testing.T) {
		status, body, err := askRepositoriesQuery(ts.URL, ts.accountToken(t, edgeAccount, "", time.Hour), "project=PRIV&project=OTHER")
		if err != nil {
			t.Fatal(err)
		}
		requireRefused(t, status, body, http.StatusBadRequest, "PROJECT_REQUIRED")
	})
	if calls := dispatch.Calls(); calls != 0 {
		t.Fatalf("the refused calls asked Dispatch %d times, want none", calls)
	}
	status, body := ts.repositories(t, otherEdgeAccount, "OTHER")
	requireRepositories(t, status, body, "OTHER", []string{"acme/other"})
	status, body = ts.repositories(t, edgeAccount, "PRIV")
	requireRepositories(t, status, body, "PRIV", []string{"acme/pub", "acme/two"})
}

// TestAnUnreadableDispatchAnswerIsUnknown: when the broker cannot get a whole answer from
// Dispatch it answers 503 REPOSITORIES_UNKNOWN and never a list, an old one or an empty one:
// Dispatch's 503, a Dispatch that stalls 3 s (the broker gives it 2), a body it cannot read, a
// project Dispatch does not hold, a Dispatch refusing the broker's own token and a broker that
// reads no Dispatch at all.
func TestAnUnreadableDispatchAnswerIsUnknown(t *testing.T) {
	for _, row := range []struct {
		name  string
		setup func(*projectrepostest.Server)
		none  bool
	}{
		{name: "Dispatch's 503", setup: func(d *projectrepostest.Server) { d.SetDown(true) }},
		{name: "a 3 s stall", setup: func(d *projectrepostest.Server) { d.SetStall(3 * time.Second) }},
		{name: "an unreadable body", setup: func(d *projectrepostest.Server) {
			d.SetAnswer(http.StatusOK, `{"project":"PRIV","repositories":["acme/pub"`)
		}},
		{name: "a project Dispatch does not hold", setup: func(d *projectrepostest.Server) {
			d.SetAnswer(http.StatusNotFound, `{"code":"PROJECT_NOT_FOUND","error":"no such project"}`)
		}},
		{name: "Dispatch refusing the broker's token", setup: func(d *projectrepostest.Server) {
			d.SetAnswer(http.StatusForbidden, `{"code":"FORBIDDEN","error":"not an access account"}`)
		}},
		{name: "a broker that reads no Dispatch", none: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			dispatch := repositoriesDispatch(t)
			reader := dispatch
			if row.none {
				reader = nil
			}
			ts := newRepositoriesServer(t, reader)
			// An answer read first, so a broker that kept it would have one to give.
			status, body := ts.repositories(t, edgeAccount, "PRIV")
			if !row.none {
				requireRepositories(t, status, body, "PRIV", []string{"acme/pub", "acme/two"})
				row.setup(dispatch)
			}
			start := time.Now()
			status, body = ts.repositories(t, edgeAccount, "PRIV")
			if elapsed := time.Since(start); elapsed > projectrepos.Timeout+time.Second {
				t.Fatalf("the answer took %s, want at most about %s", elapsed, projectrepos.Timeout)
			}
			requireRefused(t, status, body, http.StatusServiceUnavailable, "REPOSITORIES_UNKNOWN")
		})
	}
}

// TestTheSeventeenthRepositoryCallInFlightIsBusy: an edge listener has at most 16 calls in flight
// to Dispatch; its 17th is 503 REPOSITORIES_BUSY at once and never reaches Dispatch, while another
// edge listener is answered; once the 16 are answered, the next call is answered too.
func TestTheSeventeenthRepositoryCallInFlightIsBusy(t *testing.T) {
	dispatch := repositoriesDispatch(t)
	ts := newRepositoriesServer(t, dispatch)
	bearer := ts.accountToken(t, edgeAccount, "", time.Hour)
	release := dispatch.Hold("PRIV")
	defer release()
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make(chan result, 16)
	var started sync.WaitGroup
	for range 16 {
		started.Add(1)
		go func() {
			defer started.Done()
			status, body, err := askRepositories(ts.URL, bearer, "PRIV")
			results <- result{status, body, err}
		}()
	}
	// The held calls give up at projectrepos.Timeout, so the 17th is sent well before that.
	ctx, cancel := context.WithTimeout(t.Context(), projectrepos.Timeout/2)
	defer cancel()
	for dispatch.InFlight() < 16 {
		select {
		case <-ctx.Done():
			t.Fatalf("%d calls reached Dispatch, want 16 in flight", dispatch.InFlight())
		case <-time.After(5 * time.Millisecond):
		}
	}
	status, body, err := askRepositories(ts.URL, bearer, "PRIV")
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, status, body, http.StatusServiceUnavailable, "REPOSITORIES_BUSY")
	if calls := dispatch.Calls(); calls != 16 {
		t.Fatalf("Dispatch took %d calls, want 16: the 17th must not reach it", calls)
	}
	status, body = ts.repositories(t, otherEdgeAccount, "OTHER")
	requireRepositories(t, status, body, "OTHER", []string{"acme/other"})
	release()
	started.Wait()
	close(results)
	for answer := range results {
		if answer.err != nil {
			t.Fatal(answer.err)
		}
		requireRepositories(t, answer.status, answer.body, "PRIV", []string{"acme/pub", "acme/two"})
	}
	status, body = ts.repositories(t, edgeAccount, "PRIV")
	requireRepositories(t, status, body, "PRIV", []string{"acme/pub", "acme/two"})
}

// TestAGoogleEdgeListenerIsAnsweredItsProjectsRepositories: an edge listener on a machine holding a
// Google identity token is looked up by the account its email claim names, as a listed caller of
// that kind is, and held to its entry's projects the same way.
func TestAGoogleEdgeListenerIsAnsweredItsProjectsRepositories(t *testing.T) {
	const edgeGoogleAccount = "example-edge@example-project.iam.gserviceaccount.com"
	google := oidctest.New(t)
	key := google.PublishKey(t, "google-key")
	verifier, err := oidc.New(context.Background(), google.URL(), machineAudience)
	if err != nil {
		t.Fatal(err)
	}
	services, err := natscallout.ParseServices("edge-listener/edge-g/PRIV=google:" + edgeGoogleAccount)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := repositoriesDispatch(t)
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.GoogleAccountTokens = api.GoogleTokens{Verifier: verifier}
		d.EdgeListeners = natscallout.EdgeListeners(services)
		d.ProjectRepositories = projectrepos.New(dispatch.URL, dispatch.Account, nil)
	})
	token := func(account string) string {
		now := time.Now()
		claims := google.Claims(account, machineAudience)
		claims["email"], claims["email_verified"] = account, true
		claims["iat"], claims["nbf"], claims["exp"] = now.Add(-5*time.Second).Unix(), now.Add(-5*time.Second).Unix(), now.Add(time.Hour).Unix()
		return google.Mint(t, key, claims)
	}
	for _, row := range []struct {
		account, project string
		status           int
		code             string
	}{
		{edgeGoogleAccount, "PRIV", http.StatusOK, ""},
		{edgeGoogleAccount, "OTHER", http.StatusForbidden, "PROJECT_NOT_LISTED"},
		{strangerGoogleAccount, "PRIV", http.StatusForbidden, "NOT_AN_EDGE_LISTENER"},
	} {
		t.Run(fmt.Sprintf("%s asking for %s", row.account, row.project), func(t *testing.T) {
			status, body, err := askRepositories(ts.URL, token(row.account), row.project)
			if err != nil {
				t.Fatal(err)
			}
			if row.status == http.StatusOK {
				requireRepositories(t, status, body, row.project, []string{"acme/pub", "acme/two"})
				return
			}
			requireRefused(t, status, body, row.status, row.code)
		})
	}
}
