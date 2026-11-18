package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mergedPull is GitHub's answer for pull request number of repo, merged at head sha, with that
// title and body and a branch that names no issue.
func mergedPull(repo string, number int, sha, title, body string) string {
	return fmt.Sprintf(`{"number":%d,"title":%q,"body":%q,"html_url":"https://github.com/%s/pull/%d",
		"state":"closed","merged":true,"merged_at":"2026-11-05T07:15:08Z","updated_at":"2026-11-05T07:15:08Z",
		"head":{"ref":"feature/no-key","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
		number, title, body, repo, number, sha, repo)
}

// mergedPullAt is GitHub's answer for a commit that is the head of one merged pull request with
// that title and body, in whichever repository was asked about.
func mergedPullAt(number int, title, body string) func(repo, sha string) string {
	return func(repo, sha string) string { return "[" + mergedPull(repo, number, sha, title, body) + "]" }
}

// patchIssue sends one PATCH of an issue and fails the test unless it answers 200.
func patchIssue(t *testing.T, handler http.Handler, key string, body map[string]any) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, body, "alice"); response.Code != http.StatusOK {
		t.Fatalf("patch %s with %v: status=%d body=%s", key, body, response.Code, response.Body.String())
	}
}

// githubIssueLink is the external_links value of one link to a GitHub issue.
func githubIssueLink(url string) []map[string]string {
	return []map[string]string{{"url": url, "kind": "github_issue"}}
}

// A tie to the check's issue made after its pull request merged, where the result was recorded
// while its record stored no row for the commit: the merged row is stored by the writer of a read
// of GitHub, with the texts of GitHub's answer. At the results read the statement gives the tie
// from those stored facts, so the result stands at the merged head and the read asks GitHub
// nothing. A merged row is read again by no pass of Dispatch's once the current rule has judged
// it, and the statement needs no read. Four ties no event carries:
//
//   - a link of the check's issue to a GitHub issue the pull request's body cites;
//   - a parent, with the pull request's repository, given to the issue the title names;
//   - the pull request's repository made the default of the issue's project;
//   - a cited GitHub issue's link moved from the row's issue to the check's issue.
func TestATieMadeAfterAMergeIsFollowedAtTheResultsRead(t *testing.T) {
	for _, kase := range []struct {
		name string
		// setUp makes the issues and answers the check's issue, the issue the merged row is
		// stored under ("" for none) and GitHub's title and body; tie makes the tie.
		setUp func(t *testing.T, handler http.Handler) (check, stored, title, body string)
		tie   func(t *testing.T, handler http.Handler, check, stored string)
	}{
		{
			name: "a_link_to_a_github_issue_its_body_cites",
			setUp: func(t *testing.T, handler http.Handler) (string, string, string, string) {
				return issueInRepository(t, handler, "RMTA", "acme/widgets", "Cited").Key, "", "A change", "Fixes #81."
			},
			tie: func(t *testing.T, handler http.Handler, check, _ string) {
				patchIssue(t, handler, check, map[string]any{"external_links": githubIssueLink("https://github.com/acme/widgets/issues/81")})
			},
		},
		{
			name: "a_parent_with_the_repository_of_the_pull_request",
			setUp: func(t *testing.T, handler http.Handler) (string, string, string, string) {
				child := issueInRepository(t, handler, "RMTB", "acme/gadgets", "Child")
				if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
					map[string]any{"project": "RMTB"}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("map acme/widgets to RMTB: status=%d body=%s", response.Code, response.Body.String())
				}
				created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
					map[string]any{"project": "RMTB", "title": "Parent", "spec": "A spec\n", "repo": "acme/widgets"}, "alice")
				if created.Code != http.StatusCreated {
					t.Fatalf("create the parent: status=%d body=%s", created.Code, created.Body.String())
				}
				return child.Key, "", child.Key + " the work", ""
			},
			tie: func(t *testing.T, handler http.Handler, check, _ string) {
				patchIssue(t, handler, check, map[string]any{"parent": "RMTB-2"})
			},
		},
		{
			name: "the_repository_of_the_pull_request_made_the_projects_default",
			setUp: func(t *testing.T, handler http.Handler) (string, string, string, string) {
				issue := issueInRepository(t, handler, "RMTC", "acme/gadgets", "Mapped")
				return issue.Key, "", issue.Key + " the work", ""
			},
			tie: func(t *testing.T, handler http.Handler, _, _ string) {
				if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
					map[string]any{"project": "RMTC", "default": true}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("map acme/widgets to RMTC as its default: status=%d body=%s", response.Code, response.Body.String())
				}
			},
		},
		{
			name: "a_cited_github_issues_link_moved_from_the_rows_issue",
			setUp: func(t *testing.T, handler http.Handler) (string, string, string, string) {
				first := issueInRepository(t, handler, "RMTD", "acme/widgets", "First")
				second := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
					map[string]any{"project": "RMTD", "title": "Second", "spec": "A spec\n"}, "alice"))
				patchIssue(t, handler, first.Key, map[string]any{"external_links": githubIssueLink("https://github.com/acme/widgets/issues/82")})
				return second.Key, first.Key, "A change", "Fixes #82."
			},
			tie: func(t *testing.T, handler http.Handler, check, stored string) {
				patchIssue(t, handler, stored, map[string]any{"external_links": []map[string]string{}})
				patchIssue(t, handler, check, map[string]any{"external_links": githubIssueLink("https://github.com/acme/widgets/issues/82")})
			},
		},
	} {
		t.Run(kase.name, func(t *testing.T) {
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			check, stored, title, body := kase.setUp(t, handler)
			uploadDocument(t, handler, "/api/v1/issues/"+check, "verification.md", twoChecks)
			// GitHub names no pull request for the commit at the record, so the record stores no
			// row for it.
			recorded := decodeBody[recordedResult](t, recordResult(t, handler, check, resultBody("k1", "pass", headCommit, nil)))
			// The pull request merges at that commit, and a read of GitHub stores its row and the
			// texts GitHub answered. The row of the fourth case stores the issue an older rule gave
			// it, which no reader answers for a row with texts.
			storeMergedRowWithTexts(t, database, 81, title, body, stored)
			kase.tie(t, handler, check, stored)
			fake.answers(mergedPullAt(81, title, body))
			served.commitPulls = delivery.CommitPullReads{}
			spent := fake.asks("acme/widgets")

			read := readResults(t, handler, check)
			asked := fake.asks("acme/widgets") - spent
			t.Logf("tie=%s asks=%d compared_with=%+v", kase.name, asked, checkOf(t, read, "k1").ComparedWith)
			standsAt(t, read, "k1", recorded.Result.ID,
				"a result at the merged head of a pull request tied to the check's issue after its merge", model.CommitAtBasis, false)
			if asked != 0 {
				t.Errorf("the read asked GitHub %d times, want none: the row has its texts, so the statement gives the tie", asked)
			}
		})
	}
}

// storeMergedRowWithTexts stores acme/widgets#number merged at headCommit, with title and body,
// through the writer of a read of GitHub, with the texts that GitHub answered: its branch names
// no issue, and its commit messages are read and empty. stored is the issue the row stores, ""
// for none, which one update writes, as a row of an older release holds it.
func storeMergedRowWithTexts(t *testing.T, database *store.Store, number int, title, body, stored string) {
	t.Helper()
	if err := delivery.UpsertPullRequestHead(context.Background(), database.Pool, delivery.PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: headCommit, HeadRef: "feature/no-key", BaseRef: "main",
		State: "merged", Title: title, URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		Texts: &delivery.PullRequestTexts{HeadRepo: "acme/widgets", BaseRef: "main", Title: title, Body: body,
			CommitMessages: []string{}, MessagesRead: true},
	}); err != nil {
		t.Fatalf("store the merged row of #%d with its texts: %v", number, err)
	}
	if stored == "" {
		return
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update pull_request_heads set issue_key = $2, attribution_source = 'github_issue_link' where repo = 'acme/widgets' and number = $1
	`, number, stored); err != nil {
		t.Fatalf("store the issue of #%d as an older rule gave it: %v", number, err)
	}
}

// A result whose commit heads a merged pull request beside an open pull request of the same head
// commit compares at the first read (B1 of review 5, in this build's form). The merged #81 is
// stored through the writer of a read of GitHub, with the texts of GitHub's answer (body "Fixes
// #81."). In the second subtest an open #91 of another pull request heads the same commit, stored
// by SQL. The check's issue then links GitHub issue #81. The result stands at its basis at the
// first read, and the read asks GitHub nothing, in both subtests.
func TestAMergedHeadBesideAnOpenRowOfTheSameCommitComparesAtTheFirstRead(t *testing.T) {
	for _, withOpen := range []bool{false, true} {
		name := map[bool]string{false: "the_merged_row_alone", true: "with_an_open_row_of_another_pull_request_at_the_commit"}[withOpen]
		t.Run(name, func(t *testing.T) {
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			check := issueInRepository(t, handler, "RVFA", "acme/widgets", "Cited").Key
			uploadDocument(t, handler, "/api/v1/issues/"+check, "verification.md", twoChecks)
			recorded := decodeBody[recordedResult](t, recordResult(t, handler, check, resultBody("k1", "pass", headCommit, nil)))
			storeMergedRowWithTexts(t, database, 81, "A change", "Fixes #81.", "")
			if withOpen {
				if _, err := database.Pool.Exec(context.Background(), `
					insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, url, event_at, checked_at)
					values ('acme/widgets', 91, $1, 'feature/no-key', 'release', 'open', 'https://github.com/acme/widgets/pull/91', now(), now())
				`, headCommit); err != nil {
					t.Fatalf("store the open row of #91: %v", err)
				}
			}
			patchIssue(t, handler, check, map[string]any{"external_links": githubIssueLink("https://github.com/acme/widgets/issues/81")})
			fake.answers(func(repo, sha string) string {
				pulls := mergedPull(repo, 81, sha, "A change", "Fixes #81.")
				if withOpen {
					pulls += "," + fmt.Sprintf(`{"number":91,"title":"A backport","body":"","html_url":"https://github.com/%s/pull/91",
						"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
						"head":{"ref":"feature/no-key","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"release"}}`, repo, sha, repo)
				}
				return "[" + pulls + "]"
			})
			served.commitPulls = delivery.CommitPullReads{}
			spent := fake.asks("acme/widgets")
			first := readResults(t, handler, check)
			asked := fake.asks("acme/widgets") - spent
			t.Logf("case=%s asks=%d compared_with=%+v", name, asked, checkOf(t, first, "k1").ComparedWith)
			standsAt(t, first, "k1", recorded.Result.ID, "the first read after the tie", model.CommitAtBasis, false)
			if asked != 0 {
				t.Errorf("the first read asked GitHub %d times, want none", asked)
			}
		})
	}
}

// What the read-through costs for a child's merged head on its parent's tab: one GitHub call for
// a commit that heads no stored row. The child's merged pull request has no stored row, so the
// first read of the parent's results reads the commit through, and stores the merged row that
// GitHub names, credited to the child. From then on a merged row heads the commit, so no read
// asks again (delivery.HeadStoredAt). Three reads in a row ask GitHub once.
func TestAChildsMergedHeadOnTheParentsTabIsReadThroughOnceAWindow(t *testing.T) {
	handler, _, served, fake := readThroughGitHub(t, "acme/widgets")
	parent := issueInRepository(t, handler, "RMTE", "acme/widgets", "Parent")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RMTE", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	recorded := decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil)))
	fake.answers(mergedPullAt(98, child.Key+" the work", ""))
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/widgets")

	for read := 1; read <= 3; read++ {
		standsAt(t, readResults(t, handler, parent.Key), "k1", recorded.Result.ID,
			fmt.Sprintf("read %d of the child's result at the child's merged head", read), model.CommitOtherCommit, false)
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Fatalf("three reads of the parent's results asked GitHub %d times for the child's merged head, want 1", asked)
	}
}

// What the read never spends a GitHub call on: a result at a commit that heads a merged row
// credited to the check's own issue. The row is a basis of the check, so the result stands at it,
// and the read reads through no result at a basis (comparisonReader.learn). The control reads
// every result through, at a basis or not: one ask.
func TestAMergedHeadOfTheChecksOwnIssueIsNotReadThrough(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RMTF", "acme/widgets", "Own")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	writeHead(t, database, "acme/widgets", 83, headCommit, "merged", issue.Key)
	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	fake.answers(mergedPullAt(83, issue.Key+" the work", ""))
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/widgets")

	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"a result at the merged head of the check's own pull request", model.CommitAtBasis, false)
	if asked := fake.asks("acme/widgets") - spent; asked != 0 {
		t.Fatalf("the read asked GitHub %d times for a commit a merged row of the check's own issue heads, want none", asked)
	}
}

// onePass runs the head arm of one reconcile pass over database: every row
// delivery.ListStalePullRequestHeads hands it, read from GitHub (delivery.RefreshPullRequestHead).
func onePass(t *testing.T, database *store.Store, served *server) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	stale, err := delivery.ListStalePullRequestHeads(ctx, database.Pool, delivery.PullRequestHeadBatch, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, head := range stale {
		if _, _, err := delivery.RefreshPullRequestHead(ctx, database.Pool, served.deps.GitHub, nil, head.Repo, head.Number, now); err != nil {
			t.Fatalf("read %s#%d: %v", head.Repo, head.Number, err)
		}
	}
}

// writeReadMergedRow stores the row a complete read of GitHub leaves for pull request number of
// repo, merged at sha: its URL, credited to issueKey ("" for none), read now and judged by the
// current rule (delivery.AttributionRuleVersion), so the reconcile offers it no read of its own.
func writeReadMergedRow(t *testing.T, database *store.Store, repo string, number int, sha, issueKey string) {
	t.Helper()
	var attributed *string
	if issueKey != "" {
		attributed = &issueKey
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, url, event_at, checked_at, attribution_rule)
		values ($1, $2, $3, 'feature/no-key', 'main', 'merged', $4, $5, now(), now(), $6)
	`, repo, number, sha, attributed, fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), delivery.AttributionRuleVersion); err != nil {
		t.Fatalf("store the merged row of %s#%d: %v", repo, number, err)
	}
}

// comparedWithPull reports whether a check compares with the head of pull request number.
func comparedWithPull(t *testing.T, read issueResults, blockID string, number int) bool {
	t.Helper()
	for _, basis := range checkOf(t, read, blockID).ComparedWith {
		if basis.PullRequest != nil && *basis.PullRequest == number {
			return true
		}
	}
	return false
}

// checkedAt is the checked_at of one stored head row, nil where no read has reached it since its
// last write or mark.
func checkedAt(t *testing.T, database *store.Store, repo string, number int) *time.Time {
	t.Helper()
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select checked_at from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&at); err != nil {
		t.Fatalf("read the head of %s#%d: %v", repo, number, err)
	}
	return at
}

// A PATCH that links an issue to a merged pull request, or unlinks it, marks the pull request's
// stored row, and the reconcile's next pass attributes the row with the links as they stand, with
// no event of the pull request and no result at its head. The PATCH asks GitHub nothing. Between
// the PATCH and the pass the results route compares the issue's checks with the row as it was. A
// link to a URL that is no pull request's, and one to a pull request Dispatch stores no row for,
// mark nothing; the second is read through at a results read of a result at its head, as at main.
func TestALinkWrittenToAMergedPullRequestIsFollowedByTheNextPass(t *testing.T) {
	t.Run("a_link_added", func(t *testing.T) {
		handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
		issue := issueInRepository(t, handler, "RMTG", "acme/widgets", "Linked after the merge")
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
		writeReadMergedRow(t, database, "acme/widgets", 84, headCommit, "")
		fake.pullAnswers(func(repo, number string) string {
			if repo != "acme/widgets" || number != "84" {
				return ""
			}
			return mergedPull(repo, 84, headCommit, "A change", "")
		})
		spent := fake.asks("acme/widgets")
		patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/widgets/pull/84"}}})
		marked := checkedAt(t, database, "acme/widgets", 84)
		between := comparedWithPull(t, readResults(t, handler, issue.Key), "k1", 84)
		onePass(t, database, served)
		after := comparedWithPull(t, readResults(t, handler, issue.Key), "k1", 84)
		t.Logf("checked_at_after_patch=%v compared_between=%v compared_after_one_pass=%v patch_asks=%d", marked, between, after, fake.asks("acme/widgets")-spent)
		if marked != nil {
			t.Errorf("the PATCH that linked #84 left its row read at %s, want it marked unread", marked.Format(time.RFC3339Nano))
		}
		if between {
			t.Errorf("between the PATCH and the pass the check compares with #84, want the row as it was")
		}
		if !after {
			t.Errorf("after one pass the check does not compare with #84, which the issue links")
		}
	})
	t.Run("a_link_removed", func(t *testing.T) {
		handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
		issue := issueInRepository(t, handler, "RMTH", "acme/widgets", "Unlinked after the merge")
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
		patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/widgets/pull/85"}}})
		writeReadMergedRow(t, database, "acme/widgets", 85, headCommit, issue.Key)
		fake.pullAnswers(func(repo, number string) string {
			if repo != "acme/widgets" || number != "85" {
				return ""
			}
			return mergedPull(repo, 85, headCommit, "A change", "")
		})
		patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{}})
		marked := checkedAt(t, database, "acme/widgets", 85)
		onePass(t, database, served)
		after := comparedWithPull(t, readResults(t, handler, issue.Key), "k1", 85)
		t.Logf("checked_at_after_patch=%v compared_after_one_pass=%v", marked, after)
		if marked != nil {
			t.Errorf("the PATCH that unlinked #85 left its row read at %s, want it marked unread", marked.Format(time.RFC3339Nano))
		}
		if after {
			t.Errorf("after one pass the check still compares with #85, which the issue no longer links")
		}
	})
	t.Run("a_link_to_no_pull_request_and_one_to_a_pull_request_with_no_row", func(t *testing.T) {
		handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
		issue := issueInRepository(t, handler, "RMTI", "acme/widgets", "Links that mark nothing")
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
		writeReadMergedRow(t, database, "acme/widgets", 86, newHeadCommit, "")
		before := checkedAt(t, database, "acme/widgets", 86)
		patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{
			{"url": "https://example.com/a-design-note"}, {"url": "https://github.com/acme/widgets/pull/87"},
		}})
		after := checkedAt(t, database, "acme/widgets", 86)
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from pull_request_heads`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if before == nil || after == nil || !after.Equal(*before) || rows != 1 {
			t.Errorf("the PATCH changed the row of #86 from checked_at %v to %v, %d rows stored: want the one row as it was", before, after, rows)
		}
		// #87 has no row, so a result at its head is read through, as at main.
		recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
		fake.answers(func(repo, sha string) string {
			return "[" + fmt.Sprintf(`{"number":87,"title":"A change","body":"","html_url":"https://github.com/%s/pull/87",
				"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
				"head":{"ref":"feature/no-key","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`, repo, sha, repo) + "]"
		})
		served.commitPulls = delivery.CommitPullReads{}
		spent := fake.asks("acme/widgets")
		standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
			"a result at the head of a linked pull request Dispatch stored no row for", model.CommitAtBasis, false)
		if asked := fake.asks("acme/widgets") - spent; asked != 1 {
			t.Errorf("the read asked GitHub %d times for the commit of the linked pull request with no row, want 1", asked)
		}
	})
}
