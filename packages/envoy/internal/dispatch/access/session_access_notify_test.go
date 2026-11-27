package access

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestATokensTieAndListedReachAnnounceThemselves: the tie a broker-issued token records
// (TieSession) and the private projects it lists (RecordListedReach) each announce their write on
// events.SessionAccessChannel, on which the document sockets' access epoch moves; the same call
// again, which writes no row, announces nothing, and neither does a list naming only a public
// project. Each silence is checked by order: a marker notification sent after it is the next one
// heard, as Postgres delivers notifications in commit order. The first write of each is the
// control: the listener hears it, so the silences after it are silences.
func TestATokensTieAndListedReachAnnounceThemselves(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('PRIV', 'Private', 'alice'), ('PUB', 'Public', 'alice')`,
		`update projects set visibility = 'private' where key = 'PRIV'`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
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
			t.Fatalf("wait for a notification on %s: %v", events.SessionAccessChannel, err)
		}
		return notification.Payload
	}
	silent := func(what string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `select pg_notify($1, 'marker')`, events.SessionAccessChannel); err != nil {
			t.Fatalf("send the marker: %v", err)
		}
		if payload := next(); payload != "marker" {
			t.Fatalf("%s announced itself (payload %q) before the marker; want no notification", what, payload)
		}
	}
	announced := func(what string) {
		t.Helper()
		if payload := next(); payload != "" {
			t.Fatalf("%s announced %q, want the empty payload", what, payload)
		}
		silent(what + "'s second notification")
	}

	if err := TieSession(ctx, database.Pool, "ses_token", "carol@example.com"); err != nil {
		t.Fatalf("tie the session: %v", err)
	}
	announced("the session's first tie")
	if err := TieSession(ctx, database.Pool, "ses_token", "carol@example.com"); err != nil {
		t.Fatalf("tie the session again: %v", err)
	}
	silent("a tie a row already holds")

	if err := RecordListedReach(ctx, database.Pool, "ses_token", []string{"PUB"}); err != nil {
		t.Fatalf("record a public project's reach: %v", err)
	}
	silent("a list naming only a public project")
	if err := RecordListedReach(ctx, database.Pool, "ses_token", []string{"PRIV", "PUB"}); err != nil {
		t.Fatalf("record a private project's reach: %v", err)
	}
	announced("the session's first reach of PRIV")
	if err := RecordListedReach(ctx, database.Pool, "ses_token", []string{"PRIV"}); err != nil {
		t.Fatalf("record PRIV's reach again: %v", err)
	}
	silent("a reach a row already holds")
}
