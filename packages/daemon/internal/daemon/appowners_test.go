package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// appOwnersSource is the GitHub App token source of plan.appOwners' test: each App installed on
// the owners installed names for it, and every lease of an owner refused names for it refused as
// GitHub refuses a narrowing (appauth.ErrNarrowingRefused). That is one of the four answers a
// bundle leaves an owner's directory out over, the one it says as "permissions refused"
// (appauth's leaveOutReason).
type appOwnersSource struct {
	installed, refused map[appauth.AppRole][]string
}

func (s appOwnersSource) lease(role appauth.AppRole, owner string) (appauth.Lease, error) {
	if slices.Contains(s.refused[role], owner) {
		return appauth.Lease{}, fmt.Errorf("%w (owner %s)", appauth.ErrNarrowingRefused, owner)
	}
	return appauth.Lease{Token: "ghs_" + string(role) + "_" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (s appOwnersSource) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return s.lease(role, owner)
}

func (s appOwnersSource) ReadToken(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return s.lease(role, owner)
}

func (s appOwnersSource) InstalledOwners(_ context.Context, role appauth.AppRole) ([]string, error) {
	return s.installed[role], nil
}

// bothApps is a source whose two Apps are installed on the same owners and refuse nothing.
func bothApps(owners ...string) appOwnersSource {
	return appOwnersSource{installed: map[appauth.AppRole][]string{appauth.Implement: owners, appauth.Review: owners}}
}

// roleOwners is what plan.appOwners answers three roles under one runtime: the implementer and
// the reviewer, which run as the two Apps and share an issue pod under Kubernetes, and the
// project controller, which runs as the review App in a pod or pane of its own.
type roleOwners struct{ implementer, reviewer, controller []string }

// A launch's addressing says of each owner it names that the role's GitHub App is installed on
// it and that the role's token reaches every repository of it (RepositoriesClause). So what the
// daemon answers a launch (plan.appOwners) is the installed owners the role's container holds a
// directory for, and not every installed owner.
//
// An owner whose lease GitHub refuses is still in the installations the bundle read, and the
// bundle holds no directory for it. The alias directory a resolving rename puts in the bundle is
// the other way round, a directory for an account the App is not installed on. Neither is
// named. The runtimes differ in what a container holds: a tmux pane holds its own role's bundle,
// while a Kubernetes issue pod projects only the directories both its Apps' bundles hold, so an
// owner the other App is not installed on, or one the other App's mint left out, is in no
// container of that pod, whichever App the role runs as. The project controller's pod holds the
// review App alone and answers alike under both runtimes. Until a bundle of each App the
// container's directories come from has been read, there is no read to report, and the answer
// is the owners of the repositories the project is mapped.
func TestAppOwnersAreTheInstalledOwnersTheRolesContainerHoldsADirectoryFor(t *testing.T) {
	moved := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		mapped  []string
		source  appOwnersSource
		// built are the roles whose bundles are built for repo before the launches are asked.
		built []claim.Role
		repo  string
		tmux  roleOwners
		pod   roleOwners
	}{
		{
			name:   "every installed owner's lease is minted",
			mapped: []string{"acme/widgets"}, source: bothApps("acme", "globex"),
			built: []claim.Role{claim.RoleImplementer, claim.RoleReviewer}, repo: "acme/widgets",
			tmux: roleOwners{[]string{"acme", "globex"}, []string{"acme", "globex"}, []string{"acme", "globex"}},
			pod:  roleOwners{[]string{"acme", "globex"}, []string{"acme", "globex"}, []string{"acme", "globex"}},
		},
		{
			name:   "an installed owner whose lease the role's own App is refused is left out",
			mapped: []string{"acme/widgets"},
			source: appOwnersSource{
				installed: bothApps("acme", "globex").installed,
				refused:   map[appauth.AppRole][]string{appauth.Implement: {"globex"}},
			},
			built: []claim.Role{claim.RoleImplementer, claim.RoleReviewer}, repo: "acme/widgets",
			tmux: roleOwners{[]string{"acme"}, []string{"acme", "globex"}, []string{"acme", "globex"}},
			// The issue pod holds no globex directory at all, so the reviewer, whose own App
			// mints globex, is not told it either; the controller's pod is the review App's own.
			pod: roleOwners{[]string{"acme"}, []string{"acme"}, []string{"acme", "globex"}},
		},
		{
			name:   "the pod's other App is not installed on an owner",
			mapped: []string{"acme/widgets"},
			source: appOwnersSource{installed: map[appauth.AppRole][]string{
				appauth.Implement: {"acme", "globex"}, appauth.Review: {"acme"},
			}},
			built: []claim.Role{claim.RoleImplementer, claim.RoleReviewer}, repo: "acme/widgets",
			tmux: roleOwners{[]string{"acme", "globex"}, []string{"acme"}, []string{"acme"}},
			pod:  roleOwners{[]string{"acme"}, []string{"acme"}, []string{"acme"}},
		},
		{
			name:   "the pod's other App's mint leaves an owner out",
			mapped: []string{"acme/widgets"},
			source: appOwnersSource{
				installed: bothApps("acme", "globex").installed,
				refused:   map[appauth.AppRole][]string{appauth.Review: {"globex"}},
			},
			built: []claim.Role{claim.RoleImplementer, claim.RoleReviewer}, repo: "acme/widgets",
			tmux: roleOwners{[]string{"acme", "globex"}, []string{"acme"}, []string{"acme"}},
			pod:  roleOwners{[]string{"acme"}, []string{"acme"}, []string{"acme"}},
		},
		{
			name:    "the alias directory of a resolving rename is no installed owner",
			renames: moved, mapped: []string{"cybermonklabs/legion"}, source: bothApps("cybermonklabs"),
			built: []claim.Role{claim.RoleImplementer, claim.RoleReviewer}, repo: "sjawhar/legion",
			tmux: roleOwners{[]string{"cybermonklabs"}, []string{"cybermonklabs"}, []string{"cybermonklabs"}},
			pod:  roleOwners{[]string{"cybermonklabs"}, []string{"cybermonklabs"}, []string{"cybermonklabs"}},
		},
		{
			name:   "the pod's other App has not been read yet",
			mapped: []string{"acme/widgets"}, source: bothApps("acme", "globex"),
			built: []claim.Role{claim.RoleImplementer}, repo: "acme/widgets",
			tmux: roleOwners{[]string{"acme", "globex"}, []string{"acme"}, []string{"acme"}},
			pod:  roleOwners{[]string{"acme"}, []string{"acme"}, []string{"acme"}},
		},
		{
			name:   "no bundle of either App built yet",
			mapped: []string{"acme/widgets", "zeta/tools"}, source: bothApps("globex"),
			tmux: roleOwners{[]string{"acme", "zeta"}, []string{"acme", "zeta"}, []string{"acme", "zeta"}},
			pod:  roleOwners{[]string{"acme", "zeta"}, []string{"acme", "zeta"}, []string{"acme", "zeta"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan{repositories: newRepositories("LEGION", tc.renames), installed: appauth.NewInstalled(), left: appauth.NewLeftOut()}
			accepted := make([]mappedRepository, len(tc.mapped))
			for i, repo := range tc.mapped {
				accepted[i] = mappedRepository{Repo: ghrepo.MustParse(repo)}
			}
			p.repositories.set(accepted, nil)
			// Each bundle is built as a runtime builds it, over the plan's own two records, so
			// what a row reads is what a real mint recorded.
			credential := appauth.GitHubCredential(tc.source, p.repositories.Accepted, p.repositories.renames, p.installed, p.left, quietLogger())
			for _, role := range tc.built {
				if _, err := credential(context.Background(), role, ghrepo.MustParse(tc.repo)); err != nil {
					t.Fatalf("build the %s's bundle for %s: %v", role, tc.repo, err)
				}
			}
			for runtimeName, want := range map[string]roleOwners{"tmux": tc.tmux, "kubernetes": tc.pod} {
				owners := p.appOwners(runtimeName)
				for role, want := range map[claim.Role][]string{
					claim.RoleImplementer: want.implementer, claim.RoleReviewer: want.reviewer, claim.RoleController: want.controller,
				} {
					if got := owners(role); !slices.Equal(got, want) {
						t.Errorf("under %s, appOwners(%s) = %q, want %q: the installed owners the %s's container holds a directory for",
							runtimeName, role, got, want, role)
					}
				}
			}
		})
	}
}
