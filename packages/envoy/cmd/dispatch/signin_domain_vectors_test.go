package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Dispatch's boot takes exactly the signInDomain values of testdata/signin-domain-vectors.json as
// DISPATCH_SIGNIN_DOMAIN under Google sign-in, through resolveBootConfig, which trims the setting
// and then holds it to checkDomainShape. envoy-infra's gcp:Dispatch holds its signInDomain input to
// a copy of that rule, the trim included, and reads the same file
// (packages/envoy/infra/gcp/dispatch_signin_domain_vectors_test.go), so neither copy changes
// without a test failing. The file's workspaceDomains are entries of
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS, which only the component's test reads until Dispatch reads that
// setting; Dispatch's test of that setting's entry rule then reads them here too.
func TestCheckDomainShapeTakesTheSharedSignInDomainVectors(t *testing.T) {
	t.Parallel()
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
	boot := func(domain string) error {
		_, err := resolveBootConfig(envGetter(googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_DOMAIN": domain})))
		return err
	}
	for _, domain := range vectors.SignInDomain.Accepted {
		if err := boot(domain); err != nil {
			t.Errorf("DISPATCH_SIGNIN_DOMAIN=%q: %v, want it taken", domain, err)
		}
	}
	for _, domain := range vectors.SignInDomain.Refused {
		if boot(domain) == nil {
			t.Errorf("DISPATCH_SIGNIN_DOMAIN=%q was taken, want it refused", domain)
		}
	}
}
