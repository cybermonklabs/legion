package gcp_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// dispatchDomainVectors are the domains Dispatch's boot takes and refuses, in
// packages/envoy/cmd/dispatch/testdata/signin-domain-vectors.json: as DISPATCH_SIGNIN_DOMAIN
// (signInDomain) and as one entry of DISPATCH_SIGNIN_WORKSPACE_DOMAINS (workspaceDomains), each
// judged once trimmed of surrounding whitespace, as the boot trims it. The component holds
// signInDomain and each signInWorkspaceDomains entry to copies of Dispatch's rules, the trim
// included, and Dispatch's own test (packages/envoy/cmd/dispatch/signin_domain_vectors_test.go)
// reads the same file, so neither copy of a rule changes without a test failing.
// .github/scripts/ci_job_reads.py declares this read, so a change to the file alone runs this
// module's job too.
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
// Dispatch takes resolves, to its text trimmed of surrounding whitespace as resolved reads it off
// the settings (what the component renders, and what Dispatch reads once it has trimmed it), and
// one it refuses is refused as a core.InputError naming input, so a domain Dispatch's boot would
// refuse fails the preview rather than the pod.
func requireDispatchJudgesDomainsAsDispatchDoes(t *testing.T, vectors dispatchDomainVectorSet, input string, set func(*gcp.DispatchArgs, string), resolved func(gcp.DispatchSettings) string) {
	t.Helper()
	for i, domain := range vectors.Accepted {
		t.Run(fmt.Sprintf("accepted %d", i), func(t *testing.T) {
			args := dispatchArgs()
			set(args, domain)
			settings, err := gcp.ResolveDispatch(args)
			require.NoError(t, err, "%s %q, which Dispatch takes", input, domain)
			require.Equal(t, strings.TrimSpace(domain), resolved(settings), "%s %q renders trimmed", input, domain)
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
// sign-in and under IAP alike: trimmed of surrounding whitespace, a lowercase DNS name in ASCII of
// two labels or more, each of at most 63 characters, and at most 253 characters in all. A padded
// domain renders trimmed on the pod, under Google sign-in and under IAP.
func TestDispatchTakesTheSignInDomainsDispatchTakes(t *testing.T) {
	requireDispatchJudgesDomainsAsDispatchDoes(t, readDispatchDomainVectors(t).SignInDomain, "signInDomain",
		func(a *gcp.DispatchArgs, domain string) { a.SignInDomain = domain },
		func(s gcp.DispatchSettings) string { return s.SignInDomain })
	t.Run("a padded domain renders trimmed", func(t *testing.T) {
		for _, row := range []struct {
			name string
			args func() *gcp.DispatchArgs
			run  func(*testing.T, *gcp.DispatchArgs) (*pulumitest.Mocks, error)
		}{
			{"Google sign-in", dispatchArgs, func(t *testing.T, a *gcp.DispatchArgs) (*pulumitest.Mocks, error) { return runDispatch(t, a, true) }},
			{"IAP", dispatchIAPArgs, func(t *testing.T, a *gcp.DispatchArgs) (*pulumitest.Mocks, error) {
				return runDispatchWithProviders(t, "example-dispatch", a, newGCPProvider, newRandomProvider)
			}},
		} {
			t.Run(row.name, func(t *testing.T) {
				args := row.args()
				args.SignInDomain = " example.com\n"
				m, err := row.run(t, args)
				require.NoError(t, err)
				require.Equal(t, "example.com", appEnv(t, m)["DISPATCH_SIGNIN_DOMAIN"])
			})
		}
	})
}

// Each signInWorkspaceDomains entry is held to the rule Dispatch holds a
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry to: trimmed of surrounding whitespace, lowercase, taken by
// IDNA lookup, and then a DNS name of two labels or more, each of at most 63 characters, at most
// 253 in all. TestDispatchRefusesASignInWorkspaceDomainsItCannotServe holds the refusals' messages,
// and TestDispatchSignInWorkspaceDomainsRenderWithSignIn the trimmed render.
func TestDispatchSignInWorkspaceDomainsTakeEveryDomainDispatchTakes(t *testing.T) {
	requireDispatchJudgesDomainsAsDispatchDoes(t, readDispatchDomainVectors(t).WorkspaceDomains, "signInWorkspaceDomains",
		func(a *gcp.DispatchArgs, domain string) { a.SignInWorkspaceDomains = []string{domain} },
		func(s gcp.DispatchSettings) string { return strings.Join(s.SignInWorkspaceDomains, ",") })
}
