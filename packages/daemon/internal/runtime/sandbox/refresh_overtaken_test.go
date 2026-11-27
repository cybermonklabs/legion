package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Key a's claim `…-implementer` is re-roled to the tester and launched into the pod it keeps, and
// that launch writes the slot `implementer`'s Secret for the review App, whose token digest its
// start frame names and its launcher waits for. A refresh that the launch overtakes leaves the
// slot on that App: one whose read of the stored claims came before the re-role, whether the
// launch then lands before the refresh reads the slot's Secret or between that read and the
// refresh's write of it, which the Secret's resourceVersion refuses. The refresh judges the slot's
// role on the Secret it writes, under that Secret's lock, and not on the claims it read when its
// tick began. The slot holds a lease the daemon has since re-minted, so the refresh has a write to
// make. The control is the same launch made before the tick.
func TestARefreshALaunchOvertakesLeavesTheSlotOnTheLaunchesApp(t *testing.T) {
	for _, tc := range []struct {
		name string
		// during is the role of the refresh's mint the launch lands in: "" for the control, which
		// launches before the tick; the architect's, a slot the tick writes before the implementer's;
		// or the implementer's own, after the refresh read that slot's Secret.
		during claim.Role
	}{
		{name: "control: the launch comes before the tick reads the stored claims"},
		{name: "the launch lands after the tick read the stored claims and before it reads the slot", during: claim.RoleArchitect},
		{name: "the launch lands after the refresh read the slot and before it writes it", during: claim.RoleImplementer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyed := claim.Token("legion-legion-legion-208-implementer")
			var g *rig
			var refreshing, landing, landed, refused atomic.Bool
			launch := func() {
				// The claim's re-role, as supervise.Machine.Rerole persists it, then its launch into
				// the kept pod as the tester.
				g.store.mu.Lock()
				for i := range g.store.claims {
					if g.store.claims[i].Token == keyed {
						g.store.claims[i].Role = claim.RoleTester
					}
				}
				g.store.mu.Unlock()
				g.spawn(testSpec(t, keyed, claim.RoleTester, testTree))
			}
			g = newRig(t, nil, withOptions(func(o *Options) {
				o.GitHubCredential = appauth.CredentialOf(func(_ context.Context, _ string, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
					if refreshing.Load() && tc.during != "" && role == tc.during && landing.CompareAndSwap(false, true) {
						launch()
						landed.Store(true)
					}
					owner := strings.ToLower(repo.Owner())
					if owner == "" {
						owner = "sjawhar"
					}
					appRole, err := appauth.AppRoleFor(role, nil, appauth.Planned{})
					if err != nil {
						return ghconfig.Bundle{}, err
					}
					app := string(appRole)
					return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
						"acme": ghconfig.Render("ghs_"+app+"_read", app, staticCredentialExpiry),
						owner:  ghconfig.Render("ghs_"+app, app, staticCredentialExpiry),
					}}, nil
				})
			}))
			g.spawn(rootSpec(t))
			issue := SandboxName(rootToken)
			slot := roleSecretName(issue, claim.RoleImplementer)
			editSecret(g, slot, func(s *corev1.Secret) { s.Data[GitHubHostsKey("sjawhar")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
			// The API server refuses a write of the slot's Secret read before the launch wrote it,
			// as its resourceVersion says: here the refresh's one write that the launch overtook.
			g.kube.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				secret := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
				if tc.during == claim.RoleImplementer && secret.Name == slot && refreshing.Load() && landed.Load() && refused.CompareAndSwap(false, true) {
					return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), slot, errors.New("the object has been modified"))
				}
				return false, nil, nil
			})
			// The keyed claim is stored, on its first step's role, as the supervisor stores it.
			g.store.mu.Lock()
			g.store.claims = append(g.store.claims, supervise.Claim{Token: keyed, Project: testProject, Tree: testTree, Issue: testTree, Role: claim.RoleImplementer})
			g.store.mu.Unlock()
			if tc.during == "" {
				launch()
			}
			refreshing.Store(true)
			g.r.refreshGitHubCredentials(g.ctx)
			refreshing.Store(false)
			if tc.during != "" && !landed.Load() {
				t.Fatal("the launch never landed inside the refresh: the case saw no window")
			}
			if tc.during == claim.RoleImplementer && !refused.Load() {
				t.Fatal("the refresh never tried to write the slot after the launch: the case's premise, a write the launch overtook")
			}
			start := lastStart(t, g, keyed)
			if start.GitHubCredential == nil {
				t.Fatal("the launch into the kept pod named no credential: the case's premise")
			}
			token, err := ghconfig.TokenFromHosts(g.secret(slot).Data[GitHubHostsKey("sjawhar")])
			if err != nil {
				t.Fatalf("the implementer slot's hosts.yml: %v", err)
			}
			testerApp, err := appauth.AppRoleFor(claim.RoleTester, nil, appauth.Planned{})
			if err != nil {
				t.Fatal(err)
			}
			if want := "ghs_" + string(testerApp); token != want {
				t.Fatalf("after the refresh the implementer slot's hosts.yml holds %q, the App of the claim's old role %s; the launch's start frame waits for %q (digest %s), the App of its role now, %s",
					token, claim.RoleImplementer, want, start.GitHubCredential.TokenDigest, claim.RoleTester)
			}
		})
	}
}

// The record a launch leaves on its slot's Secret (launchedRoleAnnotation) is of the process that
// launch started, and a later process of the claim, started by a release that writes no record —
// one the daemon rolled back to, whose launches leave the record as it was — runs the role the
// stored claim holds. So the refresher reads the record only while the stored claim is at the
// generation that launch started or before it: key a's claim, re-roled to the tester and launched
// at generation 1, which left the record, is stored again as the implementer at generation 2, and
// its slot is refreshed for the implement App. The control is the same stored claim at
// generation 1, whose slot holds the tester's App the record names.
func TestARefreshReadsTheLaunchRecordOnlyOfTheClaimsLatestGeneration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation uint64
		role       claim.Role
		want       string
	}{
		{name: "control: the stored claim is at the generation the record is of", generation: 1, role: claim.RoleTester, want: "ghs_review"},
		{name: "the stored claim is at a later generation a release that writes no record started", generation: 2, role: claim.RoleImplementer, want: "ghs_implement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyed := claim.Token("legion-legion-legion-208-implementer")
			g := newRig(t, nil, withOptions(func(o *Options) {
				o.GitHubCredential = appauth.CredentialOf(func(_ context.Context, _ string, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
					owner := strings.ToLower(repo.Owner())
					if owner == "" {
						owner = "sjawhar"
					}
					appRole, err := appauth.AppRoleFor(role, nil, appauth.Planned{})
					if err != nil {
						return ghconfig.Bundle{}, err
					}
					app := string(appRole)
					return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
						"acme": ghconfig.Render("ghs_"+app+"_read", app, staticCredentialExpiry),
						owner:  ghconfig.Render("ghs_"+app, app, staticCredentialExpiry),
					}}, nil
				})
			}))
			g.spawn(rootSpec(t))
			g.store.mu.Lock()
			g.store.claims = append(g.store.claims, supervise.Claim{Token: keyed, Project: testProject, Tree: testTree, Issue: testTree,
				Role: claim.RoleTester, Generation: 1})
			g.store.mu.Unlock()
			launch := testSpec(t, keyed, claim.RoleTester, testTree)
			launch.Generation = 1
			g.spawn(launch)
			g.store.mu.Lock()
			for i := range g.store.claims {
				if g.store.claims[i].Token == keyed {
					g.store.claims[i].Role, g.store.claims[i].Generation = tc.role, tc.generation
				}
			}
			g.store.mu.Unlock()
			slot := roleSecretName(SandboxName(rootToken), claim.RoleImplementer)
			editSecret(g, slot, func(s *corev1.Secret) { s.Data[GitHubHostsKey("sjawhar")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })

			g.r.refreshGitHubCredentials(g.ctx)

			token, err := ghconfig.TokenFromHosts(g.secret(slot).Data[GitHubHostsKey("sjawhar")])
			if err != nil {
				t.Fatalf("the implementer slot's hosts.yml: %v", err)
			}
			if token != tc.want {
				t.Fatalf("after the refresh the implementer slot's hosts.yml holds %q, want %q: the stored claim is the %s at generation %d",
					token, tc.want, tc.role, tc.generation)
			}
		})
	}
}
