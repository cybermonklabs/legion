// This file is package api rather than api_test for the reason routes_table_test.go is: it walks
// the broker's own route table (routes()), which is not worth exporting so a test can see it.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/logtest"
)

// pathParameter is a wildcard of a route's pattern, which a request for the route names some
// value of.
var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

// testEdgeURL is BROKER_EDGE_URL in this file's edges.
const testEdgeURL = "https://secrets.edge.example"

// newTestEdgeMux is the edge's mux as RegisterEdge mounts it over a Deps it accepts. The edge
// needs no database to mount: it is built on a provider with no store, which serves nothing, so
// a test of it may inspect the mux and send it only what the mux answers itself.
func newTestEdgeMux(t *testing.T) *http.ServeMux {
	t.Helper()
	return newTestEdgeMuxPinging(t, nil)
}

// newTestEdgeMuxPinging is newTestEdgeMux with ping as the Deps' DatabasePing, the ping its GET
// /healthz answers from.
func newTestEdgeMuxPinging(t *testing.T, ping func(context.Context) error) *http.ServeMux {
	t.Helper()
	provider, err := oauth.New(oauth.Config{
		Secret: make([]byte, 32), DeviceLifetime: time.Minute, AccessLifetime: time.Minute, RefreshLifetime: time.Hour,
		VerificationURL: "https://dispatch.internal.example/credentials/machine", ProofSkew: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		PublicURL:    "https://secrets.internal.example",
		OAuth:        provider,
		MachineLogin: &machine.Service{Audience: "https://secrets.internal.example", EdgeAudience: testEdgeURL},
		Machine:      &requests.Machine{Audience: "https://secrets.internal.example", EdgeAudience: testEdgeURL},
		Enroll:       &enroll.Service{Chain: &record.ChainVerifier{Audience: "https://secrets.internal.example", EdgeAudience: testEdgeURL}},
		DeviceLogin:  func(context.Context, url.Values, string) error { return nil },
		TokenHooks: oauth.TokenHooks{
			Before: func(context.Context, oauth2.GrantType, map[string]any) error { return nil },
			After:  func(context.Context, oauth2.GrantType, map[string]any) (map[string]any, error) { return nil, nil },
		},
		DatabasePing: ping,
	}
	mux := http.NewServeMux()
	RegisterEdge(mux, deps, Edge{URL: testEdgeURL, VerificationURL: "https://dispatch.edge.example/credentials/machine", WorkspaceDomains: []string{"example.com"}})
	return mux
}

// TestTheEdgesHealthIsServedFromItsCachedPing: RegisterEdge sets the edge's Deps.DatabasePing to
// the cached ping (cachedPing) over the Deps' own, and the edge's GET /healthz answers from it, so
// fifty checks of the edge within a second cost one ping of the database, and each answers 200
// ok.
//
// The named revert: leave the edge's Deps.DatabasePing the broker's own, so its GET /healthz pings
// on each check as the API port's does, and the fifty checks cost fifty pings.
func TestTheEdgesHealthIsServedFromItsCachedPing(t *testing.T) {
	var pings atomic.Int64
	mux := newTestEdgeMuxPinging(t, func(context.Context) error { pings.Add(1); return nil })
	for i := range 50 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok"`) {
			t.Fatalf("check %d of the edge's GET /healthz = %d %s, want 200 ok", i+1, w.Code, w.Body)
		}
	}
	if got := pings.Load(); got != 1 {
		t.Fatalf("fifty checks of the edge's GET /healthz pinged the database %d times, want 1", got)
	}
}

// TestTheEdgesHealthWritesOneLineAPingAndNoneForACallerWhoLeft: anyone on the internet can call
// the edge's GET /healthz, so no caller writes a log line of its own. While the database stalls,
// five callers that give up each get their answer and write no line; once the database fails,
// fifty callers within a second all get 503 DATABASE_UNAVAILABLE and the one ping they share
// writes one ERROR line, naming port=edge.
//
// The named reverts: log every failed check through writeUnavailable at the edge too, and the
// stalled callers write five lines and the failing ones fifty, none naming a port.
func TestTheEdgesHealthWritesOneLineAPingAndNoneForACallerWhoLeft(t *testing.T) {
	log := &logtest.Recorder{}
	logtest.SetDefault(t, log)
	failing := fmt.Errorf("database away")
	var stall atomic.Bool
	stall.Store(true)
	mux := newTestEdgeMuxPinging(t, func(ctx context.Context) error {
		if stall.Load() {
			<-ctx.Done()
			return ctx.Err()
		}
		return failing
	})
	for i := range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(ctx))
		cancel()
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("stalled check %d of the edge's GET /healthz = %d %s, want 503", i+1, w.Code, w.Body)
		}
	}
	if records := log.Records(); len(records) != 0 {
		t.Fatalf("five edge health checks that gave up while the database stalled wrote %d log lines, want none: %v", len(records), records)
	}
	stall.Store(false)
	for i := range 50 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"DATABASE_UNAVAILABLE"`) {
			t.Fatalf("check %d of the edge's GET /healthz with the database failing = %d %s, want 503 DATABASE_UNAVAILABLE", i+1, w.Code, w.Body)
		}
	}
	records := log.Records()
	if len(records) != 1 {
		t.Fatalf("fifty edge health checks within a second of a failing database wrote %d log lines, want 1: %v", len(records), records)
	}
	var port string
	records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "port" {
			port = a.Value.String()
		}
		return true
	})
	if records[0].Level != slog.LevelError || port != answer.PortEdge {
		t.Fatalf("the failed ping's line is %v at %v naming port %q, want an ERROR naming port=%s", records[0].Message, records[0].Level, port, answer.PortEdge)
	}
}

// TestTheEdgeMountsOnlyTheDeviceLoginRoutes walks every row of the broker's route table and asks
// the edge's mux which pattern it would serve a request for that row with: POST
// /v1/oauth/device_authorization, POST /v1/oauth/token and GET /healthz each resolve to
// themselves, and every other row, the enrollment routes, the routes Dispatch relays a person's
// decisions through and the approval page among them, resolves to none, so the mux answers it 404
// or 405 and no handler of the broker runs.
//
// The named revert: add "POST /v1/enrollments" to edgeRoutes, and this fails naming it.
func TestTheEdgeMountsOnlyTheDeviceLoginRoutes(t *testing.T) {
	mux := newTestEdgeMux(t)
	want := map[string]bool{
		http.MethodPost + " " + deviceAuthorizationPath: true,
		http.MethodPost + " " + tokenPath:               true,
		http.MethodGet + " /healthz":                    true,
	}
	served := 0
	for _, route := range routes() {
		name := route.Method + " " + route.Pattern
		_, pattern := mux.Handler(httptest.NewRequest(route.Method, testEdgeURL+pathParameter.ReplaceAllString(route.Pattern, "x"), nil))
		switch {
		case want[name] && pattern != name:
			t.Errorf("the edge serves %s with %q, want the row itself", name, pattern)
		case !want[name] && pattern != "":
			t.Errorf("the edge serves %s with %q: the edge mounts the device login's two routes and the health check, and no other row of the broker's table", name, pattern)
		case want[name]:
			served++
		}
	}
	if served != len(want) {
		t.Fatalf("the edge serves %d of its %d routes", served, len(want))
	}
}

// TestTheEdgeAnswersOnlyPostOnItsLoginPaths: the load balancer's route to the edge matches a path
// and no method, so the method is the edge's to hold. Every method but POST on
// /v1/oauth/device_authorization and /v1/oauth/token is answered 405 with Allow: POST, before any
// handler or gate of the broker runs, and GET /healthz is still the health check.
//
// The named revert: mount the edge's rows by path alone, so the mux serves any method, and the
// GET, PUT and DELETE rows reach the login handlers' gates.
func TestTheEdgeAnswersOnlyPostOnItsLoginPaths(t *testing.T) {
	mux := newTestEdgeMux(t)
	for _, path := range []string{deviceAuthorizationPath, tokenPath} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
			answer := httptest.NewRecorder()
			mux.ServeHTTP(answer, httptest.NewRequest(method, testEdgeURL+path, nil))
			if answer.Code != http.StatusMethodNotAllowed || answer.Header().Get("Allow") != http.MethodPost {
				t.Errorf("%s %s at the edge = %d, Allow %q; want 405, Allow POST", method, path, answer.Code, answer.Header().Get("Allow"))
			}
		}
	}
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, testEdgeURL+"/healthz", nil)); pattern != http.MethodGet+" /healthz" {
		t.Fatalf("GET /healthz at the edge is served by %q, want the health check", pattern)
	}
}

// TestTheEdgeRefusesADepsItCannotServe: RegisterEdge panics at wiring, as Register does, on a Deps
// that would leave a login the edge opened unreadable by the routes that decide it — a
// machine-login service, request machine or launcher credentials' chain that does not take the
// edge's URL as its EdgeAudience — or that serves no device grant.
func TestTheEdgeRefusesADepsItCannotServe(t *testing.T) {
	provider, err := oauth.New(oauth.Config{
		Secret: make([]byte, 32), DeviceLifetime: time.Minute, AccessLifetime: time.Minute, RefreshLifetime: time.Hour,
		VerificationURL: "https://dispatch.internal.example/credentials/machine", ProofSkew: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	noDeviceGrant, err := oauth.New(oauth.Config{
		Secret: make([]byte, 32), DeviceLifetime: time.Minute, AccessLifetime: time.Minute, RefreshLifetime: time.Hour, ProofSkew: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wired := func() Deps {
		return Deps{
			OAuth:        provider,
			MachineLogin: &machine.Service{EdgeAudience: testEdgeURL},
			Machine:      &requests.Machine{EdgeAudience: testEdgeURL},
			Enroll:       &enroll.Service{Chain: &record.ChainVerifier{EdgeAudience: testEdgeURL}},
			DeviceLogin:  func(context.Context, url.Values, string) error { return nil },
			TokenHooks: oauth.TokenHooks{
				Before: func(context.Context, oauth2.GrantType, map[string]any) error { return nil },
				After:  func(context.Context, oauth2.GrantType, map[string]any) (map[string]any, error) { return nil, nil },
			},
		}
	}
	for _, row := range []struct {
		name  string
		spoil func(*Deps)
	}{
		{"no device grant", func(d *Deps) { d.OAuth = noDeviceGrant }},
		{"a machine-login service with no edge", func(d *Deps) { d.MachineLogin.EdgeAudience = "" }},
		{"a request machine with no edge", func(d *Deps) { d.Machine.EdgeAudience = "" }},
		{"a launcher chain with no edge", func(d *Deps) { d.Enroll.Chain.EdgeAudience = "" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			deps := wired()
			row.spoil(&deps)
			defer func() {
				if recover() == nil {
					t.Fatalf("RegisterEdge with %s did not panic", row.name)
				}
			}()
			RegisterEdge(http.NewServeMux(), deps, Edge{URL: testEdgeURL, VerificationURL: "https://dispatch.edge.example/credentials/machine"})
		})
	}
	RegisterEdge(http.NewServeMux(), wired(), Edge{URL: testEdgeURL, VerificationURL: "https://dispatch.edge.example/credentials/machine"})
}

// TestEveryEdgeRouteTakesNoCredential: the edge's server holds none of the buckets a route that
// takes a credential spends (its accountLimiter, rereadLimiter and rereadOverall are nil:
// RegisterEdge), which is safe only while every row it mounts takes none. Each row of edgeRoutes is
// a row of the broker's route table, and each is public: authNone, with no power to hold.
//
// The named revert: add "POST /v1/enrollments", a launcher's route, to edgeRoutes, and this fails
// naming it.
func TestEveryEdgeRouteTakesNoCredential(t *testing.T) {
	table := map[string]apiRoute{}
	for _, route := range routes() {
		table[route.Method+" "+route.Pattern] = route
	}
	for pattern := range edgeRoutes {
		route, ok := table[pattern]
		if !ok {
			t.Errorf("the edge mounts %s, which is no row of the broker's route table", pattern)
			continue
		}
		if route.Handler.auth != authNone || route.Handler.power != noPower {
			t.Errorf("the edge mounts %s, which takes a credential (auth %d, power %d): the edge's server has no bucket for a credentialed caller", pattern, route.Handler.auth, route.Handler.power)
		}
	}
}

// TestTheEdgesTokenHooksServeAPersonOutsideTheWorkspaceAlone holds edgeTokenHooks to what it
// lets through, on the claims the library hands it. A refresh or device code grant runs the
// broker's own Before only for a login whose operator is an address outside the Workspace, and is
// refused invalid_grant EDGE_WORKSPACE_LOGIN for any other operator, no operator included. A trade
// takes only a token the device login's client was issued, and of such a token only one whose
// person is outside the Workspace: a Google machine's token and a person sign-in's token, which
// their own trades mint with a credential id, are another client's and refused
// EDGE_GRANT_REFUSED, wherever their principal points.
//
// The named reverts: drop the client check, and the Google machine's and the person sign-in's
// tokens pass; drop the person check, and the Workspace person's token passes.
func TestTheEdgesTokenHooksServeAPersonOutsideTheWorkspaceAlone(t *testing.T) {
	var ran int
	hooks := edgeTokenHooks(oauth.TokenHooks{
		Before: func(context.Context, oauth2.GrantType, map[string]any) error { ran++; return nil },
		After:  func(context.Context, oauth2.GrantType, map[string]any) (map[string]any, error) { return nil, nil },
	}, []string{"example.com", "second.example"})
	refusedAs := func(t *testing.T, what string, err error, oauthError, code string) {
		t.Helper()
		refusal, ok := err.(*oauth.Refusal)
		if !ok || refusal.Status != http.StatusBadRequest || refusal.OAuthError != oauthError || refusal.Code != code {
			t.Errorf("%s = %v, want 400 %s %s", what, err, oauthError, code)
		}
	}
	for _, operator := range []string{"ada@example.com", "bob@eng.second.example", "ADA@EXAMPLE.COM", "ada", ""} {
		for _, grant := range []oauth2.GrantType{oauth2.GrantTypeRefreshToken, oauth2.GrantTypeDeviceCode} {
			refusedAs(t, fmt.Sprintf("%s of a login whose operator is %q", grant, operator),
				hooks.Before(context.Background(), grant, map[string]any{"operator": operator}), "invalid_grant", "EDGE_WORKSPACE_LOGIN")
		}
	}
	if ran != 0 {
		t.Fatalf("the broker's own Before ran %d times for logins the edge refuses", ran)
	}
	if err := hooks.Before(context.Background(), oauth2.GrantTypeRefreshToken, map[string]any{"operator": "outside@outside.example"}); err != nil || ran != 1 {
		t.Fatalf("a refresh of an outside person's login = %v with the broker's own Before run %d times, want it run once", err, ran)
	}
	for _, row := range []struct {
		what    string
		subject map[string]any
		code    string
	}{
		{"a Workspace person's device login token", map[string]any{"client_id": oauthwire.ClientID, "sub": "ada@example.com", oauthwire.ExtraCredentialID: "c1"}, "EDGE_WORKSPACE_LOGIN"},
		{"a device login token naming no address", map[string]any{"client_id": oauthwire.ClientID, "sub": "ada", oauthwire.ExtraCredentialID: "c1"}, "EDGE_WORKSPACE_LOGIN"},
		{"a Google machine's token", map[string]any{"client_id": oauthwire.ExchangeClientID, "sub": "google:vm@project.iam.gserviceaccount.com", oauthwire.ExtraCredentialID: "c2"}, "EDGE_GRANT_REFUSED"},
		{"a person sign-in's token", map[string]any{"client_id": oauthwire.ExchangeClientID, "sub": "person:outside@outside.example", oauthwire.ExtraCredentialID: "c3"}, "EDGE_GRANT_REFUSED"},
		{"a session's token naming an outside person", map[string]any{"client_id": oauthwire.ExchangeClientID, "sub": "outside@outside.example"}, "EDGE_GRANT_REFUSED"},
		{"a token naming no client", map[string]any{"sub": "outside@outside.example"}, "EDGE_GRANT_REFUSED"},
	} {
		refusedAs(t, "the trade at the edge of "+row.what, hooks.Trade(context.Background(), row.subject), "", row.code)
	}
	if err := hooks.Trade(context.Background(), map[string]any{"client_id": oauthwire.ClientID, "sub": "outside@outside.example", oauthwire.ExtraCredentialID: "c4"}); err != nil {
		t.Fatalf("the trade of an outside person's device login token = %v, want it taken", err)
	}
}
