package gcp_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// dispatchDomainVectors are the domains Dispatch's boot takes and refuses, in
// packages/envoy/cmd/dispatch/testdata/signin-domain-vectors.json: as DISPATCH_SIGNIN_DOMAIN
// (signInDomain) and as one entry of DISPATCH_SIGNIN_WORKSPACE_DOMAINS (workspaceDomains). The
// component holds signInDomain and each signInWorkspaceDomains entry to copies of Dispatch's rules,
// and Dispatch's own test (packages/envoy/cmd/dispatch/signin_domain_vectors_test.go) reads the same
// file, so neither copy of a rule changes without a test failing.
type dispatchDomainVectors struct {
	SignInDomain     dispatchDomainVectorSet `json:"signInDomain"`
	WorkspaceDomains dispatchDomainVectorSet `json:"workspaceDomains"`
}

type dispatchDomainVectorSet struct {
	Accepted []string `json:"accepted"`
	Refused  []string `json:"refused"`
}

// readDispatchDomainVectors reads the shared vectors, refusing a set with nothing in it, which would
// otherwise pass every test that reads it.
func readDispatchDomainVectors(t *testing.T) dispatchDomainVectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "cmd", "dispatch", "testdata", "signin-domain-vectors.json"))
	require.NoError(t, err)
	var vectors dispatchDomainVectors
	require.NoError(t, json.Unmarshal(raw, &vectors))
	for name, set := range map[string]dispatchDomainVectorSet{"signInDomain": vectors.SignInDomain, "workspaceDomains": vectors.WorkspaceDomains} {
		require.NotEmpty(t, set.Accepted, "the vectors hold %s values Dispatch takes", name)
		require.NotEmpty(t, set.Refused, "the vectors hold %s values Dispatch refuses", name)
	}
	return vectors
}

// requireDispatchJudgesDomainsAsDispatchDoes resolves args with each vector set by set: one
// Dispatch takes resolves, and one it refuses is refused as a core.InputError naming input, so a
// domain Dispatch's boot would refuse fails the preview rather than the pod.
func requireDispatchJudgesDomainsAsDispatchDoes(t *testing.T, vectors dispatchDomainVectorSet, input string, set func(*gcp.DispatchArgs, string)) {
	t.Helper()
	for i, domain := range vectors.Accepted {
		t.Run(fmt.Sprintf("accepted %d", i), func(t *testing.T) {
			args := dispatchArgs()
			set(args, domain)
			_, err := gcp.ResolveDispatch(args)
			require.NoError(t, err, "%s %q, which Dispatch takes", input, domain)
		})
	}
	for i, domain := range vectors.Refused {
		t.Run(fmt.Sprintf("refused %d", i), func(t *testing.T) {
			args := dispatchArgs()
			set(args, domain)
			_, err := gcp.ResolveDispatch(args)
			require.Error(t, err, "%s %q, which Dispatch refuses", input, domain)
			requireInputError(t, err, input)
		})
	}
}

// signInDomain is held to the rule Dispatch holds DISPATCH_SIGNIN_DOMAIN to at boot, under Google
// sign-in and under IAP alike: a lowercase DNS name in ASCII of two labels or more, each of at most
// 63 characters, and at most 253 characters in all.
func TestDispatchTakesTheSignInDomainsDispatchTakes(t *testing.T) {
	requireDispatchJudgesDomainsAsDispatchDoes(t, readDispatchDomainVectors(t).SignInDomain, "signInDomain",
		func(a *gcp.DispatchArgs, domain string) { a.SignInDomain = domain })
}

// Each signInWorkspaceDomains entry is held to the rule Dispatch holds a
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry to: lowercase, taken by IDNA lookup, and then a DNS name
// of two labels or more, each of at most 63 characters, at most 253 in all.
// TestDispatchRefusesASignInWorkspaceDomainsItCannotServe holds the refusals' messages.
func TestDispatchSignInWorkspaceDomainsTakeEveryDomainDispatchTakes(t *testing.T) {
	requireDispatchJudgesDomainsAsDispatchDoes(t, readDispatchDomainVectors(t).WorkspaceDomains, "signInWorkspaceDomains",
		func(a *gcp.DispatchArgs, domain string) { a.SignInWorkspaceDomains = []string{domain} })
}
