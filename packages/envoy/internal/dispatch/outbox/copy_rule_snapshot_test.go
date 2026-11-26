package outbox

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox/copyrule"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The copy rule reads an earlier event's record in one snapshot (copyrule.Holder,
// model.MentionCopyRuleSQL). Earlier event K of T-1 owes only the role topic of reviewer - its topic
// family reached, its forward to the role's holder not yet - recorded as of T-1's last event, and
// session S holds the role. A later comment mentions S. Right after the rule's first statement has
// read, the route moves to session:S, with issue.updated, and a batch records K again, owing S's
// own subject, whose send fails. Read as two statements - the earlier events that owe a key S may
// take, then the role routes they owe - the first finds no key and the second no role, and S's copy
// passes K, which owes S a send before the write and after it. Read in one snapshot, the copy
// waits behind K. The controls in the same run are the rule read alone before the write and after
// it, each of which holds the copy behind K, and the record the batch left, which shows the write
// fell where the test put it.
func TestTheCopyRuleReadsAnEarlierEventsRecordInOneSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "role:reviewer"
	seedIssue(t, database, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	earlier := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0505", IssueKey: new("T-1"), Body: "earlier"},
	})
	const session = "ses_holder"
	subject := contracts.LegacyAgentSubject(session)
	body := "@session:" + session + " look"
	commentID := seedComment(t, database, "T-1", alice, body, nil)
	comment := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: new("T-1"), Body: body,
			Mentions: []model.Mention{{Target: "session:" + session, Delivery: "steer"}},
		}},
	})
	// K as a batch that read T-1 as of the comment recorded it: its topic family reached, the
	// forward to the role's holder owed.
	if _, err := database.Pool.Exec(ctx, `
		update events set published_destinations = array[$2], owed_keys = array[$3],
		       owed_seq = (select last_seq from issues where key = 'T-1'), attempt_count = 1
		where id = $1
	`, earlier.ID, "notifications.dispatch.issue.T-1.message.created", contracts.RoleTopicPrefix+"reviewer"); err != nil {
		t.Fatalf("record the earlier event owing the role's topic: %v", err)
	}
	holds := func(_ context.Context, role string) bool { return role == "reviewer" }

	before, err := copyrule.Holder(ctx, database.Pool, comment.ID, subject, holds)
	if err != nil || before == nil || before.EventID != earlier.ID {
		t.Fatalf("control: before the write S's copy waits behind %s (%v), want event %d, whose forward to the role S holds is owed",
			describeHolder(before), err, earlier.ID)
	}

	deps := Deps{Store: database, Publisher: &recordingPublisher{failTopic: subject}, Broker: broker}
	outbox := holding(t, deps)
	wrote := false
	between := func() {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the route's move: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `update issues set route = 'session:`+session+`' where key = 'T-1'`); err != nil {
			t.Fatalf("move the route: %v", err)
		}
		if _, err := broker.Append(ctx, tx, model.Event{
			IssueKey: new("T-1"), Type: "issue.updated", Actor: alice, Payload: map[string]any{"changed_fields": []string{"route"}},
		}); err != nil {
			t.Fatalf("append issue.updated: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the route's move: %v", err)
		}
		outbox.scan(ctx, unfenced{})
		wrote = true
	}
	by, err := copyrule.Holder(ctx, &afterFirstStatement{pool: database.Pool, then: between}, comment.ID, subject, holds)
	if err != nil {
		t.Fatalf("read the rule: %v", err)
	}
	owed, _, _, _ := eventRecord(t, database, earlier.ID)
	t.Logf("the route moved and a batch ran after the rule's first statement: %t; K now owes %v; S's copy waits behind %s",
		wrote, owed, describeHolder(by))
	if !wrote || !slices.Equal(owed, []string{subject}) {
		t.Fatalf("the write after the rule's first statement ran %t and left K owing %v, want it run and K owing %s alone", wrote, owed, subject)
	}
	if by == nil || by.EventID != earlier.ID {
		t.Fatalf("S's copy waits behind %s, want event %d: K owed S a send through the role S holds before the write, and to S's own subject after it",
			describeHolder(by), earlier.ID)
	}

	after, err := copyrule.Holder(ctx, database.Pool, comment.ID, subject, holds)
	if err != nil || after == nil || after.EventID != earlier.ID {
		t.Fatalf("control: after the write S's copy waits behind %s (%v), want event %d, which owes S's own subject",
			describeHolder(after), err, earlier.ID)
	}
}

// afterFirstStatement is a pool on which then runs once, as the first statement's rows are
// closed: between that statement and any the reader makes after it.
type afterFirstStatement struct {
	pool copyrule.Querier
	then func()
	ran  bool
}

func (q *afterFirstStatement) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := q.pool.Query(ctx, sql, args...)
	if err != nil || q.ran {
		return rows, err
	}
	q.ran = true
	return &rowsThen{Rows: rows, then: q.then}, nil
}

// rowsThen runs then once its rows are closed.
type rowsThen struct {
	pgx.Rows
	then func()
	done bool
}

func (r *rowsThen) Close() {
	r.Rows.Close()
	if !r.done {
		r.done = true
		r.then()
	}
}
