package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
)

// censusRoleHolder is the session that holds every role this file's outbox resolves.
const censusRoleHolder = "ses_census_role_holder"

// censusRoles is the role claim store and the session reader of this file's outbox, standing in
// for Envoy's role and session buckets, which this package has none of: every role is claimed by
// censusRoleHolder, a live session that subscribed itself, so a role route is forwarded to it. It
// judges that session on the windows roles.Buckets judges one on (roles.Windows).
type censusRoles struct{ roles.Windows }

func (censusRoles) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{HolderSessionID: censusRoleHolder}, nil
}

func (censusRoles) Holder(context.Context, string) (session.SessionEntry, error) {
	return session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().UnixMilli()}, nil
}

func (censusRoles) LastSeen(string) int64 { return 0 }

// censusStream is the outbox's NATS client as a stream that acknowledges every send at once, and
// the window of each batch is that same stream: what this file reads is what the outbox records
// once its sends are answered, not the sends.
type censusStream struct{}

func (censusStream) WindowUntil(func() time.Time) bus.Sender { return censusStream{} }

func (censusStream) Connected() bool { return true }

func (censusStream) ConnectionChanged() <-chan struct{} { return nil }

func (censusStream) PublishAsync(contracts.Envelope) <-chan error { return acknowledged() }

func (censusStream) PublishStoredAsync(string, contracts.Envelope, string) <-chan error {
	return acknowledged()
}

func (censusStream) Request(string, contracts.Envelope, bus.RequestOptions) <-chan error {
	return acknowledged()
}

func (censusStream) Probe(time.Duration) error { return nil }

func acknowledged() <-chan error {
	answer := make(chan error, 1)
	answer <- nil
	return answer
}

// addressingWrites writes the rows and events that address a role, each as the API's handler for
// it writes them, on a migrated database and this process's event broker. This package cannot run
// the API, so each write names the handler whose rows and event it writes.
type addressingWrites struct {
	database *store.Store
	broker   *events.Broker
}

func newAddressingWrites(t *testing.T) addressingWrites {
	t.Helper()
	writes := addressingWrites{database: storetest.Open(t), broker: events.NewBroker()}
	if _, err := writes.database.Pool.Exec(context.Background(),
		`insert into projects (key, name, default_assignee) values ('CENS', 'Census', 'alice')`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	return writes
}

// write commits one write as the API's handler for it commits it: the rows rows writes and the
// event it answers, appended under the event's owner lock in the same transaction, and then the
// event handed to this process's broker, as the handler does after its commit (the server's
// publish), which wakes an outbox running on it.
func (w addressingWrites) write(t *testing.T, rows func(ctx context.Context, tx pgx.Tx) (model.Event, error)) model.Event {
	t.Helper()
	ctx := context.Background()
	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	defer tx.Rollback(ctx)
	event, err := rows(ctx, tx)
	if err != nil {
		t.Fatalf("write the rows of a %s: %v", event.Type, err)
	}
	appended, err := w.broker.Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append the %s event: %v", event.Type, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the %s: %v", event.Type, err)
	}
	w.broker.Publish(appended)
	return appended
}

// createIssue writes an issue as the API's createIssue does: the row, and the issue.created event
// whose payload is the issue it made, routed to no one.
func (w addressingWrites) createIssue(t *testing.T, key string, number int, rank string, by model.Actor) model.Event {
	t.Helper()
	return w.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		creator, err := json.Marshal(by)
		if err != nil {
			return model.Event{}, err
		}
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'CENS', $2, 'Routed and re-routed', $3, $4)
		`, key, number, creator, rank); err != nil {
			return model.Event{}, err
		}
		return model.Event{
			IssueKey: new(key), Type: "issue.created", Actor: by,
			Payload: model.IssueEventPayload{
				Issue:   model.Issue{Key: key, Title: "Routed and re-routed", Status: "todo"},
				Changed: []string{"title", "status"},
			},
		}, nil
	})
}

// route routes an issue to a role as the API's patchIssue does: the row's route set, and the
// issue.updated event whose payload is the issue as the write left it, naming the route as what
// it changed.
func (w addressingWrites) route(t *testing.T, key, role string, by model.Actor) model.Event {
	t.Helper()
	route := "role:" + role
	return w.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if _, err := tx.Exec(ctx, `update issues set route = $2 where key = $1`, key, route); err != nil {
			return model.Event{}, err
		}
		return model.Event{
			IssueKey: new(key), Type: "issue.updated", Actor: by,
			Payload: model.IssueEventPayload{
				Issue:   model.Issue{Key: key, Title: "Routed and re-routed", Status: "todo", Route: &route},
				Changed: []string{"route"},
			},
		}, nil
	})
}

// targetMessage writes a message aimed at a role as the API's createStoredMessage stores it: the
// row, the message.created event, and the message's first attempt, pending and naming no session
// yet.
func (w addressingWrites) targetMessage(t *testing.T, key, role string, by model.Actor) model.Event {
	t.Helper()
	return w.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		author, err := json.Marshal(by)
		if err != nil {
			return model.Event{}, err
		}
		target := "role:" + role
		message := model.Message{IssueKey: new(key), Author: by, Body: "For the role alone.", Target: &target}
		if err := tx.QueryRow(ctx, `
			insert into messages (issue_key, author, body, target) values ($1, $2, $3, $4)
			returning id::text, created_at
		`, message.IssueKey, author, message.Body, target).Scan(&message.ID, &message.CreatedAt); err != nil {
			return model.Event{}, err
		}
		if _, err := tx.Exec(ctx, `
			insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
			values ($1, 1, 'steer', '', 'pending', $2)
		`, message.ID, author); err != nil {
			return model.Event{}, err
		}
		return model.Event{
			IssueKey: new(key), Type: "message.created", Actor: by,
			Payload: model.MessageEventPayload{Message: message},
		}, nil
	})
}

// mentionRole writes a comment that mentions a role as the API's createCommentFor writes it: the
// row, the mention, its first attempt pinned to the session the role's lookup named and claimed
// by the sender, and the comment.created event naming the mention. It answers the comment's id
// and its event.
func (w addressingWrites) mentionRole(t *testing.T, key, role, reader string, by model.Actor) (string, model.Event) {
	t.Helper()
	target := "role:" + role
	var id string
	event := w.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		author, err := json.Marshal(by)
		if err != nil {
			return model.Event{}, err
		}
		comment := model.Comment{IssueKey: new(key), Author: by, Body: "@role:" + role + " a word for you."}
		if err := tx.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ($1, $2, $3)
			returning id::text, created_at
		`, comment.IssueKey, author, comment.Body).Scan(&comment.ID, &comment.CreatedAt); err != nil {
			return model.Event{}, err
		}
		id = comment.ID
		if _, err := tx.Exec(ctx, `
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id) values ($1, $2, 'steer', $3)
		`, comment.ID, target, reader); err != nil {
			return model.Event{}, err
		}
		if _, err := tx.Exec(ctx, `
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, claimed_at)
			values ($1, $2, 1, 'steer', $3, 'pending', now())
		`, comment.ID, target, reader); err != nil {
			return model.Event{}, err
		}
		comment.Mentions = []model.Mention{{Target: target, Delivery: "steer", SessionID: new(reader)}}
		comment.Deliveries = []model.CommentDelivery{}
		return model.Event{
			IssueKey: new(key), Type: "comment.created", Actor: by,
			Payload: model.CommentEventPayload{Comment: comment},
		}, nil
	})
	return id, event
}

// settleMention settles a comment's first attempt at a role sent, as the mention sender's settle
// does: the row, the session it reached (graphstore.Reached) and the comment.delivery receipt.
func (w addressingWrites) settleMention(t *testing.T, key, commentID, role, reader string, by model.Actor) model.Event {
	t.Helper()
	target := "role:" + role
	return w.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		var attemptedAt time.Time
		if err := tx.QueryRow(ctx, `
			update comment_deliveries set state = $3 where comment_id = $1 and target = $2 and attempt = 1
			returning created_at
		`, commentID, target, graphstore.DeliverySent).Scan(&attemptedAt); err != nil {
			return model.Event{}, err
		}
		if err := graphstore.Reached(ctx, tx, graphstore.DeliverySent, reader, by, attemptedAt); err != nil {
			return model.Event{}, err
		}
		return model.Event{
			IssueKey: new(key), Type: "comment.delivery", Actor: by,
			Payload: model.CommentDeliveryEventPayload{
				CommentID: commentID, Target: target, Attempt: 1, Delivery: "steer",
				SessionID: new(reader), State: graphstore.DeliverySent,
			},
		}, nil
	})
}

// roleCensusRig is addressingWrites with the real outbox running over its database and broker
// (outbox.New, Run), which writes what each event it publishes states about the people and agents
// of the store (the outbox's deliveredGraph).
type roleCensusRig struct {
	addressingWrites
	stop func()
}

func newRoleCensusRig(t *testing.T) *roleCensusRig {
	t.Helper()
	writes := newAddressingWrites(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		outbox.New(outbox.Deps{
			Store: writes.database, Publisher: censusStream{}, Broker: writes.broker,
			Listen: writes.database.Pool.Listener, RoleClaims: censusRoles{}, Sessions: censusRoles{},
		}).Run(ctx)
		close(done)
	}()
	rig := &roleCensusRig{addressingWrites: writes}
	stopped := false
	rig.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("the outbox did not stop")
		}
	}
	t.Cleanup(rig.stop)
	return rig
}

// published waits until the outbox has published every event the log holds. The record that
// marks an event published is the statement that writes what the event states in the store
// (the outbox's recordBatch), so the store holds all of it then.
func (rig *roleCensusRig) published(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var unpublished int
		if err := rig.database.Pool.QueryRow(context.Background(),
			`select count(*) from events where published_at is null`).Scan(&unpublished); err != nil {
			t.Fatalf("count the events the outbox has not published: %v", err)
		}
		if unpublished == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left %d event(s) unpublished", unpublished)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The store census holds the people and agents of the store to every role a row addresses now and
// every role an event of the log ever addressed (R19), and the outbox writes a role's node and its
// page for every role an event it publishes addresses. A node is history and never goes, so this
// drives the four ways a role is addressed through the writes that address it, and requires the
// census to read zero differences over the roles and pages they named, and each of those roles
// and pages to stand in the store.
//
//   - An issue routed to a role, published, then re-routed to another and published again: the
//     measured case of CORE-12, where the census read the first role as a live node no row
//     projects, because it read the issue's current route alone and the issue names only the
//     second now. The issue's two issue.updated events name both.
//   - A message targeted at a role (POST /api/v1/issues/{key}/messages, target "role:X"), which
//     reaches no route: messages.target names the role, and so does the event, from which the
//     outbox writes its node.
//   - A comment's @role: mention on an issue routed to another role: comment_deliveries.target
//     names the role, and so does the comment.created event's mention.
//   - A route an agent's session sets, whose event wakes nobody (events.Broker.Notify), so the
//     outbox routes nothing to it: issues.route names the role, and so does the event.
//
// The rows and events are written as the API's handlers write them (addressingWrites), since this
// package cannot run the API; the events are published by the real outbox (outbox.New, Run). A
// comment's own node is written by the backfill (Backfill), the one writer of a comment this
// package reaches, so that the census's comment kind agrees as well.
func TestTheRoleCensusReadsEveryRoleTheLogEverAddressed(t *testing.T) {
	const (
		routedFirst = "census-route-first"
		routedNow   = "census-route-now"
		messaged    = "census-message-target"
		mentioned   = "census-comment-mention"
		agentRouted = "census-agent-route"
		// reader is the session the comment's mention of its role was sent to.
		reader = "ses_census_mention_reader"
	)
	ctx := context.Background()
	rig := newRoleCensusRig(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	agent := model.Actor{Kind: "session", ID: "ses_census_routing_agent"}

	// The measured case: routed, published, re-routed, published again.
	rig.createIssue(t, "CENS-1", 1, "U", alice)
	rig.route(t, "CENS-1", routedFirst, alice)
	rig.published(t)
	rig.route(t, "CENS-1", routedNow, alice)
	rig.published(t)

	rig.targetMessage(t, "CENS-1", messaged, alice)
	commentID, _ := rig.mentionRole(t, "CENS-1", mentioned, reader, alice)
	rig.settleMention(t, "CENS-1", commentID, mentioned, reader, alice)

	// A route an agent's session sets on an issue of its own.
	rig.createIssue(t, "CENS-2", 2, "V", alice)
	if routed := rig.route(t, "CENS-2", agentRouted, agent); routed.Notify {
		t.Fatalf("an agent's route change notifies, so the outbox would route it; the case needs one it does not")
	}
	rig.published(t)
	rig.stop()

	if _, err := Backfill(ctx, rig.database, io.Discard); err != nil {
		t.Fatalf("backfill the comment's node: %v", err)
	}
	requireRoleHistoryOnly(t, rig.database, routedFirst)
	requireTheRoleCensusAgrees(t, rig.database, routedFirst, routedNow, messaged, mentioned, agentRouted)
}

// requireRoleHistoryOnly fails unless no row of the store names role now, so that only the event
// log addresses it.
func requireRoleHistoryOnly(t *testing.T, database *store.Store, role string) {
	t.Helper()
	var namedNow int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from issues where route = $1)
		     + (select count(*) from messages where target = $1)
		     + (select count(*) from comment_deliveries where target = $1)
	`, "role:"+role).Scan(&namedNow); err != nil {
		t.Fatalf("count the rows naming role %s: %v", role, err)
	}
	if namedNow != 0 {
		t.Fatalf("%d row(s) still name role %s, so the case would not read history", namedNow, role)
	}
}

// requireTheRoleCensusAgrees takes the store census and fails unless it reads no difference over
// any kind, and reads exactly one role row and one page row, each with its live node, per role of
// addressed. Each of those roles must hold its node and its page live, with the page part_of the
// role now.
func requireTheRoleCensusAgrees(t *testing.T, database *store.Store, addressed ...string) {
	t.Helper()
	report := storeCensusOf(t, database)
	for _, kind := range report.Kinds {
		t.Logf("kind=%s rows=%d nodes=%d row differences=%d node differences=%d",
			kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
		for _, difference := range append(append([]Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if report.Differences() != 0 || report.Refused() {
		t.Fatalf("the census read %d difference(s) and refused=%t, want none", report.Differences(), report.Refused())
	}
	for _, name := range []string{graphstore.KindRole, graphstore.KindPage} {
		if kind := censusKind(t, report, name); kind.Rows != int64(len(addressed)) || kind.Nodes != int64(len(addressed)) {
			t.Fatalf("the census read %d rows and %d live nodes of kind %s, want %d of each: one per role addressed",
				kind.Rows, kind.Nodes, name, len(addressed))
		}
	}
	for _, role := range addressed {
		requireRoleAndPage(t, database, role)
	}
}

// requireRoleAndPage fails unless the store holds role's node and its page live, with the page
// part_of the role now, and answers the role's node.
func requireRoleAndPage(t *testing.T, database *store.Store, role string) graphstore.Node {
	t.Helper()
	ctx := context.Background()
	page := graphstore.RolePageID(role)
	var roleNode graphstore.Node
	for _, node := range []struct{ kind, id string }{{graphstore.KindRole, role}, {graphstore.KindPage, page}} {
		held, found, err := graphstore.ReadNode(ctx, database.Pool, node.kind, node.id)
		if err != nil {
			t.Fatalf("read node %s %s: %v", node.kind, node.id, err)
		}
		if !found || held.State != graphstore.StateLive {
			t.Fatalf("the store holds node %s %s %t in state %q, want it live", node.kind, node.id, found, held.State)
		}
		if node.kind == graphstore.KindRole {
			roleNode = held
		}
	}
	parts, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindPage, page, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read what the page %s is part of: %v", page, err)
	}
	if len(parts) != 1 || parts[0].ToKind != graphstore.KindRole || parts[0].ToID != role {
		t.Fatalf("the page %s is part of %+v now, want the role %s alone", page, parts, role)
	}
	return roleNode
}
