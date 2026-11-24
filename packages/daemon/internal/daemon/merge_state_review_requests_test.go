package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The record keeps how many review requests GitHub holds open on a pull request beside its merge
// state, since a `blocked` head with one open waits for that review rather than ending its round
// (classify.MergeOutcomeOf). A GET /pulls/{number} answers them as `requested_reviewers` and
// `requested_teams`, people, Apps and teams alike, and both count; an answer that holds neither
// list is no count at all, which the round does not end on, never a count of zero.
func TestAReadOfOnePullRequestCountsItsOpenReviewRequests(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       *int
	}{
		{"two reviewers and a team", `"requested_reviewers":[{"login":"octocat"},{"login":"legion-reviewer[bot]"}],"requested_teams":[{"slug":"engineering"}]`, new(3)},
		{"two empty lists", `"requested_reviewers":[],"requested_teams":[]`, new(0)},
		{"null lists", `"requested_reviewers":null,"requested_teams":null`, nil},
		{"no lists at all", `"title":"no lists"`, nil},
	} {
		var pull githubPullRequest
		if err := json.Unmarshal([]byte(`{"number":86,"state":"open","mergeable":true,"mergeable_state":"blocked",`+tc.body+`}`), &pull); err != nil {
			t.Fatal(err)
		}
		if got := pull.reviewsRequested(); (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("%s: the read counts %v review requests, want %v", tc.name, got, tc.want)
		}
	}
}

// A base's batch reads the count as `reviewRequests { totalCount }` and the head's rollup as the
// last commit's `statusCheckRollup` in its one query. The rollup speaks of the head only when the
// last commit is the head the answer names; a null rollup is a commit with no check and no status,
// so nothing of it runs; PENDING is a check or a status still running; and any word GitHub's
// schema does not have reads as no rollup.
func TestABatchAnswerReadsTheHeadsRollupOnlyForTheHeadItNames(t *testing.T) {
	node := func(head, commit, rollup string) graphQLPull {
		var pull graphQLPull
		body := batchNode(86, head, "MERGEABLE", "BLOCKED")
		body["commits"] = rollupOf(commit, rollup)
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &pull); err != nil {
			t.Fatal(err)
		}
		return pull
	}
	for _, tc := range []struct {
		name   string
		pull   graphQLPull
		checks record.HeadChecks
	}{
		{"success", node("h2", "h2", "SUCCESS"), record.HeadChecksConcluded},
		{"failure", node("h2", "h2", "FAILURE"), record.HeadChecksConcluded},
		{"a required status nobody reported", node("h2", "h2", "EXPECTED"), record.HeadChecksConcluded},
		{"no check and no status", node("h2", "h2", ""), record.HeadChecksConcluded},
		{"a check still running", node("h2", "h2", "PENDING"), record.HeadChecksRunning},
		{"a word the schema does not have", node("h2", "h2", "LATER"), record.HeadChecksUnread},
		{"the rollup of another commit", node("h2", "h1", "SUCCESS"), record.HeadChecksUnread},
		{"no commit at all", func() graphQLPull { p := node("h2", "h2", "SUCCESS"); p.Commits.Nodes = nil; return p }(), record.HeadChecksUnread},
	} {
		if got := tc.pull.githubPullRequest().batch.checks; got != tc.checks {
			t.Errorf("%s: the rollup reads %q, want %q", tc.name, got, tc.checks)
		}
	}
	nullCount := node("h2", "h2", "SUCCESS")
	nullCount.ReviewRequests = nil
	if got := nullCount.githubPullRequest().reviewsRequested(); got != nil {
		t.Errorf("a null reviewRequests counts %d, want no count", *got)
	}
}

// A base push records the count and the rollup the batch read beside the merge state, so a
// `blocked` head's round decides on what GitHub answered, never on a zero it did not.
func TestABasePushRecordsTheReviewRequestsAndRollupGitHubAnswered(t *testing.T) {
	t.Parallel()
	if query := chunkQuery([]int{86}); !strings.Contains(query, "reviewRequests { totalCount }") || !strings.Contains(query, "statusCheckRollup { state }") {
		t.Fatalf("the batch query asks %q, want each pull request's reviewRequests { totalCount } and its head's statusCheckRollup { state }", query)
	}
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		node := batchNode(86, "head-86", "MERGEABLE", "BLOCKED")
		node["reviewRequests"] = map[string]any{"totalCount": 2}
		node["commits"] = rollupOf("head-86", "PENDING")
		return http.StatusOK, batchAnswer(t, asked, node)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the batch's answer for 86 to be recorded", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked && rig.ended()
	})
	pr := recordedPull(t, pool, "CAPTURE-1")
	if pr.ReviewsRequested == nil || *pr.ReviewsRequested != 2 || pr.HeadChecks != record.HeadChecksRunning {
		t.Fatalf("the record holds %v review requests and checks %q for 86, want the 2 and the running rollup GitHub answered", pr.ReviewsRequested, pr.HeadChecks)
	}
}

// An error inside an answered pull request names one of its fields, and GitHub answers that field
// null. A batch that met one reads neither the count nor the rollup of that pull request - they are
// the nullable parts the round decides on - so the record holds no count and no rollup rather than
// none open and nothing running, and the base's reads keep reading it, since a `blocked` answer
// missing either is one the round cannot decide on (classify.MergeAnswerIncomplete).
func TestAFieldErrorLeavesTheCountAndRollupUnread(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		body := batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "BLOCKED"))
		if query > 1 {
			return http.StatusOK, body
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(body), &answer); err != nil {
			t.Errorf("decode the batch answer: %v", err)
		}
		answer["errors"] = []map[string]any{{"type": "FORBIDDEN", "path": []any{"repository", pullAlias(86), "reviewRequests"},
			"message": "Resource not accessible by integration"}}
		raw, err := json.Marshal(answer)
		if err != nil {
			t.Errorf("encode the batch answer: %v", err)
		}
		return http.StatusOK, string(raw)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the first answer for 86 to be recorded", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked
	})
	if first := recordedPull(t, pool, "CAPTURE-1"); rig.queries.Load() == 1 && (first.ReviewsRequested != nil || first.HeadChecks != record.HeadChecksUnread) {
		t.Fatalf("after the field error the record holds %v review requests and checks %q, want neither read", first.ReviewsRequested, first.HeadChecks)
	}
	testwait.Eventually(t, "the base's reads to read 86 again and end on a whole answer", func() bool {
		pr := recordedPull(t, pool, "CAPTURE-1")
		return rig.queries.Load() >= 2 && pr.ReviewsRequested != nil && pr.HeadChecks == record.HeadChecksConcluded && rig.ended()
	})
}

// A REST read answers no rollup of the head, so a read GitHub answers `blocked` and MERGEABLE asks
// GitHub's GraphQL API for it before the answer is applied (readHeadChecks); a GraphQL answer about
// another head, or none, is no rollup, which the readers read again for.
func TestAReadOfABlockedHeadAsksForItsRollup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		rollup string
		head   string
		want   record.HeadChecks
	}{
		{"a check still running", "PENDING", "code", record.HeadChecksRunning},
		{"every check done", "SUCCESS", "code", record.HeadChecksConcluded},
		{"an answer about another head", "SUCCESS", "older", record.HeadChecksUnread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			captureInTesting(t, pool)
			rest := `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`
			api := pullStandInWith(t, rest, func(asked []int) string {
				node := batchNode(86, tc.head, "MERGEABLE", "BLOCKED")
				node["commits"] = rollupOf(tc.head, tc.rollup)
				return batchAnswer(t, asked, node)
			})
			w := readRuntime(pool, api.URL, quietLogger())
			state, incomplete, _, err := w.readMergeState(context.Background(), "CAPTURE-1")
			if err != nil || state != record.MergeStateBlocked {
				t.Fatalf("the read answered %s, %v; want blocked", state, err)
			}
			if pr := recordedPull(t, pool, "CAPTURE-1"); pr.HeadChecks != tc.want {
				t.Fatalf("the record holds checks %q, want %q", pr.HeadChecks, tc.want)
			}
			if wantIncomplete := tc.want == record.HeadChecksUnread; incomplete != wantIncomplete {
				t.Fatalf("the read is incomplete = %t, want %t", incomplete, wantIncomplete)
			}
		})
	}
}

// pullStandInWith is pullStandIn whose GraphQL endpoint answers with graphQL, given the numbers
// the query asks for.
func pullStandInWith(t *testing.T, pull string, graphQL func(asked []int) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			_, _ = w.Write([]byte(pull))
		case "/repos/acme/widgets/pulls/86/reviews":
			_, _ = w.Write([]byte(`[]`))
		case "/graphql":
			_, _ = fmt.Fprint(w, graphQL(readBatchRequest(t, r)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
