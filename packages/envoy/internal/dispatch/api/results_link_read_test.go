package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// linkedOpenPull is GitHub's answer for the open pull request number of repo at head sha. Its
// title, body and branch name no issue, so only a link ties it to an issue.
func linkedOpenPull(repo string, number int, sha string) string {
	return fmt.Sprintf(`{"number":%d,"title":"A change","body":"","html_url":"https://github.com/%s/pull/%d",
		"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
		"head":{"ref":"feature/no-key","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
		number, repo, number, sha, repo)
}

// A person links an open pull request to an issue by hand, with a PATCH of external_links (R5 of
// the results lookup). Dispatch has no event, no result and no head row of the pull request, and
// GitHub holds its head. One reconcile pass reads the pull request from GitHub and stores its head
// row. The results read of the issue then compares check k1 with that head. The test fails where
// the link write owes no read: then no step reads the pull request, and no head row is stored.
func TestALinkByHandStoresTheHeadOfThePullRequestItLinks(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/tools")
	issue := issueInRepository(t, handler, "ACME", "acme/tools", "Linked by hand")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	fake.pullAnswers(func(repo, number string) string {
		if repo != "acme/tools" || number != "12" {
			return ""
		}
		return linkedOpenPull(repo, 12, headCommit)
	})
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/tools/pull/12"}}})
	onePass(t, database, served)
	var sha string
	var readAt *time.Time
	err := database.Pool.QueryRow(context.Background(), `
		select head_sha, github_read_at from pull_request_heads where repo = 'acme/tools' and number = 12
	`).Scan(&sha, &readAt)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("after the link by hand and one pass, no head row of acme/tools#12 is stored; want the head that GitHub holds, %s", headCommit)
	}
	if err != nil {
		t.Fatalf("read the head row of acme/tools#12: %v", err)
	}
	if sha != headCommit || readAt == nil {
		t.Fatalf("the head row of acme/tools#12 holds %s with github_read_at %v; want %s, from a read of GitHub", sha, readAt, headCommit)
	}
	read := readResults(t, handler, issue.Key)
	if !comparedWithPull(t, read, "k1", 12) {
		check := checkOf(t, read, "k1")
		t.Fatalf("check k1 compares with %+v for the reason %q; want the head of acme/tools#12", check.ComparedWith, check.ComparedNothing)
	}
}

// A link by hand by the name a repository had before a rename gives the linking issue the head
// of its pull request (R5 of the results lookup), as a link by the name it has today does
// (TestALinkByHandStoresTheHeadOfThePullRequestItLinks): oldco/tools is renamed to acme/tools, and
// the issue links https://github.com/oldco/tools/pull/13. The link owes the read under acme/tools,
// one pass stores the head that GitHub holds, and check k1 compares with it. When the statement
// matched a link with the stored URL alone, the head row was stored and k1 compared with nothing,
// which main's linked reader did not do (review of #4209, B1). The link check and the resolve read
// a link by the same rule: another issue's link of acme/tools/pull/13 is refused with 409
// EXTERNAL_LINK_TAKEN, and the reference acme/tools#13 resolves to the issue that links it.
func TestALinkByHandByTheNameBeforeARenameGivesItsIssueTheHead(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/tools")
	issue := issueInRepository(t, handler, "ACME", "acme/tools", "Linked by the old name")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/oldco/tools",
		map[string]any{"project": "ACME", "renamed_to": "acme/tools"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("record oldco/tools as renamed to acme/tools: status=%d body=%s", response.Code, response.Body.String())
	}
	fake.pullAnswers(func(repo, number string) string {
		if repo != "acme/tools" || number != "13" {
			return ""
		}
		return linkedOpenPull(repo, 13, headCommit)
	})
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/oldco/tools/pull/13"}}})
	if owed := owedReads(t, database); !slices.Equal(owed, []string{"acme/tools#13 failure"}) {
		t.Fatalf("after a link to oldco/tools#13, the owed reads are %q; want the read of acme/tools#13", owed)
	}
	onePass(t, database, served)
	if !comparedWithPull(t, readResults(t, handler, issue.Key), "k1", 13) {
		check := checkOf(t, readResults(t, handler, issue.Key), "k1")
		t.Fatalf("after the link by the old name and one pass, check k1 compares with %+v for the reason %q; want the head of acme/tools#13", check.ComparedWith, check.ComparedNothing)
	}

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "ACME", "title": "Linked by the new name", "spec": "A spec\n"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a second issue of ACME: status=%d body=%s", created.Code, created.Body.String())
	}
	other := decodeBody[model.Issue](t, created)
	taken := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+other.Key,
		map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/tools/pull/13"}}}, "alice")
	if taken.Code != http.StatusConflict || !strings.Contains(taken.Body.String(), `"code":"EXTERNAL_LINK_TAKEN"`) || !strings.Contains(taken.Body.String(), issue.Key) {
		t.Fatalf("a link of acme/tools/pull/13 on %s, which %s links by the old name: status=%d body=%s; want 409 EXTERNAL_LINK_TAKEN naming %s",
			other.Key, issue.Key, taken.Code, taken.Body.String(), issue.Key)
	}
	resolved := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/resolve?ref=acme/tools%2313", nil, "alice")
	if resolved.Code != http.StatusOK || !strings.Contains(resolved.Body.String(), issue.Key) {
		t.Fatalf("resolve acme/tools#13: status=%d body=%s; want %s", resolved.Code, resolved.Body.String(), issue.Key)
	}
}

// A link by hand asks GitHub for the pull request it links only where Dispatch holds no head of
// that pull request (R5 of the results lookup). In the first case Dispatch holds no head of
// acme/tools#12: the link owes a read, one pass reads #12 from GitHub one time and stores its
// head, and the linking issue's check k1 compares with it. In the second case Dispatch holds that
// head, from that read: the link moves to another issue, which owes no read, so the next pass asks
// GitHub nothing about #12, and the new issue's check k1 compares with the held head at once (R7:
// a new link reaches a stored pull request with no read of GitHub). The held head is found under
// the repository's canonical name in any letter case: a link of https://github.com/ACME/Tools/pull/12
// owes no read, and nor does a link of acme/tools#12 where the head row is stored under
// oldco/tools, the name the repository had before its rename to acme/tools (oweLinkedRead), as a
// release before the rename stored it. The test fails where a link to a held head owes a read, in
// either of those spellings, and where a link to a head Dispatch does not hold owes none.
func TestALinkByHandAsksGitHubForAPullRequestOnlyWhereItsHeadIsNotHeld(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/tools")
	first := issueInRepository(t, handler, "ACME", "acme/tools", "Linked first")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "ACME", "title": "Linked next", "spec": "A spec\n"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the second issue of ACME: status=%d body=%s", created.Code, created.Body.String())
	}
	second := decodeBody[model.Issue](t, created)
	uploadDocument(t, handler, "/api/v1/issues/"+first.Key, "verification.md", twoChecks)
	uploadDocument(t, handler, "/api/v1/issues/"+second.Key, "verification.md", twoChecks)
	fake.pullAnswers(func(repo, number string) string {
		if repo != "acme/tools" || number != "12" {
			return ""
		}
		return linkedOpenPull(repo, 12, headCommit)
	})
	link := []map[string]string{{"url": "https://github.com/acme/tools/pull/12"}}

	t.Run("the_head_is_not_held", func(t *testing.T) {
		patchIssue(t, handler, first.Key, map[string]any{"external_links": link})
		if owed := owedReads(t, database); !slices.Equal(owed, []string{"acme/tools#12 failure"}) {
			t.Fatalf("after a link to acme/tools#12, whose head Dispatch does not hold, the owed reads are %q; want the read of #12", owed)
		}
		onePass(t, database, served)
		if reads := fake.pullReadsOf("acme/tools", 12); reads != 1 {
			t.Fatalf("after the link, one pass read acme/tools#12 from GitHub %d times; want 1", reads)
		}
		if !comparedWithPull(t, readResults(t, handler, first.Key), "k1", 12) {
			t.Fatalf("after the link and one pass, %s's check k1 does not compare with the head of acme/tools#12", first.Key)
		}
	})

	t.Run("the_head_is_held", func(t *testing.T) {
		patchIssue(t, handler, first.Key, map[string]any{"external_links": []map[string]string{}})
		patchIssue(t, handler, second.Key, map[string]any{"external_links": link})
		if owed := owedReads(t, database); len(owed) != 0 {
			t.Fatalf("after a link to acme/tools#12, whose head Dispatch holds, the owed reads are %q; want none", owed)
		}
		if !comparedWithPull(t, readResults(t, handler, second.Key), "k1", 12) {
			t.Fatalf("at once after the link, %s's check k1 does not compare with the held head of acme/tools#12", second.Key)
		}
		onePass(t, database, served)
		if reads := fake.pullReadsOf("acme/tools", 12); reads != 1 {
			t.Fatalf("after the link to the held head and one more pass, GitHub served acme/tools#12 %d times in all; want 1, the first case's read alone", reads)
		}
		if !comparedWithPull(t, readResults(t, handler, second.Key), "k1", 12) {
			t.Fatalf("after the link and one pass, %s's check k1 does not compare with the head of acme/tools#12", second.Key)
		}
	})

	t.Run("the_head_is_held_under_other_letters", func(t *testing.T) {
		patchIssue(t, handler, second.Key, map[string]any{"external_links": []map[string]string{}})
		patchIssue(t, handler, first.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/ACME/Tools/pull/12"}}})
		if owed := owedReads(t, database); len(owed) != 0 {
			t.Fatalf("after a link to ACME/Tools#12, whose head Dispatch holds as acme/tools#12, the owed reads are %q; want none", owed)
		}
	})

	t.Run("the_head_is_stored_under_the_name_before_a_rename", func(t *testing.T) {
		patchIssue(t, handler, first.Key, map[string]any{"external_links": []map[string]string{}})
		// The row of #12 as a release before the rename stored it, under the old name; the owes that
		// this write and the rename cause are cleared, so only the link's owe is read below.
		if _, err := database.Pool.Exec(context.Background(),
			`update pull_request_heads set repo = 'oldco/tools' where repo = 'acme/tools' and number = 12`); err != nil {
			t.Fatalf("store #12's head row under oldco/tools: %v", err)
		}
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/oldco/tools",
			map[string]any{"project": "ACME", "renamed_to": "acme/tools"}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("record oldco/tools as renamed to acme/tools: status=%d body=%s", response.Code, response.Body.String())
		}
		if _, err := database.Pool.Exec(context.Background(), `delete from pull_request_reads_owed`); err != nil {
			t.Fatalf("clear the owed reads: %v", err)
		}
		patchIssue(t, handler, second.Key, map[string]any{"external_links": link})
		if owed := owedReads(t, database); len(owed) != 0 {
			t.Fatalf("after a link to acme/tools#12, whose head row Dispatch holds under oldco/tools, the name before the rename, the owed reads are %q; want none", owed)
		}
	})
}

// owedReads is each read that pull_request_reads_owed holds, as "<repo>#<number> <reason>", in
// order.
func owedReads(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select repo || '#' || number || ' ' || reason from pull_request_reads_owed order by 1`)
	if err != nil {
		t.Fatalf("read the owed reads: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the owed reads: %v", err)
	}
	return owed
}

// A link by hand owes a read for each URL of a pull request that it adds and whose head Dispatch
// does not hold: also in a repository that no project maps and the App reads (R5 of the results
// lookup). A link to a pull request whose head row is stored owes nothing: Dispatch holds that
// head already. GitHub serves a pull request at /issues/<n> as well as at /pull/<n>, so the
// /issues/ link of #31 owes a read too, which tells whether #31 is a pull request. Three links owe
// nothing, because none of them is the URL of a pull request:
//
//   - a page of a pull request;
//   - a URL of another site;
//   - a number with a zero before it.
//
// A link held before the write, in other letters, owes nothing again.
func TestALinkByHandOwesAReadForEachURLOfAPullRequestAndForNoOtherURL(t *testing.T) {
	handler, database, _, _ := readThroughGitHub(t, "acme/tools", "acme/elsewhere")
	issue := issueInRepository(t, handler, "ACME", "acme/tools", "Links by hand")
	writeReadMergedRow(t, database, "acme/tools", 30, headCommit, "")
	others := []map[string]string{
		{"url": "https://github.com/acme/tools/issues/31", "kind": "github_issue"},
		{"url": "https://github.com/acme/tools/pull/32/files"},
		{"url": "https://example.com/acme/tools/pull/33"},
		{"url": "https://github.com/acme/tools/pull/035"},
	}
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": others})
	if owed := owedReads(t, database); !slices.Equal(owed, []string{"acme/tools#31 failure"}) {
		t.Fatalf("after an /issues/ link and three links that are no URL of a pull request, the owed reads are %q; want the read of #31 alone", owed)
	}
	pulls := []map[string]string{
		{"url": "https://github.com/acme/elsewhere/pull/34"},
		{"url": "https://github.com/acme/tools/pull/30"},
		{"url": "https://github.com/acme/tools/pull/36"},
	}
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": append(slices.Clone(others), pulls...)})
	want := []string{"acme/elsewhere#34 failure", "acme/tools#31 failure", "acme/tools#36 failure"}
	if owed := owedReads(t, database); !slices.Equal(owed, want) {
		t.Fatalf("after links to acme/elsewhere#34 (no project maps its repository), acme/tools#30 (its head row is stored, so it owes nothing) and acme/tools#36 (no row), the owed reads are %q; want %q", owed, want)
	}
	if _, err := database.Pool.Exec(context.Background(), `delete from pull_request_reads_owed`); err != nil {
		t.Fatalf("settle the owed reads: %v", err)
	}
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": append(slices.Clone(others),
		map[string]string{"url": "https://GitHub.com/Acme/Elsewhere/pull/34"},
		map[string]string{"url": "https://github.com/ACME/tools/pull/30"},
		map[string]string{"url": "https://GitHub.com/Acme/Tools/pull/36"})})
	if owed := owedReads(t, database); len(owed) != 0 {
		t.Fatalf("after the same links in other letters, the owed reads are %q; want none", owed)
	}
}

// A person links an open pull request of acme/loose to an issue whose own repository is acme/loose.
// No project maps acme/loose, and the App reads it. No event and no result come. One reconcile pass
// reads the pull request from GitHub one time (a GET of the pull request) and stores its head, and
// the results read then compares check k1 with that head (R5 of the results lookup). That read
// reads no commit messages, since the link names the issue; the same pass's facts line asks its
// batched query for them (readFactsLine), which this test does not count. The test fails where the
// link owes a read only in a repository that a project maps: then no step reads the pull request,
// and k1 compares with nothing.
func TestALinkByHandInARepositoryThatNoProjectMapsGetsItsHeadFromOneRead(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/loose")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": "LOOS", "name": "Loose"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project LOOS: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "LOOS", "title": "Linked in a repository that no project maps", "spec": "A spec\n", "repo": "acme/loose"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue in acme/loose: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	if issue.ResolvedRepo == nil || *issue.ResolvedRepo != "acme/loose" {
		t.Fatalf("SETUP: the issue resolves to %v, want its own acme/loose", issue.ResolvedRepo)
	}
	var mappings int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from repo_projects`).Scan(&mappings); err != nil || mappings != 0 {
		t.Fatalf("SETUP: repo_projects holds %d rows (%v), want none", mappings, err)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	fake.pullAnswers(func(repo, number string) string {
		if repo != "acme/loose" || number != "12" {
			return ""
		}
		return linkedOpenPull(repo, 12, headCommit)
	})
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/loose/pull/12"}}})
	onePass(t, database, served)
	if reads := fake.pullReadsOf("acme/loose", 12); reads != 1 {
		t.Errorf("after the link by hand, one pass read acme/loose#12 from GitHub %d times; want 1", reads)
	}
	read := readResults(t, handler, issue.Key)
	if !comparedWithPull(t, read, "k1", 12) {
		check := checkOf(t, read, "k1")
		t.Fatalf("after the link by hand and one pass, check k1 compares with %+v for the reason %q; want the head of acme/loose#12", check.ComparedWith, check.ComparedNothing)
	}
	if owed := owedReads(t, database); len(owed) != 0 {
		t.Errorf("after the pass read acme/loose#12, the owed reads are %q; want none", owed)
	}
}

// A person links a pull request of acme/hidden to an issue of acme/hidden, a repository that the
// App is not installed on. GitHub refuses the first read of it, so the read stays owed as a
// refusal (R4 of the results lookup), with refused_as app_not_installed and refused_since the time
// of that refusal, and is recorded as unreadable: the second pass does not read it again, so it is
// read one time after two passes. The results route says that it cannot read the repository:
// check k1 compares with nothing for repository_unreadable, and the recorded pass of k1 does not
// count (pass_stands unknown). The test fails where a refused read of an owed pull request ends the
// owe, and where a pass reads it again.
func TestALinkByHandToAPullRequestThatGitHubRefusesStaysOwedAsARefusal(t *testing.T) {
	handler, database, served, _ := readThroughGitHub(t, "acme/tools")
	issue := issueInRepository(t, handler, "HIDE", "acme/hidden", "Linked where the App cannot read")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/hidden/pull/40"}}})
	if owed, want := owedReads(t, database), []string{"acme/hidden#40 failure"}; !slices.Equal(owed, want) {
		t.Fatalf("after the link to acme/hidden#40, the owed reads are %q; want %q", owed, want)
	}
	var first time.Time
	for pass := 1; pass <= 2; pass++ {
		onePass(t, database, served)
		var reason string
		var reads int
		var refusedAs *string
		var refusedSince *time.Time
		err := database.Pool.QueryRow(context.Background(), `
			select reason, reads, refused_as, refused_since from pull_request_reads_owed where repo = 'acme/hidden' and number = 40
		`).Scan(&reason, &reads, &refusedAs, &refusedSince)
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("after pass %d, no read of acme/hidden#40 is owed; want a refusal, since GitHub refused each read", pass)
		}
		if err != nil {
			t.Fatalf("read the owed read of acme/hidden#40: %v", err)
		}
		if pass == 1 && refusedSince != nil {
			first = *refusedSince
		}
		if reason != "refusal" || reads != 1 || refusedAs == nil || *refusedAs != model.RefusedAsAppNotInstalled ||
			refusedSince == nil || !refusedSince.Equal(first) {
			t.Fatalf("after pass %d, acme/hidden#40 is owed as %q, read %d times, refused as %v since %v; want a refusal read one time, refused as %s since the first refusal %v",
				pass, reason, reads, refusedAs, refusedSince, model.RefusedAsAppNotInstalled, first)
		}
	}
	comparedNothing(t, readResults(t, handler, issue.Key), "k1", model.ReasonRepositoryUnreadable, model.PassStandsUnknown)
}
