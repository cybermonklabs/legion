package refs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The canonical name of a repository is the new name of its rename pair, else the name itself,
// compared without case. A repository with no pair is its own canonical name, so the rule
// changes nothing for it.
//
// The pairs are the repo_projects rows with a renamed_to, which no route of this release writes.
// The boot reads them once (store.ReadRepoRenames) into one Renames, and gives that value to
// every reader of a repository name. No reader reads the table again, so no statement but that
// one read names repo_projects for a rename. A process whose boot finds the table gone reads no
// pairs and serves with none: dispatch://INFRA-4411 drops it, and a statement that named a
// dropped table would fail.
//
// This is the server's one repository-renaming rule, and it is written here once because every
// site must come to the same answer or the rename is only half known: delivery's attributionHeld
// puts both the issue's resolved repository and the pull request's own through it, and its link
// sources compare a URL under both names of a pair (attributionCandidates); the results route
// canonicalises the repository of a check's issue before it asks GitHub anything
// (api/issue_repo.go, server.checkIssueRepository); the three writers of a
// pull_request_heads row key the row by it (delivery/pull_request_heads.go:
// Attributor.RefreshPullRequestHead, Attributor.ReadThroughCommitPulls and
// Intake.recordPullRequestHead); GET /api/v1/issues/resolve finds a link under either name
// (api/issue_refs.go, resolveIssueRef); and the CI reconcile reads every repository it covers
// under the name it has now, so no pass asks GitHub about a name it cannot read
// (cireconcile/repositories.go, Reconciler.covered).
//
// Every write of a stored repository name or GitHub URL puts it through the same rule first
// (Canonical, CanonicalURL), so no write stores an old name: an issue's own repository and its
// external links on a create and a patch, a project's default repository, the delivery
// settings' repositories, an architecture source's repository and a result's linked repository
// (api; check_results.commit_repo), and the issue URLs a pull request's body cites (delivery's
// citedIssues). The rows stored before this rule are rewritten once, by the boot's pass
// (store.RewriteRenamedRepositories).
//
// One step is enough: the route that wrote the pairs refused a pair pointing at a repository
// that is itself renamed, and refused renaming a repository another pair already points at, so
// no chain and no cycle exists to walk. Dispatch never asks GitHub to resolve a name. For a
// repository renamed away, GitHub answers 404 for its installation, so the old name cannot be
// read at all and the pair is declared, never discovered.

// Renames is the rename pairs this process read at boot, which every reader of a repository
// name is given. It is immutable: NewRenames copies the pairs, and nothing changes them after.
// The zero value holds no pairs, and every rule below then answers each name as it is given.
type Renames struct {
	pairs []renamePair
}

// renamePair is one pair with its URL rule in each direction: toNew rewrites a URL that names
// the old name so that it names the new one, and toOld does the opposite.
type renamePair struct {
	store.RepoRename
	toNew, toOld store.RepoURLRule
}

// NewRenames is the Renames of pairs, as store.ReadRepoRenames answers them: both names in lower
// case, and one pair for each old name.
func NewRenames(pairs []store.RepoRename) Renames {
	if len(pairs) == 0 {
		return Renames{}
	}
	built := make([]renamePair, 0, len(pairs))
	for _, pair := range pairs {
		built = append(built, renamePair{
			RepoRename: pair,
			toNew:      store.NewRepoURLRule(pair.From, pair.To),
			toOld:      store.NewRepoURLRule(pair.To, pair.From),
		})
	}
	return Renames{pairs: built}
}

// Canonical is the canonical name of repo: the new name of the pair whose old name is repo, in
// any case, else repo as it is given. So canonicalising a name with no pair never changes the
// case of a stored row's key.
func (r Renames) Canonical(repo string) string {
	for _, pair := range r.pairs {
		if strings.EqualFold(pair.From, repo) {
			return pair.To
		}
	}
	return repo
}

// CanonicalURL is url under the canonical name of the repository it names: url rewritten to the
// new name of the pair whose old name it names, by the URL rule (store.RepoURLRule), else url as
// it is given. So a write that stores a GitHub URL never stores an old name, and a URL that names
// no old name keeps every character it has.
func (r Renames) CanonicalURL(url string) string {
	for _, pair := range r.pairs {
		if rewritten, named := pair.toNew.Rewrite(url); named {
			return rewritten
		}
	}
	return url
}

// CanonicalRepoSQL is Canonical as a SQL expression over repo, an expression naming a
// repository. With no pairs it is repo itself. Otherwise it is
// `case lower(repo) when '<old>' then '<new>' … else repo end`, which is null where repo is
// null, so a comparison against it stays null and does not become true. The expression text of
// repo is spliced twice, so repo must be a column, a bound parameter or another expression with
// no side effect.
func CanonicalRepoSQL(renames Renames, repo string) string {
	if len(renames.pairs) == 0 {
		return repo
	}
	var b strings.Builder
	b.WriteString(`case lower(` + repo + `)`)
	for _, pair := range renames.pairs {
		b.WriteString(` when ` + store.QuoteLiteral(pair.From) + ` then ` + store.QuoteLiteral(pair.To))
	}
	b.WriteString(` else ` + repo + ` end`)
	return b.String()
}

// URLSpellings is url, then url under the other name of each pair that url names by either name,
// by the URL rule (store.RepoURLRule). So a reader that looks up every spelling finds a URL
// stored under either name of a pair.
func (r Renames) URLSpellings(url string) []string {
	spellings := []string{url}
	for _, pair := range r.pairs {
		if other, named := pair.toNew.Rewrite(url); named {
			spellings = append(spellings, other)
		}
		if other, named := pair.toOld.Rewrite(url); named {
			spellings = append(spellings, other)
		}
	}
	return spellings
}

// URLSpellingsSQL is URLSpellings as a SQL text[] over url, an expression naming a URL, with each
// spelling in lower case: url itself, and url rewritten to the other name of each pair. A
// rewrite of a URL that names neither name of a pair is url again, which changes nothing about
// a comparison with the array. The spellings are in lower case for a comparison
// `lower(column) = any(…)`, which an index on lower(column) serves, as
// issue_external_links_url_lower serves delivery's attributionCandidates.
func URLSpellingsSQL(renames Renames, url string) string {
	var b strings.Builder
	b.WriteString(`array[lower(` + url + `)`)
	for _, pair := range renames.pairs {
		b.WriteString(`, lower(` + pair.toNew.RewriteSQL(url) + `), lower(` + pair.toOld.RewriteSQL(url) + `)`)
	}
	b.WriteString(`]`)
	return b.String()
}

// The two halves of a repository GitHub could hold, copied from the daemon's own reading of the
// same grammar (packages/daemon/internal/ghrepo's ownerName and repositoryName): packages/envoy
// cannot import packages/daemon, and the two gates of one product must hold a repository to one
// rule, so the rule is written out here with its source named, once for the whole server
// (SplitGitHubRepo names the places that read it, and GitHubOwnerPattern and GitHubNamePattern
// hold it for a SQL pattern as well). An owner is a letter or digit and then up to 38 letters,
// digits, hyphens and underscores, which is looser than GitHub's sign-up form on purpose: that
// form's rule, single hyphens with none leading or trailing, binds new accounts alone, and GitHub
// still serves a login with a doubled hyphen (`hello--world`, the organisation `foo--bar`), one
// ending in a hyphen (`ArtOfCode-`) and a managed user's `_<shortcode>` (`mona-cat_octo`), whose
// repositories an issue has to be able to name. A repository's own name is 1 to 100 letters,
// digits, `.`, `-` and `_`, the most GitHub allows. The pattern holds the characters alone and
// gitHubName the 100, where the daemon's pattern holds both: that is where the bound is written,
// not a difference in the names either rule takes, since each puts the bound to the name as it is
// given. A route bounds the name it stores because it lower-cases the name and trims one `.git`
// off it (ExternalRepo) before it puts the name to this rule: `acme/<100 characters>.git`
// is written for that repository and stored as its 100 characters. The names the two rules take
// differ in two places, both in a trailing `.git`, which a clone URL carries and GitHub strips
// from a name at creation, and in nothing else. The daemon's ghrepo.Parse refuses every name
// that ends in a lower-case `.git`, where this rule takes one that passes it as it is and once
// that `.git` is trimmed (SplitGitHubRepo), since a name stored can still end in one: a route
// trims one `.git` and no more, so `acme/x.git.git` written is `acme/x.git` stored, which this
// rule takes and Parse refuses. And this rule finds that suffix in any letter case, lower-casing
// the name before it trims (`strings.ToLower` before `strings.TrimSuffix`), where Parse finds a
// lower-case `.git` alone (`strings.CutSuffix`): `acme/x.GIT` is taken by both, here once `x`
// passes the rule too, and `acme/.GIT`, `acme/..GIT` and `acme/...GIT`, which are empty, `.` and
// `..` once that suffix is trimmed, are refused here and taken by Parse as they are. Both letter
// cases are written out in the patterns because the grammar is the grammar whatever the case, so
// a name GitHub holds is taken however it was typed. The two names `.` and `..`, which no working
// copy could hold and which a GitHub path reads as a dot segment, are refused beside the
// patterns, where a pattern would state them worse.
//
// GitHubOwnerPattern and GitHubNamePattern are the owner's and the name's patterns of the grammar
// above, neither anchored nor grouped, so that a Go regexp and a SQL one are built from one text
// and cannot hold a name to two rules: this package anchors them for its own reads, and a
// statement that matches a name in SQL puts them in its own pattern. They are written in what
// RE2 and Postgres' advanced regular expressions read alike, bracket expressions and a bounded
// repeat, and hold no quote, so they go into a SQL string literal as they are. The name's bound
// of 100 and the two names `.` and `..` are no part of them (gitHubName holds those).
const (
	GitHubOwnerPattern = `[A-Za-z0-9][A-Za-z0-9_-]{0,38}`
	GitHubNamePattern  = `[A-Za-z0-9._-]+`
)

var (
	repoOwnerPattern = regexp.MustCompile(`^` + GitHubOwnerPattern + `$`)
	repoNamePattern  = regexp.MustCompile(`^` + GitHubNamePattern + `$`)
)

// repoNameMax is the most characters GitHub allows in a repository's own name.
const repoNameMax = 100

// gitHubOwner answers whether owner is a login GitHub serves, by the grammar above.
func gitHubOwner(owner string) bool {
	return repoOwnerPattern.MatchString(owner)
}

// gitHubName answers whether name is a repository name GitHub allows, by the grammar above:
// 1 to 100 letters, digits, `.`, `-` and `_`, and neither `.` nor `..`. It reads one name as it
// is; SplitGitHubRepo puts a name to it as a GitHub path carries it and once one trailing `.git`
// is trimmed.
func gitHubName(name string) bool {
	return name != "." && name != ".." && len(name) <= repoNameMax && repoNamePattern.MatchString(name)
}

// ErrNotGitHubRepo is SplitGitHubRepo's refusal: a name no repository GitHub could hold is
// called, which no read of GitHub, made now or later, can find.
var ErrNotGitHubRepo = errors.New("not an owner/name GitHub allows")

// SplitGitHubRepo splits full, an `owner/name` as a GitHub path is built from it, into the two
// halves that path takes, each held to the names GitHub allows: the owner to gitHubOwner, and the
// name to gitHubName twice, as it is and once one trailing `.git` is trimmed. The bound is the
// name's as it goes into the path, so `acme/<96 characters>.git` is taken and
// `acme/<100 characters>.git`, whose name would reach GitHub as 104 characters, is refused. The
// trimmed name is held to the rule as well because GitHub strips a trailing `.git` from a name at
// creation, and a name Dispatch stores can end in one (ExternalRepo trims one `.git` and no
// more): `acme/.git.git` and `acme/...git.git` written are `acme/.git` and `acme/...git` stored,
// which are empty and `..` once that `.git` is trimmed, so no repository GitHub holds is called
// by them, and this refuses them. A route that takes a name from a person canonicalises it first
// and puts the form it stores to this (ParseGitHubRepo), so a name written with the suffix is
// bounded as it is stored: `acme/<100 characters>.git` written is the 100 characters. The halves
// come back as full spells them, so a caller keeps its own case and its own suffix; the refusal
// is ErrNotGitHubRepo, naming full.
//
// It is the rule the routes and these readers read. The API routes that take a repository from a
// person put the canonical form they store to it and refuse a name it refuses, each through
// ParseGitHubRepo (api's parseSourceRepo, and parseIssueRepo, which an issue's own repository
// and a project's default repository are read by). Delivery, wherever it turns a stored name into a
// GitHub path, cireconcile's splitRepo, the architecture importer's resolve and api's readCIRuns
// each refuse one there, so a name stored before the routes held it to the rule, or by direct
// SQL, reaches GitHub through none of them. Delivery's linked pull requests
// (linkedPullRequestTerms) read a link's owner and name in SQL by the same patterns
// (GitHubOwnerPattern, GitHubNamePattern), and refuse a name of `.` or `..` there; a read that
// puts such a repository into a GitHub path splits it here. One reader does not put the name to
// it: the Legion metrics' readMergeTimes (legion/metrics/population.go) splits
// delivery_pull_requests.repo with strings.Cut into delivery.FetchPullRequest's path. Those rows
// are written from the names GitHub itself gives, so only a name planted there by direct SQL
// reaches GitHub through that read.
func SplitGitHubRepo(full string) (owner, name string, err error) {
	owner, name, found := strings.Cut(full, "/")
	if !found || !gitHubOwner(owner) || !gitHubName(name) || !gitHubName(strings.TrimSuffix(strings.ToLower(name), ".git")) {
		return "", "", fmt.Errorf("repository %q is %w: an owner of letters, digits, - and _, and a name of 1 to 100 letters, digits, ., - and _ that is not . or .., nor empty, . or .. once a trailing .git is trimmed", full, ErrNotGitHubRepo)
	}
	return owner, name, nil
}
