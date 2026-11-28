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

// pullRequestStateEvent is the body GitHub posts for one `pull_request` event of a pull request
// of acme/widgets on issue's branch: the action, and the pull request as GitHub held it when the
// event happened, its updated_at among it. state is open, closed, or merged for one GitHub calls
// closed and merged.
func pullRequestStateEvent(issue, action, state string, number int, updatedAt string) map[string]any {
	pullRequest := map[string]any{
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
	}
	if state == "merged" {
		pullRequest["state"], pullRequest["merged"], pullRequest["merged_at"] = "closed", true, updatedAt
		pullRequest["merge_commit_sha"] = fmt.Sprintf("%040x", 0xa0000+number)
	}
	return map[string]any{
		"action":       action,
		"repository":   map[string]any{"full_name": "acme/widgets"},
		"pull_request": pullRequest,
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

// reconcileAnswering runs one pass of the reconcile's re-read of the heads
// (ListStalePullRequestHeads) at now, with GitHub answering every pull request of acme/widgets
// as issue's, in state, last updated at updatedAt.
func (s stateIntake) reconcileAnswering(issue, state, updatedAt string, now time.Time) {
	s.t.Helper()
	fake := newFakeGitHub(s.t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, _ := strconv.Atoi(r.PathValue("number"))
		mustEncode(s.t, w, pullRequestStateEvent(issue, "", state, number, updatedAt)["pull_request"])
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(s.t, w, []any{})
	})
	if err := NewReconcile(s.pool, fake.newTestClient(), nil, unrenamed()).reconcilePullRequestHeads(s.ctx, now); err != nil {
		s.t.Fatalf("reconcile the pull request heads: %v", err)
	}
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

// Two events of one pull request in the same second can reach Dispatch in either order, and
// GitHub's updated_at, which counts whole seconds, cannot say which of their copies of the pull
// request is the newer; a copy with no updated_at cannot be ordered at all. An event whose copy is
// no newer than the row and says another state leaves the row open and due for the reconcile's
// next read (checked_at null), and that read stores the state GitHub answers. So a stale copy
// never closes an open pull request, whose row the reconcile would never read again, and keeps a
// closed one open only until that read. A copy no newer than the row is no read of it, so a
// redelivery of the same copy leaves the row due. Each case delivers its events in the order
// listed.
func TestAStateConflictAnEventCannotOrderIsSettledByAReadOfGitHub(t *testing.T) {
	t.Parallel()
	type event struct{ action, state, updatedAt string }
	for i, tc := range []struct {
		name   string
		events []event
		github string
	}{
		{
			// A label applied just before a reopen carries the closed copy, and arrives last.
			name: "a label's copy from before a reopen, delivered after the reopen",
			events: []event{
				{"opened", "open", "2026-11-23T21:00:00Z"}, {"closed", "closed", "2026-11-23T21:05:00Z"},
				{"reopened", "open", "2026-11-23T21:10:00Z"}, {"labeled", "closed", "2026-11-23T21:10:00Z"},
			},
			github: "open",
		},
		{
			// A label applied just before a close carries the open copy, and arrives last.
			name: "a label's copy from before a close, delivered after the close",
			events: []event{
				{"opened", "open", "2026-11-23T21:00:00Z"},
				{"closed", "closed", "2026-11-23T21:10:00Z"}, {"labeled", "open", "2026-11-23T21:10:00Z"},
			},
			github: "closed",
		},
		{
			name:   "a close in the same second as the open, in order",
			events: []event{{"opened", "open", "2026-11-23T21:10:00Z"}, {"closed", "closed", "2026-11-23T21:10:00Z"}},
			github: "closed",
		},
		{
			name: "a label's copy with no event time from before a reopen, delivered after the reopen",
			events: []event{
				{"opened", "open", "2026-11-23T21:00:00Z"}, {"closed", "closed", "2026-11-23T21:05:00Z"},
				{"reopened", "open", "2026-11-23T21:10:00Z"}, {"labeled", "closed", ""},
			},
			github: "open",
		},
		{
			name: "a label's copy from before a close, delivered twice after the close",
			events: []event{
				{"opened", "open", "2026-11-23T21:00:00Z"}, {"closed", "closed", "2026-11-23T21:10:00Z"},
				{"labeled", "open", "2026-11-23T21:10:00Z"}, {"labeled", "open", "2026-11-23T21:10:00Z"},
			},
			github: "closed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			intake := newStateIntake(t)
			number := 6480 + i
			issue := seedHeadIssue(t, intake.ctx, intake.pool, number)
			for _, e := range tc.events {
				intake.deliver(number, pullRequestStateEvent(issue, e.action, e.state, number, e.updatedAt))
			}
			unsettled := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
			if subjects := intake.subjects(issue); unsettled.State != "open" || unsettled.CheckedAt != nil || len(subjects) != 1 {
				t.Fatalf("after the same-second conflict #%d is stored %q checked at %v and %s's head set holds %v, want open, unchecked and #%d",
					number, unsettled.State, unsettled.CheckedAt, issue, subjects, number)
			}
			intake.reconcileAnswering(issue, tc.github, "2026-11-23T21:10:00Z", time.Now())
			settled := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
			wantSubjects := map[string]int{"open": 1, "closed": 0}[tc.github]
			if subjects := intake.subjects(issue); settled.State != tc.github || settled.CheckedAt == nil || len(subjects) != wantSubjects {
				t.Fatalf("after the reconcile #%d is stored %q checked at %v and %s's head set holds %v, want %s as GitHub answers, checked, and %d head(s)",
					number, settled.State, settled.CheckedAt, issue, subjects, tc.github, wantSubjects)
			}
		})
	}
}

// A merge is final: an event whose copy of the pull request is from before its merge, delivered
// after the merge in the same second, changes nothing of the merged row — not its state, its
// final head, its read time or the rule that judged it, so a merged row an older rule judged is
// still offered to the reconcile (ListStalePullRequestHeads). A redelivered close of an earlier
// unmerged close is one such copy, of a pull request that closed, reopened and merged within a
// second; a label applied just before the merge, at an earlier head, is another.
func TestAMergedPullRequestStaysMerged(t *testing.T) {
	t.Parallel()
	intake := newStateIntake(t)
	const number = 6490
	issue := seedHeadIssue(t, intake.ctx, intake.pool, number)
	intake.deliver(number, pullRequestStateEvent(issue, "opened", "open", number, "2026-11-23T21:00:00Z"))
	intake.deliver(number, pullRequestStateEvent(issue, "closed", "merged", number, "2026-11-23T21:10:00Z"))
	stampLegacyAttributionRule(t, intake.ctx, intake.pool, "acme/widgets", number)
	merged := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
	earlierHead := pullRequestStateEvent(issue, "labeled", "open", number, "2026-11-23T21:10:00Z")
	earlierHead["pull_request"].(map[string]any)["head"].(map[string]any)["sha"] = "9b406e9cfc000000000000000000000000000000"
	intake.deliver(number, pullRequestStateEvent(issue, "closed", "closed", number, "2026-11-23T21:10:00Z"))
	intake.deliver(number, earlierHead)
	stored := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
	if stored.State != "merged" || stored.HeadSHA != merged.HeadSHA || stored.attributionRule != 0 ||
		stored.CheckedAt == nil || merged.CheckedAt == nil || !stored.CheckedAt.Equal(*merged.CheckedAt) {
		t.Fatalf("after copies from before the merge #%d is %q at %s, rule %d, checked at %v; want merged at %s, rule 0, checked at %v",
			number, stored.State, stored.HeadSHA, stored.attributionRule, stored.CheckedAt, merged.HeadSHA, merged.CheckedAt)
	}
	offered, err := ListStalePullRequestHeads(intake.ctx, intake.pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	if len(offered) != 1 || offered[0].Number != number {
		t.Fatalf("the reconcile is offered %+v, want #%d, a merged row an older rule judged", offered, number)
	}
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

// An envelope with no state whose action names no state either can only guess that the pull
// request is open, so its write is no read of the state: it leaves the row's checked_at where
// the last read of the pull request put it. So a guess after a close holds only until the
// reconcile's next read of the row, however many such envelopes come, and that read closes it.
func TestAGuessedStateDoesNotPutOffTheReconcilesRead(t *testing.T) {
	fastPageRetries(t)
	intake := newStateIntake(t)
	const number = 6465
	issue := seedHeadIssue(t, intake.ctx, intake.pool, number)
	intake.deliver(number, pullRequestStateEvent(issue, "opened", "open", number, "2026-11-23T21:00:00Z"))
	intake.deliver(number, pullRequestStateEvent(issue, "closed", "closed", number, "2026-11-23T21:10:00Z"))
	closed := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
	if closed.State != "closed" || closed.CheckedAt == nil {
		t.Fatalf("after the close #%d is stored %q checked at %v, want closed and checked", number, closed.State, closed.CheckedAt)
	}
	for _, at := range []string{"2026-11-23T21:11:00Z", "2026-11-23T21:12:00Z"} {
		payload := envelopePayload(t, pullRequestStateEvent(issue, "labeled", "closed", number, at))
		delete(payload, "state")
		intake.deliverPayload(number, payload)
	}
	guessed := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
	if guessed.CheckedAt == nil || !guessed.CheckedAt.Equal(*closed.CheckedAt) {
		t.Fatalf("after two labels with no state #%d is checked at %v, want %v, the close's read", number, guessed.CheckedAt, closed.CheckedAt)
	}
	intake.reconcileAnswering(issue, "closed", "2026-11-23T21:12:00Z", closed.CheckedAt.Add(PullRequestHeadStaleAfter+time.Second))
	if stored := readHead(t, intake.ctx, intake.pool, "acme/widgets", number); stored.State != "closed" {
		t.Fatalf("after the reconcile's read due from the close #%d is stored %q, want closed", number, stored.State)
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
	intake.reconcileAnswering(issue, "closed", "2026-11-23T21:59:00Z", time.Now())
	stored := readHead(t, intake.ctx, intake.pool, "acme/widgets", number)
	if subjects := intake.subjects(issue); stored.State != "closed" || len(subjects) != 0 {
		t.Fatalf("after the reconcile #%d is stored %q and %s's head set holds %v, want it closed and none", number, stored.State, issue, subjects)
	}
}
