package delivery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// titlesIssue is the issue the session titles fixture writes its events against, in its own
// number band among this package's fixtures (intake_test.go's bands).
const (
	titlesIssue     = "TTLS-1"
	titlesIssueBand = 3000
)

// seedTitlesIssue writes titlesIssue in deliveryProject and clears its events, so a run against
// the database an earlier run left reads only what this test writes.
func seedTitlesIssue(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	seedDeliveryProject(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ($1, $2, $3, $1, 'todo', '{"kind":"system","id":"delivery-test"}', $1)
		on conflict do nothing`, titlesIssue, deliveryProject, titlesIssueBand+1); err != nil {
		t.Fatalf("seed issue %s: %v", titlesIssue, err)
	}
	if _, err := pool.Exec(ctx, `delete from events where issue_key = $1`, titlesIssue); err != nil {
		t.Fatalf("clear the events of %s: %v", titlesIssue, err)
	}
}

// writeSessionEvent appends one event of titlesIssue at at, stamped with actor as the writer
// stored it: a write's actor is JSONB on the events row, which is where ResolveSessionTitles
// reads a session's title from.
func writeSessionEvent(t *testing.T, ctx context.Context, pool *store.Pool, at time.Time, actor json.RawMessage) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select $1, coalesce(max(seq), 0) + 1, 'message.created', $2::jsonb, '{}', false, $3
		from events where issue_key = $1
	`, titlesIssue, actor, at); err != nil {
		t.Fatalf("write the session event of %s at %s: %v", titlesIssue, at, err)
	}
}

// actorJSON is the actor a write stamps, as json.Marshal stores it: ActorOrigin's fields are
// omitempty, so an origin holding no title is stored as {} and a nil one not at all.
func actorJSON(t *testing.T, actor model.Actor) json.RawMessage {
	t.Helper()
	stamped, err := json.Marshal(actor)
	if err != nil {
		t.Fatalf("marshal the actor of %s: %v", actor.ID, err)
	}
	return stamped
}

// TestASessionKeepsItsNameAfterAWriteThatCarriesNoTitle: a session's name is the newest title it
// stamped, not the title on its newest event. It writes three events -- one carrying the title
// "Release sender", then one whose actor carries no origin at all (what postDeliveryReply
// writes), then one whose origin carries an empty title -- and is still named by the title it
// stamped. A session that wrote but never stamped a title is absent from the map, which is the
// limit chosen for INFRA-1368: nothing here backfills a name, so SessionTitle answers its bare id.
func TestASessionKeepsItsNameAfterAWriteThatCarriesNoTitle(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedTitlesIssue(t, ctx, pool)

	const (
		named    = "ses_release_sender"
		nameless = "ses_never_titled"
	)
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	writeSessionEvent(t, ctx, pool, at, actorJSON(t, model.Actor{
		Kind: "session", ID: named, Origin: &model.ActorOrigin{SessionTitle: "Release sender"},
	}))
	writeSessionEvent(t, ctx, pool, at.Add(time.Minute), actorJSON(t, model.Actor{Kind: "session", ID: named}))
	// An empty title reaches a row through a write that builds its actor in SQL rather than
	// through json.Marshal, which drops the field: both must fail the read's title condition.
	writeSessionEvent(t, ctx, pool, at.Add(2*time.Minute),
		json.RawMessage(`{"kind":"session","id":"`+named+`","origin":{"session_title":""}}`))
	writeSessionEvent(t, ctx, pool, at.Add(3*time.Minute), actorJSON(t, model.Actor{Kind: "session", ID: nameless}))

	var empty int
	if err := pool.QueryRow(ctx, `
		select count(*) from events
		where issue_key = $1 and actor->>'id' = $2 and actor->'origin'->>'session_title' = ''
	`, titlesIssue, named).Scan(&empty); err != nil {
		t.Fatalf("count the empty-title events of %s: %v", named, err)
	}
	if empty != 1 {
		t.Fatalf("events of %s holding an empty session_title = %d, want the one this test wrote", named, empty)
	}

	titles, err := ResolveSessionTitles(ctx, pool, []string{named, nameless})
	if err != nil {
		t.Fatalf("ResolveSessionTitles: %v", err)
	}
	if got := titles[named]; got != "Release sender" {
		t.Errorf("the title of %s = %q, want %q: the newest title it stamped, which the two later writes carry none of", named, got, "Release sender")
	}
	if got, shown := titles[nameless]; shown {
		t.Errorf("the title of a session that never stamped one = %q, want it absent from the map", got)
	}
	if got := SessionTitle(titles, named); got != "Release sender" {
		t.Errorf("SessionTitle of %s = %q, want %q", named, got, "Release sender")
	}
	if got := SessionTitle(titles, nameless); got != nameless {
		t.Errorf("SessionTitle of a session that never stamped one = %q, want its bare id %q", got, nameless)
	}
}
