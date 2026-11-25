package outbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// replyOrderTarget is the @role: mention the person's reply names in every case of
// TestEveryOrderOfAReplysEventsLeavesACopyExactlyWhereR6Says.
const replyOrderTarget = "role:legion-reviewer-t1"

// replyOrderCase is one order of a reply's events: how the reply's first attempt of its @role:
// mention is written with the comment (first), how the outbox's read of the role's claim answers
// (roles), and the steps that follow the reply, in order (replyOrder.do).
//
// first is one of "unnamed" - the listener's lookup of the role went unanswered, so the attempt
// names no session and its sender holds it pending; "unnamed resumed" - the same attempt past the
// lease the outbox waits a send out for, its sender's claim lapsed; "no holder" - the listener
// named no live holder, so the attempt failed naming no session; "named C" - the listener named
// C, so the attempt is pinned to C and pending; and "named C failed" - that attempt's send
// failed. roles is "unreadable" (the outbox cannot read the role's claim), "held elsewhere" (it
// reads roleHolder as the holder) or "held by A".
type replyOrderCase struct {
	name, first, roles string
	steps              []string
}

// TestEveryOrderOfAReplysEventsLeavesACopyExactlyWhereR6Says drives the attempt state machine of
// one person's reply in B's thread, B live, under every order of a small, enumerated set, and
// reads at the end which session holds a copy of what. The issue is the thread rule's
// (seedThreadRuleIssue): nobody routes or claims it, its primary document credits A and C, and the
// reply names @role:R, which may name either. The events are the reply, the person's edit of it
// (as the API writes it), the person's later comment on the issue, a Retry of the mention, a late
// pin of the first attempt to A or to C, and the failure of that attempt's send; each send that
// settles appends its comment.delivery receipt, as the API's sender does. The invariant is R6 of
// dispatch://INFRA-3686: a session holds a copy of the reply exactly when R6 says it does. The
// thread's session (B) does, once; a session the reply names whose mention failed (A or C, by
// whichever attempt named it) does, once, by the outbox's copy, and one whose mention delivered
// holds the mention and no copy of the outbox's; a session only a document credits, while the
// thread's session is live, holds none, and neither does any other session. The edit follows the
// reply: each session gets the edit exactly as often as it gets the outbox's copy of the reply. The
// person's later comment, which is no reply, reaches A and C, the primary document's credits, and
// not B. Where a case moves the issue's claim or route to A while the reply is still unpublished,
// dispatch://INFRA-1368's R1 obliges A as well, and the case says so.
//
// The order of arrival and a wait inside a bound are not part of the invariant: every case is
// driven until the outbox settles - every event published and every fallback row settled, each
// read making the backed-off events and the waiting rows due at once, and a case ages the
// attempts past unnamedForwardBound where it needs to - and only then is what each session got
// read. Each case ends with every attempt settled, so the outbox can settle.
//
// The named cases are the scenarios of the red tests of R6, rounds 5 to 10, each under the name
// of the test it is. The generated ones are every order of the edit, the late pin, the first
// attempt's failure and a failed Retry, with the outbox reading after the reply and after each
// event, and the later comment in a different place in each order; once with the first attempt
// pinned to C and the Retry to A, and once with both pinned to A. A pin that comes after the
// failure pins nothing: the first attempt then failed with no holder, as round 10's did. The
// Retry is one event, as round 10's is: it opens an attempt pinned to the session the role
// re-resolves to, and that send fails.
func TestEveryOrderOfAReplysEventsLeavesACopyExactlyWhereR6Says(t *testing.T) {
	for _, c := range append(replyOrderNamedCases(), replyOrderGeneratedCases()...) {
		t.Run(c.name, func(t *testing.T) { runReplyOrder(t, c) })
	}
}

// replyOrderNamedCases is the scenario of each red test of R6, rounds 5 to 10, and of the
// implementer's test that is the other side of round 7's.
func replyOrderNamedCases() []replyOrderCase {
	return []replyOrderCase{
		{
			name:  "round 5 TestAReplysRoleMentionPinnedToACreditAfterTheBatchStillReachesIt, the attempt pending",
			first: "unnamed", roles: "unreadable", steps: []string{"read", "pin A", "fail"},
		},
		{
			name:  "round 5 TestAReplysRoleMentionPinnedToACreditAfterTheBatchStillReachesIt, the attempt resumed",
			first: "unnamed resumed", roles: "unreadable", steps: []string{"read", "pin A", "fail"},
		},
		{
			name: "round 5 TestAnEditMadeWhileTheReplysMentionOfACreditIsBeingSentReachesItOnceThatMentionFails" +
				" and round 6 TestAPersonsEditAsTheAPIWritesItReachesACreditTheReplyNamedWhoseMentionFailed, the mention pending",
			first: "named C", roles: "unreadable", steps: []string{"edit", "read", "fail"},
		},
		{
			name:  "round 6 TestAPersonsEditAsTheAPIWritesItReachesACreditTheReplyNamedWhoseMentionFailed, the mention failed",
			first: "named C failed", roles: "unreadable", steps: []string{"edit", "read"},
		},
		{
			name:  "round 6 TestAReplysRoleMentionPinnedToACreditPastTheForwardsBoundStillReachesIt",
			first: "unnamed", roles: "unreadable", steps: []string{"read", "age", "read", "pin A", "fail"},
		},
		{
			name:  "round 6 TestAReplysRoleMentionTheOutboxResolvedElsewherePinnedToACreditStillReachesIt",
			first: "unnamed", roles: "held elsewhere", steps: []string{"read", "pin A", "fail"},
		},
		{
			name:  "round 7 TestAReplysRoleMentionTheOutboxResolvedToACreditThatTheSenderPinsElsewhereLeavesTheCreditOut, pinned to C, sent",
			first: "unnamed resumed", roles: "held by A", steps: []string{"read", "pin C", "land"},
		},
		{
			name:  "round 7 TestAReplysRoleMentionTheOutboxResolvedToACreditThatTheSenderPinsElsewhereLeavesTheCreditOut, pinned to C, failed",
			first: "unnamed resumed", roles: "held by A", steps: []string{"read", "pin C", "fail"},
		},
		{
			name:  "round 7 TestAReplysRoleMentionTheOutboxResolvedToACreditThatTheSenderPinsElsewhereLeavesTheCreditOut, failed with no session",
			first: "unnamed resumed", roles: "held by A", steps: []string{"read", "fail"},
		},
		{
			name:  "round 7 TestAnEditOfAReplyWhoseLateRoleMentionOfACreditFailsReachesIt",
			first: "unnamed", roles: "unreadable", steps: []string{"age", "edit", "read", "pin A", "fail"},
		},
		{
			name:  "round 7 TestACreditRowLeavesTheReplyToTheSessionTheRetryReads, A claims the issue",
			first: "unnamed", roles: "unreadable",
			steps: []string{"age", "topic fails", "read", "claim A", "topic heals", "pin H", "land"},
		},
		{
			name:  "round 7 TestACreditRowLeavesTheReplyToTheSessionTheRetryReads, the issue is routed to A",
			first: "unnamed", roles: "unreadable",
			steps: []string{"age", "topic fails", "read", "route A", "topic heals", "pin H", "land"},
		},
		{
			name:  "round 7's other side TestACreditRowThatSettlesWithNothingSentSendsNoEditOfTheReply, pinned to A, sent",
			first: "unnamed", roles: "unreadable", steps: []string{"age", "edit", "read", "pin A", "land"},
		},
		{
			name:  "round 7's other side TestACreditRowThatSettlesWithNothingSentSendsNoEditOfTheReply, pinned to C, sent",
			first: "unnamed", roles: "unreadable", steps: []string{"age", "edit", "read", "pin C", "land"},
		},
		{
			name:  "round 9 TestAPersonsLaterCommentReachesACreditWithoutWaitingOnTheReplysCopy",
			first: "unnamed", roles: "unreadable", steps: []string{"read", "later", "read", "fail"},
		},
		{
			name:  "round 10 TestARetriedRoleMentionPinnedToACreditThatFailsStillReachesIt",
			first: "no holder", roles: "unreadable", steps: []string{"read", "retry A"},
		},
	}
}

// replyOrderGeneratedCases is every order of the edit, the late pin of the first attempt, that
// attempt's failure and a failed Retry, the outbox reading after the reply and after each event,
// with the person's later comment at the i-th place of the i-th order (modulo the five places it
// can take): once with the first attempt pinned to C and the Retry to A, and once with both pinned
// to A. That is 2 x 4! = 48 orders.
func replyOrderGeneratedCases() []replyOrderCase {
	var cases []replyOrderCase
	for _, family := range []struct{ pin, retry string }{{pin: "C", retry: "A"}, {pin: "A", retry: "A"}} {
		for i, order := range permutations([]string{"edit", "pin " + family.pin, "fail", "retry " + family.retry}) {
			order = slices.Insert(order, i%(len(order)+1), "later")
			steps := []string{"read"}
			for _, event := range order {
				steps = append(steps, event, "read")
			}
			cases = append(cases, replyOrderCase{
				name:  fmt.Sprintf("pinned to %s, Retry to %s: %s", family.pin, family.retry, strings.Join(order, " then ")),
				first: "unnamed", roles: "unreadable", steps: steps,
			})
		}
	}
	return cases
}

// permutations is every order of items, in the lexicographic order of their places in items.
func permutations(items []string) [][]string {
	if len(items) <= 1 {
		return [][]string{slices.Clone(items)}
	}
	var orders [][]string
	for i, first := range items {
		rest := slices.Concat(items[:i], items[i+1:])
		for _, order := range permutations(rest) {
			orders = append(orders, append([]string{first}, order...))
		}
	}
	return orders
}

// replyOrder is one case's world: the issue, the reply and every event of it the case appended,
// and the outbox that reads them.
type replyOrder struct {
	t         *testing.T
	ctx       context.Context
	database  *store.Store
	broker    *events.Broker
	issue     threadRuleIssue
	publisher *recordingPublisher
	deps      Deps
	instance  uuid.UUID
	root      string
	reply     string
	created   model.Event
	edits     []model.Event
	later     []model.Event
	// sessions names every session the case reads by its letter: A, B and C of the issue, D, which
	// credits a document no thread here is on, H, a session the role resolves to that no rule of
	// the outbox reaches, and E, the holder the outbox reads for a role held elsewhere.
	sessions map[string]string
	// obliged holds the letters of the sessions the issue's claim or route was moved to while the
	// reply was unpublished (dispatch://INFRA-1368's R1).
	obliged map[string]bool
}

func runReplyOrder(t *testing.T, c replyOrderCase) {
	ctx := context.Background()
	database := storetest.Open(t)
	issue := seedThreadRuleIssue(t, database)
	w := &replyOrder{
		t: t, ctx: ctx, database: database, broker: events.NewBroker(), issue: issue,
		publisher: &recordingPublisher{},
		sessions: map[string]string{
			"A": issue.a.ID, "B": issue.b.ID, "C": issue.c.ID, "D": issue.d.ID, "E": roleHolder, "H": "session-h",
		},
		obliged: map[string]bool{},
	}
	w.root = seedThreadComment(t, database, "T-1", "", issue.b, nil)
	w.reply = seedThreadComment(t, database, "T-1", "", issue.person, &w.root)
	// Each first attempt as the comment's write leaves it, and the session it names; a failed one's
	// receipt follows the comment, as its sender's settle does.
	first := map[string]struct{ session, outcome string }{
		"unnamed":         {"", "pending"},
		"unnamed resumed": {"", "resumed"},
		"no holder":       {"", "failed"},
		"named C":         {issue.c.ID, "pending"},
		"named C failed":  {issue.c.ID, "failed"},
	}
	attempt, ok := first[c.first]
	if !ok {
		t.Fatalf("no first attempt %q", c.first)
	}
	seedDelivery(t, database, w.reply, replyOrderTarget, attempt.session, attempt.outcome)
	w.created = appendEvent(t, database, w.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: issue.person,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: w.reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &w.root,
			Mentions: []model.Mention{{Target: replyOrderTarget, Delivery: "steer"}},
		}},
	})
	if attempt.outcome == "failed" {
		w.receipt(1, attempt.session, "failed")
	}
	w.deps = Deps{
		Store: database, Publisher: w.publisher, Broker: events.NewBroker(),
		Registry: newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}}),
	}
	switch c.roles {
	case "unreadable":
		w.deps.RoleClaims, w.deps.Sessions = unreadableRoleClaims{}, heldRoles{}
	case "held elsewhere":
		w.deps = withHeldRoles(w.deps)
	case "held by A":
		claims := claimedBy{holder: issue.a.ID}
		w.deps.RoleClaims, w.deps.Sessions = claims, claims
	default:
		t.Fatalf("no role claims %q", c.roles)
	}
	w.instance = holding(t, w.deps).instance
	for _, step := range c.steps {
		w.do(step)
	}
	w.settle()
	w.check()
}

// do takes one step of a case: "read", the outbox's read of whatever is due (read); "edit", the
// person's edit of the reply as the API writes it (mentions []); "later", the person's comment on
// the issue, no reply; "pin X", the first attempt's sender pinning it to X where it is pending and
// names no session yet; "fail" and "land", that attempt's send failing or being taken where it is
// pending, with its receipt; "retry X", the person's Retry, an attempt pinned to X whose send
// fails, with its receipt; "age", every attempt of the reply aged past unnamedForwardBound with
// its claim renewed, the sender alive; "topic fails" and "topic heals", every send of the reply
// to the issue's topic failing or no longer failing; and "claim A" and "route A", the issue's
// claim or route moved to A.
func (w *replyOrder) do(step string) {
	t := w.t
	verb, who, _ := strings.Cut(step, " ")
	switch step {
	case "read":
		w.read()
		return
	case "edit":
		w.edits = append(w.edits, appendEvent(t, w.database, w.broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.edited", Actor: w.issue.person,
			Payload: model.CommentEventPayload{Comment: model.Comment{
				ID: w.reply, IssueKey: new("T-1"), Author: w.issue.person, ReplyTo: &w.root, Body: "Edited.",
				Mentions: []model.Mention{},
			}},
		}))
		return
	case "later":
		comment := seedThreadComment(t, w.database, "T-1", "", w.issue.person, nil)
		w.later = append(w.later, appendCommentEvent(t, w.database, w.broker, "comment.created", w.issue.person, model.Comment{ID: comment}))
		return
	case "fail", "land":
		state := map[string]string{"fail": "failed", "land": "sent"}[step]
		var session string
		err := w.database.Pool.QueryRow(w.ctx, `
			update comment_deliveries
			set state = $3, error = case when $3 = 'failed' then 'envoy listener: delivery failed' end
			where comment_id = $1 and target = $2 and attempt = 1 and state = 'pending'
			returning coalesce(session_id, '')
		`, w.reply, replyOrderTarget, state).Scan(&session)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return
		case err != nil:
			t.Fatalf("settle the first attempt %s: %v", state, err)
		}
		w.receipt(1, session, state)
		return
	case "age":
		agedUnnamedAttempts(t, w.database, w.reply, unnamedForwardBound+30*time.Second)
		return
	case "topic fails":
		w.publisher.setFailTopic("notifications.dispatch.issue.T-1.comment.created")
		return
	case "topic heals":
		w.publisher.setFailTopic("")
		return
	}
	sessionID, ok := w.sessions[who]
	if !ok {
		t.Fatalf("no step %q", step)
	}
	switch verb {
	case "pin":
		if _, err := w.database.Pool.Exec(w.ctx, `
			update comment_deliveries set session_id = $3
			where comment_id = $1 and target = $2 and attempt = 1 and state = 'pending' and session_id is null
		`, w.reply, replyOrderTarget, sessionID); err != nil {
			t.Fatalf("pin the first attempt to %s: %v", sessionID, err)
		}
	case "retry":
		seedDelivery(t, w.database, w.reply, replyOrderTarget, sessionID, "failed")
		var attempt int
		if err := w.database.Pool.QueryRow(w.ctx, `
			select max(attempt) from comment_deliveries where comment_id = $1 and target = $2
		`, w.reply, replyOrderTarget).Scan(&attempt); err != nil {
			t.Fatalf("read the Retry's attempt: %v", err)
		}
		w.receipt(attempt, sessionID, "failed")
	case "claim":
		w.obliged[who] = w.obliged[who] || publishedAt(t, w.database, w.created.ID) == nil
		claimIssue(t, w.database, "T-1", model.Actor{Kind: "session", ID: sessionID})
	case "route":
		w.obliged[who] = w.obliged[who] || publishedAt(t, w.database, w.created.ID) == nil
		if _, err := w.database.Pool.Exec(w.ctx, `update issues set route = $2 where key = $1`, "T-1", "session:"+sessionID); err != nil {
			t.Fatalf("route the issue to %s: %v", sessionID, err)
		}
	default:
		t.Fatalf("no step %q", step)
	}
}

// receipt appends the comment.delivery receipt the API's sender appends as it settles attempt of
// the reply's mention, pinned to sessionID ("" for none), in state.
func (w *replyOrder) receipt(attempt int, sessionID, state string) {
	var session *string
	if sessionID != "" {
		session = &sessionID
	}
	failure := ""
	if state == "failed" {
		failure = "envoy listener: delivery failed"
	}
	appendEvent(w.t, w.database, w.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.delivery", Actor: w.issue.person,
		Payload: model.CommentDeliveryEventPayload{
			CommentID: w.reply, Target: replyOrderTarget, Attempt: attempt, Delivery: "steer",
			SessionID: session, State: state, Error: failure,
		},
	})
}

// read is one read of the outbox, as a running one makes after a wake: every backed-off event and
// every waiting fallback row due at once, a batch, and the fallback pass.
func (w *replyOrder) read() {
	if _, err := w.database.Pool.Exec(w.ctx, `update events set next_attempt_at = null where published_at is null`); err != nil {
		w.t.Fatalf("make the events due: %v", err)
	}
	if _, err := w.database.Pool.Exec(w.ctx, `update comment_fallback_copies set next_attempt_at = null where settled_at is null`); err != nil {
		w.t.Fatalf("make the fallback rows due: %v", err)
	}
	if _, err := scanBatch(w.ctx, w.deps, unfenced{}, w.instance); err != nil {
		w.t.Fatalf("scan batch: %v", err)
	}
	publishFallbackCopies(w.ctx, w.deps, unfenced{}, w.instance)
}

// settle reads until every event is published and every fallback row settled, and fails the case
// where eight reads leave either: each case ends with every attempt settled, so nothing is left
// for the outbox to wait on.
func (w *replyOrder) settle() {
	var unpublished, unsettled []string
	for range 8 {
		w.read()
		if err := w.database.Pool.QueryRow(w.ctx, `
			select
				coalesce((select array_agg(id::text || ' ' || type order by id) from events where published_at is null), '{}'),
				coalesce((select array_agg(destination order by destination) from comment_fallback_copies where settled_at is null), '{}')
		`).Scan(&unpublished, &unsettled); err != nil {
			w.t.Fatalf("read what the outbox has still to do: %v", err)
		}
		if len(unpublished) == 0 && len(unsettled) == 0 {
			return
		}
	}
	w.t.Fatalf("the outbox did not settle: events %v unpublished, fallback rows of %v unsettled", unpublished, unsettled)
}

// check reads what each session got and holds it against R6.
func (w *replyOrder) check() {
	t := w.t
	failed, sent := map[string]bool{}, map[string]bool{}
	rows, err := w.database.Pool.Query(w.ctx, `
		select session_id, state from comment_deliveries where comment_id = $1 and session_id is not null
	`, w.reply)
	if err != nil {
		t.Fatalf("read the reply's attempts: %v", err)
	}
	var sessionID, state string
	if _, err := pgx.ForEachRow(rows, []any{&sessionID, &state}, func() error {
		failed[sessionID] = failed[sessionID] || state == "failed"
		sent[sessionID] = sent[sessionID] || model.DeliveryTaken(state)
		return nil
	}); err != nil {
		t.Fatalf("read the reply's attempts: %v", err)
	}
	all := w.publisher.all()
	for _, letter := range slices.Sorted(maps.Keys(w.sessions)) {
		sessionID := w.sessions[letter]
		credited := letter == "A" || letter == "C"
		want, why := 0, "a session no rule of the reply reaches"
		switch {
		case letter == "B":
			want, why = 1, "the thread's session, live"
		case w.obliged[letter] && sent[sessionID]:
			why = "the session R1 obliges, which its delivered mention gave the reply"
		case w.obliged[letter]:
			want, why = 1, "the session R1 obliges, the issue's claim or route moved to it before the reply was published"
		case credited && sent[sessionID]:
			why = "a session the reply names whose mention delivered it, which holds that mention and no copy of the outbox's"
		case credited && failed[sessionID]:
			want, why = 1, "a session the reply names whose mention failed"
		case credited:
			why = "a session only a document credits, which no failed mention of the reply names, while the thread's session is live"
		}
		if got := copiesOf(all, w.created, sessionID); len(got) != want {
			t.Errorf("%s (%s) got %d copies of the reply from the outbox, keyed %v, want %d: %s (fallback rows %v)",
				letter, sessionID, len(got), keysOf(got), want, why, heldDestinations(t, w.database, w.reply))
		}
		for _, edit := range w.edits {
			if got := len(copiesOf(all, edit, sessionID)); got != want {
				t.Errorf("%s (%s) got %d copies of the person's edit, want %d, as many as of the reply: the edit follows the reply, and %s is %s",
					letter, sessionID, got, want, letter, why)
			}
		}
		wantLater := 0
		if credited {
			wantLater = 1
		}
		for _, later := range w.later {
			if got := len(copiesOf(all, later, sessionID)); got != wantLater {
				t.Errorf("%s (%s) got %d copies of the person's later comment on the issue, want %d: it is no reply, so the primary document's credits get it and the thread's session does not",
					letter, sessionID, got, wantLater)
			}
		}
	}
}
