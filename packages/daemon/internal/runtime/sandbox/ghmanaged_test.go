package sandbox

import (
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// managedUser is a GitHub Enterprise Managed User's account, whose login ends in
// `_<shortcode>`: GitHub serves repositories under it as under any login, and its gh directory
// is named for that login as every owner's is (ghconfig.IsOwner).
const managedUser = "mona-cat_octo"

// managedIssue is the repository of an issue under managedUser, spelt in the case GitHub may
// spell it, which the bundle's directory lowercases.
var managedIssue = ghrepo.MustParse("Mona-Cat_Octo/widgets")

// servingManagedUser is a daemon whose two Apps are installed on sjawhar and managedUser and
// which serves the smoke repository alone, with the daemon's own gh answers
// (appauth.GitHubCredential and appauth.GitHubDirs over the two records their bundles share):
// the managed user's repository is no repository the daemon serves, and an issue on it is run
// because both Apps are installed on that account (INFRA-2543 R4).
func servingManagedUser(o *Options) {
	served := servesOneOf(func() []ghrepo.Repository { return []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion-smoke")} })
	installed, left := appauth.NewInstalled(), appauth.NewLeftOut()
	apps := sourceOf(refusingOwnerTokens{refuse: new(atomic.Bool)}, installedOnOwners{managedUser, "sjawhar"})
	o.GitHubCredential = appauth.GitHubCredential(apps, served, ghrepo.Renames{}, installed, left, o.DeploymentApps, o.Log)
	o.GitHubDirs = func(repo ghrepo.Repository, roles []appauth.AppRole) []string {
		return appauth.GitHubDirs(served, ghrepo.Renames{}, installed, left, roles, repo)
	}
}

// managedIssueSpec is workerSpec on managedIssue.
func managedIssueSpec(t *testing.T) runtime.SpawnSpec {
	spec := workerSpec(t)
	spec.Repository = managedIssue
	return spec
}

// An issue whose own repository's owner is a managed user is launched as any other is: its pod
// projects that owner's directory beside the other owner both Apps are installed on, the role's
// Secret records it as the default and holds that owner's own lease in it, and the role's
// generation is started with GH_CONFIG_DIR naming it, the directory its own repository routes
// to. A directory grammar narrower than ghrepo's let the daemon admit such an issue and then
// failed every role's launch on its own default.
func TestAManagedUsersIssueLaunchesWithItsOwnersDirectory(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(servingManagedUser))
	loc := g.spawn(managedIssueSpec(t))

	if held := ghItems(t, g.pod(loc.Sandbox.Name), claim.RoleTester); !slices.Equal(held, ghItemsFor(managedUser, "sjawhar")) {
		t.Fatalf("the tester's gh volume projects %+v, want the two files of %s and of sjawhar", held, managedUser)
	}
	if dir := startEnv(lastStart(t, g, workerToken))["GH_CONFIG_DIR"]; dir != ghconfig.Dir(GHConfigDir, managedUser) {
		t.Errorf("the tester is started with GH_CONFIG_DIR=%q, want %q, the directory its issue's repository routes to", dir, ghconfig.Dir(GHConfigDir, managedUser))
	}
	secret := g.secret(roleSecretName(loc.Sandbox.Name, claim.RoleTester))
	if got := string(secret.Data[GitHubDefaultKey]); got != managedUser {
		t.Errorf("the tester's Secret records the default %q, want %s", got, managedUser)
	}
	want := "ghs_" + string(appOf(t, claim.RoleTester)) + "_" + managedUser + "_write"
	if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(managedUser)]); err != nil || token != want {
		t.Errorf("the tester's Secret holds %q (%v) for %s, want %q, that owner's own lease", token, err, managedUser, want)
	}
}

// A managed user's gh files are carried under Secret keys the API server takes — a data key is
// `[-._a-zA-Z0-9]+`, the underscore among them — projected to paths inside the gh volume, and
// read back by GitHubHostsDir as the directory they were written for: ghKeyTail turns no
// underscore into anything, and a login holds no dot for it to mistake for the slash it writes.
// The drift check reads a pod's projection by that rule (ghProjectionOf), so the pod launched
// for such an issue holds the directory a pod created now would hold, and has drifted from
// nothing. Read back as any other name, the managed user's directory would be lacking at every
// sweep and the pod replaced over it for ever.
func TestAManagedUsersGhKeysReadBackAsItsDirectoryAndItsPodHasNotDrifted(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(servingManagedUser))
	loc := g.spawn(managedIssueSpec(t))
	pod := g.pod(loc.Sandbox.Name)

	secret := g.secret(roleSecretName(loc.Sandbox.Name, claim.RoleTester))
	for _, key := range []string{GitHubHostsKey(managedUser), GitHubConfigKey(managedUser)} {
		if _, held := secret.Data[key]; !held {
			t.Errorf("the tester's Secret holds no %s", key)
		}
		if errs := validation.IsConfigMapKey(key); len(errs) > 0 {
			t.Errorf("%s is no key a Secret may hold: %v", key, errs)
		}
	}
	for _, item := range ghItems(t, pod, claim.RoleTester) {
		if !filepath.IsLocal(item.Path) {
			t.Errorf("the gh volume projects %s at %q, which is no path inside it", item.Key, item.Path)
		}
	}
	if dir, ok := GitHubHostsDir(GitHubHostsKey(managedUser)); !ok || dir != managedUser {
		t.Errorf("GitHubHostsDir(%q) = %q, %t; want %s", GitHubHostsKey(managedUser), dir, ok, managedUser)
	}
	if projected, _ := ghProjectionOf(pod, claim.RoleTester); !slices.Contains(projected.dirs, managedUser) {
		t.Errorf("the tester's projection reads as the directories %q, want %s among them", projected.dirs, managedUser)
	}
	apps, err := podAppRoles(g.r, podKindFor(claim.RoleTester))
	if err != nil {
		t.Fatal(err)
	}
	if served := g.r.gitHubDirs(managedIssue, apps); !slices.Contains(served, managedUser) {
		t.Fatalf("the daemon serves %q, want %s among them: the drift check would compare nothing of that owner's", served, managedUser)
	}
	if named := ghDriftReasons(t, g, loc, claim.RoleTester); len(named) != 0 {
		t.Errorf("PodDrift names %q, want nothing: the pod holds every directory a pod created now would", named)
	}
}
