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
// leases, and `<owner> read` refuses its read lease alone, which is what leaves a read-only
// directory out while the write lease of the same owner stands.
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

// A role's gh bundle holds every owner of the project: its App's write lease for the one
// repository its issue works, in that repository's owner's directory, which is the bundle's
// default, a directory of its own under that owner for every other repository the mapping serves
// there, each holding a lease narrowed to that repository alone, and one read-only lease per
// other owner over the repositories that owner serves. A repository outside the project's set
// still gets its write lease, since a role's repository is its issue's, from the daemon's own
// record. A call that names no repository writes to nothing: every directory is read-only, as
// the review App for a controller, and the default is the first owner sorted. A mint that fails
// fails the bundle, naming what it would have reached; a project with no owner at all and no
// repository of its own has no bundle.
func TestGitHubCredentialBundlesEveryOwnerOfTheProject(t *testing.T) {
	repos := projectRepos("sjawhar/legion", "Acme/widgets", "acme/gadgets")
	credential := GitHubCredential(ownerTokens{}, repos, ghrepo.Renames{}, nil, quietBundleLog())
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
				"acme":         "implement-token-for-acme",
				"acme/gadgets": "implement-read-for-acme",
				"sjawhar":      "implement-read-for-sjawhar",
			}, "implement"},
		{"a reviewer on sjawhar's only repository", claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion"), "sjawhar",
			map[string]string{
				"acme":    "review-read-for-acme",
				"sjawhar": "review-token-for-sjawhar",
			}, "review"},
		{"a merger on a repository outside the set", claim.RoleMerger, ghrepo.MustParse("Other/thing"), "other",
			map[string]string{
				"acme":    "implement-read-for-acme",
				"other":   "implement-token-for-other",
				"sjawhar": "implement-read-for-sjawhar",
			}, "implement"},
		{"the controller, which names no repository", claim.RoleController, ghrepo.Repository{}, "acme",
			map[string]string{
				"acme":    "review-read-for-acme",
				"sjawhar": "review-read-for-sjawhar",
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
				if rendered.App != tc.appName || rendered.Config != ghconfig.Config {
					t.Errorf("%s's render = App %q, config %q; want %q and the fixed config", dir, rendered.App, rendered.Config, tc.appName)
				}
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
			if dirs := GitHubDirs(repos, ghrepo.Renames{}, nil, Roles, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	refused := GitHubCredential(ownerTokens{refuse: "sjawhar"}, repos, ghrepo.Renames{}, nil, quietBundleLog())
	if _, err := refused(context.Background(), claim.RoleImplementer, ghrepo.MustParse("acme/widgets")); err == nil ||
		!strings.Contains(err.Error(), "sjawhar") || !strings.Contains(err.Error(), string(claim.RoleImplementer)) {
		t.Errorf("a refused read mint = %v, want an error naming the owner and the role", err)
	}
	none := GitHubCredential(ownerTokens{}, projectRepos(), ghrepo.Renames{}, nil, quietBundleLog())
	if _, err := none(context.Background(), claim.RoleController, ghrepo.Repository{}); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
	}
	if dirs := GitHubDirs(projectRepos(), ghrepo.Renames{}, nil, Roles, ghrepo.Repository{}); len(dirs) != 0 {
		t.Errorf("GitHubDirs of a project with no repository = %q, want none", dirs)
	}
}

// The issue's own owner serves the repository the role writes to and others beside it. Its
// directory holds the write token for the issue's repository alone; each sibling repository has a
// directory of its own whose token names that repository and no other; and another owner's one
// directory names that owner's repositories. No directory holds a token wider than its name.
func TestGitHubCredentialNarrowsEverySiblingRepositoryToItsOwnDirectory(t *testing.T) {
	var minted []string
	repos := projectRepos("cybermonklabs/legion", "theorem-labs/agent-infra", "theorem-labs/nats-server")
	credential := GitHubCredential(countingTokens{minted: &minted}, repos, ghrepo.Renames{}, nil, quietBundleLog())
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("theorem-labs/agent-infra"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	want := map[string]string{
		"theorem-labs":             "implement-token-for-theorem-labs",
		"theorem-labs/nats-server": "implement-read-for-theorem-labs",
		"cybermonklabs":            "implement-read-for-cybermonklabs",
	}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
	if bundle.Default != "theorem-labs" {
		t.Errorf("Default = %q, want theorem-labs, the issue's own owner's directory", bundle.Default)
	}
	// The issue's own repository has no directory of its own: its token is the write one, in the
	// owner's directory, and a second directory for it would hold a read-only token the role
	// would read in place of the write one.
	if _, held := bundle.Dirs["theorem-labs/agent-infra"]; held {
		t.Error("the bundle holds a directory for the issue's own repository beside its owner's")
	}
	if want := []string{"cybermonklabs", "theorem-labs", "theorem-labs/nats-server"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("directories = %q, want %q", bundle.DirNames(), want)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, nil, Roles, ghrepo.MustParse("theorem-labs/agent-infra")); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
	}
	if want := []string{"cybermonklabs read", "theorem-labs", "theorem-labs read"}; !slices.Equal(minted, want) {
		t.Errorf("minted for %v, want one lease per scope %v", minted, want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
}

// legionRename is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: one repository under two names, whose owners the bundle holds one entry for.
var legionRename = ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")

// A configured rename whose `to` the project owns puts the owner the repository moved from in
// the bundle, with no repository of its own: its directory is minted for the owner the
// repository moved to. Which lease that is turns on the issue's own repository. Where it is the
// moved one, the recorded owner's directory and the serving owner's hold one write lease,
// minted once for the repository as cybermonklabs holds it now, since a pod launched for an
// issue recorded on sjawhar before the transfer has to write the repository as GitHub holds it;
// where it is any other repository, or none, every directory of the pair is read-only, the
// alias owner's over the repositories the owner that serves the moved repository serves. Every
// owner outside the pair is minted for itself, as it always was.
//
// The repository directories are not the same pair twice over, and the two owners here are why:
// the project serves cybermonklabs/tools and no sjawhar/tools, so the name `sjawhar/tools`
// resolves to a repository the project does not serve and is no directory of the bundle. A
// directory of that name could only carry cybermonklabs/tools' token, which is another owner's
// repository: a `gh` command spelling sjawhar/tools would be answered with a token that cannot
// reach it. cybermonklabs/tools keeps its own directory, which is where that lease is read.
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
		// owner directories wherever one lease is theirs to share. A repository directory holds
		// no lease any other directory of this bundle shares, each of them naming the one
		// repository its own name resolves to.
		oneLease [][2]string
	}{
		{
			// The record still names sjawhar, so sjawhar's directory is the default and holds the
			// write token minted for cybermonklabs, the owner the repository moved to.
			name: "an implementer on the repository as it was recorded", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("sjawhar/legion"), def: "sjawhar", appName: "implement",
			tokens: map[string]string{
				"acme":                "implement-read-for-acme",
				"cybermonklabs":       "implement-token-for-cybermonklabs",
				"cybermonklabs/tools": "implement-read-for-cybermonklabs",
				"sjawhar":             "implement-token-for-cybermonklabs",
			},
			minted:   []string{"acme read", "cybermonklabs", "cybermonklabs read"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
		{
			// The record names the repository as GitHub holds it, so the write lease stands in
			// the cybermonklabs directory alone: sjawhar is neither the recorded owner nor the
			// owner serving the issue's repository, and a directory only the rename puts in the
			// bundle holds the read lease of the owner that serves the moved repository.
			name: "an implementer on the repository as GitHub holds it now", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("Cybermonklabs/legion"), def: "cybermonklabs", appName: "implement",
			tokens: map[string]string{
				"acme":                "implement-read-for-acme",
				"cybermonklabs":       "implement-token-for-cybermonklabs",
				"cybermonklabs/tools": "implement-read-for-cybermonklabs",
				"sjawhar":             "implement-read-for-cybermonklabs",
			},
			minted: []string{"acme read", "cybermonklabs", "cybermonklabs read"},
		},
		{
			name: "an implementer on another owner of the project", role: claim.RoleImplementer,
			repo: ghrepo.MustParse("acme/widgets"), def: "acme", appName: "implement",
			tokens: map[string]string{
				"acme":          "implement-token-for-acme",
				"cybermonklabs": "implement-read-for-cybermonklabs",
				"sjawhar":       "implement-read-for-cybermonklabs",
			},
			minted:   []string{"acme", "cybermonklabs read"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
		{
			name: "the controller, which writes to nothing", role: claim.RoleController,
			repo: ghrepo.Repository{}, def: "acme", appName: "review",
			tokens: map[string]string{
				"acme":          "review-read-for-acme",
				"cybermonklabs": "review-read-for-cybermonklabs",
				"sjawhar":       "review-read-for-cybermonklabs",
			},
			minted:   []string{"acme read", "cybermonklabs read"},
			oneLease: [][2]string{{"sjawhar", "cybermonklabs"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted []string
			renames := ghrepo.MustRenames(legionRename)
			credential := GitHubCredential(countingTokens{minted: &minted}, repos, renames, nil, quietBundleLog())
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
			// Every pair of directories that shares a lease holds the one entry, minted once:
			// the pair's two owner directories where one lease is theirs to share, and the two
			// repository directories under them where the bundle has any.
			for _, pair := range tc.oneLease {
				from, to := bundle.Dirs[pair[0]], bundle.Dirs[pair[1]]
				if _, held := bundle.Dirs[pair[0]]; !held {
					continue
				}
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
			if dirs := GitHubDirs(repos, renames, nil, Roles, tc.repo); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
		})
	}

	// With no pair configured, the owner the repository moved from is an owner like any other:
	// its own read-only lease, minted for itself, as today.
	t.Run("no pair configured", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.Renames{}, nil, quietBundleLog())
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("cybermonklabs/legion"))
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{
			"cybermonklabs": "implement-token-for-cybermonklabs",
			"sjawhar":       "implement-read-for-sjawhar",
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) || bundle.Default != "cybermonklabs" {
			t.Errorf("Default %q and tokens %v, want cybermonklabs and %v", bundle.Default, got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs", "sjawhar read"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
	})

	// Where the project serves the pair's `from` as well as its `to`, the pair resolves for
	// nobody (ghrepo.Renames.Serving: a name the project serves under its own name is a
	// repository of its own, somebody having made one at the old location, which deletes
	// GitHub's redirect). So the owner the repository moved from is no alias, its own entry
	// wins — its repositories are the project's and its token is its own — and nothing is
	// warned of anywhere: the rule decides it per repository rather than leaving two owners to
	// argue over one directory.
	t.Run("the owner it moved from is an owner of the project under the pair's own from", func(t *testing.T) {
		var minted []string
		both := projectRepos("Cybermonklabs/legion", "sjawhar/legion")
		credential := GitHubCredential(countingTokens{minted: &minted}, both, ghrepo.MustRenames(legionRename), nil, quietBundleLog())
		bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("cybermonklabs/legion"))
		if err != nil {
			t.Fatalf("credential: %v", err)
		}
		want := map[string]string{
			"cybermonklabs": "implement-token-for-cybermonklabs",
			"sjawhar":       "implement-read-for-sjawhar",
		}
		if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
			t.Errorf("tokens = %v, want %v: each owner keeps its own token", got, want)
		}
		if !slices.Equal(minted, []string{"cybermonklabs", "sjawhar read"}) {
			t.Errorf("minted for %v, want each owner its own lease", minted)
		}
		if _, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("sjawhar/legion")); err != nil {
			t.Fatalf("credential: %v", err)
		}
	})
}

// A resolving pair moves one repository, never an owner: the write lease stands in the directory
// named for the issue's recorded owner and the one named for the owner whose installation serves
// that repository, and in no other, so every directory named for an owner the project serves
// repositories of under its own name keeps that owner's own read lease over that owner's own
// repositories.
//
// Both cases are the state production was in on 2026-11-06: the pair sjawhar/legion ->
// cybermonklabs/legion resolving while the project also serves sjawhar/oh-my-pi. Redirecting
// every directory of the pair to the write lease, as the bundle did while an alias owner's
// directory took its mint owner's lease whatever the issue's repository was, put a write lease
// for sjawhar/oh-my-pi in the directory named cybermonklabs, and left sjawhar/oh-my-pi reachable
// from no directory at all for an issue on cybermonklabs/legion. Neither case may change a
// directory name, which is why each asserts the whole set against GitHubDirs as well: a pod's gh
// projection is fixed when the pod is created, so a name this rule added or dropped would be
// drift the sweep answers with an update round over every worker in the pod.
//
// Two further cases are that state with a third repository of the old owner served, which is
// where composing the pair with the per-repository directories took reach away: a write lease
// names one repository, so the old owner's directory reaches nothing else of that owner's own,
// and where the pair resolves the owner serving the role's repository is not the owner the
// issue is recorded on, so the serving clause names none of the recorded owner's repositories.
// So each repository the issue's recorded owner serves beside the one the role writes to holds
// a directory of its own, read-only and minted for that owner (bundleDirs' recorded clause),
// and every case here asserts as well that no directory outside the two a write lease may
// stand in holds a writing token.
//
// Where the project serves the role's own repository, the owner whose installation serves it
// is an owner the project serves under its own name, so mintOwner answers that owner itself
// and the serving clause cuts that owner's repositories. Four cases here pinned repository
// directories that clause cut for the pair's `to` instead, while mintOwner redirected a served
// owner: each of those directories named a repository the `to`'s own directory already reads,
// so their going is the fault going and not reach going, and every repository the project
// serves is still named by the lease of some directory of the bundle.
//
// One of those cases is that state with the tree recorded on the pair's `from`, which is the
// tree the failure stopped: it is pinned here so that no later change can re-break the case
// this rule exists for, and it may not be deleted.
//
// The last four cases are the rule each repository directory is held to, whichever cut named
// it: the directory holds the lease of the repository its own name resolves to, and exists for
// no other. Three of them are the state where both owners serve a repository called tools, and
// they differ in which cut names sjawhar/tools and what the name means there: the recorded
// clause names it for an issue recorded on the pair's `from`, where it holds sjawhar's own
// tools, the repository the project serves under that name; no cut names it for an issue on
// the moved repository, where the sjawhar directory's own read lease reaches that repository;
// and no cut names it for an issue on sjawhar's own tools, where the sjawhar directory's write
// lease is that repository's. cybermonklabs/tools stays reachable in all three, so one
// directory name never answers for two repositories. The fourth is the two owners serving
// repositories of different names, where the serving clause names sjawhar/widgets and that
// name resolves to a repository the project does not serve. In each of them a directory named
// for one owner would otherwise have answered with another owner's repository, which is a
// token that cannot reach what a `gh` command spelling the name asked for.
func TestEveryServedOwnerKeepsItsOwnReadLeaseWhileAPairResolves(t *testing.T) {
	base := projectRepos("Cybermonklabs/legion", "acme/widgets", "sjawhar/oh-my-pi")
	renames := ghrepo.MustRenames(legionRename)
	for _, tc := range []struct {
		name string
		// served is the project's repositories where they are not the table's own three.
		served []string
		repo   ghrepo.Repository
		dirs   []string
		tokens map[string]string
		minted []string
		// writing are the directories R3's writing clause names, the only ones a write token
		// may stand in: the issue's recorded owner's, and that of the owner whose installation
		// serves the repository the role writes to.
		writing []string
	}{
		{
			// sjawhar's own repository is the issue's, so the sjawhar directory holds its write
			// lease — and the cybermonklabs directory holds cybermonklabs's own read lease, not
			// a write lease for a repository of another owner. The repository directories are
			// cut for sjawhar, the owner whose installation serves the role's repository, and
			// sjawhar serves nothing beside the repository that write lease names, so the
			// bundle holds none: the cybermonklabs/legion and sjawhar/legion directories this
			// case pinned while mintOwner redirected a served owner were the fault and not
			// reach, cybermonklabs' own directory naming that repository already.
			name: "an issue on the old owner's own other repository", repo: ghrepo.MustParse("sjawhar/oh-my-pi"),
			dirs: []string{"acme", "cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"acme":          "implement-read-for-acme",
				"cybermonklabs": "implement-read-for-cybermonklabs",
				"sjawhar":       "implement-token-for-sjawhar",
			},
			minted:  []string{"acme read", "cybermonklabs read", "sjawhar"},
			writing: []string{"sjawhar"},
		},
		{
			// The issue is the moved repository's, recorded under the name GitHub has, so the
			// write lease stands in the cybermonklabs directory alone and the sjawhar directory
			// reads sjawhar's own repositories: the project's own sjawhar/oh-my-pi is reachable.
			name: "an issue on the repository the pair moved", repo: ghrepo.MustParse("cybermonklabs/legion"),
			dirs: []string{"acme", "cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"acme":          "implement-read-for-acme",
				"cybermonklabs": "implement-token-for-cybermonklabs",
				"sjawhar":       "implement-read-for-sjawhar",
			},
			minted:  []string{"acme read", "cybermonklabs", "sjawhar read"},
			writing: []string{"cybermonklabs"},
		},
		{
			// The production failure of 2026-11-06, which is why this case may not be deleted.
			// The pair resolving while the old owner serves one other repository is the state
			// adding sjawhar/oh-my-pi to the project's mapping put production in, and the owner
			// test the rename rule replaced read that one repository as the old owner being the
			// project's own, stopped resolving the pair, and left 47 trees unable to push. A
			// tree recorded on the pair's `from` is such a tree: its recorded owner's directory
			// and the directory of the owner whose installation serves the repository now hold
			// the one write lease, minted for cybermonklabs, so a role writes the repository
			// GitHub holds through either name — and sjawhar/oh-my-pi, which the recorded owner
			// serves beside it, keeps a read-only directory of its own.
			name: "an issue recorded on the pair's from while the old owner serves one other repository",
			repo: ghrepo.MustParse("sjawhar/legion"),
			dirs: []string{"acme", "cybermonklabs", "sjawhar", "sjawhar/oh-my-pi"},
			tokens: map[string]string{
				"acme":             "implement-read-for-acme",
				"cybermonklabs":    "implement-token-for-cybermonklabs",
				"sjawhar":          "implement-token-for-cybermonklabs",
				"sjawhar/oh-my-pi": "implement-read-for-sjawhar",
			},
			minted:  []string{"acme read", "cybermonklabs", "sjawhar read"},
			writing: []string{"cybermonklabs", "sjawhar"},
		},
		{
			// The old owner serves a third repository beside the issue's own. The write lease
			// in its directory names the issue's repository alone, so the third one holds a
			// directory of its own, read-only and minted for the old owner over it alone: the
			// repository directories are cut for sjawhar, the owner whose installation serves
			// the role's repository, whatever the pair moved away from that owner.
			//
			// The cybermonklabs/legion and sjawhar/legion directories this case pinned while
			// mintOwner redirected a served owner were the fault and not reach: both named
			// cybermonklabs/legion, which the cybermonklabs directory's own read lease already
			// holds.
			name:   "an issue on the old owner's other repository while it serves a third",
			served: []string{"Cybermonklabs/legion", "acme/widgets", "sjawhar/oh-my-pi", "sjawhar/tools"},
			repo:   ghrepo.MustParse("sjawhar/oh-my-pi"),
			dirs:   []string{"acme", "cybermonklabs", "sjawhar", "sjawhar/tools"},
			tokens: map[string]string{
				"acme":          "implement-read-for-acme",
				"cybermonklabs": "implement-read-for-cybermonklabs",
				"sjawhar":       "implement-token-for-sjawhar",
				"sjawhar/tools": "implement-read-for-sjawhar",
			},
			minted:  []string{"acme read", "cybermonklabs read", "sjawhar", "sjawhar read"},
			writing: []string{"sjawhar"},
		},
		{
			// The issue is the moved repository's, recorded under the name the old owner had,
			// so the old owner's directory holds the write lease for the repository the pair's
			// `to` serves and reaches neither of the old owner's own two. Each of those holds a
			// directory of its own, minted for the old owner, while the sjawhar directory keeps
			// the cybermonklabs write lease, which is the one entry the cybermonklabs directory
			// holds too.
			name:   "an issue on the moved repository as the record names it, the old owner serving two",
			served: []string{"Cybermonklabs/legion", "acme/widgets", "sjawhar/oh-my-pi", "sjawhar/tools"},
			repo:   ghrepo.MustParse("sjawhar/legion"),
			dirs:   []string{"acme", "cybermonklabs", "sjawhar", "sjawhar/oh-my-pi", "sjawhar/tools"},
			tokens: map[string]string{
				"acme":             "implement-read-for-acme",
				"cybermonklabs":    "implement-token-for-cybermonklabs",
				"sjawhar":          "implement-token-for-cybermonklabs",
				"sjawhar/oh-my-pi": "implement-read-for-sjawhar",
				"sjawhar/tools":    "implement-read-for-sjawhar",
			},
			minted:  []string{"acme read", "cybermonklabs", "sjawhar read"},
			writing: []string{"cybermonklabs", "sjawhar"},
		},
		{
			// Both owners serve a repository called tools. The serving clause cuts the
			// repository directories for cybermonklabs, the owner that serves the role's
			// repository, and the recorded clause cuts sjawhar's own beside the one the write
			// lease names, which is where sjawhar/tools comes from. R3 gives that directory
			// the lease of the repository its own name resolves to — sjawhar's own tools,
			// which the project serves under that name — and cybermonklabs/tools stays
			// reachable through the directory named for it, so one directory name never
			// answers for two repositories.
			name:   "an issue recorded on the pair's from while both owners serve a repository of one name",
			served: []string{"Cybermonklabs/legion", "cybermonklabs/tools", "sjawhar/tools"},
			repo:   ghrepo.MustParse("sjawhar/legion"),
			dirs:   []string{"cybermonklabs", "cybermonklabs/tools", "sjawhar", "sjawhar/tools"},
			tokens: map[string]string{
				"cybermonklabs":       "implement-token-for-cybermonklabs",
				"cybermonklabs/tools": "implement-read-for-cybermonklabs",
				"sjawhar":             "implement-token-for-cybermonklabs",
				"sjawhar/tools":       "implement-read-for-sjawhar",
			},
			minted:  []string{"cybermonklabs", "cybermonklabs read", "sjawhar read"},
			writing: []string{"cybermonklabs", "sjawhar"},
		},
		{
			// The same collide state with the issue recorded on the moved repository under the
			// name GitHub has. The sjawhar directory writes nothing here, so it keeps its own
			// read lease over sjawhar's tools, and the repository directories are cut for
			// cybermonklabs, the owner serving the role's repository: cybermonklabs/tools is
			// the one name they produce, the role's own repository needing none. The
			// sjawhar/tools directory this case pinned while mintOwner redirected a served
			// owner was the fault and not reach: it held the lease the sjawhar directory above
			// it already holds.
			name:   "an issue on the moved repository while both owners serve a repository of one name",
			served: []string{"Cybermonklabs/legion", "cybermonklabs/tools", "sjawhar/tools"},
			repo:   ghrepo.MustParse("cybermonklabs/legion"),
			dirs:   []string{"cybermonklabs", "cybermonklabs/tools", "sjawhar"},
			tokens: map[string]string{
				"cybermonklabs":       "implement-token-for-cybermonklabs",
				"cybermonklabs/tools": "implement-read-for-cybermonklabs",
				"sjawhar":             "implement-read-for-sjawhar",
			},
			minted:  []string{"cybermonklabs", "cybermonklabs read", "sjawhar read"},
			writing: []string{"cybermonklabs"},
		},
		{
			// The same collide state with the issue recorded on the old owner's own tools,
			// which that owner serves under its own name. The repository directories are cut
			// for sjawhar, the owner serving the role's repository, and the one repository
			// sjawhar serves is the one its write lease names, so the bundle holds no
			// repository directory at all: a read-only directory beside the owner's own would
			// stand for a repository the directory above it reaches more widely, and
			// cybermonklabs' directory reads both of that owner's repositories.
			//
			// The cybermonklabs/legion, cybermonklabs/tools and sjawhar/legion directories
			// this case pinned while mintOwner redirected a served owner were the fault and
			// not reach: the clause cut cybermonklabs' repositories because mintOwner answered
			// cybermonklabs for sjawhar, the owner whose installation serves the role's own
			// repository, and the cybermonklabs directory's own read lease names both of them.
			name:   "an issue on the old owner's own repository of the colliding name",
			served: []string{"Cybermonklabs/legion", "cybermonklabs/tools", "sjawhar/tools"},
			repo:   ghrepo.MustParse("sjawhar/tools"),
			dirs:   []string{"cybermonklabs", "sjawhar"},
			tokens: map[string]string{
				"cybermonklabs": "implement-read-for-cybermonklabs",
				"sjawhar":       "implement-token-for-sjawhar",
			},
			minted:  []string{"cybermonklabs read", "sjawhar"},
			writing: []string{"sjawhar"},
		},
		{
			// The two owners' repository-name sets differ: the project serves cybermonklabs'
			// widgets and nothing of sjawhar's but the moved repository. The serving clause
			// names sjawhar/widgets, since the repository directories are cut over the names
			// the owner serving the role's repository holds, and that name resolves to
			// sjawhar/widgets, which the project does not serve: no lease of this bundle may
			// hold it, so the bundle holds no such directory. A directory of that name could
			// only carry cybermonklabs/widgets' token, and a `gh` command spelling
			// sjawhar/widgets means a repository that token cannot reach. cybermonklabs/widgets
			// keeps its own directory, and the pair's own two owner directories are untouched.
			name:   "an issue recorded on the pair's from while the two owners serve different names",
			served: []string{"Cybermonklabs/legion", "cybermonklabs/widgets"},
			repo:   ghrepo.MustParse("sjawhar/legion"),
			dirs:   []string{"cybermonklabs", "cybermonklabs/widgets", "sjawhar"},
			tokens: map[string]string{
				"cybermonklabs":         "implement-token-for-cybermonklabs",
				"cybermonklabs/widgets": "implement-read-for-cybermonklabs",
				"sjawhar":               "implement-token-for-cybermonklabs",
			},
			minted:  []string{"cybermonklabs", "cybermonklabs read"},
			writing: []string{"cybermonklabs", "sjawhar"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repos := base
			if tc.served != nil {
				repos = projectRepos(tc.served...)
			}
			var minted []string
			credential := GitHubCredential(countingTokens{minted: &minted}, repos, renames, nil, quietBundleLog())
			bundle, err := credential(context.Background(), claim.RoleImplementer, tc.repo)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			tokens := bundleTokens(t, bundle)
			if !maps.Equal(tokens, tc.tokens) {
				t.Errorf("tokens = %v, want %v", tokens, tc.tokens)
			}
			// The write lease stands in the directories R3's writing clause names and in no
			// other: a repository directory the recorded clause adds is read-only, as is every
			// directory of an owner the issue is not recorded on.
			for dir, token := range tokens {
				if strings.Contains(token, "-token-for-") != slices.Contains(tc.writing, dir) {
					t.Errorf("%s holds %q; the writing token stands in %q and nowhere else", dir, token, tc.writing)
				}
			}
			if !slices.Equal(minted, tc.minted) {
				t.Errorf("minted for %v, want %v: one lease per scope, each owner's own", minted, tc.minted)
			}
			if got := bundle.DirNames(); !slices.Equal(got, tc.dirs) {
				t.Errorf("the bundle's directories are %q, want %q: this rule renames none", got, tc.dirs)
			}
			if dirs := GitHubDirs(repos, renames, nil, Roles, tc.repo); !slices.Equal(dirs, tc.dirs) {
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

// An owner the project serves repositories of under its own name is minted for itself, so the
// repository directories are cut for the owner whose installation serves the role's own
// repository even where a second pair moved one of that owner's repositories on to a third
// owner.
//
// This catches mintOwner redirecting that owner: the directories would be cut over
// theorem-labs' repositories, which theorem-labs' own directory already reads, and
// cybermonklabs/tools would be named by no lease of the bundle at all — `legion gh` and git for
// it would fall back to the cybermonklabs directory, whose write lease names
// cybermonklabs/legion alone, and GitHub would refuse them.
func TestASecondPairDoesNotRedirectTheOwnerThatServesTheRolesRepository(t *testing.T) {
	served := []string{"Cybermonklabs/legion", "cybermonklabs/tools", "theorem-labs/agent-infra", "theorem-labs/nats-server"}
	repos := projectRepos(served...)
	recorded := ghrepo.MustParse("sjawhar/legion")
	credential := GitHubCredential(ownerTokens{}, repos, chainedRenames, nil, quietBundleLog())
	bundle, err := credential(context.Background(), claim.RoleImplementer, recorded)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	tokens := bundleTokens(t, bundle)
	want := map[string]string{
		"cybermonklabs":       "implement-token-for-cybermonklabs",
		"cybermonklabs/tools": "implement-read-for-cybermonklabs",
		"sjawhar":             "implement-token-for-cybermonklabs",
		"theorem-labs":        "implement-read-for-theorem-labs",
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
	if dirs := GitHubDirs(repos, chainedRenames, nil, Roles, recorded); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want the bundle's own %q, which is what a running pod is compared against", dirs, bundle.DirNames())
	}
}

// ControllerCredential is one read-only directory per owner, each narrowed to the repositories
// that owner serves and holding no repository directory of its own: the controller writes to
// none. Its default is the owner it is asked for — an alias owner included, which its launch may
// have recorded — or the first owner sorted when it is asked for none. An owner outside the
// project is refused before anything is minted, and a project with no owner at all has no bundle.
func TestControllerCredentialIsReadOnlyForEveryOwner(t *testing.T) {
	repos := projectRepos("Cybermonklabs/legion", "cybermonklabs/tools", "acme/widgets")
	renames := ghrepo.MustRenames(legionRename)
	want := map[string]string{
		"acme":          "review-read-for-acme",
		"cybermonklabs": "review-read-for-cybermonklabs",
		"sjawhar":       "review-read-for-cybermonklabs",
	}
	for asked, def := range map[string]string{"": "acme", "Cybermonklabs": "cybermonklabs", "Sjawhar": "sjawhar"} {
		bundle, err := ControllerCredential(ownerTokens{}, repos, renames, nil, quietBundleLog())(context.Background(), asked)
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
	outside := ControllerCredential(countingTokens{minted: &minted}, repos, renames, nil, quietBundleLog())
	if _, err := outside(context.Background(), "Outside-Project"); !errors.Is(err, ErrOwnerOutsideProject) ||
		!strings.Contains(err.Error(), `"outside-project"`) || len(minted) != 0 {
		t.Errorf("the controller asking for an owner outside the project = %v, minted %q; want ErrOwnerOutsideProject naming it and no mint", err, minted)
	}
	none := ControllerCredential(ownerTokens{}, projectRepos(), ghrepo.Renames{}, nil, quietBundleLog())
	if _, err := none(context.Background(), ""); !errors.Is(err, runtime.ErrNoGitHubOwner) {
		t.Errorf("a controller bundle with no owner = %v, want runtime.ErrNoGitHubOwner", err)
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

// GitHub answers 422 to a token request its installation cannot serve and mints nothing, so the
// read lease of one owner is refused whole. The refusal costs that owner's directory alone: the
// lease is asked for again, once for each repository the directory was built to serve, and the
// bundle is served without that directory rather than the role's launch failing for every
// owner. Each repository the directory would have served is said once on the log.
func TestARefusedReadLeaseCostsItsOwnersDirectoryAndNotTheBundle(t *testing.T) {
	var minted []string
	log, said := bundleLog()
	repos := projectRepos("sjawhar/legion", "acme/widgets", "acme/gadgets")
	tokens := countingTokens{ownerTokens: ownerTokens{unreachable: map[string]bool{"acme read": true}}, minted: &minted}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

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
	// The refused lease, then the ask made again for each repository the directory would have
	// served, and the write lease, which this owner's refusal costs nothing.
	if asked := []string{"acme read", "acme read", "acme read", "sjawhar"}; !slices.Equal(minted, asked) {
		t.Errorf("minted for %v, want %v", minted, asked)
	}
	for _, named := range []string{"repository=gadgets", "repository=widgets"} {
		if count := strings.Count(said.String(), named); count != 1 {
			t.Errorf("the log holds %d lines naming %q, want one:\n%s", count, named, said.String())
		}
	}
	for _, named := range []string{"app=implement", "owner=acme"} {
		if !strings.Contains(said.String(), named) {
			t.Errorf("the log line does not name %q:\n%s", named, said.String())
		}
	}
}

// A sibling repository of the role's own owner holds a directory of its own, over that repository
// alone, so a refusal of its lease is a refusal of everything that directory could hold: it is
// left out of the bundle, and every other directory — the write lease among them — is minted and
// served.
func TestGitHubCredentialLeavesOutASiblingRepositoryGitHubRefuses(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("theorem-labs/agent-infra", "theorem-labs/nats-server", "cybermonklabs/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"theorem-labs read": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("theorem-labs/agent-infra"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	want := map[string]string{
		"cybermonklabs": "implement-read-for-cybermonklabs",
		"theorem-labs":  "implement-token-for-theorem-labs",
	}
	if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
		t.Errorf("tokens = %v, want %v: the refused repository's directory is left out and the rest whole", got, want)
	}
	if _, held := bundle.Dirs["theorem-labs/nats-server"]; held {
		t.Error("the bundle holds a directory for a repository the installation does not reach")
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	if count := strings.Count(said.String(), "repository=nats-server"); count != 1 {
		t.Errorf("the log holds %d lines naming the refused repository, want one:\n%s", count, said.String())
	}
}

// The write lease is not softened: a role that cannot act on its own issue's repository has
// nothing to do, so a refusal of it fails the whole bundle, naming the role, the App and the
// repository the lease would have reached.
func TestGitHubCredentialStillFailsOnARefusedWriteLease(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("acme/widgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"acme": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

	bundle, err := credential(context.Background(), claim.RoleReviewer, ghrepo.MustParse("acme/widgets"))
	if !errors.Is(err, ErrNarrowingRefused) || len(bundle.Dirs) != 0 {
		t.Fatalf("a refused write lease = %+v, %v; want no bundle and the refusal", bundle, err)
	}
	for _, named := range []string{"review", string(claim.RoleReviewer), "acme/widgets"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %q: %v", named, err)
		}
	}
	if said.Len() != 0 {
		t.Errorf("the log holds %q, want nothing: a refused write lease is the bundle's failure, not a repository left out", said.String())
	}
}

// A repository GitHub refuses is refused at every launch and every refresh of every role, so the
// line is said once for the life of the process rather than at each of them, as a Manager says a
// grant shortfall once.
func TestGitHubCredentialSaysARefusedRepositoryOnceOverTwoBundles(t *testing.T) {
	log, said := bundleLog()
	repos := projectRepos("oncelabs/served", "oncelabs/twice")
	tokens := ownerTokens{unreachable: map[string]bool{"oncelabs read": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, nil, log)

	for i := range 2 {
		if _, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("other/thing")); err != nil {
			t.Fatalf("bundle %d: %v", i+1, err)
		}
	}
	if count := strings.Count(said.String(), "repository=twice"); count != 1 {
		t.Errorf("the log holds %d lines over two bundles, want one:\n%s", count, said.String())
	}
}

// gh keeps an owner's own hosts.yml and config.yml in that owner's directory, so a repository of
// either name has no directory of its own to hold its lease: writing one would make a directory
// where a file of the bundle's is (ghconfig.IsDirName). That repository is left out, said once on
// the log, and every other directory of the bundle is written.
func TestGitHubCredentialLeavesOutARepositoryNamedLikeGhsOwnFiles(t *testing.T) {
	for owner, named := range map[string]string{"hostlabs": ghconfig.HostsFile, "conflabs": ghconfig.ConfigFile} {
		t.Run(named, func(t *testing.T) {
			log, said := bundleLog()
			repos := projectRepos(owner+"/legion", owner+"/"+named)
			credential := GitHubCredential(ownerTokens{}, repos, ghrepo.Renames{}, nil, log)

			bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse(owner+"/legion"))
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			want := map[string]string{owner: "implement-token-for-" + owner}
			if got := bundleTokens(t, bundle); !maps.Equal(got, want) {
				t.Errorf("tokens = %v, want %v: the owner's own directory alone", got, want)
			}
			if err := bundle.Validate(); err != nil {
				t.Errorf("the bundle does not validate: %v", err)
			}
			if dirs := GitHubDirs(repos, ghrepo.Renames{}, nil, Roles, ghrepo.MustParse(owner+"/legion")); !slices.Equal(dirs, bundle.DirNames()) {
				t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
			}
			if count := strings.Count(said.String(), "repository="+named); count != 1 {
				t.Errorf("the log holds %d lines naming the repository left out, want one:\n%s", count, said.String())
			}
		})
	}
}

// A bundle's default is the directory its GH_CONFIG_DIR names, so it is chosen from the owners
// the mint answered for and not from the mapping: a caller that names no repository of its own —
// a tmux tree pane whose claim records none, every controller launch — would otherwise take the
// first owner sorted, and where GitHub refuses a lease over every repository that owner serves
// the bundle would come back without the one directory it is served from, which
// ghconfig.Bundle.Validate refuses long after the ask that could say why.
func TestABundleTakesItsDefaultFromTheOwnersTheMintAnsweredFor(t *testing.T) {
	repos := projectRepos("acme/widgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"acme": true}}
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, NewLeftOut(), quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.Repository{})
	if err != nil {
		t.Fatalf("a bundle whose first owner is refused: %v", err)
	}
	if bundle.Default != "sjawhar" {
		t.Errorf("the default is %q, want sjawhar: acme answered for no repository", bundle.Default)
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
// served another owner's directory instead, so an owner GitHub refuses every lease of is
// ErrNarrowingRefused, naming the owner and the repositories the lease would have reached.
func TestABundleRefusesADefaultOwnerGitHubRefusesEveryLeaseOf(t *testing.T) {
	repos := projectRepos("acme/widgets", "acme/gadgets", "sjawhar/legion")
	tokens := ownerTokens{unreachable: map[string]bool{"acme": true}}
	left := NewLeftOut()

	controller := ControllerCredential(tokens, repos, ghrepo.Renames{}, left, quietBundleLog())
	bundle, err := controller(context.Background(), "Acme")
	if !errors.Is(err, ErrNarrowingRefused) || len(bundle.Dirs) != 0 {
		t.Fatalf("the controller's bundle on a refused default owner = %+v, %v; want no bundle and ErrNarrowingRefused", bundle, err)
	}
	for _, named := range []string{"acme", "gadgets", "widgets"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %q: %v", named, err)
		}
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
// the bundle — the write lease, the launch and the controller's own pod — waits on none of it
// (INFRA-513).
func TestADirectoryGitHubRefusesIsLeftOutAndHeldAgainOnceGitHubAnswers(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	// Whether the account grants the App what a read lease asks for: it approves the grant
	// part-way through.
	grants := false
	github := &keyModeGitHub{owners: []string{"acme", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		if !grants && body.Permissions["contents"] == "read" {
			return http.StatusUnprocessableEntity, refusedNarrowingAnswer
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "gadgets", "widgets")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	repos := projectRepos("acme/gadgets", "acme/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, NewLeftOut(), quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle while acme's read lease is refused: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Fatalf("the bundle holds %q, want %q: the refused owner's directory is left out", bundle.DirNames(), want)
	}
	// The refused read lease and the write lease: the asks made again for each of acme's
	// repositories are that same scope, which the Manager answers from the refusal it remembers.
	if asked := github.exchanges(); len(asked) != 2 {
		t.Fatalf("%d exchanges, want the refused read lease and the write lease", len(asked))
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
		t.Fatalf("the bundle once the account granted the read lease: %v", err)
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
	// Whether the App's installation on cachedlabs mints what a read lease asks for: the
	// account drops the App part-way through.
	reaches := true
	github := &keyModeGitHub{owners: []string{"cachedlabs", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		if !reaches && body.Permissions["contents"] == "read" {
			return http.StatusUnprocessableEntity, refusedNarrowingAnswer
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "gadgets", "widgets")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	repos := projectRepos("cachedlabs/gadgets", "cachedlabs/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, NewLeftOut(), quietBundleLog())

	// An earlier bundle minted cachedlabs' read lease, and the Manager holds it for the hour it
	// lasts.
	cached, err := tokens.ReadToken(context.Background(), Implement, "cachedlabs")
	if err != nil {
		t.Fatalf("the read lease an earlier bundle minted for cachedlabs: %v", err)
	}

	reaches = false
	now = now.Add(time.Minute)
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the account dropped the App from cachedlabs: %v", err)
	}
	if got := bundleTokens(t, bundle)["cachedlabs"]; got != cached.Token {
		t.Errorf("cachedlabs' directory holds %q, want the cached lease %q", got, cached.Token)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}

	// Fewer than twenty minutes are left on that lease, so the next bundle asks GitHub, which
	// refuses: the directory goes, and the write lease and the bundle stand.
	now = now.Add(40 * time.Minute)
	bundle, err = credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the cached lease neared its expiry: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q: the directory goes with the lease that stood for it", bundle.DirNames(), want)
	}
}

// A directory left out is said on the log once for each repository it was built to serve, and
// recorded left out (LeftOut), so the drift check's own answer names what a bundle built now
// holds and no pod is replaced for a directory no mint produces. The rest of the bundle — the
// write lease among it — is minted and served.
func TestEveryRepositoryOfADirectoryLeftOutIsSaidOnceOnTheLog(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"gonelabs", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		if body.Permissions["contents"] == "read" {
			return http.StatusUnprocessableEntity, refusedNarrowingAnswer
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	log, said := bundleLog()
	repos := projectRepos("gonelabs/gadgets", "gonelabs/widgets", "sjawhar/legion")
	left := NewLeftOut()
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, left, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle once the App's installation refuses an owner: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q: the owner's directory left out and the write lease whole", bundle.DirNames(), want)
	}
	if err := bundle.Validate(); err != nil {
		t.Errorf("the bundle does not validate: %v", err)
	}
	if dirs := GitHubDirs(repos, ghrepo.Renames{}, left, Roles, ghrepo.MustParse("sjawhar/legion")); !slices.Equal(dirs, bundle.DirNames()) {
		t.Errorf("GitHubDirs = %q, want the bundle's own directories %q", dirs, bundle.DirNames())
	}
	for _, named := range []string{"repository=gadgets", "repository=widgets"} {
		if count := strings.Count(said.String(), named); count != 1 {
			t.Errorf("the log holds %d lines naming %q, want one:\n%s", count, named, said.String())
		}
	}
	if !strings.Contains(said.String(), "owner=gonelabs") {
		t.Errorf("the log line does not name the owner:\n%s", said.String())
	}
}

// One ask decides a directory. The bundle asks the owner's lease once, and a refusal leaves the
// directory out: the asks it makes again, one for each repository the directory was built to
// serve, are that same scope, which the Manager answers from the refusal it remembers rather
// than from GitHub (narrowingRefusalWindow). So a refused owner costs one exchange, where an
// ask per repository would cost one for each, and a rate limit on those asks would fail every
// bundle of the project.
func TestOneRefusedAskDecidesADirectoryAndGitHubIsAskedNoMore(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"pairlabs", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		if body.Permissions["contents"] == "read" {
			return http.StatusUnprocessableEntity, refusedNarrowingAnswer
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	repos := projectRepos("pairlabs/gadgets", "pairlabs/widgets", "sjawhar/legion")
	credential := GitHubCredential(tokens, repos, ghrepo.Renames{}, NewLeftOut(), quietBundleLog())

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("the implementer's bundle against an installation that refuses an owner: %v", err)
	}
	if want := []string{"sjawhar"}; !slices.Equal(bundle.DirNames(), want) {
		t.Errorf("the bundle holds %q, want %q", bundle.DirNames(), want)
	}
	if asked := github.exchanges(); len(asked) != 2 {
		t.Fatalf("%d exchanges, want the one refused read lease and the write lease", len(asked))
	}
	if _, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion")); err != nil {
		t.Fatalf("the second bundle: %v", err)
	}
	if asked := github.exchanges(); len(asked) != 2 {
		t.Errorf("%d exchanges over two bundles, want none added: the refusal and the write lease are both served from the Manager", len(asked))
	}
}

// troubledTokens answers every read with GitHub's trouble that also reads as a narrowing
// refusal, which a rate-limited 422 is: the lease a caller is refused that way says nothing
// about what the account grants the App's installation.
type troubledTokens struct{ ownerTokens }

func (troubledTokens) ReadToken(context.Context, AppRole, string) (Lease, error) {
	return Lease{}, &TransientError{Err: fmt.Errorf("%w: GitHub App token exchange failed (422)", ErrNarrowingRefused)}
}

// A read refused with GitHub's own trouble leaves no directory out, whatever else the refusal
// reads as: the bundle fails, so the caller tries again, where leaving a directory out would
// record an owner as unreachable on the strength of a rate limit (dirEntry, NarrowingRefused).
func TestGitHubsTroubleThatReadsAsARefusalFailsTheBundleAndLeavesNothingOut(t *testing.T) {
	log, said := bundleLog()
	left := NewLeftOut()
	repos := projectRepos("acme/gadgets", "acme/widgets", "sjawhar/legion")
	credential := GitHubCredential(troubledTokens{}, repos, ghrepo.Renames{}, left, log)

	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	var trouble *TransientError
	if !errors.As(err, &trouble) {
		t.Fatalf("the implementer's bundle under GitHub's trouble = %v, %v; want the trouble, so the caller asks again", bundle, err)
	}
	if said.Len() != 0 {
		t.Errorf("the log holds a left-out line for a refusal that is GitHub's trouble:\n%s", said.String())
	}
	dirs := GitHubDirs(repos, ghrepo.Renames{}, left, Roles, ghrepo.MustParse("sjawhar/legion"))
	for _, want := range []string{"acme", "sjawhar"} {
		if !slices.Contains(dirs, want) {
			t.Errorf("the directories a bundle would hold are %v, want %s among them: nothing is recorded left out", dirs, want)
		}
	}
}
