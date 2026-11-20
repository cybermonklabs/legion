package redact

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestThePublicNameTableHoldsOnlyContractNamesNoCredentialTravelsUnder: every entry of the table
// of names whose values are public names the contract that declares it, and none is a name a
// credential travels under (testdata/credential-names.json, which the TypeScript guard reads too).
// An entry with no source, or a credential's name, fails here.
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
	if len(forbidden.Names) == 0 || len(forbidden.HeaderPatterns) == 0 || len(publicRequestNames) == 0 {
		t.Fatal("the credential names, their patterns or the public-name table hold nothing, so nothing was checked")
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
}

// TestARequestsValueIsACredentialUnlessItsNameIsPublic: a value under a name no contract declares
// public, a new one included, is among the request's credentials, and a value under a listed name
// is not, in a header, a query, a form and a JSON body alike.
func TestARequestsValueIsACredentialUnlessItsNameIsPublic(t *testing.T) {
	form := url.Values{"session": {"public-session-name"}, "a_new_parameter": {"unlisted-value-RDX"}}
	request, err := http.NewRequest(http.MethodPost, "https://broker.example/token?scope=public-scope&new_query=unlisted-query-RDX", strings.NewReader(form.Encode()))
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
	body := `{"operator":"ada@example.com","pod_token":"pod-token-RDX","nested":{"session_id":"public-session-id","proof":"nested-proof-RDX"},"delegates":["sa@example.iam.gserviceaccount.com"]}`
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
	for _, public := range []string{"ada@example.com", "public-session-id", "sa@example.iam.gserviceaccount.com"} {
		if strings.Contains(got, public) {
			t.Errorf("requestTokens holds %q from a public JSON field: %q", public, got)
		}
	}
}
