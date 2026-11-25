// check_states_install_test.go holds the settler to the former heads the install of
// pull_request_former_heads seeded (migration 20261124122830), and to the candidates a result
// recorded since leaves: the commits of results the settle of an open pull request's issue reads,
// unverified until the settler has read that pull request's commits (verifyFormerHeads). The rig
// is check_states_test.go's.
package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedInstallCandidates writes what the install leaves for #70 of the rig's repository: each of
// shas as a candidate former head of its pull request, unverified, and the issue marked owed.
func seedInstallCandidates(t *testing.T, rig *checkStatesRig, shas ...string) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into pull_request_former_heads (pull, head_sha, verified)
		select pull_request_former_heads_resolve($1, 70), sha, false from unnest($2::text[]) sha`, ciJobRepository, shas); err != nil {
		t.Fatalf("seed the install's candidates %v: %v", shas, err)
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, rig.issue); err != nil {
		t.Fatalf("mark %s owed: %v", rig.issue, err)
	}
}

// forgetCandidates deletes every row pull_request_former_heads holds for #70 of the rig's
// repository, the records the writes of #70's row appended, and what the triggers left pending for
// the rig's issue, so the database holds there what the release before the install left: that
// release kept no former head and appended nothing. A test of the install's seed writes #70's row
// after its results, and this release answers the results and that write with candidates owed to
// the issue (pull_request_former_heads_of_result, pull_request_former_heads_begin), which the
// release before never wrote; the test calls this right after that write, before any settle, and
// then writes the install's candidates with seedInstallCandidates.
func forgetCandidates(t *testing.T, rig *checkStatesRig) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `
		with former as (delete from pull_request_former_heads where pull = pull_request_former_heads_pull_of($1, 70)),
			moves as (delete from pull_request_former_heads_moves where number = 70)
		delete from pull_request_former_heads_pending where issue_key = $2`, ciJobRepository, rig.issue); err != nil {
		t.Fatalf("delete #70's former heads and what was left for %s: %v", rig.issue, err)
	}
}

// candidatesOf is each former head the rig's #70 holds, by commit, and whether it is verified,
// read once the rig's issue is drained as each of its settles first drains it
// (pull_request_former_heads_drain): the records of its open pull requests applied, and the
// candidates the writes since its last settle owe it left.
func candidatesOf(t *testing.T, rig *checkStatesRig) map[string]bool {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `select pull_request_former_heads_drain($1)`, rig.issue); err != nil {
		t.Fatalf("drain %s: %v", rig.issue, err)
	}
	rows, err := rig.database.Pool.Query(context.Background(), `
		select head_sha, verified from pull_request_former_heads where pull = pull_request_former_heads_pull_of($1, 70)`, ciJobRepository)
	if err != nil {
		t.Fatalf("read the former heads of #70: %v", err)
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var sha string
		var verified bool
		if err := rows.Scan(&sha, &verified); err != nil {
			t.Fatalf("read the former heads of #70: %v", err)
		}
		held[sha] = verified
	}
	return held
}

// TestAFormerHeadTheInstallSeededIsPublishedOnceItsPullRequestListsIt: before the install, the tester
// recorded k1 passing at H1, while #70 was at H1, and a result at H3, a commit of another pull
// request; #70 is now at H2, and nothing recorded that it left H1. The install seeds H1 and H3 as
// #70's candidates and marks the issue owed. The settle reads #70's commits once, which list H1
// and H2: it keeps H1, publishes k1 yes there, and deletes H3, where nothing is published. A later
// settle reads #70's commits no more.
func TestAFormerHeadTheInstallSeededIsPublishedOnceItsPullRequestListsIt(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH3)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); held {
		t.Fatal("k1 is published at H1 before the install, which nothing recorded #70 left")
	}
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	seedInstallCandidates(t, rig, eachHeadH1, eachHeadH3)

	settleOwed(t, rig)
	if row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); !held || row.state == nil ||
		*row.state != model.PassStandsYes {
		t.Fatalf("k1 at H1, a head #70 left before the install and its commits list, is published as %s, want yes", publishedLine(row))
	}
	if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH3, ""); held {
		t.Fatal("H3, a commit #70's commits do not list, holds published states")
	}
	if got := candidatesOf(t, rig); len(got) != 1 || !got[eachHeadH1] {
		t.Fatalf("#70 holds the former heads %v after the settle, want H1 alone, verified", got)
	}
	if reads := rig.github.pullCommitReadsOf(70); reads != 1 {
		t.Fatalf("the settle read #70's commits %d times, want once", reads)
	}
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH1)
	settleOwed(t, rig)
	if row, _ := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k2"); row.state == nil || *row.state != model.PassStandsNo {
		t.Fatalf("k2 at H1 after the tester's fail is published as %s, want no", publishedLine(row))
	}
	if reads := rig.github.pullCommitReadsOf(70); reads != 1 {
		t.Fatalf("#70's commits were read %d times after a later settle, want still once: the candidates are verified", reads)
	}
}

// TestAFormerHeadTheInstallSeededWaitsForAReadOfItsPullRequestThatFailed: the install seeded H1 for
// #70 as above, and GitHub fails the read of #70's commits. H1 stays an unverified candidate with
// nothing published there, and the issue keeps a mark due after the backoff; once GitHub answers
// and that mark falls due, the settle verifies H1 and publishes k1 there. A pull request GitHub
// answers 404 for is no commit list either: a candidate seeded then stays, unverified.
func TestAFormerHeadTheInstallSeededWaitsForAReadOfItsPullRequestThatFailed(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	rig.github.failPullCommits(http.StatusBadGateway)
	seedInstallCandidates(t, rig, eachHeadH1)

	settleOwed(t, rig)
	if got := candidatesOf(t, rig); len(got) != 1 || got[eachHeadH1] {
		t.Fatalf("#70 holds the former heads %v after a failed read of its commits, want H1, unverified", got)
	}
	if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); held {
		t.Fatal("k1 is published at H1, which no read of #70's commits has confirmed")
	}
	var later int
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select count(*) from check_states_owed where issue_key = $1 and due_at > now() + interval '10 seconds'`, rig.issue).Scan(&later); err != nil {
		t.Fatalf("read the marks of %s: %v", rig.issue, err)
	}
	if later != 1 {
		t.Fatalf("%s holds %d marks due after the backoff, want one: the unverified candidate owes another settle", rig.issue, later)
	}

	rig.github.failPullCommits(0)
	if _, err := rig.database.Pool.Exec(context.Background(), `update check_states_owed set due_at = now() where issue_key = $1`, rig.issue); err != nil {
		t.Fatalf("bring %s's mark due: %v", rig.issue, err)
	}
	settleOwed(t, rig)
	if row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); !held || row.state == nil ||
		*row.state != model.PassStandsYes {
		t.Fatalf("k1 at H1 once #70's commits are read is published as %s, want yes", publishedLine(row))
	}

	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH3)
	seedInstallCandidates(t, rig, eachHeadH3)
	rig.github.mu.Lock()
	delete(rig.github.pullCommits, 70)
	rig.github.mu.Unlock()
	settleOwed(t, rig)
	if got := candidatesOf(t, rig); len(got) != 2 || !got[eachHeadH1] || got[eachHeadH3] {
		t.Fatalf("#70 holds the former heads %v after GitHub answered 404 for its commits, want H1 verified and H3 still unverified: a 404 is no commit list", got)
	}
}

// TestAFormerHeadPastTheCommitsGitHubListsIsKeptByItsComparisons: #70 holds 502 commits and is at
// the last of them, H2; the settle reads five pages of 100 of GitHub's comparison of #70's base
// revision and head, and no more. The install seeded H1, the 501st, where the tester recorded k1
// passing; a commit of the base branch, which the head holds too; a commit the head does not hold;
// a commit the repository does not hold; and a commit whose comparison GitHub fails. The pages
// leave out what they cannot hold, so what they do not list is decided by comparisons with the
// head and the base revision Dispatch stores (base.sha, never the branch's name, which can move):
// H1 is kept and k1 is published yes there, the three commits outside #70 are dropped, and the one
// GitHub did not answer for stays a candidate, unverified, with a mark that reads it again after
// the backoff.
func TestAFormerHeadPastTheCommitsGitHubListsIsKeptByItsComparisons(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	commits := make([]string, 502)
	for index := range commits {
		commits[index] = fmt.Sprintf("%040x", 0xc0000+index)
	}
	h1, h2 := commits[500], commits[501]
	onBase, apart, absent, unanswered := fmt.Sprintf("%040x", 0xb0001), fmt.Sprintf("%040x", 0xa0001),
		fmt.Sprintf("%040x", 0xd0001), fmt.Sprintf("%040x", 0xe0001)
	for _, sha := range []string{h1, onBase, apart, absent, unanswered} {
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", sha)
	}
	upsertHead(t, rig, 70, h2, rig.issue, "A change")
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	rig.github.setPullCommits(70, commits...)
	rig.github.setPull(70, h2, "main", ciJobBaseSHA)
	rig.github.setCompare(h2, h1, ciJobCompare{status: "behind"})
	rig.github.setCompare(ciJobBaseSHA, h1, ciJobCompare{status: "ahead"})
	rig.github.setCompare(h2, onBase, ciJobCompare{status: "behind"})
	rig.github.setCompare(ciJobBaseSHA, onBase, ciJobCompare{status: "behind"})
	rig.github.setCompare(h2, absent, ciJobCompare{failed: http.StatusNotFound})
	rig.github.setAbsentCommit(absent)
	rig.github.setCompare(h2, unanswered, ciJobCompare{failed: http.StatusBadGateway})
	seedInstallCandidates(t, rig, h1, onBase, apart, absent, unanswered)

	settleOwed(t, rig)
	if got, want := candidatesOf(t, rig), map[string]bool{h1: true, unanswered: false}; !maps.Equal(got, want) {
		t.Fatalf("#70 holds the former heads %v after the settle, want %v: H1, the 501st of its 502 commits, kept though the five pages of the comparison leave it out; the commits of the base branch, apart from the head and absent from the repository dropped; the one GitHub did not answer for still a candidate", got, want)
	}
	if row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, h1, "k1"); !held || row.state == nil ||
		*row.state != model.PassStandsYes {
		t.Fatalf("k1 at H1, a head #70 left past the pages of the comparison, is published as %s, want yes", publishedLine(row))
	}
	pages, pulls := rig.github.pullCommitReadsOf(70), rig.github.pullReadsOf(70)
	if pages != 5 || pulls != 1 {
		t.Fatalf("the settle read %d pages of the comparison of #70's base revision and head and #70 itself %d times, want 5 pages and #70 once", pages, pulls)
	}
	for _, pair := range [][2]string{{h2, h1}, {ciJobBaseSHA, h1}, {h2, onBase}, {ciJobBaseSHA, onBase}, {h2, apart}, {h2, absent}, {h2, unanswered}} {
		if reads := rig.github.compareReadsOf(pair[0], pair[1]); reads != 1 {
			t.Fatalf("the comparison %s...%s was read %d times, want once", pair[0][:6], pair[1][:6], reads)
		}
	}
	for _, pair := range [][2]string{{ciJobBaseSHA, apart}, {ciJobBaseSHA, absent}, {ciJobBaseSHA, unanswered}, {"main", h1}, {"main", onBase}} {
		if reads := rig.github.compareReadsOf(pair[0], pair[1]); reads != 0 {
			t.Fatalf("the comparison %s...%s was read %d times, want none: the head's comparison already decided it, and no comparison names the base branch by its name", pair[0], pair[1][:6], reads)
		}
	}
	var later int
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select count(*) from check_states_owed where issue_key = $1 and due_at > now() + interval '10 seconds'`, rig.issue).Scan(&later); err != nil {
		t.Fatalf("read the marks of %s: %v", rig.issue, err)
	}
	if later != 1 {
		t.Fatalf("%s holds %d marks due after the backoff, want one: the candidate GitHub did not answer for owes another settle", rig.issue, later)
	}
	t.Logf("the settle read 5 pages of the comparison of #70's base revision and head, #70 itself once and 7 comparisons")
}

// TestAResultAfterTheInstallAtAHeadLeftBeforeItIsPublishedOnceItsPullRequestListsIt: #70 left H1
// for H2 before the install, so no trigger recorded H1, and no result was recorded at H1 then, so
// the install seeded nothing there. After the install the tester records k1 passing at H1: on the
// issue, or on a child of it against the issue's check, a result the settle of the issue reads as
// its own either way. The result leaves H1 as a candidate of #70, unverified, and once #70's
// commits list H1 the settle keeps it and publishes k1 yes there, in one read of #70's commits.
func TestAResultAfterTheInstallAtAHeadLeftBeforeItIsPublishedOnceItsPullRequestListsIt(t *testing.T) {
	for _, child := range []bool{false, true} {
		name := "recorded on the issue"
		if child {
			name = "recorded on a child of the issue against its check"
		}
		t.Run(name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			settleOwed(t, rig)
			rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
			recorder := rig.issue
			if child {
				filed := rig.asPerson(t, http.MethodPost, "/api/v1/issues",
					map[string]any{"project": ciJobProject, "title": "A child", "parent": rig.issue, "force": true, "spec": "A spec\n"})
				if filed.Code != http.StatusCreated {
					t.Fatalf("file a child of %s: status=%d body=%s", rig.issue, filed.Code, filed.Body.String())
				}
				recorder = decodeBody[model.Issue](t, filed).Key
			}
			eachHeadResult(t, rig.handler, recorder, "k1", "tester", "pass", eachHeadH1)
			if got := candidatesOf(t, rig); len(got) != 1 || got[eachHeadH1] {
				t.Fatalf("after a result at H1, a head #70 left before the install, #70 holds the former heads %v, want H1, unverified", got)
			}
			settleOwed(t, rig)
			if row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); !held || row.state == nil ||
				*row.state != model.PassStandsYes {
				t.Fatalf("k1 at H1 once #70's commits list it is published as %s, want yes", publishedLine(row))
			}
			if got := candidatesOf(t, rig); len(got) != 1 || !got[eachHeadH1] {
				t.Fatalf("#70 holds the former heads %v after the settle, want H1, verified", got)
			}
			if reads := rig.github.pullCommitReadsOf(70); reads != 1 {
				t.Fatalf("the settle read #70's commits %d times, want once", reads)
			}
		})
	}
}

// TestOnePullRequestsCommitsAreReadOnceWhileItMovesToAnotherIssue: the tester recorded k1 passing
// at H1 before the install, which seeded H1 for #70 of ACME-1, and ACME-1's settle starts its
// read of #70's commits, which GitHub holds open. #70 then moves to another issue, whose settle
// runs while that read is open: it reads #70's commits no second time. Once the first read
// answers, H1 is verified and the other issue owed, and its settle publishes at H1 with no further
// read.
func TestOnePullRequestsCommitsAreReadOnceWhileItMovesToAnotherIssue(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	other := rig.fileIssue(t, "Where the pull request moves")
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	entered, release := rig.github.holdPullCommitsOnce(t, 70)
	seedInstallCandidates(t, rig, eachHeadH1)

	first := make(chan struct{})
	go func() {
		defer close(first)
		rig.settler.settle(store.WithTransactionTracking(context.Background()), rig.issue)
	}()
	ciAwait(t, "the first settle's read of #70's commits", entered)
	upsertHead(t, rig, 70, eachHeadH2, other, "A change")
	second := rig.newSettler(t, "check states of the other issue")
	if ran, failed, _ := second.settle(store.WithTransactionTracking(t.Context()), other); !ran || failed {
		t.Fatalf("the settle of %s answered ran=%v failed=%v, want a settle that ran", other, ran, failed)
	}
	if reads := rig.github.pullCommitReadsOf(70); reads != 1 {
		t.Fatalf("%s's settle read #70's commits while %s's read of them was open: %d reads, want one", other, rig.issue, reads)
	}
	release()
	ciAwait(t, "the first settle to end", first)
	settleOwed(t, rig)
	if got := candidatesOf(t, rig); len(got) != 1 || !got[eachHeadH1] {
		t.Fatalf("#70 holds the former heads %v once the read answered, want H1, verified", got)
	}
	if _, held := publishedRow(t, rig.database, other, ciJobRepository, eachHeadH1, ""); !held {
		t.Fatalf("%s published nothing at H1 once the read of #70's commits confirmed it", other)
	}
	if reads := rig.github.pullCommitReadsOf(70); reads != 1 {
		t.Fatalf("#70's commits were read %d times in all, want once", reads)
	}
}

// TestAResultBeforeItsPullRequestStartsAtTheIssueIsACandidate: the tester records k1 passing at
// H1 for the issue while no open row of #70 is the issue's, so no trigger leaves H1 to #70: no
// row of #70 exists yet, or #70's row is another issue's, or #70's row is closed. GitHub has moved
// #70 on to H2 meanwhile, and Dispatch then writes #70's row at H2: its first write, as the
// read-through of the result's commit writes it after the result's transaction; a write that
// moves #70 to the issue; or a write that reopens #70. No trigger saw #70 leave H1, so the write
// itself leaves H1 as a candidate, unverified, as the install does, and marks the issue owed:
// once the read of #70's commits lists H1, k1 is published yes there.
func TestAResultBeforeItsPullRequestStartsAtTheIssueIsACandidate(t *testing.T) {
	for _, row := range []struct {
		name   string
		before func(t *testing.T, rig *checkStatesRig)
	}{
		{"the row is written for the first time", func(*testing.T, *checkStatesRig) {}},
		{"the pull request moves to the issue", func(t *testing.T, rig *checkStatesRig) {
			upsertHead(t, rig, 70, eachHeadH2, rig.fileIssue(t, "Where the pull request starts"), "A change")
		}},
		{"the pull request reopens", func(t *testing.T, rig *checkStatesRig) {
			upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
			writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "closed", rig.issue)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			row.before(t, rig)
			settleOwed(t, rig)
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			settleOwed(t, rig)
			if got := candidatesOf(t, rig); len(got) != 0 {
				t.Fatalf("#70 holds the former heads %v before its row is the issue's and open, want none", got)
			}
			rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			if got, want := candidatesOf(t, rig), map[string]bool{eachHeadH1: false}; !maps.Equal(got, want) {
				t.Fatalf("#70 holds the former heads %v once its row is the issue's and open at H2, want %v: H1, where a result the issue's settle reads was recorded, a candidate no read has confirmed yet", got, want)
			}
			settleOwed(t, rig)
			if got := candidatesOf(t, rig); len(got) != 1 || !got[eachHeadH1] {
				t.Fatalf("#70 holds the former heads %v after the settle, want H1, verified", got)
			}
			if row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); !held || row.state == nil ||
				*row.state != model.PassStandsYes {
				t.Fatalf("k1 at H1, which #70 left before its row was the issue's, is published as %s, want yes", publishedLine(row))
			}
			if reads := rig.github.pullCommitReadsOf(70); reads != 1 {
				t.Fatalf("the settle read #70's commits %d times, want once", reads)
			}
		})
	}
}
