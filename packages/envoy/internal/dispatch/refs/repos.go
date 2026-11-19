package refs

import (
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
// (api/results_comparison.go, server.checkIssueRepository); the three writers of a
// pull_request_heads row key the row by it (delivery/pull_request_heads.go:
// Attributor.RefreshPullRequestHead, Attributor.ReadThroughCommitPulls and
// Intake.recordPullRequestHead); GET /api/v1/issues/resolve finds a link under either name
// (api/issue_refs.go, resolveIssueRef); and the CI reconcile reads every repository it covers
// under the name it has now, so no pass asks GitHub about a name it cannot read
// (cireconcile/repositories.go, Reconciler.covered).
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
