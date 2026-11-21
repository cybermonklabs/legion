package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue whose only link to its pull request is GitHub's /issues/ form of the number compares
// its checks with that pull request's head. `dispatch issue --external acme/widgets#7` stores
// https://github.com/acme/widgets/issues/7, and GitHub serves pull request #7 at that URL as well
// as at /pull/7. #7's branch, title, body and commit messages name no issue, and GitHub names no
// pull request for k1's commit, so the link is the one thing that can tie #7 to the issue: its
// write must owe a read of #7, and the attribution must take the link in either form.
//
// The control links /pull/7 by hand, the form that always worked: it shows the harness's owed read,
// pass and attribution can compare at all. The quiet case is an /issues/ link of a number that is
// an issue of GitHub's (#8, no pull request): it compares with nothing. Its owed read stays owed
// here until GitHub's own count proves the pull request absent (delivery's pullRequestAbsent), a
// query this fake does not answer.
func TestAnIssuesLinkOfAPullRequestsNumberComparesWithItsHead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		external string // `dispatch issue --external`'s reference; "" links by hand instead
		link     string // the link written by hand, when external is ""
		want     []int
	}{
		{"issues_link_of_a_pull_request", "acme/widgets#7", "", []int{7}},
		{"control_pull_link_by_hand", "", "https://github.com/acme/widgets/pull/7", []int{7}},
		{"quiet_issues_link_of_an_issue", "acme/widgets#8", "", []int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
				map[string]string{"key": "ISLK", "name": "Issues links"}, "alice"); response.Code != http.StatusCreated {
				t.Fatalf("create project ISLK: status=%d body=%s", response.Code, response.Body.String())
			}
			if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
				map[string]any{"project": "ISLK", "default": true}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("map acme/widgets to ISLK: status=%d body=%s", response.Code, response.Body.String())
			}
			body := map[string]any{"project": "ISLK", "title": "Linked by number", "spec": "A spec\n", "repo": "acme/widgets"}
			if tc.external != "" {
				body["external"] = tc.external
			}
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
			}
			issue := decodeBody[model.Issue](t, created)
			if tc.link != "" {
				if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
					map[string]any{"external_links": []map[string]string{{"url": tc.link}}}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("link %s by hand: status=%d body=%s", tc.link, response.Code, response.Body.String())
				}
			}
			uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
			recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
			fake.pullAnswers(func(repo, number string) string {
				if repo != "acme/widgets" || number != "7" {
					return ""
				}
				return fmt.Sprintf(`{"number":7,"title":"A change","body":"No key here.","state":"open","merged":false,
					"html_url":"https://github.com/acme/widgets/pull/7","updated_at":"2026-11-19T19:54:00Z",
					"head":{"ref":"work/no-key","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}`, headCommit)
			})
			if rows := headRowsOf(t, database, 7); rows != 0 {
				t.Fatalf("SETUP: %d rows of #7 before the pass, want none: GitHub names no pull request for k1's commit", rows)
			}

			onePass(t, database, served)
			k1 := checkOf(t, readResults(t, handler, issue.Key), "k1")
			var owed int
			if err := database.Pool.QueryRow(context.Background(),
				`select count(*) from pull_request_reads_owed where number in (7, 8)`).Scan(&owed); err != nil {
				t.Fatalf("count the owed reads: %v", err)
			}
			compared := pullRequestsComparedWith(k1)
			t.Logf("%s: rows_of_7=%d compared_with=%v compared_nothing=%q owed_reads_left=%d",
				tc.name, headRowsOf(t, database, 7), compared, k1.ComparedNothing, owed)
			if !slices.Equal(compared, tc.want) {
				t.Fatalf("after one pass k1 compares with %v, want %v: an issue whose only link is /issues/7 never reaches pull request #7", compared, tc.want)
			}
			if len(tc.want) > 0 && owed != 0 {
				t.Fatalf("after one pass %d owed reads of #7 are left, want none: GitHub answered for #7", owed)
			}
		})
	}
}
