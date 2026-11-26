package api

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// TestAProjectWithNoSourceListsThePullRequestsItsDayRowsCount: over deliverytest's history, the
// second project (DELIVX) holds no deploy source of its own, and its merges count for it through
// their issues (INFRA-978 R27; the history's attribution inputs are left unread, so the issue each
// pull request names is its attribution, leaveAttributionInputsUnread). Its pr-open-to-merge items
// list exactly the pull requests its day rows count, as a number and the rows behind it are one
// set.
func TestAProjectWithNoSourceListsThePullRequestsItsDayRowsCount(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Days: 5})
	leaveAttributionInputsUnread(t, database.Pool)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, fixture.To.Add(-time.Minute))
	project := fixture.SecondProject
	window := evals.Range{From: fixture.From, To: fixture.To}

	counted, err := evals.CountedIn(t.Context(), database.Pool, project, evals.KindDelivery, []string{""}, nil, window)
	if err != nil {
		t.Fatalf("read the items %s's day rows count: %v", project, err)
	}
	want := map[string]bool{}
	for _, item := range counted {
		if strings.HasPrefix(item.RowKey, "pull_request:") {
			want[item.RowKey] = true
		}
	}
	if len(want) == 0 {
		t.Fatalf("%s's day rows count no pull request over %s..%s, so the comparison compares nothing",
			project, window.From.Format(time.DateOnly), window.To.Format(time.DateOnly))
	}

	query := url.Values{
		"from": {window.From.Format(time.RFC3339)}, "to": {window.To.Format(time.RFC3339)},
		"row_kind": {evalRowPullRequest}, "limit": {"200"},
	}
	answer := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/"+project+"/evals/"+evals.SlugPROpenToMerge+"/items?"+query.Encode())
	got := map[string]bool{}
	for _, row := range answer.Items {
		repo, _ := row.Cols["repo"].(string)
		number, _ := row.Cols["number"].(float64)
		got[evals.PullRequestMember(repo, int(number))] = true
	}
	if listed, wanted := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(listed, wanted) {
		t.Errorf("%s's pr-open-to-merge items list %v, want the %d pull requests its day rows count, %v", project, listed, len(wanted), wanted)
	}
	t.Logf("%s's pr-open-to-merge items list the %d pull requests its day rows count", project, len(want))
}
