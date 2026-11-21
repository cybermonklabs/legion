package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// unreadableLinksOf is the unreadable_links of the results read of issue, as "<url> since=<set or
// none>" in order.
func unreadableLinksOf(t *testing.T, handler http.Handler, issue string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	read := decodeBody[struct {
		UnreadableLinks []struct {
			URL          string     `json:"url"`
			RefusedSince *time.Time `json:"refused_since"`
		} `json:"unreadable_links"`
	}](t, response)
	listed := []string{}
	for _, link := range read.UnreadableLinks {
		since := "none"
		if link.RefusedSince != nil {
			since = "set"
		}
		listed = append(listed, link.URL+" since="+since)
	}
	return listed
}

// owedReadOf is the reason, the count of reads and the refusal of the read owed for number of
// upstream/project, with owed false where none is owed.
func owedReadOf(t *testing.T, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, number int) (reason string, reads int, refusedAs string, owed bool) {
	t.Helper()
	var refused *string
	err := database.QueryRow(context.Background(), `
		select reason, reads, refused_as from pull_request_reads_owed where repo = 'upstream/project' and number = $1
	`, number).Scan(&reason, &reads, &refused)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", false
	}
	if err != nil {
		t.Fatalf("read the owed read of upstream/project#%d: %v", number, err)
	}
	if refused != nil {
		refusedAs = *refused
	}
	return reason, reads, refusedAs, true
}

// An issue of acme/tools links a GitHub issue and a pull request of upstream/project, a
// repository the App is not installed on, as a bug report of another project is linked. Each link
// owes a read of its number (R5 of the results lookup), and GitHub refuses the first read: the App
// is not installed. The read is then recorded as unreadable, so no later pass reads it: after three
// passes each was read one time, and the issue's results name both links in unreadable_links.
// When the issue links the pull request again, the link owes the read again, and the next pass
// reads it one time more and records it as unreadable again. Control: a link to the comments page
// of an issue names no pull request, so it owes nothing and is not named.
func TestALinkToARepositoryTheAppIsNotInstalledOnIsReadOnceUntilTheLinkIsWrittenAgain(t *testing.T) {
	handler, database, served, _ := readThroughGitHub(t, "acme/tools")
	issue := issueInRepository(t, handler, "UPST", "acme/tools", "Links an upstream report")
	links := []map[string]string{
		{"url": "https://github.com/upstream/project/issues/41", "kind": "github_issue"},
		{"url": "https://github.com/upstream/project/pull/42"},
		{"url": "https://github.com/upstream/project/issues/43/comments", "kind": "github_issue"},
	}
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": links})
	if owed, want := owedReads(t, database), []string{"upstream/project#41 failure", "upstream/project#42 failure"}; !slices.Equal(owed, want) {
		t.Fatalf("after the links, the owed reads are %q; want %q: the comments page owes nothing", owed, want)
	}
	for pass := 1; pass <= 3; pass++ {
		onePass(t, database, served)
		for _, number := range []int{41, 42} {
			reason, reads, refusedAs, owed := owedReadOf(t, database.Pool, number)
			t.Logf("pass %d: upstream/project#%d owed=%v reason=%s reads=%d refused_as=%s", pass, number, owed, reason, reads, refusedAs)
			if !owed || reason != "refusal" || reads != 1 || refusedAs != model.RefusedAsAppNotInstalled {
				t.Errorf("after pass %d, upstream/project#%d is owed %v as %q, read %d times, refused as %q; want a refusal as %s read one time: the refusal records it as unreadable",
					pass, number, owed, reason, reads, refusedAs, model.RefusedAsAppNotInstalled)
			}
		}
	}
	if listed, want := unreadableLinksOf(t, handler, issue.Key), []string{
		"https://github.com/upstream/project/issues/41 since=set", "https://github.com/upstream/project/pull/42 since=set",
	}; !slices.Equal(listed, want) {
		t.Errorf("the results of %s name unreadable_links %q; want %q", issue.Key, listed, want)
	}

	patchIssue(t, handler, issue.Key, map[string]any{"external_links": links[:1]})
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": links[:2]})
	for pass := 1; pass <= 2; pass++ {
		onePass(t, database, served)
		reason, reads, refusedAs, owed := owedReadOf(t, database.Pool, 42)
		t.Logf("after the second link, pass %d: upstream/project#42 owed=%v reason=%s reads=%d refused_as=%s", pass, owed, reason, reads, refusedAs)
		if !owed || reason != "refusal" || reads != 2 || refusedAs != model.RefusedAsAppNotInstalled {
			t.Errorf("after the second link and pass %d, upstream/project#42 is owed %v as %q, read %d times, refused as %q; want a refusal as %s read two times",
				pass, owed, reason, reads, refusedAs, model.RefusedAsAppNotInstalled)
		}
	}
	if _, reads, _, _ := owedReadOf(t, database.Pool, 41); reads != 1 {
		t.Errorf("upstream/project#41, linked once, was read %d times; want one", reads)
	}
}
