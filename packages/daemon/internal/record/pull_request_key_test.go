package record

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// keyedPullRequest is an open pull request of LEGION-208 in repo, numbered number, on the issue's
// own branch, onto base.
func keyedPullRequest(repo string, number int, base string) PullRequest {
	return PullRequest{Issue: planIssue, Repo: repo, Number: number, Branch: "legion/" + planIssue, Base: base,
		HeadSHA: fmt.Sprintf("head-%d", number), Failing: []string{}, State: PullRequestOpen}
}

// repoNumbers is each of prs as owner/name#number, the repository lowered, in order.
func repoNumbers(prs []PullRequest) []string {
	named := make([]string, len(prs))
	for i, pr := range prs {
		named[i] = fmt.Sprintf("%s#%d", lowerASCII(pr.Repo), pr.Number)
	}
	return named
}

// lowerASCII is name with its ASCII capitals lowered, as PostgreSQL's lower() reads a repository.
func lowerASCII(name string) string {
	lowered := []byte(name)
	for i, c := range lowered {
		if 'A' <= c && c <= 'Z' {
			lowered[i] = c + 'a' - 'A'
		}
	}
	return string(lowered)
}

// The record keeps one row for each pull request of an issue, keyed by its repository and its
// number (dispatch://INFRA-5910 D1), and the issue is a column of the row. LEGION-208's plan has
// one task, a, which holds the issue's own branch, legion/LEGION-208, in the issue's repository,
// acme/widgets, and whose start fixed its base, the issue's default branch. Four pull requests
// stand on that one branch name: two in acme/widgets, #40 onto a branch of its own and #41 onto
// the task's base, and two in acme/gadgets, #41 and #46. All four read back by their repository
// and number, each its own row. A read that has only a branch answers each open row on it, and a
// row closed leaves that answer. A row written again under another spelling of its repository is
// the same row: the issue keeps one row of it, and the write lands on it.
//
// A pull request of another repository on a branch name a task holds is no task's (decision 22):
// acme/gadgets' two read the task "", and the plan holds no branch of acme/gadgets. Of the two of
// task a, the task's own is #41, the first whose base is the task's base (decision 5), though #40
// has the lower number; #40 is another pull request of the task. The task reads leave out each
// pull request of another repository, and the set of the issue holds the task's rows and the
// linked acme/gadgets#41, not the unlinked acme/gadgets#46.
//
// The fault it catches is the key (issue, branch): the second row of the branch replaces the first,
// and acme/gadgets' rows take acme/widgets' place.
func TestAPullRequestIsKeyedByRepositoryAndNumber(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture(planIssue)
	branch := "legion/" + planIssue
	other := keyedPullRequest("acme/widgets", 40, branch+"-ci-base")
	own := keyedPullRequest("acme/widgets", 41, issue.DefaultBranch)
	gadgets := keyedPullRequest("acme/gadgets", 41, "main")
	gadgetsSecond := keyedPullRequest("acme/gadgets", 46, "main")
	every := []PullRequest{other, own, gadgets, gadgetsSecond}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		v := PlanVersion{Issue: planIssue, Generation: issue.Generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner,
			GivenBy: planOwner, GivenAt: planGivenAt}
		putPlan(t, tx, v, []PlanTask{{Issue: planIssue, Generation: issue.Generation, Task: "a", PlanVersion: 1,
			BlockText: "Build the widget.", State: TaskRunning}}, nil)
		given, err := records.AssignTaskBranch(ctx, tx, planIssue, issue.Generation, "a")
		must(t, err)
		if given != branch {
			t.Fatalf("task a's branch = %q, want the issue's own, %s", given, branch)
		}
		must(t, records.SetTaskBase(ctx, tx, planIssue, issue.Generation, "a", issue.DefaultBranch))
		for _, pr := range every {
			must(t, records.PutPullRequest(ctx, tx, pr))
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		var rows int
		must(t, tx.QueryRow(ctx, `select count(*) from pull_requests where issue = $1`, planIssue).Scan(&rows))
		if rows != len(every) {
			t.Fatalf("LEGION-208 holds %d pull request rows, want %d: one for each pull request, whatever its branch", rows, len(every))
		}
		for _, want := range every {
			got, err := records.PullRequestByNumber(ctx, tx, want.Repo, want.Number)
			must(t, err)
			if got == nil || got.Issue != planIssue || got.Repo != want.Repo || got.Branch != want.Branch || got.Base != want.Base ||
				got.HeadSHA != want.HeadSHA {
				t.Fatalf("%s#%d by number = %+v, want its own row: %+v", want.Repo, want.Number, got, want)
			}
			row, err := records.PullRequestRowByNumber(ctx, tx, want.Repo, want.Number)
			must(t, err)
			if row == nil || row.HeadSHA != want.HeadSHA {
				t.Fatalf("the row of %s#%d by number = %+v, want its own row at %s", want.Repo, want.Number, row, want.HeadSHA)
			}
		}
		for repo, want := range map[string][]string{
			"acme/widgets": {"acme/widgets#40", "acme/widgets#41"},
			"ACME/Gadgets": {"acme/gadgets#41", "acme/gadgets#46"},
		} {
			whole, err := records.OpenPullRequestsOnBranch(ctx, tx, repo, branch)
			must(t, err)
			rowsOnly, err := records.OpenPullRequestRowsOnBranch(ctx, tx, repo, branch)
			must(t, err)
			if got := repoNumbers(whole); !reflect.DeepEqual(got, want) {
				t.Fatalf("the open pull requests of %s on %s = %v, want %v: each open row on the branch", repo, branch, got, want)
			}
			if got := repoNumbers(rowsOnly); !reflect.DeepEqual(got, want) {
				t.Fatalf("the open pull request rows of %s on %s = %v, want %v", repo, branch, got, want)
			}
		}

		// Decision 22: a pull request of another repository on the task's branch name is no task's.
		for _, want := range []struct {
			pr   PullRequest
			task string
		}{{other, "a"}, {own, "a"}, {gadgets, ""}, {gadgetsSecond, ""}} {
			got, err := records.PullRequestByNumber(ctx, tx, want.pr.Repo, want.pr.Number)
			must(t, err)
			if got.Task != want.task {
				t.Fatalf("the task of %s#%d = %q, want %q: a task holds its branch in its issue's repository alone", want.pr.Repo, want.pr.Number,
					got.Task, want.task)
			}
		}
		stored := readPlan(t, tx, planIssue, issue.Generation)
		if !stored.HoldsBranch(issue, "ACME/widgets", branch) || stored.HoldsBranch(issue, "acme/gadgets", branch) {
			t.Fatalf("the plan holds %s in acme/widgets = %t and in acme/gadgets = %t, want the issue's repository alone, in any spelling",
				branch, stored.HoldsBranch(issue, "ACME/widgets", branch), stored.HoldsBranch(issue, "acme/gadgets", branch))
		}
		if task, ok := stored.TaskOfBranch(issue, "acme/gadgets", branch); ok || task != "" {
			t.Fatalf("the task of %s in acme/gadgets = %q, %t; want none", branch, task, ok)
		}
		if tasks := stored.TasksOfBranch(issue, "acme/gadgets", branch); len(tasks) != 0 {
			t.Fatalf("the tasks of %s in acme/gadgets = %v, want none", branch, tasks)
		}

		// Decision 5: the task's own pull request is the first whose base is the task's base.
		ofTask, err := records.PullRequestOfTask(ctx, tx, planIssue, "a")
		must(t, err)
		rowOfTask, err := records.PullRequestRowOfTask(ctx, tx, planIssue, "a")
		must(t, err)
		merge, err := records.MergePullRequest(ctx, tx, planIssue)
		must(t, err)
		for name, got := range map[string]*PullRequest{"PullRequestOfTask": ofTask, "PullRequestRowOfTask": rowOfTask, "MergePullRequest": merge} {
			if got == nil || got.Repo != own.Repo || got.Number != own.Number {
				t.Fatalf("%s of task a = %+v, want %s#%d, the first on the task's base", name, got, own.Repo, own.Number)
			}
		}
	})

	// A link of acme/gadgets#41: the task reads leave it out, and the issue's set holds it.
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ReplacePullRequestLinks(ctx, tx, planIssue, []PullRequestLink{{Repo: "Acme/Gadgets", Number: 41}}))
	})
	inTx(t, st, func(tx pgx.Tx) {
		linked, err := records.PullRequestByNumber(ctx, tx, gadgets.Repo, gadgets.Number)
		must(t, err)
		unlinked, err := records.PullRequestByNumber(ctx, tx, gadgetsSecond.Repo, gadgetsSecond.Number)
		must(t, err)
		if !linked.Linked || unlinked.Linked {
			t.Fatalf("acme/gadgets#41 linked = %t and #46 linked = %t, want the link read for #41 alone", linked.Linked, unlinked.Linked)
		}
		tasks, err := records.TaskPullRequests(ctx, tx, planIssue)
		must(t, err)
		if got, want := repoNumbers(tasks), []string{"acme/widgets#41", "acme/widgets#40"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the task pull requests of %s = %v, want %v: the task's own first among its rows, and no pull request of another repository",
				planIssue, got, want)
		}
		sets, err := records.PullRequestSetsOf(ctx, tx, []string{planIssue, "LEGION-209"})
		must(t, err)
		if got, want := repoNumbers(sets[planIssue]), []string{"acme/gadgets#41", "acme/widgets#40", "acme/widgets#41"}; !reflect.DeepEqual(got, want) ||
			len(sets) != 1 {
			t.Fatalf("the sets of %s and LEGION-209 = %v, want %s's alone, %v: the task's pull requests and the linked one", planIssue, sets,
				planIssue, want)
		}
	})

	// The same pull request written under another spelling of its repository is the same row.
	again := own
	again.Repo, again.HeadSHA = "ACME/WIDGETS", "head-41-again"
	closed := gadgetsSecond
	closed.State = PullRequestClosed
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutPullRequest(ctx, tx, again))
		must(t, records.PutPullRequest(ctx, tx, closed))
	})
	inTx(t, st, func(tx pgx.Tx) {
		var rows int
		must(t, tx.QueryRow(ctx, `select count(*) from pull_requests where lower(repo) = 'acme/widgets' and number = 41`).Scan(&rows))
		if rows != 1 {
			t.Fatalf("acme/widgets#41 holds %d rows after a write under ACME/WIDGETS, want one", rows)
		}
		for _, name := range []string{"acme/widgets", "Acme/Widgets"} {
			got, err := records.PullRequestByNumber(ctx, tx, name, 41)
			must(t, err)
			if got == nil || got.HeadSHA != again.HeadSHA || got.Issue != planIssue {
				t.Fatalf("%s#41 = %+v, want the row the second spelling wrote, at %s", name, got, again.HeadSHA)
			}
		}
		open, err := records.OpenPullRequestsOnBranch(ctx, tx, "acme/gadgets", branch)
		must(t, err)
		if got, want := repoNumbers(open), []string{"acme/gadgets#41"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the open pull requests of acme/gadgets on %s after #46 closed = %v, want %v", branch, got, want)
		}
	})
}

// A row of each kind that names a pull request by its number, as the release before this one
// wrote it, carries no repository: that release read each in its issue's repository. Such a row
// still decodes, and it names the issue's repository (D8 of dispatch://INFRA-5910): a merge-queue
// read, a merge-queue removal, a conflict's second read, a merge forward's message and a close of
// task pull requests. A row of this release names the repository it carries, in another
// repository than the issue's too.
func TestAnOutboxRowOfTheReleaseBeforeReadsInTheIssuesRepository(t *testing.T) {
	const issueRepo, otherRepo = "acme/widgets", "acme/gadgets"
	firstRead := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		kind    OutboxKind
		payload string
	}{
		{OutboxKindMergeQueueRead, `{"number":42,"head":"c0de"}`},
		{OutboxKindMergeQueueRemoval, `{"number":42,"head":"c0de","unread":true}`},
		{OutboxKindConflictConfirm, `{"number":42,"head":"c0de","firstReadAt":"` + firstRead.Format(time.RFC3339) + `"}`},
		{OutboxKindMergeForward, `{"role":"implementer","step":"ask","head":"c0de","base":"main","phase":"implementing","pullRequest":42,"why":"` +
			string(ConflictCheckNotGreen) + `"}`},
		{OutboxKindTaskPullRequestsClose, `{"generation":3,"numbers":[42,43],"comment":"Merged in #45."}`},
	} {
		t.Run(string(row.kind), func(t *testing.T) {
			for _, tc := range []struct {
				payload, want string
			}{
				{row.payload, issueRepo},
				{row.payload[:len(row.payload)-1] + `,"repo":"` + otherRepo + `"}`, otherRepo},
			} {
				payload, err := DecodeOutboxPayload(OutboxRow{ID: 7, Kind: row.kind, Issue: planIssue, Payload: json.RawMessage(tc.payload)})
				if err != nil {
					t.Fatalf("decode the row %s: %v", tc.payload, err)
				}
				named, ok := payload.(interface{ RepoOf(issueRepo string) string })
				if !ok {
					t.Fatalf("the %s payload %T names no repository of its pull request", row.kind, payload)
				}
				if got := named.RepoOf(issueRepo); got != tc.want {
					t.Fatalf("the row %s names the repository %q, want %q", tc.payload, got, tc.want)
				}
			}
		})
	}
}
