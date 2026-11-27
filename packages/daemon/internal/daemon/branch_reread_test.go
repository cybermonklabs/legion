package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// replacedDraft is how far above the walk's own pull request the replay's draft is numbered: the
// draft #3955 of INFRA-1276 stood 301 above #3654, the pull request whose row it took.
const replacedDraft = 301

// seedReplaced records the walk's issue as an earlier daemon left the record of INFRA-1276: the
// issue in reviewing, with status needs_review, the rank and the sequence Dispatch gave, its
// implementer's phases row at stepRecordsHead, and on its branch the one row that daemon kept, of
// the draft numbered draft onto legion/<KEY>-ci-base, open, written in place of the walk's own
// pull request, which the record holds no row of.
func (r *stepRecordsRig) seedReplaced(draft int, created createdIssue) {
	r.t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: r.title, Phase: phase.Reviewing, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
			Rank: created.Rank, LastDispatchSeq: created.LastSeq, HandedOver: true,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry) values ($1, 'implementer', $2, $3, 1, '', 1)`,
			r.key, string(r.token(claim.RoleImplementer)), stepRecordsHead); err != nil {
			return err
		}
		pr := stepRecordsPullRequest(r.key, draft)
		pr.Base = claim.IssueBranch(r.key) + "-ci-base"
		return r.records.PutPullRequest(ctx, tx, pr)
	}); err != nil {
		r.t.Fatalf("record %s as an earlier daemon left it: %v", r.key, err)
	}
}

// closeUnmerged is GitHub closing pull request number of the walk's branch, onto base, with no
// merge, and Envoy relaying GitHub's event of it, as a person's close of a draft is relayed.
func (r *stepRecordsRig) closeUnmerged(number int, base string) {
	r.t.Helper()
	r.github.closedUnmerged(number)
	payload, err := json.Marshal(map[string]string{"action": "closed", "kind": "pr", "repo": liveRepo, "number": fmt.Sprint(number), "merged": "false",
		"head_sha": stepRecordsHead, "base_ref": base, "head_ref": claim.IssueBranch(r.key), "head_repo": liveRepo,
		"updated_at": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		r.t.Fatal(err)
	}
	r.publish(fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), number), "github", fmt.Sprintf("pr-%d-closed", number), string(payload))
}

// The close of the last pull request the record holds open reads the branch on GitHub again before
// it tells the architect anything (dispatch://INFRA-5910 D5, R2 of dispatch://INFRA-4091), which is
// the case of INFRA-4091 replayed in the walk's repository: the record of INFRA-1276 held the draft
// #3955 on legion/INFRA-1276, written in place of the open #3654 by an earlier daemon, and the close
// of the draft told the architect the issue's pull request closed unmerged. The whole daemon boots
// with a Postgres, a NATS, the real Dispatch and a fake GitHub (githubtest) over a record that holds
// the walk's issue in reviewing with the draft, numbered replacedDraft above the walk's own pull
// request, open onto legion/<KEY>-ci-base, as the one row of its branch. The draft then closes with
// no merge through GitHub's event.
//   - Case 1: GitHub holds the walk's own pull request open on the same branch onto main. The daemon
//     reads the branch, the record then holds the own pull request open, the architect gets one
//     pr-closed-others-open that names it, no pr-closed-unmerged is sent, and the issue stays in
//     reviewing.
//   - Case 2: GitHub holds no other open pull request on the branch (the own one is closed there):
//     the close sends pr-closed-unmerged, as today, and the record holds no row of the own one.
//
// The faults it catches: the close handler that sends pr-closed-unmerged with no read of the
// branch, under which case 1 finds that notice and no record of the own pull request; and a read
// that does not record what it finds, under which case 1 finds the record without it.
func TestTheCloseOfTheLastRecordedPullRequestReadsTheBranchAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ownOpen bool
	}{
		{name: "case 1: GitHub holds the replaced pull request open on the branch", ownOpen: true},
		{name: "case 2: GitHub holds no other open pull request on the branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var draft int
			r := newStepRecordsRigSeeded(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1,
				func(r *stepRecordsRig, created createdIssue) bool {
					draft = r.number + replacedDraft
					r.github.openOnto(claim.IssueBranch(r.key), draft, claim.IssueBranch(r.key)+"-ci-base")
					if !tc.ownOpen {
						r.github.closedUnmerged(r.number)
					}
					r.seedReplaced(draft, created)
					r.seedArchitect()
					return true
				})
			own, drafted := fmt.Sprintf("%s#%d", liveRepo, r.number), fmt.Sprintf("%s#%d", liveRepo, draft)
			r.stands("the boot", phase.Reviewing)
			if got := r.pullRequestSet(); !slices.Equal(got, []string{drafted + " open"}) {
				t.Fatalf("the record of %s after the boot holds %v, want the draft %s alone, open, as the earlier daemon left it", r.key, got, drafted)
			}

			r.closeUnmerged(draft, claim.IssueBranch(r.key)+"-ci-base")
			testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the close of "+drafted+" to be told to the architect", func() bool {
				return len(r.architectNotices(r.key, "pr-closed-others-open")) > 0 || len(r.architectNotices(r.key, "pr-closed-unmerged")) > 0
			})
			// What must not come comes after the first notice, if at all: a second notice of the
			// close, or the other kind.
			time.Sleep(liveSettle)
			others, unmerged := r.architectNotices(r.key, "pr-closed-others-open"), r.architectNotices(r.key, "pr-closed-unmerged")
			if tc.ownOpen {
				if len(unmerged) != 0 || r.pullState(liveRepo, r.number) != string(record.PullRequestOpen) {
					t.Fatalf("the close of %s sent pr-closed-unmerged %v and the record holds %s as %q, want no such notice and %s recorded open: GitHub holds it open on the branch",
						drafted, unmerged, own, r.pullState(liveRepo, r.number), own)
				}
				if len(others) != 1 || !strings.Contains(others[0].Payload, own) || !strings.Contains(others[0].Payload, drafted) {
					t.Fatalf("the close of %s sent pr-closed-others-open %v, want one that names %s closed and %s still open", drafted, others, drafted, own)
				}
				if got, want := r.pullRequestSet(), []string{own + " open", drafted + " closed"}; !slices.Equal(got, want) {
					t.Fatalf("the record of %s after the read holds %v, want %v", r.key, got, want)
				}
			} else {
				want := fmt.Sprintf("pull request %s closed without merging while %s was in reviewing", drafted, r.key)
				if len(unmerged) != 1 || unmerged[0].Payload != want || len(others) != 0 {
					t.Fatalf("the close of %s sent pr-closed-unmerged %v and pr-closed-others-open %v, want one pr-closed-unmerged, %q, and no other: GitHub holds nothing open on the branch",
						drafted, unmerged, others, want)
				}
				if got := r.pullState(liveRepo, r.number); got != "" {
					t.Fatalf("the record holds %s as %q after the read, want no row of it: GitHub shows it closed", own, got)
				}
			}
			r.stands("the close of "+drafted, phase.Reviewing)
		})
	}
}
