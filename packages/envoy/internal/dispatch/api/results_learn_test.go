package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// openPull is the one pull request of openPullAt's answer, as GitHub answers
// GET /repos/{owner}/{repo}/pulls/{number} for it.
func openPull(number int, issueKey, repo, sha string) string {
	return strings.TrimSuffix(strings.TrimPrefix(openPullAt(number, issueKey)(repo, sha), "["), "]")
}

// The limit of R5: where the write of a read of the results owes a read, the next read compares.
// acme/widgets#99 has a results row under the issue that no read of GitHub wrote. The row holds
// another head and another title, and its time is later than the time of GitHub's answer. GitHub
// names #99 at the commit of k1's pass. The read-through writes GitHub's answer over the row, and
// the trigger pull_request_heads_older_read owes a read. So the first read compares with no head,
// names #99 in heads_not_read, and leaves one owed read of #99. One head pass of the reconcile
// reads #99 and settles that read. The next read compares with #99, and the pass stands. The
// control marks the same row read by GitHub (github_read_at). The guard of the write keeps that
// row, so no read is owed, and the first read compares with #99.
func TestAReadThroughWhoseOwnWriteOwesAReadComparesAtTheNextRead(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "the_write_owes_a_read", true: "control_the_row_marked_read"}[marked], func(t *testing.T) {
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			issue := issueInRepository(t, handler, "ACME", "acme/widgets", "Owed by its own write")
			uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
			recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
			var readAt *time.Time
			if marked {
				now := time.Now()
				readAt = &now
			}
			if _, err := database.Pool.Exec(context.Background(), `
				insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, title, url,
					event_at, checked_at, github_read_at)
				values ('acme/widgets', 99, $1, 'legion/x', 'main', 'open', $2, 'An older title',
					'https://github.com/acme/widgets/pull/99', now(), now(), $3)
			`, newHeadCommit, issue.Key, readAt); err != nil {
				t.Fatalf("store the row of #99 that no read of GitHub wrote: %v", err)
			}
			fake.answers(func(repo, sha string) string {
				if sha != headCommit {
					return `[]`
				}
				return openPullAt(99, issue.Key)(repo, sha)
			})
			fake.pullAnswers(func(repo, number string) string {
				if repo != "acme/widgets" || number != "99" {
					return ""
				}
				return openPull(99, issue.Key, repo, headCommit)
			})
			served.commitPulls = delivery.CommitPullReads{}

			spent := fake.asks("acme/widgets")
			first := checkOf(t, readResults(t, handler, issue.Key), "k1")
			asked, owed := fake.asks("acme/widgets")-spent, readsOwedOf(t, database, 99)
			t.Logf("first read: asks=%d compared_with=%v compared_nothing=%q heads_not_read=%+v pass_stands=%s reads_owed_of_99=%d",
				asked, pullRequestsComparedWith(first), first.ComparedNothing, first.HeadsNotRead, first.PassStands.Answer, owed)
			if asked != 1 {
				t.Fatalf("SETUP: the first read asked GitHub %d times, want 1: the read must read #99 through", asked)
			}
			if marked {
				if compared := pullRequestsComparedWith(first); !slices.Equal(compared, []int{99}) || len(first.HeadsNotRead) != 0 || owed != 0 {
					t.Fatalf("control: the first read compares with %v, names heads_not_read %+v and leaves %d owed reads, want [99], none and 0: the guard keeps a row that a read of GitHub wrote",
						compared, first.HeadsNotRead, owed)
				}
				return
			}
			if len(first.ComparedWith) != 0 || !slices.Equal(first.HeadsNotRead, []model.HeadNotRead{{Number: 99}}) || owed != 1 {
				t.Errorf("the first read compares with %v, names heads_not_read %+v and leaves %d owed reads of #99, want [], [{number: 99}] and 1: the write of the read owes a read",
					pullRequestsComparedWith(first), first.HeadsNotRead, owed)
			}

			onePass(t, database, served)
			next := checkOf(t, readResults(t, handler, issue.Key), "k1")
			t.Logf("after one pass: compared_with=%v heads_not_read=%+v pass_stands=%s reads_owed_of_99=%d",
				pullRequestsComparedWith(next), next.HeadsNotRead, next.PassStands.Answer, readsOwedOf(t, database, 99))
			if compared := pullRequestsComparedWith(next); !slices.Equal(compared, []int{99}) || next.PassStands.Answer != model.PassStandsYes {
				t.Errorf("the read after one pass compares with %v and answers pass_stands %s, want [99] and yes: the pass read #99",
					compared, next.PassStands.Answer)
			}
		})
	}
}

// A pull request that the same read of the results learns is not known in that answer. k1 of
// NKLR-1 has a pass at headCommit, and Dispatch holds no pull request. The read asks GitHub once,
// and GitHub names the open #98 at that commit. Its branch, title and body name no key. GitHub
// answers 500 to the page of its commits, so its commit messages are not read. The read stores
// #98 with those facts. The same answer must read the pull requests whose answer is not known
// after that write. So the result is unknown with issue_not_known, and pass_stands is unknown.
// The control reads one commit message that names NKLR-1: #98 is the issue's, and the pass stands
// at its head.
func TestAPullRequestThatTheSameReadLearnsIsNotKnownInThatAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string // the one commit message of #98, "" where GitHub answers 500 to the page
	}{
		{"learned_and_not_known", ""},
		{"control_learned_and_the_messages_name_the_issue", "Work.\n\nDispatch: %s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			key := issueInRepository(t, handler, "NKLR", "acme/widgets", "Learned in the read").Key
			uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", twoChecks)
			recordResult(t, handler, key, resultBody("k1", "pass", headCommit, nil))
			stored := headRowsOf(t, database, 98)
			at := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
			fake.answers(func(repo, sha string) string {
				if sha != headCommit {
					return `[]`
				}
				return fmt.Sprintf(`[{"number":98,"title":"A change","body":"No key here.","html_url":"https://github.com/%s/pull/98",
					"state":"open","merged":false,"updated_at":%q,
					"head":{"ref":"work/no-key","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
					repo, at.Format(time.RFC3339), headCommit, repo)
			})
			fake.commitsAnswer(func(string, string) (int, string) {
				if tc.message == "" {
					return http.StatusInternalServerError, ""
				}
				return http.StatusOK, fmt.Sprintf(`[{"commit":{"message":%q}}]`, fmt.Sprintf(tc.message, key))
			})
			served.commitPulls = delivery.CommitPullReads{}

			spent := fake.asks("acme/widgets")
			read := readResults(t, handler, key)
			asked := fake.asks("acme/widgets") - spent
			var messagesRead bool
			if err := database.Pool.QueryRow(context.Background(), `
				select commit_messages is not null from pull_request_texts where repo = 'acme/widgets' and number = 98
			`).Scan(&messagesRead); err != nil {
				t.Fatalf("read the stored texts of #98: %v", err)
			}
			k1 := checkOf(t, read, "k1")
			stands := "none"
			if len(k1.Results) > 0 && k1.Results[0].CommitStands != nil {
				stands = k1.Results[0].CommitStands.Answer + "/" + k1.Results[0].CommitStands.Reason
			}
			t.Logf("%s: rows_before=%d asks=%d messages_read=%v compared_with=%v compared_nothing=%q issues_not_known=%+v commit_stands=%s pass_stands=%s",
				tc.name, stored, asked, messagesRead, pullRequestsComparedWith(k1), k1.ComparedNothing, k1.IssuesNotKnown, stands, k1.PassStands.Answer)
			if stored != 0 || asked != 1 || messagesRead != (tc.message != "") {
				t.Fatalf("SETUP: %d rows of #98 before the read, %d asks of GitHub in it, and messages read %v, want 0, 1 and %v: the read must learn #98",
					stored, asked, messagesRead, tc.message != "")
			}
			if tc.message == "" {
				if stands != model.CommitUnknown+"/"+model.ReasonIssueNotKnown || k1.PassStands.Answer != model.PassStandsUnknown {
					t.Errorf("the read that learned #98 answers commit_stands %s and pass_stands %s, want %s/%s and %s",
						stands, k1.PassStands.Answer, model.CommitUnknown, model.ReasonIssueNotKnown, model.PassStandsUnknown)
				}
				return
			}
			if stands != model.CommitAtBasis+"/" || k1.PassStands.Answer != model.PassStandsYes {
				t.Errorf("control: commit_stands %s and pass_stands %s, want %s and yes: #98 is the issue's",
					stands, k1.PassStands.Answer, model.CommitAtBasis)
			}
		})
	}
}
