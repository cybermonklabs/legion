package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// searchTextVersionsVersion creates the keyword table and its fill's cursor row's table.
const searchTextVersionsVersion = 20261120021308

// textVersionsTestLayout is the layout a cut is written in, to the microsecond in UTC, spelled
// here rather than read from the code under test.
const textVersionsTestLayout = "2006-01-02T15:04:05.000000Z"

// textVersionsLiteral is the literal a cut is written into the relation as.
func textVersionsLiteral(cut time.Time) string {
	return "'" + cut.UTC().Format(textVersionsTestLayout) + "'::timestamptz"
}

// A foreign key locks the table it names, and every table a keyword version names holds rows, so
// neither of the two tables carries one.
func TestTheKeywordTableHasNoForeignKey(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := store.Pool.Query(ctx, `
		select conrelid::regclass::text || ' ' || conname from pg_constraint
		 where contype = 'f'
		   and conrelid in ('public.search_text_versions'::regclass, 'public.search_text_versions_fill'::regclass)`)
	if err != nil {
		t.Fatalf("read the keyword tables' foreign keys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan a foreign key: %v", err)
		}
		t.Errorf("the keyword table carries the foreign key %s", key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the foreign keys: %v", err)
	}
}

// The fill writes every keyword version; no migration writes one. A database holding issues,
// comments, messages, asks and their events, as the release before the keyword table wrote them,
// holds no row in either table once the table's migration has run over it.
func TestNoMigrationWritesAKeywordVersion(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTestStore(t, searchTextVersionsVersion-1)
	seedCoreWithADefaultAssignee(t, store)
	if _, err := store.Pool.Exec(ctx, `
		with issue as (insert into issues (key, project_key, number, title, created_by, rank)
		               values ('CORE-1', 'CORE', 1, 'Rotate the signing key', '{"kind":"user","id":"alice"}', 'A') returning key),
		     comment as (insert into comments (issue_key, author, body)
		                 select key, '{"kind":"user","id":"alice"}', 'The old key expires on Friday.' from issue returning id, body),
		     message as (insert into messages (issue_key, author, body)
		                 select key, '{"kind":"session","id":"s1"}', 'Rotated in staging.' from issue returning id, body),
		     ask as (insert into asks (issue_key, author, question)
		             select key, '{"kind":"session","id":"s1"}', 'Rotate production now?' from issue returning id, question)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', 1, 'issue.created', '{"kind":"user","id":"alice"}'::jsonb,
		       jsonb_build_object('key', 'CORE-1', 'title', 'Rotate the signing key'), false
		union all
		select 'CORE-1', 2, 'comment.created', '{"kind":"user","id":"alice"}', jsonb_build_object('id', c.id, 'body', c.body), true from comment c
		union all
		select 'CORE-1', 3, 'message.created', '{"kind":"session","id":"s1"}', jsonb_build_object('id', m.id, 'body', m.body), true from message m
		union all
		select 'CORE-1', 4, 'ask.opened', '{"kind":"session","id":"s1"}', jsonb_build_object('id', k.id, 'question', k.question), true from ask k
	`); err != nil {
		t.Fatalf("seed as the release before %d wrote: %v", searchTextVersionsVersion, err)
	}

	if err := store.migrate(ctx, migrationsThrough(t, searchTextVersionsVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", searchTextVersionsVersion, err)
	}
	if got := schemaVersion(t, store); got != searchTextVersionsVersion {
		t.Fatalf("schema version = %d, want %d", got, searchTextVersionsVersion)
	}
	var issues, comments, messages, asks, events, versions, cursors int
	if err := store.Pool.QueryRow(ctx, `
		select (select count(*) from issues), (select count(*) from comments), (select count(*) from messages),
		       (select count(*) from asks), (select count(*) from events),
		       (select count(*) from search_text_versions), (select count(*) from search_text_versions_fill)`,
	).Scan(&issues, &comments, &messages, &asks, &events, &versions, &cursors); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	// The seed is still there, so the migration ran over items it could have written versions of.
	if issues != 1 || comments != 1 || messages != 1 || asks != 1 || events != 4 {
		t.Fatalf("seeded rows after the migration: %d issues, %d comments, %d messages, %d asks, %d events; want 1, 1, 1, 1 and 4",
			issues, comments, messages, asks, events)
	}
	if versions != 0 || cursors != 0 {
		t.Fatalf("after %d: search_text_versions holds %d rows and search_text_versions_fill %d, want none", searchTextVersionsVersion, versions, cursors)
	}
}

// The relation's columns, their types and their order are what its readers build on.
func TestTheKeywordRelationAnswersTheContractColumns(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := store.Pool.Query(ctx, `select * from `+TextVersions{Cut: time.Now()}.SQL()+` v limit 0`)
	if err != nil {
		t.Fatalf("select from the relation: %v", err)
	}
	var names []string
	var oids []uint32
	for _, field := range rows.FieldDescriptions() {
		names = append(names, field.Name)
		oids = append(oids, field.DataTypeOID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("select from the relation: %v", err)
	}
	var types []string
	if err := store.Pool.QueryRow(ctx, `
		select coalesce(array_agg(format_type(u.oid, null) order by u.n), '{}') from unnest($1::oid[]) with ordinality as u(oid, n)`,
		oids).Scan(&types); err != nil {
		t.Fatalf("name the columns' types: %v", err)
	}
	var got []string
	for i := range names {
		got = append(got, names[i]+" "+types[i])
	}
	want := []string{
		"kind text",
		"id text",
		"issue_key text",
		"project_key text",
		"event_id bigint",
		"event_seq integer",
		"started_at timestamp with time zone",
		"body text",
		"spec_artifact_id uuid",
		"spec_version integer",
		"search tsvector",
		"deleted_at timestamp with time zone",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the relation's columns:\n got %q\nwant %q", got, want)
	}
}

// A version holds at T when it started at or before T and no later version of its item, in seq
// order, started at or before T. Two comments and an ask are written straight into the table:
// one comment's three versions in seq order, the other comment's reversed pair (its later seq
// started first, as a transaction that began before another and committed after it writes), and
// an ask that shares the first comment's id, so an item is its kind and id together. Each time
// probed is a version's start, the microsecond before it, or a time past every start.
func TestTheKeywordRelationGivesOneVersionPerItemAtEachTime(t *testing.T) {
	ctx := context.Background()
	store := migratedEmptyStore(t)
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	// Starts fall on odd microseconds, so a cut written to the second, or rounded, would read
	// the microsecond before a start as the start itself.
	start := func(minute int) time.Time {
		return base.Add(time.Duration(minute)*time.Minute + 123457*time.Microsecond)
	}
	type version struct {
		kind, id string
		seq      int
		minute   int
	}
	for _, row := range []version{
		{"comment", "c1", 1, 10},
		{"comment", "c1", 3, 20},
		{"comment", "c1", 7, 30},
		{"comment", "c2", 2, 25},
		{"comment", "c2", 5, 15},
		{"ask", "c1", 4, 12},
	} {
		body := fmt.Sprintf("%s %s version %d", row.kind, row.id, row.seq)
		if _, err := store.Pool.Exec(ctx, `
			insert into search_text_versions (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from, search)
			values ($1, $2, 'CORE-1', 'CORE', $3, $4, $5, $6, 'event', to_tsvector('english', $6))`,
			row.kind, row.id, int64(row.seq), int32(row.seq), start(row.minute), body); err != nil {
			t.Fatalf("insert %s: %v", body, err)
		}
	}

	held := func(cut time.Time) map[string]int {
		t.Helper()
		rows, err := store.Pool.Query(ctx, `select kind, id, event_seq from `+TextVersions{Cut: cut}.SQL()+` v`)
		if err != nil {
			t.Fatalf("read the relation at %s: %v", cut, err)
		}
		defer rows.Close()
		got := map[string]int{}
		for rows.Next() {
			var kind, id string
			var seq int
			if err := rows.Scan(&kind, &id, &seq); err != nil {
				t.Fatalf("scan a version: %v", err)
			}
			item := kind + "/" + id
			if earlier, twice := got[item]; twice {
				t.Errorf("at %s the relation gives %s two versions, seq %d and seq %d", cut.Format(time.RFC3339Nano), item, earlier, seq)
			}
			got[item] = seq
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate the relation at %s: %v", cut, err)
		}
		return got
	}

	before := func(minute int) time.Time { return start(minute).Add(-time.Microsecond) }
	for _, probe := range []struct {
		name string
		cut  time.Time
		want map[string]int
	}{
		{"before any version", before(10), map[string]int{}},
		{"at c1's first version", start(10), map[string]int{"comment/c1": 1}},
		{"just before the ask's first version", before(12), map[string]int{"comment/c1": 1}},
		{"at the ask's first version", start(12), map[string]int{"comment/c1": 1, "ask/c1": 4}},
		{"just before c2's seq 5 started", before(15), map[string]int{"comment/c1": 1, "ask/c1": 4}},
		{"at c2's seq 5, which started before its seq 2", start(15), map[string]int{"comment/c1": 1, "comment/c2": 5, "ask/c1": 4}},
		{"just before c1's second version", before(20), map[string]int{"comment/c1": 1, "comment/c2": 5, "ask/c1": 4}},
		{"at c1's second version", start(20), map[string]int{"comment/c1": 3, "comment/c2": 5, "ask/c1": 4}},
		{"just before c2's seq 2 started", before(25), map[string]int{"comment/c1": 3, "comment/c2": 5, "ask/c1": 4}},
		{"at c2's seq 2, with both of its pair started", start(25), map[string]int{"comment/c1": 3, "comment/c2": 5, "ask/c1": 4}},
		{"just before c1's third version", before(30), map[string]int{"comment/c1": 3, "comment/c2": 5, "ask/c1": 4}},
		{"at c1's third version", start(30), map[string]int{"comment/c1": 7, "comment/c2": 5, "ask/c1": 4}},
		{"a day past every version", start(30).Add(24 * time.Hour), map[string]int{"comment/c1": 7, "comment/c2": 5, "ask/c1": 4}},
	} {
		if got := held(probe.cut); !reflect.DeepEqual(got, probe.want) {
			t.Errorf("%s (%s): the relation gives %v, want %v", probe.name, probe.cut.Format(time.RFC3339Nano), got, probe.want)
		}
	}
}

// A search over the relation reads the table's indexes: a statement shaped as the present
// search's comment arm (match, rank and snippet over the vector and body, the comment joined by
// its id) finds its matches through the GIN index and tests each for a later version of its item
// through the item index or the primary key, looked up by the item, with sequential scans off.
// The table holds versions of every kind, analyzed, few of which match, so the planner prices the
// plan as it would over a filled table rather than over an empty one, where reading every
// comment's versions in the item index costs as little as any other plan.
func TestTheKeywordRelationUsesItsIndexes(t *testing.T) {
	ctx := context.Background()
	store := migratedEmptyStore(t)
	if _, err := store.Pool.Exec(ctx, `
		with items as (
		  select k.kind, n.item,
		         case when k.kind = 'issue' then 'CORE-' || n.item else md5(k.kind || n.item)::uuid::text end as id
		    from (values ('issue', 300), ('comment', 2000), ('message', 300), ('ask', 300)) as k (kind, items),
		         generate_series(1, k.items) as n (item))
		insert into search_text_versions (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from, search)
		select i.kind, i.id, 'CORE-' || (i.item % 300 + 1), 'CORE', row_number() over (), v.seq,
		       timestamptz '2026-10-20 00:00:00+00' + (i.item % 1000) * interval '30 minutes' + v.seq * interval '2 days',
		       b.body, 'event', to_tsvector('english', b.body)
		  from items i, generate_series(1, 4) as v (seq),
		       lateral (select case when i.item % 400 = 0 then 'Rotate the signing key, step ' || v.seq
		                            else 'Notes on the release train, item ' || i.item || ', step ' || v.seq end as body) b;
		analyze search_text_versions;
	`); err != nil {
		t.Fatalf("fill the keyword table: %v", err)
	}
	plan := explainWithoutSeqScan(t, store, `
		select v.id, c.issue_key, ts_rank_cd(v.search, q.tsq, 32) as r,
		       ts_headline('english', v.body, q.tsq, 'MaxWords=24, MinWords=12, MaxFragments=1') as snippet
		  from `+TextVersions{Cut: time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC)}.SQL()+` v
		  join public.comments c on c.id = v.id::uuid,
		       (select websearch_to_tsquery('english', 'signing key') as tsq) q
		 where v.kind = 'comment' and v.search @@ q.tsq
		 order by r desc, v.id
		 limit 100`)
	t.Logf("plan:\n%s", plan)
	if !strings.Contains(plan, "search_text_versions_search") {
		t.Errorf("the match does not read search_text_versions_search:\n%s", plan)
	}
	if !strings.Contains(plan, "search_text_versions_item_start") && !strings.Contains(plan, "search_text_versions_pkey") {
		t.Errorf("the test for a later version reads neither search_text_versions_item_start nor search_text_versions_pkey:\n%s", plan)
	}
	// Looked up by the item, rather than read whole and matched afterwards.
	if !strings.Contains(plan, "(id = v.id)") {
		t.Errorf("the test for a later version does not look its item up in the index:\n%s", plan)
	}
}

// TextVersionsAt refuses a cut the fill has not reached with an error naming the reach, and
// answers a cut at the reach with the relation at that cut.
func TestTextVersionsAtRefusesACutPastTheReach(t *testing.T) {
	ctx := context.Background()

	// The refusal's words, at the times the contract spells them.
	example := &TextVersionsNotReachedError{
		Cut:     time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC),
		Reach:   time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC),
		Reached: true,
	}
	if got, want := example.Error(), "the keyword table's fill has reached 2026-11-19T20:00:00.000000Z, before the cut 2026-11-19T21:00:00.000000Z"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	store := migratedEmptyStore(t)
	reach := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	past := reach.Add(time.Microsecond)

	// No cursor row: the fill has reached no time.
	var refusal *TextVersionsNotReachedError
	if _, err := store.TextVersionsAt(ctx, reach); !errors.As(err, &refusal) || refusal.Reached {
		t.Fatalf("TextVersionsAt with no cursor row = %v, want a refusal that has reached nothing", err)
	}
	if got, want := refusal.Error(), "the keyword table's fill has reached no time yet"; got != want {
		t.Errorf("Error() with no cursor row = %q, want %q", got, want)
	}
	if _, reached, err := store.TextVersionsReach(ctx); err != nil || reached {
		t.Fatalf("TextVersionsReach with no cursor row = reached %v, %v; want nothing reached", reached, err)
	}

	// A cursor row storing the reach, and above its cursor an event written one microsecond past
	// the reach, which the fill has not written. A cut past the stored reach makes TextVersionsAt
	// compute the reach again, and on a quiet database that would come to about now - 1 s, past
	// every cut this test asks for; the unwritten event holds the computed reach before its own
	// created_at, at the stored reach, so the refusal below is the fill's and not the clock's.
	seedCoreWithADefaultAssignee(t, store)
	if _, err := store.Pool.Exec(ctx, `
		insert into events (project_key, seq, type, actor, payload, notify, created_at)
		values ('CORE', 1, 'project.created', '{"kind":"user","id":"alice"}', '{}', false, $1)`, past); err != nil {
		t.Fatalf("write an event the fill has not written: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into search_text_versions_fill (id, event_id, reach, rules) values (1, 0, $1, 1)`, reach); err != nil {
		t.Fatalf("store the fill's cursor and reach: %v", err)
	}

	_, err := store.TextVersionsAt(ctx, past)
	wrapped := fmt.Errorf("search at %s: %w", past, err)
	refusal = nil
	if !errors.As(wrapped, &refusal) {
		t.Fatalf("TextVersionsAt one microsecond past the reach = %v, want *TextVersionsNotReachedError", err)
	}
	if !refusal.Reached || !refusal.Reach.Equal(reach) || !refusal.Cut.Equal(past) {
		t.Fatalf("the refusal = %+v, want the cut %s refused at the reach %s", *refusal, past, reach)
	}
	if got, want := refusal.Error(), "the keyword table's fill has reached "+reach.Format(textVersionsTestLayout)+
		", before the cut "+past.Format(textVersionsTestLayout); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// A cut at the reach, given in another zone, is answered, and the relation is written at it in
	// UTC; so is a cut inside the reach's microsecond, which the SQL writes as the reach.
	for _, cut := range []time.Time{reach.In(time.FixedZone("UTC-8", -8*60*60)), reach.Add(999 * time.Nanosecond)} {
		versions, err := store.TextVersionsAt(ctx, cut)
		if err != nil {
			t.Fatalf("TextVersionsAt(%s) at the reach %s: %v", cut.Format(time.RFC3339Nano), reach, err)
		}
		if sql := versions.SQL(); !strings.Contains(sql, textVersionsLiteral(reach)) {
			t.Errorf("TextVersionsAt(%s).SQL() does not hold %s:\n%s", cut.Format(time.RFC3339Nano), textVersionsLiteral(reach), sql)
		}
	}

	// Once the fill has written the event and moved its cursor past it, storing no later reach,
	// the same cut past the stored reach is answered: the reach computed again passes it.
	if _, err := store.Pool.Exec(ctx, `update search_text_versions_fill set event_id = (select max(id) from events)`); err != nil {
		t.Fatalf("move the cursor past the event: %v", err)
	}
	if _, err := store.TextVersionsAt(ctx, past); err != nil {
		t.Fatalf("TextVersionsAt one microsecond past the stored reach, with nothing left unwritten: %v", err)
	}
	computed, reached, err := store.TextVersionsReach(ctx)
	if err != nil || !reached || !computed.After(past) || !computed.Before(time.Now()) {
		t.Fatalf("TextVersionsReach with nothing left unwritten = %s, reached %v, %v; want a time after %s and before now",
			computed, reached, err, past)
	}
}
