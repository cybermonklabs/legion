package sandbox

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// planOfTheRigsIssue is the plan in force of the rig's issue alone: coder, of the type
// implementation, and prober, of the type testing. Every other issue's plan defines no role.
func planOfTheRigsIssue(_ context.Context, issue string, role claim.Role) (appauth.Planned, error) {
	if issue != testTree {
		return appauth.Planned{}, nil
	}
	return map[claim.Role]appauth.Planned{
		"coder":  {Defines: true, Type: plan.TypeImplementation},
		"prober": {Defines: true, Type: plan.TypeTesting},
	}[role], nil
}

// The slot of an agent whose name is a role of the issue's plan holds the gh files of the App of
// that role's type, read on the slot's own issue: on the rig's issue, coder (implementation) holds
// the implement App's lease and prober (testing) the review App's, so a review step of the plan
// never acts as the App that wrote the change. The pod's build mints both slots for the agents'
// first steps, and the refresh, once a claim of each is stored, brings a slot whose files went
// stale back to its role's App, reading the issue off the slot's Secret.
func TestASlotOfARoleOfAPlanHoldsTheAppOfItsType(t *testing.T) {
	g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{"coder": "coder", "prober": "prober"}),
		withProjectTokensOf(refusingOwnerTokens{refuse: new(atomic.Bool)}, planOfTheRigsIssue))
	name := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree)).Sandbox.Name
	want := map[claim.Role]string{"coder": "ghs_implement_", "prober": "ghs_review_"}
	held := func(when string) {
		t.Helper()
		for slot, prefix := range want {
			secret := g.secret(roleSecretName(name, slot))
			if secret == nil {
				t.Fatalf("%s, the Sandbox has no Secret of the slot %s", when, slot)
			}
			owner := string(secret.Data[GitHubDefaultKey])
			if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(owner)]); err != nil || !strings.HasPrefix(token, prefix) {
				t.Errorf("%s, the %s Secret's %s hosts.yml holds %q (%v), want a token that begins %s", when, slot, owner, token, err, prefix)
			}
		}
	}
	held("after the pod's build")

	g.store.mu.Lock()
	for slot := range want {
		token, err := claim.AgentToken(testProject, testTree, claim.Agent(slot))
		if err != nil {
			t.Fatal(err)
		}
		g.store.claims = append(g.store.claims, supervise.Claim{Token: token, Issue: testTree, Role: slot, Generation: 1})
	}
	g.store.mu.Unlock()
	secrets := g.kube.CoreV1().Secrets(testNamespace)
	for slot := range want {
		secret := g.secret(roleSecretName(name, slot))
		owner := string(secret.Data[GitHubDefaultKey])
		secret.Data[GitHubHostsKey(owner)] = []byte(ghconfig.Render("ghs_stale", "stale", staticCredentialExpiry).Hosts)
		if _, err := secrets.Update(g.ctx, secret, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	g.r.refreshGitHubCredentials(g.ctx)
	held("after a refresh of the stale slots")
}

// A role of the deployment acts as one App on every issue whose plan does not define its name, so
// one tick builds its bundle once for the repository however many issue pods hold its slot
// (Options.RoleApps, appauth.NewBundles): the checker's slot in the pods of two issues of one
// repository is one build.
func TestATickBuildsARoleOfTheDeploymentOnceForEveryIssuePod(t *testing.T) {
	var mu sync.Mutex
	built := map[claim.Role][]string{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.RoleApps.Deployed = appauth.DeploymentApps{"checker": appauth.Implement}
		o.GitHubCredential = appauth.CredentialOf(func(ctx context.Context, issue string, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			mu.Lock()
			built[role] = append(built[role], issue)
			mu.Unlock()
			if role == "checker" {
				return staticCredential(ctx, issue, claim.RoleImplementer, repo)
			}
			return staticCredential(ctx, issue, role, repo)
		})
	}))
	g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	g.spawn(testSpec(t, childToken, claim.RoleImplementer, "LEGION-209"))
	mu.Lock()
	clear(built)
	mu.Unlock()

	g.r.refreshGitHubCredentials(g.ctx)

	mu.Lock()
	defer mu.Unlock()
	if got := built["checker"]; len(got) != 1 {
		t.Errorf("the tick built the checker's bundle for the issues %q, want one build for both pods of the repository", got)
	}
}
