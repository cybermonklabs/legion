// This file is package api rather than api_test for the reason routes_table_test.go is: it walks
// the broker's own route table (routes()), which is not worth exporting so a test can see it.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// pathParameter is a wildcard of a route's pattern, which a request for the route names some
// value of.
var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

// TestTheEdgeMountsOnlyTheDeviceLoginRoutes walks every row of the broker's route table and asks
// the edge's mux which pattern it would serve a request for that row with: POST
// /v1/oauth/device_authorization, POST /v1/oauth/token and GET /healthz each resolve to
// themselves, and every other row, the enrollment routes, the routes Dispatch relays a person's
// decisions through and the approval page among them, resolves to none, so the mux answers it 404
// or 405 and no handler of the broker runs. The edge needs no database to mount: it is built here
// on a provider with no store, which serves nothing and is only inspected.
//
// The named revert: add "POST /v1/enrollments" to edgeRoutes, and this fails naming it.
func TestTheEdgeMountsOnlyTheDeviceLoginRoutes(t *testing.T) {
	const edgeURL = "https://secrets.edge.example"
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
		MachineLogin: &machine.Service{Audience: "https://secrets.internal.example", EdgeAudience: edgeURL},
		Machine:      &requests.Machine{Audience: "https://secrets.internal.example", EdgeAudience: edgeURL},
		Enroll:       &enroll.Service{Chain: &record.ChainVerifier{Audience: "https://secrets.internal.example", EdgeAudience: edgeURL}},
		DeviceLogin:  func(context.Context, url.Values, string) error { return nil },
		TokenHooks: oauth.TokenHooks{
			Before: func(context.Context, oauth2.GrantType, map[string]any) error { return nil },
			After:  func(context.Context, oauth2.GrantType, map[string]any) (map[string]any, error) { return nil, nil },
		},
	}
	mux := http.NewServeMux()
	RegisterEdge(mux, deps, Edge{URL: edgeURL, VerificationURL: "https://dispatch.edge.example/credentials/machine", WorkspaceDomains: []string{"example.com"}})

	want := map[string]bool{
		http.MethodPost + " " + deviceAuthorizationPath: true,
		http.MethodPost + " " + tokenPath:               true,
		http.MethodGet + " /healthz":                    true,
	}
	served := 0
	for _, route := range routes() {
		name := route.Method + " " + route.Pattern
		_, pattern := mux.Handler(httptest.NewRequest(route.Method, edgeURL+pathParameter.ReplaceAllString(route.Pattern, "x"), nil))
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

// TestTheEdgeRefusesADepsItCannotServe: RegisterEdge panics at wiring, as Register does, on a Deps
// that would leave a login the edge opened unreadable by the routes that decide it — a
// machine-login service, request machine or launcher credentials' chain that does not take the
// edge's URL as its EdgeAudience — or that serves no device grant.
func TestTheEdgeRefusesADepsItCannotServe(t *testing.T) {
	const edgeURL = "https://secrets.edge.example"
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
			MachineLogin: &machine.Service{EdgeAudience: edgeURL},
			Machine:      &requests.Machine{EdgeAudience: edgeURL},
			Enroll:       &enroll.Service{Chain: &record.ChainVerifier{EdgeAudience: edgeURL}},
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
			RegisterEdge(http.NewServeMux(), deps, Edge{URL: edgeURL, VerificationURL: "https://dispatch.edge.example/credentials/machine"})
		})
	}
	RegisterEdge(http.NewServeMux(), wired(), Edge{URL: edgeURL, VerificationURL: "https://dispatch.edge.example/credentials/machine"})
}
