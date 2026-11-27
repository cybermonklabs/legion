package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueReadKeysWithoutPullRequest is every key GET /api/v1/issues/{key} answers for
// buildFullIssueRead's issue while no pull request is attributed to it, sorted.
var issueReadKeysWithoutPullRequest = []string{
	"advances", "advances_revision", "artifacts", "assignee", "blocked_by", "blocks", "children",
	"claim", "closed_at", "comment_count", "components", "created_at", "created_by",
	"decisions_artifact_id", "design_gates", "external_links", "holder_state", "key", "labels",
	"last_seen", "last_seq", "number", "open_asks", "parent", "plan_artifact_id",
	"primary_artifact_id", "priority", "progress", "project", "rank", "referenced_by_count", "repo",
	"resolved_repo", "route", "route_holder", "route_status", "route_status_reason", "status",
	"takeable_at", "title", "updated_at", "verification_artifact_id",
}

// pullRequestHead is one pull_request_heads row a case writes straight into the table, as
// writeHead does (results_test.go), with the address and the event time the case sets. An empty
// url is a row that stores no address, a nil eventAt a row whose event reported no time, and an
// empty issue a pull request the attribution named no issue for.
type pullRequestHead struct {
	repo    string
	number  int
	state   string
	issue   string
	url     string
	eventAt *time.Time
}

// writePullRequestHeads stores each head as its own row.
func writePullRequestHeads(t *testing.T, database *store.Store, heads ...pullRequestHead) {
	t.Helper()
	for _, head := range heads {
		var attributed *string
		if head.issue != "" {
			attributed = &head.issue
		}
		if _, err := database.Pool.Exec(context.Background(), `
			insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, url, event_at)
			values ($1, $2, $3, 'legion/work', 'main', $4, $5, $6, $7)
		`, head.repo, head.number, fmt.Sprintf("%040d", head.number), head.state, attributed, head.url, head.eventAt); err != nil {
			t.Fatalf("store the head of %s#%d: %v", head.repo, head.number, err)
		}
	}
}

// readIssueAnswer reads GET /api/v1/issues/{key} as alice and answers its body decoded whole.
func readIssueAnswer(t *testing.T, handler http.Handler, key string) map[string]any {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[map[string]any](t, response)
}

// wantPullRequest fails t unless the read of key names exactly this pull request: its
// repository, number, state and address, and no other field.
func wantPullRequest(t *testing.T, handler http.Handler, key, repo string, number int, state, url string) {
	t.Helper()
	want := map[string]any{"repo": repo, "number": float64(number), "state": state, "url": url}
	got, held := readIssueAnswer(t, handler, key)["pull_request"]
	if !held {
		t.Fatalf("the read of %s carries no pull_request, want %v", key, want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the read of %s names the pull request %v, want %v", key, got, want)
	}
}

// wantNoPullRequest fails t where the read of key carries a pull_request field, null included.
func wantNoPullRequest(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	if got, held := readIssueAnswer(t, handler, key)["pull_request"]; held {
		t.Fatalf("the read of %s carries pull_request %v, want no field", key, got)
	}
}

// An issue's read names its pull request (R22 of dispatch://INFRA-3940): of the rows
// pull_request_heads attributes to the issue, an open one first, else a merged one, else a closed
// one, each the newest by event_at, with a row that has none after every dated row of its state.
// It reads the row's repository, number, state and address, which is the stored url, or the one
// built from the repository and the number where the row stores none. An issue with none carries
// no field, and the answer's other fields are what they were before the read named one.
func TestIssueDetailsPullRequest(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "PULL")
	base := time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC)
	at := func(hours int) *time.Time {
		moment := base.Add(time.Duration(hours) * time.Hour)
		return &moment
	}
	address := func(repo string, number int) string {
		return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	}

	t.Run("an issue with no pull request has no pull_request field", func(t *testing.T) {
		key := createOutcomeIssue(t, handler, "PULL", "Work with no pull request yet")
		wantNoPullRequest(t, handler, key)
	})

	t.Run("an attributed open pull request reads its repository, number, state and address", func(t *testing.T) {
		key := createOutcomeIssue(t, handler, "PULL", "Work with one open pull request")
		// The stored address keeps GitHub's own spelling of the repository, which the row's key
		// does not, so the answer shows the address is the one the row stores.
		writePullRequestHeads(t, database, pullRequestHead{
			repo: "acme/widgets", number: 41, state: "open", issue: key,
			url: "https://github.com/Acme/Widgets/pull/41", eventAt: at(0),
		})
		wantPullRequest(t, handler, key, "acme/widgets", 41, "open", "https://github.com/Acme/Widgets/pull/41")
	})

	t.Run("an open pull request comes before a newer merged one", func(t *testing.T) {
		key := createOutcomeIssue(t, handler, "PULL", "Work reopened after a merge")
		const repo = "acme/open-first"
		writePullRequestHeads(t, database,
			pullRequestHead{repo: repo, number: 12, state: "open", issue: key, url: address(repo, 12), eventAt: at(0)},
			pullRequestHead{repo: repo, number: 13, state: "merged", issue: key, url: address(repo, 13), eventAt: at(1)},
		)
		wantPullRequest(t, handler, key, repo, 12, "open", address(repo, 12))
	})

	t.Run("of two merged pull requests the newer one is read", func(t *testing.T) {
		key := createOutcomeIssue(t, handler, "PULL", "Work merged twice")
		const repo = "acme/merged-twice"
		// The newer one has the lower number, so an order by number alone reads the other.
		writePullRequestHeads(t, database,
			pullRequestHead{repo: repo, number: 20, state: "merged", issue: key, url: address(repo, 20), eventAt: at(1)},
			pullRequestHead{repo: repo, number: 21, state: "merged", issue: key, url: address(repo, 21), eventAt: at(0)},
		)
		wantPullRequest(t, handler, key, repo, 20, "merged", address(repo, 20))
	})

	t.Run("a closed pull request is read only where no open or merged one is", func(t *testing.T) {
		const repo = "acme/closed-last"
		besideMerged := createOutcomeIssue(t, handler, "PULL", "A newer closed pull request beside a merged one")
		besideOpen := createOutcomeIssue(t, handler, "PULL", "A newer closed pull request beside an open one")
		closedOnly := createOutcomeIssue(t, handler, "PULL", "Two closed pull requests and nothing else")
		writePullRequestHeads(t, database,
			pullRequestHead{repo: repo, number: 30, state: "closed", issue: besideMerged, url: address(repo, 30), eventAt: at(2)},
			pullRequestHead{repo: repo, number: 31, state: "merged", issue: besideMerged, url: address(repo, 31), eventAt: at(0)},
			pullRequestHead{repo: repo, number: 32, state: "closed", issue: besideOpen, url: address(repo, 32), eventAt: at(2)},
			pullRequestHead{repo: repo, number: 33, state: "open", issue: besideOpen, url: address(repo, 33), eventAt: at(0)},
			pullRequestHead{repo: repo, number: 34, state: "closed", issue: closedOnly, url: address(repo, 34), eventAt: at(1)},
			pullRequestHead{repo: repo, number: 35, state: "closed", issue: closedOnly, url: address(repo, 35), eventAt: at(0)},
		)
		wantPullRequest(t, handler, besideMerged, repo, 31, "merged", address(repo, 31))
		wantPullRequest(t, handler, besideOpen, repo, 33, "open", address(repo, 33))
		wantPullRequest(t, handler, closedOnly, repo, 34, "closed", address(repo, 34))
	})

	t.Run("a pull request with no event_at comes after a dated one of its state", func(t *testing.T) {
		const repo = "acme/undated"
		// In each state the undated row has the higher number, so an order that puts a null
		// event_at first, as Postgres's desc does by default, or that orders by number, reads it.
		for index, state := range []string{"open", "merged", "closed"} {
			key := createOutcomeIssue(t, handler, "PULL", "An undated "+state+" pull request beside a dated one")
			dated, undated := 40+2*index, 41+2*index
			writePullRequestHeads(t, database,
				pullRequestHead{repo: repo, number: dated, state: state, issue: key, url: address(repo, dated), eventAt: at(0)},
				pullRequestHead{repo: repo, number: undated, state: state, issue: key, url: address(repo, undated)},
			)
			wantPullRequest(t, handler, key, repo, dated, state, address(repo, dated))
		}
	})

	t.Run("a pull request with no stored address reads https://github.com/<repo>/pull/<number>", func(t *testing.T) {
		key := createOutcomeIssue(t, handler, "PULL", "A pull request whose event named no address")
		writePullRequestHeads(t, database, pullRequestHead{
			repo: "acme/no-address", number: 77, state: "open", issue: key, eventAt: at(0),
		})
		wantPullRequest(t, handler, key, "acme/no-address", 77, "open", "https://github.com/acme/no-address/pull/77")
	})

	t.Run("a pull request attributed to another issue is not read", func(t *testing.T) {
		const repo = "acme/two-issues"
		merged := createOutcomeIssue(t, handler, "PULL", "The issue whose pull request merged")
		open := createOutcomeIssue(t, handler, "PULL", "Another issue whose pull request is open")
		none := createOutcomeIssue(t, handler, "PULL", "An issue in the same repository with no pull request")
		// The other issue's pull request and one attributed to nobody are open and newer, so a
		// read that took every row of the repository, or every row, would read one of them.
		writePullRequestHeads(t, database,
			pullRequestHead{repo: repo, number: 80, state: "merged", issue: merged, url: address(repo, 80), eventAt: at(0)},
			pullRequestHead{repo: repo, number: 81, state: "open", issue: open, url: address(repo, 81), eventAt: at(1)},
			pullRequestHead{repo: repo, number: 82, state: "open", url: address(repo, 82), eventAt: at(2)},
		)
		wantPullRequest(t, handler, merged, repo, 80, "merged", address(repo, 80))
		wantPullRequest(t, handler, open, repo, 81, "open", address(repo, 81))
		wantNoPullRequest(t, handler, none)
	})

	t.Run("the answer's other fields are unchanged", func(t *testing.T) {
		// The issue holds one of everything the read answers, so every statement of its batches
		// runs beside the new one.
		fixture := buildFullIssueRead(t, handler)
		before := readIssueAnswer(t, handler, fixture.key)
		if keys := slices.Sorted(maps.Keys(before)); !slices.Equal(keys, issueReadKeysWithoutPullRequest) {
			t.Fatalf("the read of %s with no pull request answers the keys\n%q\nwant\n%q",
				fixture.key, keys, issueReadKeysWithoutPullRequest)
		}
		const repo = "acme/batch"
		writePullRequestHeads(t, database, pullRequestHead{
			repo: repo, number: 90, state: "open", issue: fixture.key, url: address(repo, 90), eventAt: at(0),
		})
		after := readIssueAnswer(t, handler, fixture.key)
		want := append(slices.Clone(issueReadKeysWithoutPullRequest), "pull_request")
		slices.Sort(want)
		if keys := slices.Sorted(maps.Keys(after)); !slices.Equal(keys, want) {
			t.Fatalf("the read of %s with a pull request answers the keys\n%q\nwant those of the read without one and pull_request\n%q",
				fixture.key, keys, want)
		}
		delete(after, "pull_request")
		for key, value := range before {
			if !reflect.DeepEqual(after[key], value) {
				t.Errorf("the read of %s answers %s = %v once it names a pull request, and %v before", fixture.key, key, after[key], value)
			}
		}
	})
}
