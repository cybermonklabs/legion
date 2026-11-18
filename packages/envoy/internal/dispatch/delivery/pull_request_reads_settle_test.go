package delivery

// The intake's delete of the read owed before its read of GitHub began (settleOwedRead) keeps
// every read owed after that moment, however long the delete waits for its connection.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// settleWaitTracer holds back each delete of pull_request_reads_owed for wait before pgx sends
// it. On the intake's pool that is the delete of settleOwedRead alone, and the wait stands in for
// an acquire of a pooled connection: it comes after the intake's read of GitHub and before
// Postgres runs the statement.
type settleWaitTracer struct{ wait time.Duration }

func (s settleWaitTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "delete from pull_request_reads_owed") {
		time.Sleep(s.wait)
	}
	return ctx
}

func (settleWaitTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The read an older answer's own write owes outlives the intake's delete of the reads an earlier
// delivery owed, however long that delete waits for its connection (an older event does not
// replace a newer head; the review of cybermonklabs/legion#4209 at
// 3e60f25910, D1). During a roll the release before stores GitHub's newer answer, closed, from an
// envelope; this build's slower read lands GitHub's older answer, open, over it, and the trigger
// pull_request_heads_older_read owes the pull request a read in that same write. The delete then
// waits 3 s, longer than the read of GitHub. At 3e60f25910 the delete's line was the database's
// clock at the delete less the read's age as the process measured it before the wait, so the wait
// moved the line past the trigger's row and deleted it, and the older open head stayed a basis
// after the reconcile's next pass (the reviewer's probe,
// TestReview4SettleDeletesTheOwedReadItsOwnUpsertMade: `owed_after_intake=false`,
// `older_head_is_basis=true`). Now the delete takes only the revision the read took before it read
// GitHub, and here there was none, so the trigger's row stays whatever the wait. The control waits
// 0 s.
func TestTheReadAnOlderAnswerOwesOutlivesASlowSettle(t *testing.T) {
	for _, wait := range []time.Duration{0, 3 * time.Second} {
		t.Run(fmt.Sprintf("settle_waits_%s", wait), func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 88401)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			intake := newIntake(nil, storetest.Traced(t, pool, settleWaitTracer{wait: wait}, nil), nil, client, nil, nil)
			const number = 88401
			older, newer := fakeSHA("a", number), fakeSHA("b", number)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			now := time.Now().UTC().Truncate(time.Second)
			t1, t2 := now.Add(-10*time.Minute), now.Add(-5*time.Minute)
			writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
				HeadSHA: newer, HeadRef: branch, BaseRef: "main", State: "closed", EventAt: &t2, CheckedAt: &now},
				releaseBeforeAttribution{issue: &issue, source: string(sourceBranch), complete: true})
			github.set("acme/widgets", number, "open", older, branch, body, t1)
			if _, err := intake.handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", number, "synchronize", older, branch, body, t1)); err != nil {
				t.Fatal(err)
			}
			owedAfterIntake := owedCount(t, ctx, pool, "acme/widgets", number)

			github.set("acme/widgets", number, "closed", newer, branch, body, t2)
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			got := readHead(t, ctx, pool, "acme/widgets", number)
			olderBasis, err := HeadAttributedTo(ctx, pool, issue, older)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("settle_wait=%s owed_after_intake=%d final_head=%s state=%s older_head_is_basis=%v",
				wait, owedAfterIntake, got.HeadSHA[:6], got.State, olderBasis)
			if owedAfterIntake != 1 {
				t.Errorf("after the intake wrote the older answer over the newer row, %d reads are owed, want the one its own write owed", owedAfterIntake)
			}
			if got.HeadSHA != newer || got.State != "closed" || olderBasis {
				t.Errorf("after the reconcile's next pass the row is head %s state %s and the older head is a basis = %v, want GitHub's current %s closed and no basis at %s",
					got.HeadSHA[:6], got.State, olderBasis, newer[:6], older[:6])
			}
		})
	}
}
