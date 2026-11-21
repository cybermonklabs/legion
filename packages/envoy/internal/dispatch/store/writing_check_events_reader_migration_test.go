package store

import (
	"context"
	"testing"
	"time"
)

const (
	writingCheckEventsVersion       = 20261119050001
	writingCheckEventsReaderVersion = 20261119050002
)

// 20261119050002 adds writing_check_events.reader and admits the outcome exempt_no_reader. A row
// written before it keeps its outcome and records no reader, the census refuses nothing, and after
// it a row names its reader and a write with no person as its reader is recorded as such; an
// outcome the check does not name, the refused and forced outcomes of a check that refused writes
// and a model reader's fail-open outcomes, which nothing writes, among them, is still refused.
func TestMigrationGivesTheWritingCheckEventsAReaderAndTheNoReaderOutcome(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, writingCheckEventsReaderVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", writingCheckEventsReaderVersion-1, err)
	}
	if got := schemaVersion(t, store); got != writingCheckEventsVersion {
		t.Fatalf("schema version = %d, want %d: the migration before this one", got, writingCheckEventsVersion)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into writing_check_events (route, method, actor_kind, actor_id, outcome)
		values ('/api/v1/issues/{key}/comments', 'POST', 'session', 's-one', 'clean')
	`); err != nil {
		t.Fatalf("seed a row of the schema before: %v", err)
	}
	if got := censusCount(t, store, writingCheckEventsReaderVersion); got != 0 {
		t.Fatalf("census = %d, want 0: the migration refuses no row", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, writingCheckEventsReaderVersion)); err != nil {
		t.Fatalf("migrate through %d: %v", writingCheckEventsReaderVersion, err)
	}
	var seeded *string
	if err := store.Pool.QueryRow(ctx, `select reader from writing_check_events where actor_id = 's-one'`).Scan(&seeded); err != nil {
		t.Fatalf("read the seeded row: %v", err)
	}
	if seeded != nil {
		t.Errorf("the row written before the migration names reader %q, want none", *seeded)
	}
	if err := store.InsertWritingCheckEvent(ctx, WritingCheckEvent{
		Route: "/api/v1/issues/{key}/comments", Method: "POST", ActorKind: "session", ActorID: "s-two",
		Outcome: "findings", Reader: "alice",
	}); err != nil {
		t.Fatalf("insert a row with a reader: %v", err)
	}
	if err := store.InsertWritingCheckEvent(ctx, WritingCheckEvent{
		Route: "/api/v1/broadcasts", Method: "POST", ActorKind: "user", ActorID: "alice", ActSession: "s-three",
		Outcome: "exempt_no_reader",
	}); err != nil {
		t.Fatalf("insert a row with no reader: %v", err)
	}
	if err := store.InsertWritingCheckEvent(ctx, WritingCheckEvent{
		Route: "/api/v1/comments/{id}/reply", Method: "POST", ActorKind: "session", ActorID: "s-five",
		Outcome: "exempt_generated", Reader: "alice",
	}); err != nil {
		t.Fatalf("insert a plugin's generated reply: %v", err)
	}
	rows, err := store.Pool.Query(ctx, `select actor_id, outcome, coalesce(reader, '<none>') from writing_check_events where actor_id <> 's-one' order by id`)
	if err != nil {
		t.Fatalf("read the rows: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var actor, outcome, reader string
		if err := rows.Scan(&actor, &outcome, &reader); err != nil {
			t.Fatalf("scan a row: %v", err)
		}
		got = append(got, actor+" "+outcome+" "+reader)
	}
	if want := []string{"s-two findings alice", "alice exempt_no_reader <none>", "s-five exempt_generated alice"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("rows = %q, want %q", got, want)
	}
	for _, outcome := range []string{"exempt_unknown", "refused", "forced", "reader_timeout", "no_model"} {
		if err := store.InsertWritingCheckEvent(ctx, WritingCheckEvent{
			Route: "/api/v1/broadcasts", Method: "POST", ActorKind: "session", ActorID: "s-four", Outcome: outcome,
		}); err == nil {
			t.Errorf("the outcome %q was stored, want the outcome check to refuse it", outcome)
		}
	}
	// The counts read word_questions too, which a later migration creates.
	if err := store.migrate(ctx, migrationsThrough(t, wordQuestionsVersion)); err != nil {
		t.Fatalf("migrate through %d: %v", wordQuestionsVersion, err)
	}
	var today time.Time
	if err := store.Pool.QueryRow(ctx, `select now()`).Scan(&today); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	counts, err := store.CountWritingCheck(ctx, today.UTC(), today.UTC(), 50, 0)
	if err != nil {
		t.Fatalf("count the day: %v", err)
	}
	if len(counts.Days) != 1 {
		t.Fatalf("counts = %+v, want one day", counts)
	}
	if day := counts.Days[0]; day.Outcomes["exempt_no_reader"] != 1 || day.Outcomes["findings"] != 1 || day.Outcomes["clean"] != 1 ||
		day.Outcomes["exempt_generated"] != 1 || day.Total != 4 {
		t.Errorf("counts = %+v, want one exempt_no_reader and one exempt_generated beside the findings and the clean row", day)
	}
}
