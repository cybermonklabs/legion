package delivery

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// Acceptance round 4 of cybermonklabs/legion#4209 at ff73c7358d, the
// fresh tester's defect D8: a close that the release before writes over a row this build read is
// never read again, unless its time is GitHub's own second.
//
// During a roll the release before serves beside this build and, with delivery settings, writes
// each `pull_request` envelope's head, state and time, including an envelope GitHub never sent
// (any Envoy /v1 bearer can publish one). This build has read the pull request: open, marked. The
// release before then writes "closed" from an envelope. The trigger pull_request_heads_unread_write
// takes the mark away, and cmd/dispatch/README.md says of such a row "so the reconcile reads it
// again". The open arm of the reconcile reads open rows and its merged arm merged rows, so a closed
// row is read by neither: it stays closed, against GitHub's open, until the pull request's next
// event. R2: "The head and the state of a pull request come from a read of GitHub."
//
// The first case is the control: the release before's close carries the second GitHub's answer
// holds, so the trigger pull_request_heads_older_read owes a read and the pass repairs the row.
// The second case dates the close an hour ahead, as a forged envelope can; at ff73c7358d it fails.
//
// Run: DISPATCH_TEST_DATABASE_URL=<a fresh database of your own> go test ./internal/dispatch/delivery/
// -run '^TestAcceptACloseTheReleaseBeforeWritesOverAReadRowIsReadAgain$' -count=1 -v
func TestAcceptACloseTheReleaseBeforeWritesOverAReadRowIsReadAgain(t *testing.T) {
	for _, kase := range []struct {
		name   string
		number int
		ahead  time.Duration
	}{
		{"close_of_githubs_own_second", 88701, 0},
		{"close_an_hour_ahead", 88702, time.Hour},
	} {
		t.Run(kase.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			seedDeliveryProject(t, ctx, pool)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			now := time.Now().UTC().Truncate(time.Second)
			at := now.Add(-10 * time.Minute)
			head := fakeSHA("d8", kase.number)
			github.set("acme/widgets", kase.number, "open", head, "topic", "", at)
			if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: kase.number,
				HeadSHA: head, HeadRef: "topic", BaseRef: "main", State: "open",
				EventAt: &at, CheckedAt: &now, GitHubReadAt: &now}); err != nil {
				t.Fatal(err)
			}
			closedAt := at.Add(kase.ahead)
			writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: kase.number,
				HeadSHA: head, HeadRef: "topic", BaseRef: "main", State: "closed", EventAt: &closedAt}, noAttribution)
			if got := readHead(t, ctx, pool, "acme/widgets", kase.number); got.State != "closed" {
				t.Fatalf("the release before's write left the row %s, want closed: the setup did not happen", got.State)
			}
			NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(ctx)
			got := readHead(t, ctx, pool, "acme/widgets", kase.number)
			t.Logf("after the reconcile's next pass: state %s head %s, owed %d", got.State, got.HeadSHA[:6], owedCount(t, ctx, pool, "acme/widgets", kase.number))
			if got.State != "open" || got.HeadSHA != head {
				t.Fatalf("after the reconcile's next pass #%d is %s at %s, want GitHub's open at %s: a close the release before wrote from an envelope dated %s after GitHub's answer stands, and no reader of GitHub reads the row until the pull request's next event",
					kase.number, got.State, got.HeadSHA[:6], head[:6], kase.ahead)
			}
		})
	}
}
