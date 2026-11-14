package store

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// eventsSessionTitlesQuery is delivery.ResolveSessionTitles' statement as it read session titles
// out of events before session_titles (migration 20261113082156) held them, letter for letter. It
// is held here rather than built from anything the package ships, so the comparison below asks
// the old read its own question however the new one changes.
const eventsSessionTitlesQuery = `
		select s.id, e.session_title
		from unnest($1::text[]) s(id)
		cross join lateral (
			select actor->'origin'->>'session_title' as session_title
			from events
			where actor->>'kind' = 'session' and actor->>'id' = s.id
				and nullif(actor->'origin'->>'session_title', '') is not null
			order by created_at desc
			limit 1
		) e
	`

// titlesIssue is the issue the titles fixture writes its events on.
const titlesIssue = "TITLES-1"

// titlesBulkSessions is how many generated sessions the fixture adds beside its named cases: more
// than two of FillSessionTitles' batches, so a fill of them commits several batches and ends on
// an empty one.
const titlesBulkSessions = 2*SessionTitlesFillBatch + 500

// migratedTitlesStore is a store carrying every migration, with titlesIssue in it.
func migratedTitlesStore(t *testing.T) *Store {
	t.Helper()
	store := openEmptyTestStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `insert into projects (key, name) values ('TITLES', 'Titles')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'TITLES', 1, 'Titles', '{"kind":"user","id":"alice"}', 'a')
	`, titlesIssue); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
	return store
}

// titledEvent is one event of the fixture: its actor as stored and its created_at.
type titledEvent struct {
	actor string
	at    time.Time
}

// sessionActor is the actor JSON a write stamps for a session, with title when it is not empty.
func sessionActor(id, title string) string {
	if title == "" {
		return fmt.Sprintf(`{"kind":"session","id":%q}`, id)
	}
	return fmt.Sprintf(`{"kind":"session","id":%q,"origin":{"session_title":%q}}`, id, title)
}

// writeTitlesFixture writes, in the order listed, the events the old read and session_titles are
// compared over, and answers how many sessions among them stamped a title:
//
//   - a session that stamped one title;
//   - one that stamped a title and then wrote with no origin and with an empty title, which keep
//     the name it stamped;
//   - one whose two titled events share one created_at, as one transaction's events do, which the
//     first of them inserted names;
//   - one whose newer titled event was inserted before its older one, which the newer names;
//   - one retitled, which the later title names;
//   - one that never stamped a title, and a person's actor carrying a title under a session's id,
//     neither of which names a session;
//   - titlesBulkSessions generated sessions, each titled and then writing once untitled.
func writeTitlesFixture(t *testing.T, store *Store) int {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	named := []titledEvent{
		{sessionActor("ses_once", "Stamped once"), at(1)},
		{sessionActor("ses_untitled_newer", "Kept"), at(2)},
		{sessionActor("ses_untitled_newer", ""), at(3)},
		{`{"kind":"session","id":"ses_untitled_newer","origin":{"session_title":""}}`, at(4)},
		{sessionActor("ses_tie", "First of the instant"), at(5)},
		{sessionActor("ses_tie", "Second of the instant"), at(5)},
		{sessionActor("ses_late_older", "Newer"), at(7)},
		{sessionActor("ses_late_older", "Older"), at(6)},
		{sessionActor("ses_retitled", "Old name"), at(8)},
		{sessionActor("ses_retitled", "New name"), at(9)},
		{sessionActor("ses_never", ""), at(10)},
		{`{"kind":"user","id":"ses_never","origin":{"session_title":"A person"}}`, at(11)},
	}
	for seq, event := range named {
		if _, err := store.Pool.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ($1, $2, 'comment.created', $3::jsonb, '{}', false, $4)
		`, titlesIssue, seq+1, event.actor, event.at); err != nil {
			t.Fatalf("write event %d: %v", seq+1, err)
		}
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select $1, $2::int + 2 * n + k,
		       'comment.created',
		       case k when 0 then jsonb_build_object('kind', 'session', 'id', 'ses_bulk_' || lpad(n::text, 5, '0'),
		                                             'origin', jsonb_build_object('session_title', 'Bulk ' || n))
		              else jsonb_build_object('kind', 'session', 'id', 'ses_bulk_' || lpad(n::text, 5, '0')) end,
		       '{}', false, $3::timestamptz + make_interval(mins => k)
		from generate_series(1, $4::int) as n, generate_series(0, 1) as k
		order by n, k
	`, titlesIssue, len(named), at(20), titlesBulkSessions); err != nil {
		t.Fatalf("write the generated sessions' events: %v", err)
	}
	return 5 + titlesBulkSessions
}

// eventsReadTitles is what eventsSessionTitlesQuery answers for every session the events table
// holds and one it has never heard of, read under the plan production reads it by: through
// events_session_actor_created_at_idx, which a fixture this small would otherwise skip for a
// sequential scan, and whose order is what decides a tie.
func eventsReadTitles(t *testing.T, store *Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the old read: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	var sessions []string
	if err := tx.QueryRow(ctx, `
		select array_agg(distinct actor->>'id') || array['ses_unknown']
		from events where actor->>'kind' = 'session'
	`).Scan(&sessions); err != nil {
		t.Fatalf("list the sessions: %v", err)
	}
	return titleRows(t, tx, eventsSessionTitlesQuery, sessions)
}

// storedTitles is every row of session_titles.
func storedTitles(t *testing.T, store *Store) map[string]string {
	t.Helper()
	return titleRows(t, store.Pool, `select session_id, title from session_titles`)
}

// titleRows is statement's (session, title) rows as a map.
func titleRows(t *testing.T, queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, statement string, arguments ...any) map[string]string {
	t.Helper()
	rows, err := queryer.Query(context.Background(), statement, arguments...)
	if err != nil {
		t.Fatalf("read the titles: %v", err)
	}
	titles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var pair [2]string
		return pair, row.Scan(&pair[0], &pair[1])
	})
	if err != nil {
		t.Fatalf("read the titles: %v", err)
	}
	answered := make(map[string]string, len(titles))
	for _, pair := range titles {
		answered[pair[0]] = pair[1]
	}
	return answered
}

// requireSameTitles fails the test where stored and old differ, naming the sessions they differ on.
func requireSameTitles(t *testing.T, what string, stored, old map[string]string) {
	t.Helper()
	if maps.Equal(stored, old) {
		return
	}
	var differ []string
	for session := range maps.Keys(old) {
		if stored[session] != old[session] {
			differ = append(differ, fmt.Sprintf("%s: stored %q, old read %q", session, stored[session], old[session]))
		}
	}
	for session := range maps.Keys(stored) {
		if _, read := old[session]; !read {
			differ = append(differ, fmt.Sprintf("%s: stored %q, old read none", session, stored[session]))
		}
	}
	t.Fatalf("%s holds %d titles and the old read of events answers %d; they differ on %v", what, len(stored), len(old), differ)
}

// The trigger on events (migration 20261113090412) stores, for every session, the title the read
// of events it replaces answered, as each event is inserted: the newest title a session stamped,
// none for a session that stamped none or for a person's actor, the first event of an instant on a
// tie, and the newer of two events whichever was inserted first.
func TestTheTriggerStoresTheTitleTheEventsReadAnswered(t *testing.T) {
	store := migratedTitlesStore(t)
	titled := writeTitlesFixture(t, store)
	old := eventsReadTitles(t, store)
	if len(old) != titled {
		t.Fatalf("the old read answers %d titled sessions; want the fixture's %d", len(old), titled)
	}
	for session, want := range map[string]string{
		"ses_untitled_newer": "Kept", "ses_tie": "First of the instant", "ses_late_older": "Newer", "ses_retitled": "New name",
	} {
		if old[session] != want {
			t.Fatalf("the old read names %s %q; want %q, which is what this fixture is built to ask it", session, old[session], want)
		}
	}
	requireSameTitles(t, "session_titles, as the trigger wrote it,", storedTitles(t, store), old)
}

// The boot's fill (FillSessionTitles) stores, for the events stored before the trigger existed,
// the titles the read of events it replaces answered, over more sessions than two of its batches
// take, and writes no row where the trigger already stored the title of the session's newest
// event: it writes one row fewer than the titled sessions. Once it has finished it owes nothing:
// the progress row is gone, and a second fill reads one empty table. A fill run again over the
// filled table writes no row.
func TestTheFillStoresTheTitleTheEventsReadAnswered(t *testing.T) {
	store := migratedTitlesStore(t)
	ctx := context.Background()
	// The events below are stored as a database holds them before 20261113090412 applied.
	if _, err := store.Pool.Exec(ctx, `alter table events disable trigger events_session_title`); err != nil {
		t.Fatalf("disable the trigger: %v", err)
	}
	titled := writeTitlesFixture(t, store)
	if _, err := store.Pool.Exec(ctx, `alter table events enable trigger events_session_title`); err != nil {
		t.Fatalf("enable the trigger: %v", err)
	}
	if stored := storedTitles(t, store); len(stored) != 0 {
		t.Fatalf("session_titles holds %v before the fill; want nothing, the events having been stored before the trigger", stored)
	}
	// A retitle once the trigger is live: the trigger stores its title, which is the newest event's
	// and so the one the fill reads too, and the fill finds that row naming its event already.
	if _, err := store.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, 100000, 'comment.created', $2::jsonb, '{}', false, '2026-09-02T09:00:00Z')
	`, titlesIssue, sessionActor("ses_retitled", "Newest name")); err != nil {
		t.Fatalf("write the retitle: %v", err)
	}

	written, err := store.FillSessionTitles(ctx)
	if err != nil {
		t.Fatalf("FillSessionTitles: %v", err)
	}
	if written != int64(titled-1) {
		t.Fatalf("the fill wrote %d rows; want %d, every titled session but the one the trigger titled", written, titled-1)
	}
	old := eventsReadTitles(t, store)
	if old["ses_retitled"] != "Newest name" {
		t.Fatalf("the old read names ses_retitled %q; want the retitle's %q", old["ses_retitled"], "Newest name")
	}
	requireSameTitles(t, "session_titles, as the fill wrote it,", storedTitles(t, store), old)

	var owed int
	if err := store.Pool.QueryRow(ctx, `select count(*) from session_titles_backfill`).Scan(&owed); err != nil {
		t.Fatalf("read the fill's progress: %v", err)
	}
	if owed != 0 {
		t.Fatalf("session_titles_backfill holds %d rows after the fill; want none, the fill owed no more", owed)
	}
	if again, err := store.FillSessionTitles(ctx); err != nil || again != 0 {
		t.Fatalf("a second fill wrote %d rows (%v); want none, reading only the empty progress table", again, err)
	}
	if _, err := store.Pool.Exec(ctx, `insert into session_titles_backfill default values`); err != nil {
		t.Fatalf("owe the fill again: %v", err)
	}
	if again, err := store.FillSessionTitles(ctx); err != nil || again != 0 {
		t.Fatalf("a fill over the filled table wrote %d rows (%v); want none, every row already naming its event", again, err)
	}
	requireSameTitles(t, "session_titles, after the fill ran again,", storedTitles(t, store), old)
}
