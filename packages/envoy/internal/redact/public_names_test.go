package redact

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestThePublicNameTableHoldsOnlyContractNamesNoCredentialTravelsUnder: every entry of the table
// of names whose values are public names the contract that declares it, and each route the entry
// is public on in its source's words, or for every path of a method a protocol its source cites
// (protocolMethods), and none is a name a credential travels under
// (testdata/credential-names.json, which the TypeScript guard reads too). An entry with no source,
// a route routeProblem refuses, a credential's name, or a protocol no entry cites or whose route is
// no method's every path, fails here.
func TestThePublicNameTableHoldsOnlyContractNamesNoCredentialTravelsUnder(t *testing.T) {
	data, err := os.ReadFile("testdata/credential-names.json")
	if err != nil {
		t.Fatal(err)
	}
	var forbidden struct {
		Names          []string `json:"names"`
		HeaderPatterns []string `json:"headerPatterns"`
	}
	if err := json.Unmarshal(data, &forbidden); err != nil {
		t.Fatal(err)
	}
	if len(forbidden.Names) == 0 || len(forbidden.HeaderPatterns) == 0 || len(publicRequestNames) == 0 || len(protocolMethods) == 0 {
		t.Fatal("the credential names, their patterns, the public-name table or its protocols hold nothing, so nothing was checked")
	}
	var patterns []*regexp.Regexp
	for _, p := range forbidden.HeaderPatterns {
		patterns = append(patterns, regexp.MustCompile("(?i)"+p))
	}
	for _, entry := range publicRequestNames {
		if strings.TrimSpace(entry.Source) == "" {
			t.Errorf("the public name %q (%s) names no contract that declares its value public", entry.Name, entry.Place)
		}
		if entry.Place != "header" && entry.Place != "field" {
			t.Errorf("the public name %q is carried at %q, neither header nor field", entry.Name, entry.Place)
		}
		for _, route := range entry.Routes {
			if problem := routeProblem(entry, route); problem != "" {
				t.Errorf("the public name %q is public on %q, which %s; its source is %q", entry.Name, route, problem, entry.Source)
			}
		}
		for _, name := range forbidden.Names {
			if strings.EqualFold(entry.Name, name) {
				t.Errorf("the public name %q is a name a credential travels under", entry.Name)
			}
		}
		for _, p := range patterns {
			if entry.Place == "header" && p.MatchString(entry.Name) {
				t.Errorf("the public header %q matches %s, a credential's header", entry.Name, p)
			}
		}
	}
	for _, protocol := range protocolMethods {
		if !everyPath.MatchString(protocol.Route) || strings.TrimSpace(protocol.Source) == "" || strings.TrimSpace(protocol.Words) == "" {
			t.Errorf("the protocol %+v names no method's every path, no section or none of its words", protocol)
		}
		cited := false
		for _, entry := range publicRequestNames {
			cited = cited || (slices.Contains(entry.Routes, protocol.Route) && strings.Contains(entry.Source, protocol.Source))
		}
		if !cited {
			t.Errorf("no entry public on %q cites the protocol %q, so nothing needs it", protocol.Route, protocol.Source)
		}
	}
}

// routeShape is an entry's route: an upper-case method, one space and a path from its root; and
// everyPath one on every path of its method.
var (
	routeShape = regexp.MustCompile(`^[A-Z]+ /[^ ]*$`)
	everyPath  = regexp.MustCompile(`^[A-Z]+ /\*$`)
)

// routeProblem is why route, one of entry's, is no route its source names, or "" where it is one:
// a route on every path of its method is one a protocol of protocolMethods fixes, whose section the
// source cites, since no contract writes "/*" and a source that repeats the route names nothing;
// any other route is one the source names in its own words.
func routeProblem(entry publicRequestName, route string) string {
	switch {
	case !routeShape.MatchString(route):
		return `is no "<METHOD> /<path>"`
	case everyPath.MatchString(route):
		for _, protocol := range protocolMethods {
			if protocol.Route == route && strings.Contains(entry.Source, protocol.Source) {
				return ""
			}
		}
		return "is every path of its method, which no protocol its source cites fixes (protocolMethods)"
	case !strings.Contains(entry.Source, route):
		return "is a route its source does not name"
	}
	return ""
}

// TestARouteOnEveryPathIsOneAProtocolItsSourceCitesFixes: routeProblem passes a route on every path
// of its method only from a source that cites a protocol fixing that method, never from one that
// only repeats the route, and any other route only from a source that names it.
func TestARouteOnEveryPathIsOneAProtocolItsSourceCitesFixes(t *testing.T) {
	for _, row := range []struct {
		name, source, route string
		refused             bool
	}{
		{"repeats-every-path", "POST /* (the token request, a POST to the path its issuer sets)", "POST /*", true},
		{"cites-another-method", "RFC 6749 §3.2: the token request", "GET /*", true},
		{"cites-nothing", "the token request", "POST /*", true},
		{"cites-the-protocol", "RFC 6749 §4.1.3; the token request is a POST by RFC 6749 §3.2", "POST /*", false},
		{"names-the-route", "POST /v1/oauth/token (the broker's token exchange)", "POST /v1/oauth/token", false},
		{"names-another-route", "POST /v1/oauth/revoke (the broker's revocation)", "POST /v1/oauth/token", true},
		{"no-route-shape", "GET v1/sessions", "GET v1/sessions", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			problem := routeProblem(publicRequestName{Place: "field", Name: "planted", Source: row.source, Routes: []string{row.route}}, row.route)
			if refused := problem != ""; refused != row.refused {
				t.Errorf("routeProblem(%q, %q) = %q; want it refused: %v", row.source, row.route, problem, row.refused)
			}
		})
	}
}

// TestARequestsValueIsACredentialUnlessItsNameIsPublic: a value under a name no contract declares
// public, a new one included, is among the request's credentials, and a value under a listed name
// is not, on a route its contract names, in a header, a query, a form and a JSON body alike.
func TestARequestsValueIsACredentialUnlessItsNameIsPublic(t *testing.T) {
	form := url.Values{"session": {"public-session-name"}, "a_new_parameter": {"unlisted-value-RDX"}}
	request, err := http.NewRequest(http.MethodPost, "https://broker.example/v1/oauth/token?scope=public-scope&new_query=unlisted-query-RDX", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "public-agent")
	request.Header.Set("X-New-Header", "unlisted-header-RDX")
	got := strings.Join(requestTokens(request), "\n")
	for _, want := range []string{"unlisted-value-RDX", "unlisted-query-RDX", "unlisted-header-RDX"} {
		if !strings.Contains(got, want) {
			t.Errorf("requestTokens holds no %q, a value under a name no contract declares public: %q", want, got)
		}
	}
	for _, public := range []string{"public-session-name", "public-scope", "public-agent", "application/x-www-form-urlencoded"} {
		if strings.Contains(got, public) {
			t.Errorf("requestTokens holds %q, a value under a public name: %q", public, got)
		}
	}
	body := `{"operator":"ada@example.com","pod_token":"pod-token-RDX","nested":{"session_id":"public-session-id","proof":"nested-proof-RDX"}}`
	jsonRequest, err := http.NewRequest(http.MethodPost, "https://broker.example/v1/enrollments", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	jsonRequest.Header.Set("Content-Type", "application/json")
	got = strings.Join(requestTokens(jsonRequest), "\n")
	for _, want := range []string{"pod-token-RDX", "nested-proof-RDX"} {
		if !strings.Contains(got, want) {
			t.Errorf("requestTokens holds no %q from the JSON body: %q", want, got)
		}
	}
	for _, public := range []string{"ada@example.com", "public-session-id"} {
		if strings.Contains(got, public) {
			t.Errorf("requestTokens holds %q from a public JSON field: %q", public, got)
		}
	}
	impersonation, err := http.NewRequest(http.MethodPost, "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/writer@example.iam.gserviceaccount.com:generateAccessToken",
		strings.NewReader(`{"delegates":["sa@example.iam.gserviceaccount.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	impersonation.Header.Set("Content-Type", "application/json")
	if got := strings.Join(requestTokens(impersonation), "\n"); strings.Contains(got, "sa@example.iam.gserviceaccount.com") {
		t.Errorf("requestTokens holds %q from a public JSON field: %q", "sa@example.iam.gserviceaccount.com", got)
	}
}
