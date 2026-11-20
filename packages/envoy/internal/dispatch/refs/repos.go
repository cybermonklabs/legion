package refs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The canonical name of a repository is what its repo_projects row's renamed_to names, else the
// name itself, compared case-insensitively. A repository Dispatch holds no mapping for, and one
// whose mapping names no rename, is its own canonical name, so a repository Dispatch does not
// map is unaffected by this rule: it reads exactly as it did before the column existed.
//
// This is the server's one repository-renaming rule, and it is written here once because six
// sites must come to the same answer or the rename is only half known: delivery's attributionHeld
// puts both the issue's resolved repository and the pull request's own through it, the results
// route canonicalises the repository of a check's issue before it asks GitHub anything, in the
// one helper its results read and its record route share (api/issue_repo.go,
// server.checkIssueRepository), the three writers of a pull_request_heads row key the row by it
// (delivery/pull_request_heads.go: RefreshPullRequestHead, ReadThroughCommitPulls and
// Intake.recordPullRequestHead), the settings route reads it to decide whether a rename's
// target is itself renamed (api/repo_projects.go, refuseRepoRenameChain), and the CI reconcile
// reads every repository it covers under the name it has now, so no pass asks GitHub about a
// name it cannot read (cireconcile/repositories.go, Reconciler.covered).
//
// One step is enough: the settings route refuses a pair pointing at a repository that is itself
// renamed, and refuses renaming a repository another row already points at, so no chain and no
// cycle exists to walk (api/repo_projects.go, putRepoProject; migration
// 20261105075455_repo_projects_renamed_to). Dispatch never asks GitHub to resolve a name — for a
// repository renamed away, GitHub answers 404 for its installation, so the old name cannot be
// read at all and the pair is declared, never discovered.

// CanonicalRepoSQL is that rule as a SQL expression over repo, itself an expression naming a
// repository: null where repo is null, so a comparison against it stays null rather than
// becoming true. It reads repo_projects by a lowercased name on both sides, as every comparison
// of a repository here does, because GitHub spells an owner and a repository as their owners do
// (`Acme/Widgets`) while Dispatch stores a mapping's path canonicalised (ExternalRepo).
// The expression text is spliced twice, so repo must be a column, a bound parameter or another
// side-effect-free expression.
//
// The inner read is an aggregate, so it answers exactly one row however many rows match: the
// primary key is the repository's own spelling, so two mappings whose names differ only by case
// can exist — direct SQL, or a hand repair, which migration 20261105075455_repo_projects_renamed_to
// expects — and a plain scalar subquery over both would fail every statement this expression is
// spliced into with `more than one row returned by a subquery`. It reads only the mappings that
// name a rename, so a repository mapped with no rename coalesces to its own name.
//
// That read goes through a derived table that renames both columns it keeps, because repo is
// the caller's own expression spliced into this one and SQL resolves an unqualified name
// against the innermost query's columns first. Reading repo_projects directly here would put a
// column named `repo` in scope, so a caller splicing a bare `repo` would compare that column
// with itself — true for every row, which answers the whole table's greatest renamed_to for
// every row of every caller rather than each row's own canonical name. No caller's column is
// named rename_source or rename_target, so a caller's bare `repo` reaches the caller's own
// table, and a caller whose statement has no such column fails loudly with `column "repo" does
// not exist` rather than silently answering some other repository's name.
func CanonicalRepoSQL(repo string) string {
	return `coalesce((select max(rename.rename_target) from (
			select renamed.repo as rename_source, renamed.renamed_to as rename_target
			from repo_projects renamed where renamed.renamed_to is not null
		) rename where lower(rename.rename_source) = lower(` + repo + `)), ` + repo + `)`
}

// CanonicalRepo is that rule read through q for one full `owner/name`: the expression above,
// selected for repo, so the Go caller and every statement that splices the expression read one
// rule and cannot come to different answers. The caller's own spelling comes back unchanged
// where there is no rename, so canonicalising a name never re-cases a stored row's key; where
// there is one, the answer is the pair as the settings route canonicalised it.
func CanonicalRepo(ctx context.Context, q Queryer, repo string) (string, error) {
	if repo == "" {
		return repo, nil
	}
	var canonical string
	if err := q.QueryRow(ctx, `select `+CanonicalRepoSQL("$1::text"), repo).Scan(&canonical); err != nil {
		return "", fmt.Errorf("read the canonical name of %s: %w", repo, err)
	}
	return canonical, nil
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
// person put the canonical form they store to it and refuse a name it refuses, and so does the
// boot seed of the repository mappings, each through ParseGitHubRepo (api's parseSourceRepo,
// parseIssueRepo and ParseRepoProjects). Delivery, wherever it turns a stored name into a
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
