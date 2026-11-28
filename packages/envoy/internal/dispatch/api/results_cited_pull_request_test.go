// results_cited_pull_request_test.go holds GET /api/v1/issues/{key}/results to the attribution of
// a pull request whose body names another pull request by number. GitHub numbers a repository's
// issues and pull requests in one sequence, and Dispatch stores a link to the reference
// owner/repo#n as both .../issues/n and .../pull/n, so "#53 renamed the helper" in a body reads as
// a citation of a GitHub issue that another issue links. Where Dispatch holds pull request #53,
// that mention cites no issue, and the pull request stays the issue its branch names. The file
// drives the routes alone, with helpers main holds and no writer of a build's own, so it runs
// unchanged at any build that serves the read-through, and fails on its own assertions where the
// rule is dropped.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The pull requests of acme/widgets these cases read: the issue's own, one stacked on its branch,
// the other issue's pull request a body names, and the GitHub issue a body cites. citedElsewhereSHA
// heads a pull request of another repository, and citedOldNameSHA one of acme/widgets stored under
// its old name.
const (
	citedOwn          = 51
	citedOnTop        = 52
	citedPull         = 53
	citedIssue        = 54
	citedOwnSHA       = "5151515151515151515151515151515151515151"
	citedTopSHA       = "5252525252525252525252525252525252525252"
	citedPRSHA        = "5353535353535353535353535353535353535353"
	citedElsewhereSHA = "5454545454545454545454545454545454545454"
	citedOldNameSHA   = "5555555555555555555555555555555555555555"
)

// citedPullRequest is a pull request GitHub names for a head commit, in whichever repository the
// read asks about.
type citedPullRequest struct {
	number     int
	head, base string
	body       string
}

// citedWorld is a Dispatch whose GitHub names, for each head commit, one pull request of
// acme/widgets: the issue's own #51 (branch legion/<issue>, base main, body ownBody), #52 stacked
// on its branch (legion/<issue>-snippet-sentinels), and the other issue's #53 (legion/<other>).
// Both issues carry siblingChecks; the other issue links acme/widgets#linked as Dispatch stores a
// reference, in both URL forms. Its App is installed on acme/other and acme/old-widgets too, and
// a case names more pull requests with heads. database and fake are the store and the GitHub the
// world's server runs on, which a server built later (renamed) shares.
type citedWorld struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	fake     *commitPullsFake
	issue    string
	other    string
	heads    map[string]citedPullRequest
}

func newCitedWorld(t *testing.T, project, ownBody string, linked int) citedWorld {
	t.Helper()
	handler, database, _, fake := readThroughGitHub(t, "acme/widgets", "acme/other", "acme/old-widgets")
	issue := issueInRepository(t, handler, project, "acme/widgets", "A pull request that names another")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": project, "title": "The other issue's work", "spec": "A spec\n"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create the other issue: status=%d body=%s", response.Code, response.Body.String())
	}
	other := decodeBody[model.Issue](t, response).Key
	for _, key := range []string{issue.Key, other} {
		uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", siblingChecks)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issue_external_links (issue_key, url, kind) values
			($1, 'https://github.com/acme/widgets/issues/' || $2, 'github_issue'),
			($1, 'https://github.com/acme/widgets/pull/' || $2, 'url')`, other, strconv.Itoa(linked)); err != nil {
		t.Fatalf("link acme/widgets#%d to %s: %v", linked, other, err)
	}
	heads := map[string]citedPullRequest{
		citedOwnSHA: {citedOwn, "legion/" + issue.Key, "main", ownBody},
		citedTopSHA: {citedOnTop, "legion/" + issue.Key + "-snippet-sentinels", "legion/" + issue.Key, ""},
		citedPRSHA:  {citedPull, "legion/" + other, "main", ""},
	}
	fake.answers(func(repo, sha string) string {
		pull, named := heads[sha]
		if !named {
			return `[]`
		}
		body, err := json.Marshal(pull.body)
		if err != nil {
			t.Errorf("encode the body of #%d: %v", pull.number, err)
		}
		return fmt.Sprintf(`[{"number":%d,"title":"A change","body":%s,"html_url":"https://github.com/%s/pull/%d",
			"state":"open","merged":false,"merged_at":null,"updated_at":"2026-11-22T07:00:00Z",
			"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},"base":{"ref":%q}}]`,
			pull.number, body, repo, pull.number, pull.head, sha, repo, pull.base)
	})
	return citedWorld{t: t, handler: handler, database: database, fake: fake, issue: issue.Key, other: other, heads: heads}
}

// holdIn makes Dispatch hold pull request number of repo, headed by sha: an issue of a project of
// its own whose repository is repo, with siblingChecks, records a pass at sha, which stores the
// pull request GitHub names there. It answers that issue's key.
func (w citedWorld) holdIn(project, repo string, number int, sha string) string {
	w.t.Helper()
	holder := issueInRepository(w.t, w.handler, project, repo, "Work of "+repo)
	uploadDocument(w.t, w.handler, "/api/v1/issues/"+holder.Key, "verification.md", siblingChecks)
	w.heads[sha] = citedPullRequest{number, "legion/" + holder.Key, "main", ""}
	w.pass(holder.Key, sha)
	return holder.Key
}

// renamed answers the world on a server built after the rename pair oldName to newName is stored:
// a repo_projects row of project, seeded by SQL as the last release left it, since no route writes
// a pair. The server runs on a pool of its own to the world's database, with the same App key and
// GitHub, and reads the pairs once, as it is built (readRenames), as a boot does. Every row the
// world wrote before stands under the name it was written under.
func (w citedWorld) renamed(project, oldName, newName string) citedWorld {
	w.t.Helper()
	if _, err := w.database.Pool.Exec(context.Background(), `
		insert into repo_projects (repo, project, created_by, renamed_to)
		values ($1, $2, '{"kind":"user","id":"alice"}', $3)
	`, oldName, project, newName); err != nil {
		w.t.Fatalf("seed the pair %s -> %s: %v", oldName, newName, err)
	}
	w.handler, _, _ = newTestServer(w.t, testServerOptions{
		store:         storetest.SecondProcess(w.t, w.database),
		renames:       readRenames(w.t, w.database),
		app:           w.fake.app,
		githubAPIBase: w.fake.upstream,
	})
	return w
}

// pass records the tester's pass of k1 of key at commit. The record route reads the pull requests
// GitHub names for a commit that heads none Dispatch holds for key, and stores each, so a pass is
// how a case makes Dispatch hold a pull request, as production's results do.
func (w citedWorld) pass(key, commit string) {
	w.t.Helper()
	response := recordResult(w.t, w.handler, key, resultBody("k1", "pass", commit, map[string]any{"role": "tester"}))
	if response.Code != http.StatusCreated {
		w.t.Fatalf("record the tester's pass of %s k1 at %s: status=%d body=%s", key, commit, response.Code, response.Body.String())
	}
}

// comparedWith is the commits each check of key compared its results with, sorted, by check.
func (w citedWorld) comparedWith(key string) map[string][]string {
	w.t.Helper()
	read := readResults(w.t, w.handler, key)
	if len(read.Checks) != 2 {
		w.t.Fatalf("the results of %s hold %d checks, want siblingChecks' two: %+v", key, len(read.Checks), read)
	}
	by := map[string][]string{}
	for _, check := range read.Checks {
		commits := []string{}
		for _, basis := range check.ComparedWith {
			commits = append(commits, basis.Commit)
		}
		slices.Sort(commits)
		by[check.BlockID] = commits
	}
	return by
}

// requireCompared fails the test unless every check of key compared with want alone.
func (w citedWorld) requireCompared(key string, why string, want ...string) {
	w.t.Helper()
	slices.Sort(want)
	for check, got := range w.comparedWith(key) {
		if !slices.Equal(got, want) {
			w.t.Errorf("%s's %s compared with %v, want %v: %s", key, check, got, want, why)
		}
	}
}

// The issue's own pull request #51 names the other issue's pull request #53 by number in its body,
// and the other issue links acme/widgets#53 as .../issues/53 and .../pull/53. Dispatch holds #53
// (a pass at its head is recorded first), so "#53" cites no GitHub issue: #51 stays the issue's by
// its branch, beside #52, whose branch names the issue too, and every check of the issue compares
// with both heads. Where the citation of #53 still counted as source 2, which ranks above the
// branch, #51 went to the other issue, and the issue's checks compared with #52's head alone. The
// other issue's checks compare with #53's head alone either way.
//
// Control: a body citing #54, which the other issue links and which is no pull request Dispatch
// holds, still gives #51 to the other issue by the citation, which outranks the branch: the rule
// leaves a real citation of a GitHub issue standing.
func TestABodyNamingAPullRequestByNumberLeavesThePullRequestItsBranchsIssue(t *testing.T) {
	t.Parallel()
	t.Run("the body names a pull request Dispatch holds", func(t *testing.T) {
		world := newCitedWorld(t, "CITEPR",
			fmt.Sprintf("#%d renamed the helper, so this change calls it by its new name.", citedPull), citedPull)
		world.pass(world.other, citedPRSHA)
		world.pass(world.issue, citedOwnSHA)
		world.pass(world.issue, citedTopSHA)
		world.requireCompared(world.issue,
			fmt.Sprintf("#%d is a pull request Dispatch holds, so #%d's body cites no GitHub issue and its branch names the issue", citedPull, citedOwn),
			citedOwnSHA, citedTopSHA)
		world.requireCompared(world.other, fmt.Sprintf("the other issue's one pull request is #%d", citedPull), citedPRSHA)
	})
	t.Run("control: the body cites a GitHub issue the other issue links", func(t *testing.T) {
		world := newCitedWorld(t, "CITEIS", fmt.Sprintf("Fixes #%d.", citedIssue), citedIssue)
		world.pass(world.issue, citedOwnSHA)
		world.pass(world.issue, citedTopSHA)
		world.requireCompared(world.issue,
			fmt.Sprintf("#%d is no pull request Dispatch holds, so #%d's body cites the GitHub issue the other issue links, which ranks above the branch", citedIssue, citedOwn),
			citedTopSHA)
	})
	t.Run("control: another repository holds a pull request of the cited number", func(t *testing.T) {
		world := newCitedWorld(t, "CITEOR", fmt.Sprintf("Fixes #%d.", citedIssue), citedIssue)
		world.holdIn("CITEOO", "acme/other", citedIssue, citedElsewhereSHA)
		world.pass(world.issue, citedOwnSHA)
		world.pass(world.issue, citedTopSHA)
		world.requireCompared(world.issue,
			fmt.Sprintf("Dispatch holds #%d of acme/other alone, so #%d of acme/widgets is still the GitHub issue the other issue links, which ranks above the branch", citedIssue, citedIssue),
			citedTopSHA)
	})
	t.Run("the body names a pull request Dispatch holds under its repository's old name", func(t *testing.T) {
		world := newCitedWorld(t, "CITERN",
			fmt.Sprintf("#%d renamed the helper, so this change calls it by its new name.", citedPull), citedPull)
		// Dispatch holds #53 under acme/old-widgets before any pair names that name, so the row is
		// stored under it. The pair is seeded after, and the issue's passes and reads go through a
		// server built after the seed.
		world.holdIn("CITEOW", "acme/old-widgets", citedPull, citedOldNameSHA)
		world = world.renamed("CITEOW", "acme/old-widgets", "acme/widgets")
		world.pass(world.issue, citedOwnSHA)
		world.pass(world.issue, citedTopSHA)
		world.requireCompared(world.issue,
			fmt.Sprintf("Dispatch holds #%d of acme/widgets under its old name acme/old-widgets, so #%d's body cites no GitHub issue and its branch names the issue", citedPull, citedOwn),
			citedOwnSHA, citedTopSHA)
	})
	t.Run("the body names a pull request Dispatch holds by its repository's old name", func(t *testing.T) {
		world := newCitedWorld(t, "CITEON",
			fmt.Sprintf("https://github.com/acme/old-widgets/issues/%d renamed the helper, so this change calls it by its new name.", citedPull), citedPull)
		// Dispatch holds #53 under acme/widgets, the new name, and #51's body names it by the old
		// one, acme/old-widgets. The pair is seeded after, and the issue's passes and reads go
		// through a server built after the seed. The other issue links acme/widgets#53, which the
		// citation matches under either name, so where the cited URL's own repository was compared
		// as it is spelled, #53 read as no pull request Dispatch holds and #51 went to the other
		// issue by the citation.
		world.pass(world.other, citedPRSHA)
		world = world.renamed("CITEON", "acme/old-widgets", "acme/widgets")
		world.pass(world.issue, citedOwnSHA)
		world.pass(world.issue, citedTopSHA)
		world.requireCompared(world.issue,
			fmt.Sprintf("#%d's body names acme/old-widgets#%d, which Dispatch holds under the new name acme/widgets, so it cites no GitHub issue and its branch names the issue", citedOwn, citedPull),
			citedOwnSHA, citedTopSHA)
	})
}
