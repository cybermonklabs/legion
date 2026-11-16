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

// appOwnersSource is the GitHub App token source of plan.appOwners' test: an App installed on
// installed, whose every lease of an owner in refused GitHub refuses as it refuses a narrowing
// (appauth.ErrNarrowingRefused). That is one of the four answers a bundle leaves an owner's
// directory out over, the one it says as "permissions refused" (appauth's leaveOutReason).
type appOwnersSource struct{ installed, refused []string }

func (s appOwnersSource) lease(role appauth.AppRole, owner string) (appauth.Lease, error) {
	if slices.Contains(s.refused, owner) {
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

func (s appOwnersSource) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	return s.installed, nil
}

// A launch's addressing says of each owner it names that the role's GitHub App is installed on
// it and that the role's token reaches every repository of it (RepositoriesClause). So what the
// daemon answers a launch (plan.appOwners) is the installed owners the role's gh bundle holds a
// directory for, and not every installed owner. An owner whose lease GitHub refuses is still in
// the installations the bundle read, and the bundle holds no directory for it: named, it would
// be told as an owner the role's token reaches. The alias directory a resolving rename puts in
// the bundle is the other way round, a directory for an account the App is not installed on,
// and is never named either. Until a bundle of the role's App has been built there is no read
// to report, and the answer is the owners of the repositories the project is mapped.
func TestAppOwnersAreTheInstalledOwnersTheRolesBundleHoldsADirectoryFor(t *testing.T) {
	moved := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		mapped  []string
		source  appOwnersSource
		// repo is the repository the implementer's bundle is built for, and "" where none is
		// built; dirs are the directories that bundle holds.
		repo string
		dirs []string
		want []string
	}{
		{
			name:   "every installed owner's lease is minted",
			mapped: []string{"acme/widgets"}, source: appOwnersSource{installed: []string{"acme", "globex"}},
			repo: "acme/widgets", dirs: []string{"acme", "globex"}, want: []string{"acme", "globex"},
		},
		{
			name:   "an installed owner whose lease GitHub refuses is left out",
			mapped: []string{"acme/widgets"}, source: appOwnersSource{installed: []string{"acme", "globex"}, refused: []string{"globex"}},
			repo: "acme/widgets", dirs: []string{"acme"}, want: []string{"acme"},
		},
		{
			name:    "the alias directory of a resolving rename is no installed owner",
			renames: moved, mapped: []string{"cybermonklabs/legion"}, source: appOwnersSource{installed: []string{"cybermonklabs"}},
			repo: "sjawhar/legion", dirs: []string{"cybermonklabs", "sjawhar"}, want: []string{"cybermonklabs"},
		},
		{
			name:   "no bundle of the App built yet",
			mapped: []string{"acme/widgets", "zeta/tools"}, source: appOwnersSource{installed: []string{"globex"}},
			want: []string{"acme", "zeta"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan{repositories: newRepositories("LEGION", tc.renames), installed: appauth.NewInstalled(), left: appauth.NewLeftOut()}
			accepted := make([]mappedRepository, len(tc.mapped))
			for i, repo := range tc.mapped {
				accepted[i] = mappedRepository{Repo: ghrepo.MustParse(repo)}
			}
			p.repositories.set(accepted, nil)
			if tc.repo != "" {
				// The bundle is built as a runtime builds it, over the plan's own two records,
				// so what the row reads is what a real mint recorded.
				credential := appauth.GitHubCredential(tc.source, p.repositories.Accepted, p.repositories.renames, p.installed, p.left, quietLogger())
				bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse(tc.repo))
				if err != nil {
					t.Fatalf("build the implementer's bundle for %s: %v", tc.repo, err)
				}
				if got := bundle.DirNames(); !slices.Equal(got, tc.dirs) {
					t.Fatalf("the implementer's bundle holds the directories %q, want %q: this row proves nothing otherwise", got, tc.dirs)
				}
				if installed, read := p.installed.Owners(appauth.Implement); !read || !slices.Equal(installed, tc.source.installed) {
					t.Fatalf("the bundle recorded the implement App installed on %q (read %t), want %q: this row proves nothing otherwise", installed, read, tc.source.installed)
				}
			}
			if got := p.appOwners(claim.RoleImplementer); !slices.Equal(got, tc.want) {
				t.Errorf("appOwners(implementer) = %q, want %q: the installed owners the implementer's bundle holds a directory for", got, tc.want)
			}
			// The records are per App: no bundle of the review App was built, so a role of it is
			// answered the mapping's owners whatever the implement App's bundle found.
			if got, want := p.appOwners(claim.RoleReviewer), p.repositories.Owners(); !slices.Equal(got, want) {
				t.Errorf("appOwners(reviewer) = %q, want the mapping's owners %q while no bundle of the review App has been built", got, want)
			}
		})
	}
}
