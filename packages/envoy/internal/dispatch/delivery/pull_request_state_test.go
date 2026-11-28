package delivery

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// statelessPullRequestActions is every action of GitHub's `pull_request` event whose name says
// nothing of whether the pull request is open: every action GitHub documents for the event but
// opened and reopened, which open it, and closed, which closes it
// (https://docs.github.com/en/webhooks/webhook-events-and-payloads#pull_request). Each of them can
// come after a close: a label, an edit, a lock and an unassignment all can, and closing a pull
// request with auto-merge on or in the merge queue sends auto_merge_disabled and dequeued too.
var statelessPullRequestActions = []string{
	"assigned", "auto_merge_disabled", "auto_merge_enabled", "converted_to_draft", "demilestoned",
	"dequeued", "edited", "enqueued", "labeled", "locked", "milestoned", "ready_for_review",
	"review_request_removed", "review_requested", "stacked", "synchronize", "unassigned",
	"unlabeled", "unlocked",
}

// pullRequestStateEvent is the body GitHub posts for one `pull_request` event of an unmerged
// pull request of acme/widgets on issue's branch: the action, and the pull request as GitHub held
// it when the event happened, its state (open or closed) and updated_at among it.
func pullRequestStateEvent(issue, action, state string, number int, updatedAt string) map[string]any {
	return map[string]any{
		"action":     action,
		"repository": map[string]any{"full_name": "acme/widgets"},
		"pull_request": map[string]any{
			"number": number, "title": issue + " the work", "body": "",
			"html_url": "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
			"user":     map[string]any{"login": "author"},
			"state":    state, "merged": false, "merged_at": nil, "merge_commit_sha": nil,
			"updated_at": updatedAt,
			"head": map[string]any{
				"sha": fmt.Sprintf("%040x", number), "ref": "legion/" + issue,
				"repo": map[string]any{"full_name": "acme/widgets"},
			},
			"base": map[string]any{"ref": "main"},
		},
	}
}

// stateIntake is an intake whose head writes the tests below drive one envelope at a time, as
// the `pull_request` envelope's handler makes them (handlePullRequestEnvelope).
type stateIntake struct {
	t     *testing.T
	ctx   context.Context
	pool  *store.Pool
	inner *Intake
}

func newStateIntake(t *testing.T) stateIntake {
	t.Helper()
	pool, ctx := deliveryTestPool(t)
	return stateIntake{t: t, ctx: ctx, pool: pool,
		inner: newIntake(tuningFor(t), nil, pool, nil, newFakeGitHub(t).newTestClient(), events.NewBroker(), nil, unrenamed())}
}

// deliver records the head of the envelope the listener's normalizer makes of body.
func (s stateIntake) deliver(number int, body map[string]any) {
	s.t.Helper()
	s.deliverPayload(number, envelopePayload(s.t, body))
}

// deliverPayload records the head of one envelope's payload as the intake decodes it.
func (s stateIntake) deliverPayload(number int, payload map[string]string) {
	s.t.Helper()
	if _, err := s.inner.recordPullRequestHead(s.ctx, payload, number); err != nil {
		s.t.Fatalf("record the %s envelope of #%d: %v", payload["action"], number, err)
	}
}

// subjects is the pull requests issue's head set holds (HeadsAttributedTo, which the results
// route reads): the subjects of its `before_merge` checks.
func (s stateIntake) subjects(issue string) []int {
	s.t.Helper()
	heads, err := unrenamed().HeadsAttributedTo(s.ctx, s.pool, issue, HeadAnswers{})
	if err != nil {
		s.t.Fatalf("HeadsAttributedTo(%s): %v", issue, err)
	}
	numbers := make([]int, 0, len(heads))
	for _, head := range heads {
		numbers = append(numbers, head.Number)
	}
	return numbers
}

// A pull request that closed without merging stays closed whatever `pull_request` event comes
// after the close, and leaves its issue's head set: GitHub sends every action with the pull
// request as it stands, closed, and none of these actions reopens it. Each action closes a pull
// request of its own issue, opened at 21:39 and closed at 21:58, and then sends that action at
// 21:59, past the close, so the head write's event-time guard applies it.
func TestAClosedPullRequestStaysClosedWhateverEventFollows(t *testing.T) {
	t.Parallel()
	intake := newStateIntake(t)
	for i, action := range statelessPullRequestActions {
		number := 6400 + i
		issue := seedHeadIssue(t, intake.ctx, intake.pool, number)
		intake.deliver(number, pullRequestStateEvent(issue, "opened", "open", number, "2026-11-23T21:39:22Z"))
		intake.deliver(number, pullRequestStateEvent(issue, "closed", "closed", number, "2026-11-23T21:58:13Z"))
		if closed := readHead(t, intake.ctx, intake.pool, "acme/widgets", number); closed.State != "closed" {
			t.Fatalf("after the close #%d is stored %q, want closed", number, closed.State)
		}
		intake.deliver(number, pullRequestStateEvent(issue, action, "closed", number, "2026-11-23T21:59:00Z"))
		stored := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
		if subjects := intake.subjects(issue); stored.State != "closed" || len(subjects) != 0 {
			t.Errorf("a %q event after the close stores #%d %q, and %s's head set holds %v: a closed pull request is a subject again",
				action, number, stored.State, issue, subjects)
		}
	}
}

// A pull request reopened after a close is open again, and its issue's head set holds it again:
// the state comes from GitHub's word for it at each event, so a close never freezes a row. A
// label after the reopen keeps it open, and a second close closes it again.
func TestAReopenedPullRequestIsOpenAgain(t *testing.T) {
	t.Parallel()
	intake := newStateIntake(t)
	const number = 6457
	issue := seedHeadIssue(t, intake.ctx, intake.pool, number)
	step := func(action, state, updatedAt, want string, wantSubjects int) {
		t.Helper()
		intake.deliver(number, pullRequestStateEvent(issue, action, state, number, updatedAt))
		stored := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
		if subjects := intake.subjects(issue); stored.State != want || len(subjects) != wantSubjects {
			t.Fatalf("after the %s event #%d is stored %q and %s's head set holds %v, want %s and %d head(s)",
				action, number, stored.State, issue, subjects, want, wantSubjects)
		}
	}
	step("opened", "open", "2026-11-23T21:39:22Z", "open", 1)
	step("closed", "closed", "2026-11-23T21:58:13Z", "closed", 0)
	step("labeled", "closed", "2026-11-23T21:59:00Z", "closed", 0)
	step("reopened", "open", "2026-11-23T22:05:00Z", "open", 1)
	step("labeled", "open", "2026-11-23T22:06:00Z", "open", 1)
	step("closed", "closed", "2026-11-23T22:10:00Z", "closed", 0)
}

// An envelope a listener published before it carried the pull request's state names only the
// action, and the stream can still hold one when this build starts. Its close still stores the
// pull request closed and its reopen open, as the action says.
func TestAnEnvelopeWithNoStateStoresWhatItsActionSays(t *testing.T) {
	t.Parallel()
	intake := newStateIntake(t)
	const number = 6460
	issue := seedHeadIssue(t, intake.ctx, intake.pool, number)
	for _, event := range []struct{ action, state, updatedAt, want string }{
		{"opened", "open", "2026-11-23T21:39:22Z", "open"},
		{"closed", "closed", "2026-11-23T21:58:13Z", "closed"},
		{"reopened", "open", "2026-11-23T22:05:00Z", "open"},
	} {
		payload := envelopePayload(t, pullRequestStateEvent(issue, event.action, event.state, number, event.updatedAt))
		delete(payload, "state")
		intake.deliverPayload(number, payload)
		if stored := readHead(t, intake.ctx, intake.pool, "acme/widgets", number); stored.State != event.want {
			t.Fatalf("a %s envelope with no state stores #%d %q, want %s", event.action, number, stored.State, event.want)
		}
	}
}

// A row that holds a closed pull request as open, as an event before this build could leave
// one, is closed at the reconcile's next read of it, and its issue's head set lets it go: the
// reconcile reads every open row again (ListStalePullRequestHeads) and stores the state GitHub
// answers.
func TestTheReconcileClosesAClosedPullRequestStoredOpen(t *testing.T) {
	fastPageRetries(t)
	intake := newStateIntake(t)
	const number = 6470
	issue := seedHeadIssue(t, intake.ctx, intake.pool, number)
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, pullRequestStateEvent(issue, "", "closed", number, "2026-11-23T21:59:00Z")["pull_request"])
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{})
	})
	stale, eventAt := time.Now().Add(-10*time.Minute), time.Date(2026, 11, 23, 21, 59, 0, 0, time.UTC)
	if err := UpsertPullRequestHead(intake.ctx, intake.pool, nil, PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: fmt.Sprintf("%040x", number), HeadRef: "legion/" + issue,
		BaseRef: "main", State: "open", IssueKey: &issue, AttributionSource: "branch", AttributionComplete: true,
		EventAt: &eventAt, CheckedAt: &stale,
	}); err != nil {
		t.Fatalf("store #%d open: %v", number, err)
	}
	if subjects := intake.subjects(issue); len(subjects) != 1 {
		t.Fatalf("before the reconcile %s's head set holds %v, want #%d", issue, subjects, number)
	}
	if err := NewReconcile(intake.pool, fake.newTestClient(), nil, unrenamed()).reconcilePullRequestHeads(intake.ctx, time.Now()); err != nil {
		t.Fatalf("reconcile the pull request heads: %v", err)
	}
	stored := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
	if subjects := intake.subjects(issue); stored.State != "closed" || len(subjects) != 0 {
		t.Fatalf("after the reconcile #%d is stored %q and %s's head set holds %v, want it closed and none", number, stored.State, issue, subjects)
	}
}
