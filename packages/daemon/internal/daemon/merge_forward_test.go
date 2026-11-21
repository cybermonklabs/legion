package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The log lines of the merge-forward executor (mergeForwardMessage) these tests read.
const (
	mergeForwardDelivered = "outbox merge-forward message delivered"
	mergeForwardLeftLine  = "outbox merge-forward message finished undelivered: the workflow no longer delivers it"
	mergeForwardEnded     = "outbox merge-forward message finished undelivered: the role's claim has failed or retired"
)

// mergeForwardRunner is an outbox runner over an isolated database holding LEGION-208 in testing
// (runnerIn), its pull request #86 open at head sha-1 on main with asked as the merge forward the
// record holds, and a supervisor with no claim on it yet. It drains on the caller's goroutine
// (RunOnce), as hookRunner's does, and logs to the buffer it answers.
func mergeForwardRunner(t *testing.T, publisher notify.Publisher, asked *record.MergeForward) (*outbox, *supervisor, *store.Pool, *syncBuffer) {
	t.Helper()
	return mergeForwardRunnerIn(t, publisher, record.PullRequestOpen, asked)
}

// mergeForwardRunnerIn is mergeForwardRunner with the pull request recorded in state.
func mergeForwardRunnerIn(t *testing.T, publisher notify.Publisher, state record.PullRequestState, asked *record.MergeForward) (*outbox, *supervisor, *store.Pool, *syncBuffer) {
	t.Helper()
	ctx := context.Background()
	logs := &syncBuffer{}
	runner, sup, pool := runnerIn(t, phase.Testing, "testing", publisher, slog.New(slog.NewJSONHandler(logs, nil)))
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return runner.records.PutPullRequest(ctx, tx, record.PullRequest{State: state, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 86, Branch: "legion/LEGION-208", HeadSHA: "sha-1", HeadUpdatedAt: time.Now(), Failing: []string{},
			CheckRuns: []record.AttemptRun{}, Base: "main", MergeForward: asked})
	}); err != nil {
		t.Fatalf("seed the pull request: %v", err)
	}
	return runner, sup, pool, logs
}

// The merge forward is a message, not a phase start (dispatch://INFRA-4374 R1), so nothing else
// brings the implementer to the merge: its row wakes a released implementer, waits while no
// session holds the role, and publishes once one does, on the role's own topic under the row's
// key. A message about an ask the record no longer holds at that head (a newer ask replaced it,
// or the workflow dropped it), an ask or a hold still queued once the merge landed, a message of a
// pull request that is not open, and a message to a claim that will never hold its role again end
// with nothing published (workflow.MergeForwardAt); a landed still queued once the merge landed is
// delivered.
func TestAMergeForwardMessageWaitsForItsRoleAndEndsWithItsAsk(t *testing.T) {
	ask := record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "sha-1", Base: "main",
		Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen}
	asked := &record.MergeForward{Head: "sha-1", Base: "main", Phase: phase.Testing, Why: record.ConflictCheckNotGreen, AskedAt: time.Now()}
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))

	t.Run("a released implementer is woken, waited for, and told once its session holds the role", func(t *testing.T) {
		publisher := &holderPublisher{}
		runner, sup, pool, logs := mergeForwardRunner(t, publisher, asked)
		publisher.setAbsent(topic)
		machine := releasedClaim(t, sup, claim.RoleImplementer)
		enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ask, time.Now().Add(-time.Minute)))
		id := noticeRowID(t, pool)

		drainAt(t, runner, 0, "the row's first attempt")
		if attempts, _ := publisher.snapshot(); attempts != 0 || outboxRows(t, pool) != 1 {
			t.Fatalf("the first attempt published %d times and left %d rows, want nothing published and the row waiting for the woken pod",
				attempts, outboxRows(t, pool))
		}
		if held := machine.Claim(); held.Released() {
			t.Fatalf("the implementer is %s by %s after the first attempt, want the wake to have resumed it", held.State, held.SuspendedBy)
		}

		// The woken session holds nothing yet: the publish finds no holder, and the row waits.
		drainAt(t, runner, 2*time.Minute, "the row's second attempt")
		if attempts, delivered := publisher.snapshot(); attempts != 1 || len(delivered) != 0 || outboxRows(t, pool) != 1 {
			t.Fatalf("the second attempt made %d publishes, delivered %v and left %d rows; want one refused publish and the row waiting",
				attempts, delivered, outboxRows(t, pool))
		}

		publisher.setAbsent()
		drainAt(t, runner, 10*time.Minute, "the row's attempt once the session holds the role")
		_, delivered := publisher.snapshot()
		if len(delivered) != 1 || delivered[0].topic != topic || delivered[0].message != ask.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"}) ||
			delivered[0].key != record.OutboxKey(id) || delivered[0].keep {
			t.Fatalf("delivered %+v, want the ask once to %s under its row's key", delivered, topic)
		}
		if left := outboxRows(t, pool); left != 0 {
			t.Fatalf("outbox rows left = %d, want the delivered row finished", left)
		}
		if lines := loggedLines(t, logs.String(), mergeForwardDelivered); len(lines) != 1 || lines[0]["step"] != string(record.MergeForwardAsk) ||
			lines[0]["role"] != string(claim.RoleImplementer) || lines[0]["head"] != "sha-1" {
			t.Fatalf("the row logged %v, want one delivery of the ask of sha-1 to the implementer", lines)
		}
	})

	landed := &record.MergeForward{Head: "sha-1", Base: "main", Phase: phase.Testing, Why: record.ConflictCheckNotGreen,
		AskedAt: time.Now(), Landed: "sha-2"}
	hold := record.MergeForwardMessage{Role: claim.RoleTester, Step: record.MergeForwardHold, Head: "sha-1", Base: "main",
		Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen}
	for _, tc := range []struct {
		name    string
		state   record.PullRequestState
		asked   *record.MergeForward
		message record.MergeForwardMessage
		why     string
	}{
		{name: "an ask of another head ends undelivered", asked: &record.MergeForward{Head: "sha-2", Base: "main", Phase: phase.Testing,
			Why: record.ConflictCheckNotGreen, AskedAt: time.Now()}, message: ask, why: "of head sha-2"},
		{name: "an ask the workflow dropped ends undelivered", message: ask, why: "no merge forward"},
		// Role-message rows run in no order of the issue's: a hold that reached the tester after
		// its landed would tell it to wait for a head that already came.
		{name: "a hold queued behind the merge's landing ends undelivered", asked: landed, message: hold, why: "landed at head sha-2"},
		{name: "an ask queued behind the merge's landing ends undelivered", asked: landed, message: ask, why: "landed at head sha-2"},
		{name: "an ask of a closed pull request ends undelivered", state: record.PullRequestClosed, asked: asked, message: ask,
			why: "the pull request is closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := record.PullRequestOpen
			if tc.state != "" {
				state = tc.state
			}
			publisher := &holderPublisher{}
			runner, _, pool, logs := mergeForwardRunnerIn(t, publisher, state, tc.asked)
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", tc.message, time.Now().Add(-time.Minute)))
			drainAt(t, runner, 0, "the row's attempt")
			if attempts, _ := publisher.snapshot(); attempts != 0 || outboxRows(t, pool) != 0 {
				t.Fatalf("the row made %d publishes and %d rows are left, want the row finished with nothing published", attempts, outboxRows(t, pool))
			}
			if lines := loggedLines(t, logs.String(), mergeForwardLeftLine); len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0]["why"]), tc.why) {
				t.Fatalf("the row logged %v, want one line naming %q", lines, tc.why)
			}
		})
	}

	t.Run("a landed queued behind the merge's landing is told to the tester", func(t *testing.T) {
		publisher := &holderPublisher{}
		runner, _, pool, logs := mergeForwardRunner(t, publisher, landed)
		told := record.MergeForwardMessage{Role: claim.RoleTester, Step: record.MergeForwardLanded, Head: "sha-1", Landed: "sha-2",
			Base: "main", Phase: phase.Testing, PullRequest: 86}
		tester := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleTester))
		enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", told, time.Now().Add(-time.Minute)))
		id := noticeRowID(t, pool)

		drainAt(t, runner, 0, "the row's attempt")
		_, delivered := publisher.snapshot()
		if len(delivered) != 1 || delivered[0].topic != tester || delivered[0].message != told.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"}) ||
			delivered[0].key != record.OutboxKey(id) || delivered[0].keep {
			t.Fatalf("delivered %+v, want the landed once to %s under its row's key", delivered, tester)
		}
		if left := outboxRows(t, pool); left != 0 {
			t.Fatalf("outbox rows left = %d, want the delivered row finished", left)
		}
		if lines := loggedLines(t, logs.String(), mergeForwardDelivered); len(lines) != 1 || lines[0]["step"] != string(record.MergeForwardLanded) ||
			lines[0]["role"] != string(claim.RoleTester) || lines[0]["head"] != "sha-1" {
			t.Fatalf("the row logged %v, want one delivery of the landed of sha-1 to the tester", lines)
		}
	})

	// The merger's landed in merging holds its merge to the reviewer's approval of the new head only
	// where the landing asked the reviewer for it, which the record keeps (record.MergeForward
	// .ReviewAsked) and the attempt reads: a landing on a task with no reviewer step asks no review,
	// and its merger is told to go on at the new head. The control is the same row over a record
	// whose landing asked the review.
	for _, tc := range []struct {
		name        string
		reviewAsked bool
	}{
		{name: "a landed to the merger of a task with no reviewer step holds nothing", reviewAsked: false},
		{name: "control: a landed to the merger whose landing asked the review waits for it", reviewAsked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &holderPublisher{}
			asked := *landed
			asked.Phase, asked.ReviewAsked = phase.Merging, tc.reviewAsked
			runner, _, pool, _ := mergeForwardRunner(t, publisher, &asked)
			told := record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardLanded, Head: "sha-1", Landed: "sha-2",
				Base: "main", Phase: phase.Merging, PullRequest: 86}
			merger := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleMerger))
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", told, time.Now().Add(-time.Minute)))

			drainAt(t, runner, 0, "the row's attempt")
			_, delivered := publisher.snapshot()
			if len(delivered) != 1 || delivered[0].topic != merger {
				t.Fatalf("delivered %+v, want the landed once to %s", delivered, merger)
			}
			const wait = "only once the reviewer's approval of that head is on the pull request"
			if waits := strings.Contains(delivered[0].message, wait); waits != tc.reviewAsked {
				t.Fatalf("the merger was told %q; want the wait for the reviewer's approval: %t", delivered[0].message, tc.reviewAsked)
			}
		})
	}

	t.Run("a retired implementer is not told", func(t *testing.T) {
		publisher := &holderPublisher{}
		runner, sup, pool, logs := mergeForwardRunner(t, publisher, asked)
		if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", claim.RoleImplementer),
			Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, State: supervise.StateRetired}, ""); err != nil {
			t.Fatalf("create the retired implementer: %v", err)
		}
		enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ask, time.Now().Add(-time.Minute)))
		drainAt(t, runner, 0, "the row's attempt")
		if attempts, _ := publisher.snapshot(); attempts != 0 || outboxRows(t, pool) != 0 {
			t.Fatalf("the row made %d publishes and %d rows are left, want the row finished with nothing published", attempts, outboxRows(t, pool))
		}
		if lines := loggedLines(t, logs.String(), mergeForwardEnded); len(lines) != 1 || lines[0]["state"] != string(supervise.StateRetired) {
			t.Fatalf("the row logged %v, want one line naming the retired claim", lines)
		}
	})
}
