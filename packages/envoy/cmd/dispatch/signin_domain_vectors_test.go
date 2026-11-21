package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// checkDomainShape, the rule Dispatch holds DISPATCH_SIGNIN_DOMAIN to at boot, takes exactly the
// signInDomain values of testdata/signin-domain-vectors.json. envoy-infra's gcp:Dispatch holds its
// signInDomain input to a copy of this rule and reads the same file
// (packages/envoy/infra/gcp/dispatch_signin_domain_vectors_test.go), so neither copy changes
// without a test failing. The file's workspaceDomains are entries of
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS, which only the component's test reads until Dispatch reads that
// setting; Dispatch's test of that setting's entry rule then reads them here too.
func TestCheckDomainShapeTakesTheSharedSignInDomainVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "signin-domain-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		SignInDomain struct {
			Accepted []string `json:"accepted"`
			Refused  []string `json:"refused"`
		} `json:"signInDomain"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.SignInDomain.Accepted) == 0 || len(vectors.SignInDomain.Refused) == 0 {
		t.Fatalf("the vectors hold %d signInDomain values Dispatch takes and %d it refuses; a set with none passes nothing",
			len(vectors.SignInDomain.Accepted), len(vectors.SignInDomain.Refused))
	}
	for _, domain := range vectors.SignInDomain.Accepted {
		if err := checkDomainShape(domain); err != nil {
			t.Errorf("DISPATCH_SIGNIN_DOMAIN=%q: %v, want it taken", domain, err)
		}
	}
	for _, domain := range vectors.SignInDomain.Refused {
		if checkDomainShape(domain) == nil {
			t.Errorf("DISPATCH_SIGNIN_DOMAIN=%q was taken, want it refused", domain)
		}
	}
}
