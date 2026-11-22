package delivery

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// PullRequestOpenOrMerged answers yes for a pull request that is open or merged, whichever of
// `merged` and `merged_at` carries the merge, and no for one that closed without a merge. A pull
// request GitHub answers 404 for is the error, never a no: Dispatch cannot tell it from a pull
// request the App cannot reach.
func TestPullRequestOpenOrMergedReadsTheStateGitHubAnswers(t *testing.T) {
	fake := newFakeGitHub(t)
	answers := map[string]string{
		"1": `{"number":1,"state":"open","merged":false,"merged_at":null}`,
		"2": `{"number":2,"state":"closed","merged":true,"merged_at":"2026-11-14T00:00:00Z"}`,
		"3": `{"number":3,"state":"closed","merged_at":"2026-11-14T00:00:00Z"}`,
		"4": `{"number":4,"state":"closed","merged":false,"merged_at":null}`,
	}
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		answer, found := answers[r.PathValue("number")]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, answer)
	})
	client := fake.newTestClient()

	for _, row := range []struct {
		name   string
		number int
		want   bool
	}{
		{"an open pull request", 1, true},
		{"a pull request merged, as the single read says it", 2, true},
		{"a pull request merged, as only merged_at says it", 3, true},
		{"a pull request closed without a merge", 4, false},
	} {
		got, err := PullRequestOpenOrMerged(t.Context(), client, "acme/widgets", row.number)
		if err != nil || got != row.want {
			t.Errorf("%s: PullRequestOpenOrMerged = %v, %v, want %v and no error", row.name, got, err, row.want)
		}
	}
	if got, err := PullRequestOpenOrMerged(t.Context(), client, "acme/widgets", 5); !errors.Is(err, ErrPullRequestNotFound) {
		t.Errorf("a pull request GitHub answers 404 for: PullRequestOpenOrMerged = %v, %v, want ErrPullRequestNotFound", got, err)
	}
}

// A stored name GitHub's names do not allow never becomes a GitHub path: refs.SplitGitHubRepo
// refuses it, naming it, before any request, whichever read of the repository asks. `acme/..`
// and `acme/` are what a repository written as `acme/...git` and `acme/.git` was stored as once
// the trailing `.git` was trimmed, and `..` in a path asks GitHub about the one above the
// repository's (`/repos/acme/../installation`). A rename pair's new name stored before the
// route that wrote the pairs held one to GitHub's names, or by direct SQL, is the same name
// reached another way: the reconcile's refresh and the read-through canonicalise the repository
// they are handed under the pairs the boot read (Attributor), and ask GitHub nothing either. The
// GitHub stand-in counts every request it receives, and answers none.
func TestAStoredNameGitHubDoesNotAllowMakesNoGitHubRequest(t *testing.T) {
	const sha = "6767676767676767676767676767676767676767"
	pool, ctx := deliveryTestPool(t)
	var requests atomic.Int32
	client := newAppClient(t, githubtest.Spec{
		Record: func(*http.Request) { requests.Add(1) },
		Handle: func(w http.ResponseWriter, r *http.Request) { githubtest.Unexpected(t, w, r) },
	})

	refused := func(read, name string, err error) {
		t.Helper()
		if !errors.Is(err, refs.ErrNotGitHubRepo) || !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
			t.Errorf("%s of %q: error %v, want refs.ErrNotGitHubRepo naming it", read, name, err)
		}
	}
	for _, name := range []string{"acme/..", "acme/", "acme/...git", "acme/.git", "acme/."} {
		_, err := RepositoryHoldsCommit(ctx, client, name, sha)
		refused("RepositoryHoldsCommit", name, err)
		_, err = PullRequestOpenOrMerged(ctx, client, name, 7)
		refused("PullRequestOpenOrMerged", name, err)
		_, err = repositoryDefaultBranch(ctx, client, name)
		refused("repositoryDefaultBranch", name, err)
		_, err = askGitHubCommitOnBranch(ctx, client, name, "main", sha)
		refused("askGitHubCommitOnBranch", name, err)
	}

	// Two repositories renamed to such names, as a pair stored before the rule reads, and the
	// Attributor of the pairs a boot reads after them.
	seedProjectRepository(t, ctx, pool, "TOOL", "Tools", "acme/tools")
	renamed := map[string]string{"oldco/tools": "acme/..", "oldco/gadgets": "acme/"}
	for old, renamedTo := range renamed {
		if _, err := pool.Exec(ctx, `
			insert into repo_projects (repo, project, created_by, is_default, renamed_to)
			values ($1, 'TOOL', '{"kind":"system","id":"delivery-test"}'::jsonb, false, $2)
		`, old, renamedTo); err != nil {
			t.Fatalf("record %s as renamed to %s: %v", old, renamedTo, err)
		}
	}
	attributor := NewAttributor(renamesOf(t, ctx, pool))
	for old, renamedTo := range renamed {
		err := attributor.RefreshPullRequestHead(ctx, pool, client, nil, old, 7, time.Now())
		refused("RefreshPullRequestHead", renamedTo, err)
		_, _, err = attributor.ReadThroughCommitPulls(ctx, pool, client, nil, old, sha, time.Now())
		refused("ReadThroughCommitPulls", renamedTo, err)
	}

	if sent := requests.Load(); sent != 0 {
		t.Fatalf("the stored names sent GitHub %d requests, want none", sent)
	}
}
