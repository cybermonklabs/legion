package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A read of an issue's results reads from GitHub a pull request the issue links by hand whose head
// Dispatch does not hold, and stores it, so the read itself compares the checks with that head,
// whatever kinds of check the issue holds.
//
// Production met the case on 24 November: an issue whose only `before_merge` check is one CI
// proves linked its open pull request by hand. No event of that pull request had reached Dispatch,
// the reconcile had not read it, and the check refuses a result from every role (409
// CHECK_PROVED_BY_CI), so no result write's read-through asked GitHub about it either: the
// results said no_head while the pull request was open, linked and green at its head.
//
// Each case is an issue of acme/widgets that links #12 by its URL. GitHub holds #12 open at
// headCommit, where CI's run 51 passed, with a branch and texts that name no issue, so the link
// alone makes it the issue's. GitHub names no pull request for any commit, no intake runs and no
// reconcile pass runs: the read of the results is the only thing that can read #12. The link is
// written through the issue PATCH a person or an agent links with, or stored as a link written
// before a link owed a read. The first read of the results must compare the issue's checks with
// #12's head, and Dispatch must hold #12's head after it. A link by hand is written after #12
// opened, with no event of #12 since, as INFRA-1450's and INFRA-5094's links were. A pull
// request stacked on another branch and a draft are read the same way (cybermonklabs/legion#5316
// and #5302 had no head row).
func TestAResultsReadReadsALinkedPullRequestWhoseHeadIsNotHeld(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		document string
		// linkedBefore stores the link as a link of a release before links owed a read.
		linkedBefore bool
		// base is the branch #12 merges into, "main" where empty; draft is GitHub's draft flag.
		base  string
		draft bool
	}{
		{name: "a check CI proves, linked by hand", document: ciCheckBlock("c1", ciCheckKept)},
		{name: "two checks CI proves, linked by hand", document: ciCheckBlock("c1", ciCheckKept) + "\n" + ciCheckBlock("c2", ciCheckKept)},
		{name: "a check CI proves, linked before a link owed a read", document: ciCheckBlock("c1", ciCheckKept), linkedBefore: true},
		{name: "a before_merge check with no result, linked by hand", document: twoChecks},
		{name: "a check CI proves, a pull request stacked on another branch", document: ciCheckBlock("c1", ciCheckKept), base: "legion/OTHER-1"},
		{name: "a check CI proves, a draft pull request", document: ciCheckBlock("c1", ciCheckKept), draft: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			runs := newCheckRunsFake()
			runs.set(headCommit, completedRun(51, ciCheckRun, "success"))
			base := row.base
			if base == "" {
				base = "main"
			}
			handler, database, _ := oneRepositoryGitHub(t, "acme/widgets", runs, func(number string) string {
				if number != "12" {
					return ""
				}
				return fmt.Sprintf(`{"number":12,"title":"A change","body":"","state":"open","merged":false,"merged_at":null,
					"draft":%t,"html_url":"https://github.com/acme/widgets/pull/12","updated_at":"2026-11-24T04:36:47Z",
					"head":{"ref":"topic","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":%q}}`, row.draft, headCommit, base)
			})
			issue := issueInRepository(t, handler, "RLHR", "acme/widgets", "A linked pull request")
			uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", row.document)
			const link = "https://github.com/acme/widgets/pull/12"
			if row.linkedBefore {
				if _, err := database.Pool.Exec(context.Background(),
					`insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`, issue.Key, link); err != nil {
					t.Fatalf("store the link as a release before stored it: %v", err)
				}
			} else {
				linkPullRequests(t, handler, issue.Key, link)
			}
			held := func() string {
				var sha string
				if err := database.Pool.QueryRow(context.Background(),
					`select coalesce(max(head_sha), '') from pull_request_heads where lower(repo) = 'acme/widgets' and number = 12`).Scan(&sha); err != nil {
					t.Fatalf("read the head Dispatch holds of #12: %v", err)
				}
				return sha
			}
			if before := held(); before != "" {
				t.Fatalf("before the read Dispatch holds #12 at %s, want no head: the case is a pull request no event reached", before)
			}

			read := readResults(t, handler, issue.Key)
			if got := held(); got != headCommit {
				t.Fatalf("after one read of the results Dispatch holds #12 at %s, want %s: the read did not read the linked pull request", got, headCommit)
			}
			if row.document == twoChecks {
				compared := checkOf(t, read, "k1").ComparedWith
				if len(compared) != 1 || compared[0].PullRequest == nil || *compared[0].PullRequest != 12 || compared[0].Commit != headCommit {
					t.Fatalf("the first read compares k1 with %+v, want the head of #12 at %s", compared, headCommit)
				}
				return
			}
			for _, check := range read.Checks {
				assertCI(t, check, ciCheckRun, model.CIStatePass, "success", ciVerdictLineOf(check.BlockID, ciCheckRun, "pass", headCommit))
			}
			if owed := owedReadsOf(t, database, "acme/widgets", 12); owed != 0 {
				t.Errorf("#12 is still owed %d read(s) after its head was read, want none", owed)
			}
			if _, err := delivery.GetSettings(context.Background(), database.Pool); err == nil {
				t.Fatal("the deployment holds delivery settings, want none: production held none")
			}
		})
	}
}

// owedReadsOf counts the reads of GitHub owed for pull request number of repo, in any letter case.
func owedReadsOf(t *testing.T, database *store.Store, repo string, number int) int {
	t.Helper()
	var owed int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from pull_request_reads_owed where lower(repo) = lower($1) and number = $2`, repo, number).Scan(&owed); err != nil {
		t.Fatalf("count the reads owed for %s#%d: %v", repo, number, err)
	}
	return owed
}
