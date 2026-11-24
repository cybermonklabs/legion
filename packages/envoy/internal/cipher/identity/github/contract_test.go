package github_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/cipher/identity/identitytest"
	"github.com/sjawhar/envoy/internal/oidc"
)

const contractRun = "example-org/example-repo/.github/workflows/release.yaml@refs/heads/main"

func TestGitHubRunsTheIdentityContract(t *testing.T) {
	claims := map[string]any{
		"repository": "example-org/example-repo", "workflow_ref": contractRun, "job_workflow_ref": contractRun, "ref": "refs/heads/main",
	}
	identitytest.Contract(t, identitytest.Fixture{
		New: func(t *testing.T, issuer string, keys *oidc.Keys, audience string) identity.Provider {
			p, err := github.New(github.Config{
				Issuer: issuer, Audience: audience, Keys: keys,
				Workflows: []exchange.WorkflowEntry{{
					Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main",
					Project: "ACME", Person: "ada@example.com",
				}},
			})
			if err != nil {
				t.Fatalf("github.New: %v", err)
			}
			return p
		},
		Audience: "https://broker.example",
		Subject:  "repo:example-org/example-repo:ref:refs/heads/main",
		Claims:   claims,
		Want:     identity.Account{Kind: github.Kind, Address: contractRun},
		Trades:   []identitytest.TradeFixture{{TokenType: github.SubjectTokenType, Claims: claims, WantPrincipal: "workflow:" + contractRun}},
	})
}

func TestPrincipalSpellsTodaysWorkflowPrincipal(t *testing.T) {
	got := github.Principal("example-org/example-repo", ".github/workflows/release.yaml", "refs/heads/main")
	if want := "workflow:" + contractRun; got != want {
		t.Fatalf("Principal = %q, want %q", got, want)
	}
}

// TestTheWorkflowTypeIsSingleUse: a workflow run mints a fresh token per request, so the exchange
// refuses one presented twice (its jti), and a leaked one cannot be replayed.
func TestTheWorkflowTypeIsSingleUse(t *testing.T) {
	p, err := github.New(github.Config{})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	types := p.TokenTypes()
	if len(types) != 1 || types[0].Name != github.SubjectTokenType || !types[0].SingleUse {
		t.Fatalf("TokenTypes() = %+v, want the one workflow-run type, single use", types)
	}
}
