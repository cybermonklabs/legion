package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAStoredListenerAnswerAnnouncesItself: a listener answer the roster stores
// (storeRouteRosterQuery) announces the store on events.SessionAccessChannel, since the session
// content rule reads the stored answer's lists and the document sockets' access epoch moves on
// it; an answer the statement does not store, one sent before the stored one, announces nothing.
// The silence is checked by order: a marker notification sent after it is the next one heard, as
// Postgres delivers notifications in commit order. The first store is the control: the listener
// hears it, so the silence after it is a silence.
func TestAStoredListenerAnswerAnnouncesItself(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	conn, err := database.Pool.Listener(ctx)
	if err != nil {
		t.Fatalf("open the LISTEN connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `listen `+events.SessionAccessChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}
	next := func() string {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		notification, err := conn.WaitForNotification(wait)
		if err != nil {
			t.Fatalf("wait for a notification: %v", err)
		}
		return notification.Payload
	}
	store := func(sent time.Time) bool {
		t.Helper()
		var stored bool
		err := database.Pool.QueryRow(ctx, storeRouteRosterQuery, true, []byte(`[]`), sent, 5.0, 5.0, "", "").Scan(&stored, nil)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatalf("store a listener answer: %v", err)
		}
		return stored
	}
	marker := func(what string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `select pg_notify($1, 'marker')`, events.SessionAccessChannel); err != nil {
			t.Fatalf("send the marker: %v", err)
		}
		if payload := next(); payload != "marker" {
			t.Fatalf("%s announced itself (payload %q) before the marker; want no notification", what, payload)
		}
	}

	now := time.Now()
	if !store(now) {
		t.Fatalf("the first answer was not stored")
	}
	if payload := next(); payload != "" {
		t.Fatalf("the stored answer announced %q, want the empty payload", payload)
	}
	marker("the stored answer's second notification")
	if store(now.Add(-time.Second)) {
		t.Fatalf("an answer sent before the stored one was stored")
	}
	marker("an answer the statement did not store")
}
