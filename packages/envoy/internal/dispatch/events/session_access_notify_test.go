package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// sessionAccessListener LISTENs on SessionAccessChannel on a connection of its own and answers, in
// order, the payload of each notification it hears.
type sessionAccessListener struct {
	t        *testing.T
	database *store.Store
	conn     *pgx.Conn
}

func listenSessionAccess(t *testing.T, database *store.Store) *sessionAccessListener {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Pool.Listener(ctx)
	if err != nil {
		t.Fatalf("open the LISTEN connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `listen `+SessionAccessChannel); err != nil {
		t.Fatalf("listen on %s: %v", SessionAccessChannel, err)
	}
	return &sessionAccessListener{t: t, database: database, conn: conn}
}

// next answers the payload of the next notification, failing the test when none arrives.
func (l *sessionAccessListener) next() string {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	notification, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		l.t.Fatalf("wait for a notification on %s: %v", SessionAccessChannel, err)
	}
	return notification.Payload
}

// sentNothing sends a marker notification of its own and checks it is the next one heard: Postgres
// delivers notifications in commit order, so a write committed before the marker that announced
// itself would be heard first. It decides by order, not by a clock.
func (l *sessionAccessListener) sentNothing(what string) {
	l.t.Helper()
	if _, err := l.database.Pool.Exec(context.Background(), `select pg_notify($1, 'marker')`, SessionAccessChannel); err != nil {
		l.t.Fatalf("send the marker: %v", err)
	}
	if payload := l.next(); payload != "marker" {
		l.t.Fatalf("%s announced itself on %s (payload %q) before the marker; want no notification", what, SessionAccessChannel, payload)
	}
}

// TestASessionsFirstTieAndProjectAnnounceThemselves: an event that records a session's first tie
// to its person or first project (queueSessionProject) announces it on SessionAccessChannel in its
// own transaction, which the document sockets' access epoch moves on; an event that records nothing
// - the same session's second event in the same project, or a person's event - announces nothing.
// The first write is the control: the listener hears it, so the silence after it is a silence.
func TestASessionsFirstTieAndProjectAnnounceThemselves(t *testing.T) {
	database := storetest.Open(t)
	seedEventOwners(t, database)
	broker := NewBroker()
	listener := listenSessionAccess(t, database)
	issueEvent := func(actor model.Actor) model.Event {
		return model.Event{IssueKey: new("PP-1"), Type: "issue.updated", Actor: actor, Payload: map[string]any{}}
	}

	appendCommitted(t, database, broker, issueEvent(provenSession("ses_first")))
	if payload := listener.next(); payload != "" {
		t.Fatalf("the session's first event announced %q, want the empty payload", payload)
	}
	listener.sentNothing("the first event's notifications, one per transaction,")
	appendCommitted(t, database, broker, issueEvent(provenSession("ses_first")))
	listener.sentNothing("the session's second event in the same project, which records nothing,")
	appendCommitted(t, database, broker, issueEvent(model.Actor{Kind: "user", ID: "alice"}))
	listener.sentNothing("a person's event")
}
