package appauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ownerTokens names each lease for the App, what kind of lease it is and the owner whose
// installation mints it, which is the whole of what a lease is now (INFRA-2543), so a test reads
// which mint answered for which directory. refuse is an owner every mint for it fails, with an
// error that is no narrowing refusal, and unreachable names the owners the App's installation
// cannot mint for, as GitHub's 422 says it: the key `<owner>` refuses both of that owner's
// leases, which is what leaves an owner's directory out of a role's bundle, and `<owner> read`
// refuses its reading lease alone, which leaves that owner out of a controller's bundle while
// the role's own lease of the same owner stands.
type ownerTokens struct {
	refuse      string
	unreachable map[string]bool
}

// refusesNarrowing reports that the App's installation on owner mints no lease of this kind, as
// a 422 says it: GitHub refuses the request whole and mints nothing.
func (t ownerTokens) refusesNarrowing(owner string, read bool) error {
	named := strings.ToLower(owner)
	if t.unreachable[named] || (read && t.unreachable[named+" read"]) {
		return fmt.Errorf("%w (permissions contents: read)", ErrNarrowingRefused)
	}
	return nil
}

func (t ownerTokens) Token(_ context.Context, role AppRole, owner string) (Lease, error) {
	if strings.TrimSpace(owner) == "" {
		return Lease{}, errors.New("a write token names no owner")
	}
	if strings.EqualFold(owner, t.refuse) {
		return Lease{}, errors.New("no installation on " + owner)
	}
	if err := t.refusesNarrowing(owner, false); err != nil {
		return Lease{}, err
	}
	return Lease{Token: string(role) + "-token-for-" + strings.ToLower(owner), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (t ownerTokens) ReadToken(_ context.Context, role AppRole, owner string) (Lease, error) {
	if strings.TrimSpace(owner) == "" {
		return Lease{}, errors.New("a read token names no owner")
	}
	if strings.EqualFold(owner, t.refuse) {
		return Lease{}, errors.New("no installation on " + owner)
	}
	if err := t.refusesNarrowing(owner, true); err != nil {
		return Lease{}, err
	}
	return Lease{Token: string(role) + "-read-for-" + strings.ToLower(owner), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// installedOn is the owner accounts an App is installed on, which a bundle reads at every build
// (OwnerInstallations): the daemon's own reader asks GitHub each time, so this one counts its
// reads and may be given an owner between two builds. fail is what every read answers instead,
// which is the bundle's own failure.
type installedOn struct {
	mu     sync.Mutex
	owners []string
	fail   error
	reads  int
}

// installedOnOwners is an App installed on owners and nothing else.
func installedOnOwners(owners ...string) *installedOn { return &installedOn{owners: owners} }

// installedEverywhere is an App installed on every owner the project serves a repository of,
// which is the ordinary deployment and what a test that is not about the two differing wants.
func installedEverywhere(repos func() []ghrepo.Repository) *installedOn {
	owners := map[string]bool{}
	for _, repo := range repos() {
		owners[strings.ToLower(repo.Owner())] = true
	}
	return installedOnOwners(slices.Sorted(maps.Keys(owners))...)
}

func (i *installedOn) InstalledOwners(context.Context, AppRole) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.reads++
	if i.fail != nil {
		return nil, i.fail
	}
	return slices.Clone(i.owners), nil
}

// bundleLog is a logger a test reads what a bundle said on, and the lines it holds.
func bundleLog() (*slog.Logger, *bytes.Buffer) {
	said := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(said, &slog.HandlerOptions{Level: slog.LevelWarn})), said
}

// quietBundleLog is the logger of a test that reads no line of its own.
func quietBundleLog() *slog.Logger {
	log, _ := bundleLog()
	return log
}

// bundleTokens is each directory's token in bundle.
func bundleTokens(t *testing.T, bundle ghconfig.Bundle) map[string]string {
	t.Helper()
	tokens := map[string]string{}
	for dir, rendered := range bundle.Dirs {
		token, err := ghconfig.TokenFromHosts([]byte(rendered.Hosts))
		if err != nil {
			t.Fatalf("%s's hosts.yml: %v", dir, err)
		}
		tokens[dir] = token
	}
	return tokens
}

// projectRepos is the mapping's repositories as the daemon serves them.
func projectRepos(names ...string) func() []ghrepo.Repository {
	repos := make([]ghrepo.Repository, 0, len(names))
	for _, name := range names {
		repos = append(repos, ghrepo.MustParse(name))
	}
	return func() []ghrepo.Repository { return repos }
}

// A role's gh bundle holds one directory per owner account its App is installed on, and nothing
// smaller: each of them holds that owner's own installation lease at the role's own
// permissions, the implement App's write set or the review App's review set, and each reaches
// every repository its installation holds, so no repository of an installed owner needs a
// directory of its own and none has one. An owner whose repositories the project maps none of
// is an owner of the bundle all the same: the installation is what decides, not the mapping. A
// repository outside the project's set still makes its owner's directory the default, since a
// role's repository is its issue's, from the daemon's own record. A call that names no
// repository holds the same leases and takes the first owner sorted as its default. A mint
// that fails fails the bundle, naming the owner it would have been minted on; an App installed
// nowhere has no bundle.
func TestGitHubCredentialBundlesEveryOwnerTheAppIsInstalledOn(t *testing.T) {
	repos := projectRepos("sjawhar/legion", "Acme/widgets", "acme/gadgets")
	installed := NewInstalled()
	// The App is installed on both accounts the mapping names, spelled as GitHub spells them:
	// every directory is lowercased, whatever case the account's login carries.
	installs := installedOnOwners("Acme", "sjawhar")
	credential := GitHubCredential(ownerTokens{}, installs, repos, ghrepo.Renames{}, installed, nil, quietBundleLog())
	for _, tc := range []struct {
		name    string
		role    claim.Role
		repo    ghrepo.Repository
		def     string
		tokens  map[string]string
		appName string
	}{
		{"an implementer on one of acme's two repositories", claim.RoleImplementer, ghrepo.MustParse("Acme/widgets"), "acme",
			map[string]string{
				"acme":    "implement-token-for-acme",
				"sjawhar": "implement-token-for-sjawhar",
			}, "implement"},
		{"a reviewer on sjawhar's only repository", claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion"), "sjawhar",
			map[string]string{
				"acme":    "review-token-for-acme",
				"sjawhar": "review-token-for-sjawhar",
			}, "review"},
		{"a merger on a repository outside the set", claim.RoleMerger, ghrepo.MustParse("Other/thing"), "other",
			map[string]string{
				"acme":    "implement-token-for-acme",
				"other":   "implement-token-for-other",
				"sjawhar": "implement-token-for-sjawhar",
			}, "implement"},
		{"a claim the daemon records no repository for", claim.RoleController, ghrepo.Repository{}, "acme",
			map[string]string{
				"acme":    "review-token-for-acme",
				"sjawhar": "review-token-for-sjawhar",
			}, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle, err := credential(context.Background(), tc.role, tc.repo)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			if bundle.Default != tc.def {
				t.Errorf("Default = %q, want %q", bundle.Default, tc.def)
			}
			if got := bundleTokens(t, bundle); !maps.Equal(got, tc.tokens) {
				t.Errorf("tokens = %v, want %v", got, tc.tokens)
			}
			for dir, rendered := range bundle.Dirs {
				if strings.Contains(dir, "/") {
					t.Errorf("the bundle holds the repository directory %s: a lease is an owner's whole installation", dir)
				}
				if rendered.App != tc.appName || rendered.Config != ghconfig.Config {
					t.Errorf("%s's render = App %q, config %q; want %q and the fixed config", dir, rendered.App, rendered.Config, tc.appName)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
			roles := []AppRole{AppRoleFor(tc.role)}
			if dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, nil, roles, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	refused := GitHubCredential(ownerTokens{refuse: "sjawhar"}, installs, repos, ghrepo.Renames{}, nil, nil, quietBundleLog())
	if _, err := refused(context.Background(), claim.RoleImplementer, ghrepo.MustParse("acme/widgets")); err == nil ||
		!strings.Contains(err.Error(), "sjawhar") || !strings.Contains(err.Error(), string(claim.RoleImplementer)) {
		t.Errorf("a mint that fails = %v, want an error naming the owner and the role", err)
	}
	none := GitHubCredential(ownerTokens{}, installedOnOwners(), projectRepos(), ghrepo.Renames{}, nil, nil, quietBundleLog())
	if _, err := none(context.Background(), claim.RoleController, ghrepo.Repository{}); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a bundle of an App installed nowhere = %v, want runtime.ErrNoGitHubOwner", err)
	}
}

// A bundle reads its App's installations from GitHub at every build and keeps no set of its own,
// so an account that installs the App is in the next bundle and one that removes it is out of
// it, with no clock moved, no timer and no restart. The drift check reads the record the last
// build wrote (Installed), never GitHub, and is handed what that build saw.
//
// The case: the App is installed on two accounts and the project maps one repository of one of
// them. The bundle holds a directory for each account, one lease each and no repository
// directory — the mapping decides nothing here — and a second build, after GitHub gains a third
// installation, holds three.
func TestABundleReadsEachAppsInstallationsFromGitHubAtEveryBuild(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"acme", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "widgets")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	// The project maps one repository of one of the two accounts: the other is an owner of the
	// bundle because the App is installed on it, which the mapping says nothing about.
	repos := projectRepos("acme/widgets")
	installed := NewInstalled()
	credential := GitHubCredential(tokens, tokens, repos, ghrepo.Renames{}, installed, NewLeftOut(), quietBundleLog())
	issue := ghrepo.MustParse("acme/widgets")

	bundle, err := credential(context.Background(), claim.RoleImplementer, issue)
	if err != nil {
		t.Fatalf("the implementer's bundle: %v", err)
	}
	if want := []string{"acme", "sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Fatalf("the bundle holds %q, want %q: one directory per owner the App is installed on", bundle.DirNames(), want)
	}
	if asked := github.exchanges(); len(asked) != 2 {
		t.Fatalf("%d exchanges, want one lease each: acme's and sjawhar's", len(asked))
	}
	for _, text := range github.exchangeTexts() {
		if strings.Contains(text, "repositor") {
			t.Errorf("a token request was sent as %s, want no repository named at all: the installation is the whole of the lease", text)
		}
	}
	roles := []AppRole{Implement}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, nil, roles, issue); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want what the build read %q", dirs, bundle.DirNames())
	}

	// GitHub gains an installation. Nothing else changes: the clock stands still, the Manager
	// keeps the two leases it minted, and the next bundle holds the new owner all the same.
	github.installOn("thirdlabs")
	bundle, err = credential(context.Background(), claim.RoleImplementer, issue)
	if err != nil {
		t.Fatalf("the bundle after the App was installed on a third account: %v", err)
	}
	if want := []string{"acme", "sjawhar", "thirdlabs"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q: the installations are read at every build", bundle.DirNames(), want)
	}
	if asked := github.exchanges(); len(asked) != 3 {
		t.Errorf("%d exchanges over the two bundles, want the new owner's lease alone added: %v", len(asked), asked)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, nil, roles, issue); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want what the second build read %q", dirs, bundle.DirNames())
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
}

// A read of the App's installations that fails fails the bundle, naming the App: a bundle built
// from a set the daemon could not read would narrow or widen a role's reach without saying so,
// where a failure leaves the role's live gh files as they are and the next refresh asks again.
// Nothing is minted.
func TestABundleFailsWhenTheAppsInstallationsCannotBeRead(t *testing.T) {
	var minted []string
	repos := projectRepos("acme/widgets")
	installs := &installedOn{owners: []string{"acme"}, fail: errors.New("GitHub App installation discovery failed (503)")}
	credential := GitHubCredential(countingTokens{minted: &minted}, installs, repos, ghrepo.Renames{}, NewInstalled(), nil, quietBundleLog())

	_, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("acme/widgets"))
	if err == nil || !strings.Contains(err.Error(), "installation discovery") || !strings.Contains(err.Error(), string(Implement)) {
		t.Fatalf("a bundle whose installations cannot be read = %v, want the read's failure naming the App", err)
	}
	if len(minted) != 0 {
		t.Errorf("minted %q, want nothing: the bundle is refused before anything is minted", minted)
	}

	controller := ControllerCredential(countingTokens{minted: &minted}, installs, repos, ghrepo.Renames{}, NewInstalled(), nil, quietBundleLog())
	if _, err := controller(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), string(Review)) {
		t.Errorf("the controller's bundle whose installations cannot be read = %v, want the read's failure naming the review App", err)
	}
}

// An owner directory gh holds to be no directory's name fails the bundle rather than being left
// out of it: the directory carries that owner's whole lease and may be the bundle's default, so
// a bundle written from a name that is not one would make a file of the bundle's into a
// directory and lose every owner (ghconfig.IsDirName, WriteBundle). The drift check answers no
// set for it either, which leaves every pod where it is.
func TestABundleFailsOnAnOwnerThatIsNoDirectoryName(t *testing.T) {
	repos := projectRepos("acme/widgets")
	installed := NewInstalled()
	installs := installedOnOwners("acme", "../escape")
	credential := GitHubCredential(ownerTokens{}, installs, repos, ghrepo.Renames{}, installed, nil, quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("acme/widgets"))
	if err == nil || !strings.Contains(err.Error(), "../escape") || len(bundle.Dirs) != 0 {
		t.Fatalf("a bundle of an owner that is no directory name = %+v, %v; want no bundle and a refusal naming it", bundle, err)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, nil, []AppRole{Implement}, ghrepo.MustParse("acme/widgets")); dirs != nil {
		t.Errorf("GitHubDirs = %q, want none: a bundle no launch could write is no set to compare a pod against", dirs)
	}
}

// legionRename is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: one repository under two names, whose owners the bundle holds one entry for.
var legionRename = ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")

// An owner a configured rename moved a repository away from keeps its directory, with the token
// of the installation that repository moved to, which is this deployment's own case: the bundle
// holds sjawhar for sjawhar/legion, which GitHub serves as cybermonklabs/legion, while the App
// is installed on cybermonklabs alone. That directory holds cybermonklabs' whole lease, the
// same entry the cybermonklabs directory holds and minted once, whatever repository the issue
// names: a role's token is its App's installation lease, so the issue's own repository decides
// which directory is the default and nothing else. Every owner outside the pair is minted for
// itself, as it always was.
func TestGitHubCredentialBundlesARenamedPairAsOneOwner(t *testing.T) {
	repos := projectRepos("Cybermonklabs/legion", "cybermonklabs/tools", "acme/widgets")
	for _, tc := range []struct {
		name    string
		role    claim.Role
		repo    ghrepo.Repository
		def     string
		tokens  map[string]string
		minted  []string
		appName string
		// oneLease are the directory pairs holding the one entry, minted once: the pair's two
		// owner directories wherever one lease is theirs to share.
		oneLease [][2]string
	}{
		{
			// The record still names sjawhar, so sjawhar's directory is the default, and what
			// it holds is the lease minted on cybermonklabs, the owner the repository moved to.
			name: "an implementer on the repository as it was recorded", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("sjawhar/legion"), def: "sjawhar", appName: "implement",
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-token-for-cybermonklabs",
			},
			minted:   []string{"acme", "cybermonklabs"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
		{
			// The record names the repository as GitHub holds it. The bundle is the same one:
			// which repository the issue names decides the default directory and nothing else.
			name: "an implementer on the repository as GitHub holds it now", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("Cybermonklabs/legion"), def: "cybermonklabs", appName: "implement",
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-token-for-cybermonklabs",
			},
			minted:   []string{"acme", "cybermonklabs"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
		{
			name: "an implementer on another owner of the project", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("acme/widgets"), def: "acme", appName: "implement",
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-token-for-cybermonklabs",
			},
			minted:   []string{"acme", "cybermonklabs"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
		{
			// A claim the daemon records no repository for takes the first owner sorted as its
			// default; its directories hold the review App's own leases all the same, since
			// what a role may do is its App's installation and not what its claim records.
			name: "a role whose claim records no repository", role: claim.RoleController,
			repo: ghrepo.Repository{}, def: "acme", appName: "review",
			tokens: map[string]string{
				"acme":          "review-token-for-acme",
				"cybermonklabs": "review-token-for-cybermonklabs",
				"sjawhar":       "review-token-for-cybermonklabs",
			},
			minted:   []string{"acme", "cybermonklabs"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []string
			renames := ghrepo.MustRenames(legionRename)
			installed := NewInstalled()
			installs := installedOnOwners("acme", "cybermonklabs")
			credential := GitHubCredential(countingTokens{minted: &minted}, installs, repos, renames, installed, nil, quietBundleLog())
			bundle, err := credential(context.Background(), tc.role, tc.repo)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			if bundle.Default != tc.def {
				t.Errorf("Default = %q, want %q", bundle.Default, tc.def)
			}
			if got := bundleTokens(t, bundle); !maps.Equal(got, tc.tokens) {
				t.Errorf("tokens = %v, want %v", got, tc.tokens)
			}
			if !slices.Equal(minted, tc.minted) {
				t.Errorf("minted for %v, want %v: the pair's directories hold one lease each, minted once", minted, tc.minted)
			}
			// Every pair of directories that shares a lease holds the one entry, minted once.
			for _, pair := range tc.oneLease {
				from, to := bundle.Dirs[pair[0]], bundle.Dirs[pair[1]]
				if from != to {
					t.Errorf("%s's entry = %+v and %s's = %+v, want the one entry in both directories", pair[0], from, pair[1], to)
				}
			}
			for dir, rendered := range bundle.Dirs {
				if rendered.App != tc.appName {
					t.Errorf("%s's render = App %q, want %q", dir, rendered.App, tc.appName)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
			roles := []AppRole{AppRoleFor(tc.role)}
			if dirs := GitHubDirs(repos, renames, installed, nil, roles, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	// With no pair configured, the owner the repository moved from is an owner like any other:
	// its own lease, minted for itself, as today.
	t.Run("no pair configured", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, installedEverywhere(both), both,
			ghrepo.Renames{}, nil, nil, quietBundleLog())
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("cybermonklabs/legion"))
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{
			"cybermonklabs": "implement-token-for-cybermonklabs",
			"sjawhar":       "implement-token-for-sjawhar",
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) || bundle.Default != "cybermonklabs" {
			t.Errorf("Default %q and tokens %v, want cybermonklabs and %v", bundle.Default, got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs", "sjawhar"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
	})

	// Where the project serves the pair's `from` as well as its `to`, the pair resolves for
	// nobody (ghrepo.Renames.Serving: a name the project serves under its own name is a
	// repository of its own, somebody having made one at the old location, which deletes
	// GitHub's redirect). So the owner the repository moved from is no alias, its own entry
	// wins — its token is its own — and nothing is warned of anywhere: the rule decides it per
	// repository rather than leaving two owners to argue over one directory.
	t.Run("the owner it moved from is an owner of the project under the pair's own from", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, installedEverywhere(both), both,
			ghrepo.MustRenames(legionRename), nil, nil, quietBundleLog())
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("cybermonklabs/legion"))
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{
			"cybermonklabs": "implement-token-for-cybermonklabs",
			"sjawhar":       "implement-token-for-sjawhar",
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
			t.Errorf("tokens = %v, want %v: each owner keeps its own token", got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs", "sjawhar"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
		if _, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion")); err != nil {
			t.Fatalf("credential: %v", err)
		}
	})
}

// A resolving pair moves one repository, never an owner: every directory named for an owner the
// App is installed on holds that owner's own lease, at the role's own permissions and whichever
// repository the issue is recorded on, and only a name no installation answers for — an owner a
// pair moved a repository away from while the App is installed on no such account — holds the
// lease of the owner that serves the moved repository (INFRA-2543 R2).
//
// The cases are the state production was in on 2026-11-06: the pair sjawhar/legion ->
// cybermonklabs/legion resolving while the project also serves sjawhar/oh-my-pi. Redirecting
// every directory of the pair to one owner's lease, as the bundle did while an alias owner's
// directory took its mint owner's lease whatever the issue's repository was, put a lease of
// sjawhar's installation in the directory named cybermonklabs, and left sjawhar's own
// repositories reachable from no directory at all for an issue on cybermonklabs/legion. Neither
// case may change a directory name, which is why each asserts the whole set against GitHubDirs
// as well: a pod's gh projection is fixed when the pod is created, so a name this rule added or
// dropped would be drift the sweep answers with an update round over every worker in the pod.
//
// The third case is that state with the tree recorded on the pair's `from`, which is the tree
// the failure stopped. Both accounts have the App, so both directories hold their own owner's
// lease: the repository the pair moved is written through the cybermonklabs directory, the name
// GitHub holds it under and the one every call the daemon makes for that tree already uses
// (internal/daemon's repositories.serving), while sjawhar's own repositories stay reachable
// from sjawhar's, which no other directory can reach. It is pinned here so that no later change
// can re-break the case this rule exists for, and it may not be deleted.
//
// The last case is that same tree where the App is installed on cybermonklabs alone, which is
// this deployment's own: the sjawhar directory is the rename's, there being no installation of
// that name to hold, and what it holds is cybermonklabs' whole lease, so `legion gh` and git
// answer for sjawhar/legion from it.
func TestEveryInstalledOwnerKeepsItsOwnLeaseWhileAPairResolves(t *testing.T) {
	base := projectRepos("Cybermonklabs/legion", "acme/widgets", "sjawhar/oh-my-pi")
	renames := ghrepo.MustRenames(legionRename)
	for _, tc := range []struct {
		name string
		// installs is the owners the App is installed on, which decide the directories.
		installs []string
		repo     ghrepo.Repository
		dirs     []string
		tokens   map[string]string
		minted   []string
	}{
		{
			// Every one of the three accounts has the App, so every directory holds its own
			// owner's lease, whichever of their repositories the issue is recorded on.
			name: "an issue on the old owner's own other repository", repo: ghrepo.MustParse("sjawhar/oh-my-pi"),
			installs: []string{"acme", "cybermonklabs", "sjawhar"},
			dirs:     []string{"acme", "cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-token-for-sjawhar",
			},
			minted: []string{"acme", "cybermonklabs", "sjawhar"},
		},
		{
			// The issue is the moved repository's, recorded under the name GitHub has: nothing
			// about the bundle turns on that, and the sjawhar directory holds sjawhar's own
			// installation's lease, so sjawhar/oh-my-pi is reachable and writable.
			name: "an issue on the repository the pair moved", repo: ghrepo.MustParse("cybermonklabs/legion"),
			installs: []string{"acme", "cybermonklabs", "sjawhar"},
			dirs:     []string{"acme", "cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-token-for-sjawhar",
			},
			minted: []string{"acme", "cybermonklabs", "sjawhar"},
		},
		{
			// The production failure of 2026-11-06, which is why this case may not be deleted:
			// the tree is recorded on the pair's from while the old owner has an installation
			// of its own. Each directory holds its own owner's lease, so the moved repository
			// is written through cybermonklabs' directory — the name GitHub holds it under —
			// and sjawhar's own repositories through sjawhar's.
			name:     "an issue recorded on the pair's from while the old owner serves one other repository",
			repo:     ghrepo.MustParse("sjawhar/legion"),
			installs: []string{"acme", "cybermonklabs", "sjawhar"},
			dirs:     []string{"acme", "cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-token-for-sjawhar",
			},
			minted: []string{"acme", "cybermonklabs", "sjawhar"},
		},
		{
			// The same tree where the App is installed on cybermonklabs alone, which is this
			// deployment's: sjawhar is in the bundle because the rename puts it there, it has
			// no installation of its own to hold, and the lease it holds is cybermonklabs'
			// whole one, minted once for the two directories.
			name:     "an issue recorded on the pair's from while the App is installed on the new owner alone",
			repo:     ghrepo.MustParse("sjawhar/legion"),
			installs: []string{"acme", "cybermonklabs"},
			dirs:     []string{"acme", "cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-token-for-cybermonklabs",
			},
			minted: []string{"acme", "cybermonklabs"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []string
			installed := NewInstalled()
			credential := GitHubCredential(countingTokens{minted: &minted}, installedOnOwners(tc.installs...), base,
				renames, installed, nil, quietBundleLog())
			bundle, err := credential(context.Background(), claim.RoleImplementer, tc.repo)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			tokens := bundleTokens(t, bundle)
			if !maps.Equal(tokens, tc.tokens) {
				t.Errorf("tokens = %v, want %v", tokens, tc.tokens)
			}
			// No directory of a role's bundle holds a reading lease: a role's token is its
			// App's installation lease on one account, and there is nothing narrower for a
			// directory to hold.
			for dir, token := range tokens {
				if !strings.Contains(token, "-token-for-") {
					t.Errorf("%s holds %q, want the role's own lease of an owner", dir, token)
				}
			}
			if !slices.Equal(minted, tc.minted) {
				t.Errorf("minted for %v, want %v: one lease per scope, each owner's own", minted, tc.minted)
			}
			if got := bundle.DirNames(); !slices.Equal(got, tc.dirs) {
				t.Errorf("the bundle's directories are %q, want %q: this rule renames none", got, tc.dirs)
			}
			if dirs := GitHubDirs(base, renames, installed, nil, []AppRole{Implement}, tc.repo); !slices.Equal(dirs, tc.dirs) {
				t.Errorf("GitHubDirs = %q, want the same set %q, which is what a running pod is compared against", dirs, tc.dirs)
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
		})
	}
}

// chainedRenames are two configured pairs whose owners chain while their repositories do not:
// sjawhar/legion moved to cybermonklabs, and cybermonklabs/agent-infra moved on to
// theorem-labs. NewRenames holds them — its chain check compares whole repositories, and no
// owner's repositories move to two owners — so the owner whose installation serves a role's own
// repository is itself an owner a configured pair moved a repository away from.
var chainedRenames = ghrepo.MustRenames(legionRename, ghrepo.MustRename("cybermonklabs/agent-infra", "theorem-labs/agent-infra"))

// An owner the App is installed on is minted for itself, whatever a second pair moved away from
// it: the lease a role recorded on sjawhar/legion writes cybermonklabs' repositories with is
// cybermonklabs' own, and the cybermonklabs directory is never redirected on to theorem-labs.
// Redirected, that directory would hand out a token of a third owner's installation, which
// GitHub would refuse, and theorem-labs' directory holds that owner's lease already.
func TestASecondPairDoesNotRedirectTheOwnerThatServesTheRolesRepository(t *testing.T) {
	served := []string{"Cybermonklabs/legion", "cybermonklabs/tools", "theorem-labs/agent-infra", "theorem-labs/nats-server"}
	repos := projectRepos(served...)
	recorded := ghrepo.MustParse("sjawhar/legion")
	installed := NewInstalled()
	credential := GitHubCredential(ownerTokens{}, installedOnOwners("cybermonklabs", "theorem-labs"), repos,
		chainedRenames, installed, nil, quietBundleLog())
	bundle, err := credential(context.Background(), claim.RoleImplementer, recorded)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	tokens := bundleTokens(t, bundle)
	want := map[string]string{
		"cybermonklabs": "implement-token-for-cybermonklabs",
		"sjawhar":       "implement-token-for-cybermonklabs",
		"theorem-labs":  "implement-token-for-theorem-labs",
	}
	if !maps.Equal(tokens, want) {
		t.Errorf("tokens = %v, want %v", tokens, want)
	}
	reached := map[string]bool{}
	for _, token := range tokens {
		owner, ok := leaseOwner(token)
		if !ok {
			t.Fatalf("the lease %q names no owner: the fakes render every lease as <app>-token-for-<owner> or <app>-read-for-<owner>", token)
		}
		reached[owner] = true
	}
	for _, repo := range served {
		if owner := strings.ToLower(ghrepo.MustParse(repo).Owner()); !reached[owner] {
			t.Errorf("no lease of the bundle is minted on %s, which serves %s; the bundle holds %v minted on %v",
				owner, strings.ToLower(repo), slices.Sorted(maps.Keys(tokens)), slices.Sorted(maps.Keys(reached)))
		}
	}
	if dirs := GitHubDirs(repos, chainedRenames, installed, nil, []AppRole{Implement}, recorded); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want the bundle's own %q, which is what a running pod is compared against", dirs, bundle.DirNames())
	}
}

// ControllerCredential is one read-only directory per owner the review App is installed on, each
// reaching every repository its installation holds and holding no repository directory of its
// own: the controller writes to none. Its default is the owner it is asked for — an alias owner
// included, which its launch may have recorded — or the first owner sorted when it is asked for
// none. An owner the bundle holds no directory for is refused before anything is minted, the
// refusal saying the App is not installed on it, and an App installed nowhere has no bundle.
func TestControllerCredentialIsReadOnlyForEveryOwner(t *testing.T) {
	repos := projectRepos("Cybermonklabs/legion", "cybermonklabs/tools", "acme/widgets")
	renames := ghrepo.MustRenames(legionRename)
	installs := installedOnOwners("acme", "cybermonklabs")
	want := map[string]string{
		"acme":          "review-read-for-acme",
		"cybermonklabs": "review-read-for-cybermonklabs",
		"sjawhar":       "review-read-for-cybermonklabs",
	}
	for asked, def := range map[string]string{"": "acme", "Cybermonklabs": "cybermonklabs", "Sjawhar": "sjawhar"} {
		bundle, err := ControllerCredential(ownerTokens{}, installs, repos, renames, nil, nil, quietBundleLog())(context.Background(), asked)
		if err != nil {
			t.Fatalf("ControllerCredential(%q): %v", asked, err)
		}
		if bundle.Default != def {
			t.Errorf("ControllerCredential(%q).Default = %q, want %q", asked, bundle.Default, def)
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
			t.Errorf("ControllerCredential(%q) tokens = %v, want %v", asked, got, want)
		}
		if err := bundle.Validate(); err != nil {
			t.Errorf("the bundle does not validate: %v", err)
		}
	}

	var minted []string
	outside := ControllerCredential(countingTokens{minted: &minted}, installs, repos, renames, nil, nil, quietBundleLog())
	if _, err := outside(context.Background(), "Outside-Project"); !errors.Is(err, ErrOwnerOutsideProject) ||
		!strings.Contains(err.Error(), `"outside-project"`) || len(minted) != 0 {
		t.Errorf("the controller asking for an owner the App is not installed on = %v, minted %q; want ErrOwnerOutsideProject naming it and no mint", err, minted)
	}
	none := ControllerCredential(ownerTokens{}, installedOnOwners(), projectRepos(), ghrepo.Renames{}, nil, nil, quietBundleLog())
	if _, err := none(context.Background(), ""); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a controller bundle of an App installed nowhere = %v, want runtime.ErrNoGitHubOwner", err)
	}
}

// countingTokens records the scope of each mint, in the order the bundle asked, and answers as
// the ownerTokens it holds: a write lease is its owner's name and a read lease that name with
// `read` after it, so a test reads both what was asked for and what was refused.
type countingTokens struct {
	ownerTokens
	minted *[]string
}

func (t countingTokens) Token(ctx context.Context, role AppRole, owner string) (Lease, error) {
	*t.minted = append(*t.minted, strings.ToLower(owner))
	return t.ownerTokens.Token(ctx, role, owner)
}

func (t countingTokens) ReadToken(ctx context.Context, role AppRole, owner string) (Lease, error) {
	*t.minted = append(*t.minted, strings.ToLower(owner)+" read")
	return t.ownerTokens.ReadToken(ctx, role, owner)
}

// GitHub answers 422 to a token request its installation cannot serve and mints nothing, so one
// owner's lease is refused whole. The refusal costs that owner's directory alone: it is the
// whole of what the directory could hold, since the lease is the whole installation, so the
// directory is left out, said once on the log, and the bundle is served rather than the role's
// launch failing for every owner. One ask decides it: there is nothing narrower to ask for.
func TestARefusedLeaseCostsItsOwnersDirectoryAndNotTheBundle(t *testing.T) {
	var minted []string
	log, said := bundleLog()
	repos := projectRepos("sjawhar/legion", "refusedlabs/widgets", "refusedlabs/gadgets")
	tokens := countingTokens{ownerTokens: ownerTokens{unreachable: map[string]bool{"refusedlabs": true}}, minted: &minted}
	credential := GitHubCredential(tokens, installedEverywhere(repos), repos, ghrepo.Renames{}, nil, nil, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	want := map[string]string{"sjawhar": "implement-token-for-sjawhar"}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v: the refused owner's directory is left out and the rest of the bundle stands", got, want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	if asked := []string{"refusedlabs", "sjawhar"}; !slices.Equal(minted, asked) {
		t.Errorf("minted for %v, want %v: the refused owner's lease and the issue owner's, each asked once", minted, asked)
	}
	for _, named := range []string{"app=implement", "owner=refusedlabs"} {
		if count := strings.Count(said.String(), named); count != 1 {
			t.Errorf("the log holds %d lines naming %q, want one:\n%s", count, named, said.String())
		}
	}
}

// The lease of the bundle's own default owner is not softened: a role that cannot act on its
// own issue's repository has nothing to do, so a refusal of that one directory fails the whole
// bundle, naming the role, the App and the owner the lease would have been minted on. Nothing
// is said on the log for it: the one line an owner gets for the life of the process says a
// directory was left out of a bundle that was served, and no bundle was.
func TestGitHubCredentialFailsOnARefusedDefaultOwnersLease(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("acme/widgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"acme": true}}
	credential := GitHubCredential(tokens, installedEverywhere(repos), repos, ghrepo.Renames{}, nil, nil, log)

	bundle, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("acme/widgets"))
	if !errors.Is(err, ErrNarrowingRefused) || len(bundle.Dirs) != 0 {
		t.Fatalf("a refused default owner's lease = %+v, %v; want no bundle and the refusal", bundle, err)
	}
	for _, named := range []string{"review", string(claim.RoleReviewer), "acme"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %q: %v", named, err)
		}
	}
	if said.Len() != 0 {
		t.Errorf("the log holds %q, want nothing: a bundle nothing was served from leaves no directory out", said.String())
	}
}

// An owner GitHub refuses is refused at every launch and every refresh of every role, so the
// line is said once for the life of the process rather than at each of them, as a Manager says a
// grant shortfall once.
func TestGitHubCredentialSaysARefusedOwnerOnceOverTwoBundles(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("oncelabs/served", "oncelabs/twice")
	tokens := ownerTokens{unreachable: map[string]bool{"oncelabs": true}}
	credential := GitHubCredential(tokens, installedEverywhere(repos), repos, ghrepo.Renames{}, nil, nil, log)

	for i := range 2 {
		if _, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("other/thing")); err != nil {
			t.Fatalf("bundle %d: %v", i+1, err)
		}
	}
	if count := strings.Count(said.String(), "owner=oncelabs"); count != 1 {
		t.Errorf("the log holds %d lines over two bundles, want one:\n%s", count, said.String())
	}
}

// A bundle's default is the directory its GH_CONFIG_DIR names, so it is chosen from the owners
// the mint answered for and not from the installations read: a caller that names no repository
// of its own — a tmux tree pane whose claim records none, every controller launch — would
// otherwise take the first owner sorted, and where GitHub refuses that owner's lease the bundle
// would come back without the one directory it is served from, which ghconfig.Bundle.Validate
// refuses long after the ask that could say why.
func TestABundleTakesItsDefaultFromTheOwnersTheMintAnsweredFor(t *testing.T) {
	repos := projectRepos("defaultlabs/widgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"defaultlabs": true}}
	credential := GitHubCredential(tokens, installedEverywhere(repos), repos, ghrepo.Renames{}, nil, NewLeftOut(), quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.Repository{})
	if err != nil {
		t.Fatalf("a bundle whose first owner is refused: %v", err)
	}
	if bundle.Default != "sjawhar" {
		t.Errorf("the default is %q, want sjawhar: defaultlabs answered for nothing", bundle.Default)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("directories = %q, want %q", bundle.DirNames(), want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
}

// A caller that names the default owner itself — the controller route carrying the owner its
// GH_CONFIG_DIR already names, and a role whose issue's repository is that owner's — cannot be
// served another owner's directory instead, so an owner GitHub refuses the lease of is
// ErrNarrowingRefused, naming the owner the lease would have been minted on.
func TestABundleRefusesADefaultOwnerGitHubRefusesEveryLeaseOf(t *testing.T) {
	repos := projectRepos("deflabs/widgets", "deflabs/gadgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"deflabs": true}}
	left := NewLeftOut()

	controller := ControllerCredential(tokens, installedEverywhere(repos), repos, ghrepo.Renames{}, nil, left, quietBundleLog())
	bundle, err := controller(context.Background(), "Deflabs")
	if !errors.Is(err, ErrNarrowingRefused) || len(bundle.Dirs) != 0 {
		t.Fatalf("the controller's bundle on a refused default owner = %+v, %v; want no bundle and ErrNarrowingRefused", bundle, err)
	}
	if !strings.Contains(err.Error(), "deflabs") {
		t.Errorf("the refusal does not name the owner: %v", err)
	}
	if _, err := controller(context.Background(), "sjawhar"); err != nil {
		t.Errorf("the controller asking for an owner that answers = %v, want its bundle", err)
	}
}

// refusedNarrowingAnswer is GitHub's 422 to a token request its installation cannot serve: it
// mints nothing, whatever else the request named.
const refusedNarrowingAnswer = `{"message":"The permissions requested are not granted to this installation."}`

// A directory whose owner's lease GitHub refuses is left out of the bundle, and the Manager
// remembers that refusal for five minutes (narrowingRefusalWindow): a bundle built inside the
// window is served from that memory with GitHub asked nothing, and the first bundle past it
// holds the directory again once the account has granted what the mint asks for. The rest of
// the bundle — the issue owner's own directory, the launch and the controller's own pod —
// waits on none of it (INFRA-513).
func TestADirectoryGitHubRefusesIsLeftOutAndHeldAgainOnceGitHubAnswers(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	// Whether the account grants the App what the role's lease asks for: it approves the grant
	// part-way through.
	grants := false
	github := &keyModeGitHub{owners: []string{"acme", "sjawhar"}, slug: "legion-implementer"}
	github.refuses = func(owner string) bool { return !grants && owner == "acme" }
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "gadgets", "widgets")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	repos := projectRepos("acme/gadgets", "acme/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, tokens, repos, ghrepo.Renames{}, NewInstalled(), NewLeftOut(), quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle while acme's lease is refused: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Fatalf("the bundle holds %q, want %q: the refused owner's directory is left out", bundle.DirNames(), want)
	}
	if asked := github.exchanges(); len(asked) != 2 {
		t.Fatalf("%d exchanges, want the refused owner's lease and the issue owner's", len(asked))
	}

	now = now.Add(narrowingRefusalWindow - time.Minute)
	if _, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion")); err != nil {
		t.Fatalf("the second bundle inside the window: %v", err)
	}
	if asked := github.exchanges(); len(asked) != 2 {
		t.Errorf("%d exchanges, want none added: inside the window the refusal and the lease are both served from the Manager", len(asked))
	}

	grants = true
	now = now.Add(2 * time.Minute)
	bundle, err = credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the bundle once the account granted the lease: %v", err)
	}
	if want := []string{"acme", "sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q: past the window the owner's lease is asked for again", bundle.DirNames(), want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
}

// A lease the Manager minted for an owner is served to every bundle until fewer than twenty
// minutes remain on it (refreshWindow), so a bundle built after an account changed the App's
// installation is served the token minted before it: a cached answer says a token was minted
// within the hour, never what GitHub would mint now. The directory it belongs to goes when the
// lease nears its expiry and the ask that replaces it is GitHub's answer of that moment, which
// is what a caller reading a directory as the installation's reach must know (lease,
// INFRA-513).
func TestADirectoryIsServedFromTheOwnersCachedLeaseUntilItNearsExpiry(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	// Whether the App's installation on cachedlabs mints what the role's lease asks for: the
	// account drops the grant part-way through.
	reaches := true
	github := &keyModeGitHub{owners: []string{"cachedlabs", "sjawhar"}, slug: "legion-implementer"}
	github.refuses = func(owner string) bool { return !reaches && owner == "cachedlabs" }
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "gadgets", "widgets")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	repos := projectRepos("cachedlabs/gadgets", "cachedlabs/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, tokens, repos, ghrepo.Renames{}, NewInstalled(), NewLeftOut(), quietBundleLog())

	// An earlier bundle minted cachedlabs' lease, and the Manager holds it for the hour it
	// lasts.
	cached, err := tokens.Token(context.Background(), Implement, "cachedlabs")
	if err != nil {
		t.Fatalf("the lease an earlier bundle minted for cachedlabs: %v", err)
	}

	reaches = false
	now = now.Add(time.Minute)
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the account dropped the grant on cachedlabs: %v", err)
	}
	if got := bundleTokens(t, bundle)["cachedlabs"]; got != cached.Token {
		t.Errorf("cachedlabs' directory holds %q, want the cached lease %q", got, cached.Token)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}

	// Fewer than twenty minutes are left on that lease, so the next bundle asks GitHub, which
	// refuses: the directory goes, and the issue owner's directory and the bundle stand.
	now = now.Add(40 * time.Minute)
	bundle, err = credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the cached lease neared its expiry: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q: the directory goes with the lease that stood for it", bundle.DirNames(), want)
	}
}

// A directory left out is said on the log once, naming the owner, and recorded left out
// (LeftOut), so the drift check's own answer names what a bundle built now holds and no pod is
// replaced for a directory no mint produces. The rest of the bundle — the issue owner's own
// directory among it — is minted and served.
func TestADirectoryLeftOutIsSaidOnceAndRecorded(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"gonelabs", "sjawhar"}, slug: "legion-implementer"}
	github.refuses = func(owner string) bool { return owner == "gonelabs" }
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	log, said := bundleLog()
	repos := projectRepos("gonelabs/gadgets", "gonelabs/widgets", "sjawhar/legion")
	installed, left := NewInstalled(), NewLeftOut()
	credential := GitHubCredential(tokens, tokens, repos, ghrepo.Renames{}, installed, left, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the App's installation refuses an owner: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q: the owner's directory left out and the issue owner's whole", bundle.DirNames(), want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, left, []AppRole{Implement}, ghrepo.MustParse("sjawhar/legion"))
	if !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
	}
	if count := strings.Count(said.String(), "owner=gonelabs"); count != 1 {
		t.Errorf("the log holds %d lines naming the owner left out, want one:\n%s", count, said.String())
	}
}

// One ask decides a directory. The bundle asks the owner's lease once and a refusal leaves the
// directory out, so a refused owner costs one exchange, and the next bundle inside the window
// costs none: the Manager answers it from the refusal it remembers (narrowingRefusalWindow).
// An ask per repository would cost one for each, and a rate limit on those asks would fail
// every bundle of the project.
func TestOneRefusedAskDecidesADirectoryAndGitHubIsAskedNoMore(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"pairlabs", "sjawhar"}, slug: "legion-implementer"}
	github.refuses = func(owner string) bool { return owner == "pairlabs" }
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	repos := projectRepos("pairlabs/gadgets", "pairlabs/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, tokens, repos, ghrepo.Renames{}, NewInstalled(), NewLeftOut(), quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle against an installation that refuses an owner: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q: the refused owner's directory is left out", bundle.DirNames(), want)
	}
	if asked := github.exchanges(); len(asked) != 2 {
		t.Fatalf("%d exchanges, want the one refused owner's lease and the issue owner's", len(asked))
	}
	if _, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion")); err != nil {
		t.Fatalf("the second bundle: %v", err)
	}
	if asked := github.exchanges(); len(asked) != 2 {
		t.Errorf("%d exchanges over two bundles, want none added: the refusal and the issue owner's lease are both served from the Manager", len(asked))
	}
}

// troubledTokens answers every mint with GitHub's trouble that also reads as a narrowing
// refusal, which a rate-limited 422 is: the lease a caller is refused that way says nothing
// about what the account grants the App's installation.
type troubledTokens struct{ ownerTokens }

func (troubledTokens) Token(context.Context, AppRole, string) (Lease, error) {
	return Lease{}, &TransientError{Err: fmt.Errorf("%w: GitHub App token exchange failed (422)", ErrNarrowingRefused)}
}

// A mint refused with GitHub's own trouble leaves no directory out, whatever else the refusal
// reads as: the bundle fails, so the caller tries again, where leaving a directory out would
// record an owner as unreachable on the strength of a rate limit (dirEntry, NarrowingRefused).
func TestGitHubsTroubleThatReadsAsARefusalFailsTheBundleAndLeavesNothingOut(t *testing.T) {
	log, said := bundleLog()
	installed, left := NewInstalled(), NewLeftOut()
	repos := projectRepos("acme/gadgets", "acme/widgets", "sjawhar/legion")
	credential := GitHubCredential(troubledTokens{}, installedEverywhere(repos), repos, ghrepo.Renames{}, installed, left, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	var trouble *TransientError
	if !errors.As(err, &trouble) {
		t.Fatalf("the implementer's bundle under GitHub's trouble = %v, %v; want the trouble, so the caller asks again", bundle, err)
	}
	if said.Len() != 0 {
		t.Errorf("the log holds a left-out line for a refusal that is GitHub's trouble:\n%s", said.String())
	}
	dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, left, []AppRole{Implement}, ghrepo.MustParse("sjawhar/legion"))
	for _, want := range []string{"acme", "sjawhar"} {
		if !slices.Contains(dirs, want) {
			t.Errorf("the directories a bundle would hold are %v, want %s among them: nothing is recorded left out", dirs, want)
		}
	}
}

// GitHubDirs answers from the record the last bundle of each App wrote, never from GitHub: the
// drift check reads it on every evaluation of every pod, so a call of its own would be one per
// pod per sweep. A pod's projection is its roles' bundles intersected, so an owner one of its
// Apps is not installed on is in no answer for it, and an App no bundle has been built for
// leaves the answer unknown — no set at all, which leaves every pod where it is.
func TestGitHubDirsAnswersFromWhatTheLastBundleOfEachAppRead(t *testing.T) {
	repos := projectRepos("sjawhar/legion", "acme/widgets")
	issue := ghrepo.MustParse("sjawhar/legion")
	installed := NewInstalled()
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, nil, Roles, issue); dirs != nil {
		t.Errorf("GitHubDirs before any bundle = %q, want none: nothing has been read for either App", dirs)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, nil, nil, Roles, issue); dirs != nil {
		t.Errorf("GitHubDirs with no record = %q, want none", dirs)
	}

	// The implement App is installed on both owners and the review App on sjawhar alone.
	implement := GitHubCredential(ownerTokens{}, installedOnOwners("acme", "sjawhar"), repos, ghrepo.Renames{}, installed, nil, quietBundleLog())
	if _, err := implement(context.Background(), claim.RoleImplementer, issue); err != nil {
		t.Fatalf("the implementer's bundle: %v", err)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, installed, nil, Roles, issue); dirs != nil {
		t.Errorf("GitHubDirs = %q, want none while no bundle of the review App has been built", dirs)
	}
	if want := []string{"acme", "sjawhar"}; !slices.Equal(GitHubDirs(repos, ghrepo.Renames{}, installed, nil, []AppRole{Implement}, issue), want) {
		t.Errorf("GitHubDirs for the implement App alone = %q, want %q",
			GitHubDirs(repos, ghrepo.Renames{}, installed, nil, []AppRole{Implement}, issue), want)
	}

	review := GitHubCredential(ownerTokens{}, installedOnOwners("sjawhar"), repos, ghrepo.Renames{}, installed, nil, quietBundleLog())
	if _, err := review(context.Background(), claim.RoleReviewer, issue); err != nil {
		t.Fatalf("the reviewer's bundle: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(GitHubDirs(repos, ghrepo.Renames{}, installed, nil, Roles, issue), want) {
		t.Errorf("GitHubDirs for a pod of both Apps = %q, want %q: an owner one App is not installed on is in no pod of both",
			GitHubDirs(repos, ghrepo.Renames{}, installed, nil, Roles, issue), want)
	}
	if want := []string{"sjawhar"}; !slices.Equal(GitHubDirs(repos, ghrepo.Renames{}, installed, nil, []AppRole{Review}, issue), want) {
		t.Errorf("GitHubDirs for the review App alone = %q, want %q",
			GitHubDirs(repos, ghrepo.Renames{}, installed, nil, []AppRole{Review}, issue), want)
	}
}
