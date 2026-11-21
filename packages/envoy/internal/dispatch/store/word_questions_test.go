package store

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"
)

const wordQuestionsVersion = 20261119050003

// openWordRecordsStore is a database of its own migrated through the records' migration.
func openWordRecordsStore(t *testing.T) *Store {
	t.Helper()
	store := openEmptyTestStore(t)
	if err := store.migrate(context.Background(), migrationsThrough(t, wordQuestionsVersion)); err != nil {
		t.Fatalf("migrate through %d: %v", wordQuestionsVersion, err)
	}
	return store
}

// insertLogEvent appends an event with no owner to the log and answers its id.
func insertLogEvent(t *testing.T, store *Store, eventType string, actor, payload map[string]any, at time.Time) int64 {
	t.Helper()
	actorJSON, _ := json.Marshal(actor)
	payloadJSON, _ := json.Marshal(payload)
	var id int64
	if err := store.Pool.QueryRow(context.Background(), `
		insert into events (seq, type, actor, payload, notify, created_at)
		values ((select coalesce(max(seq), 0) + 1 from events), $1, $2, $3, false, $4) returning id`,
		eventType, actorJSON, payloadJSON, at).Scan(&id); err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
	return id
}

// usersTexts is a stand-in for wordrecords.PersonTexts: an event a user wrote is one comment of
// theirs.
func usersTexts(event WordRecordsEvent) []WordRecordsText {
	var actor struct{ Kind, ID string }
	if json.Unmarshal(event.Actor, &actor) != nil || actor.Kind != "user" {
		return nil
	}
	return []WordRecordsText{{Key: "event:" + strconv.FormatInt(event.ID, 10), Person: actor.ID, Kind: "comment"}}
}

type recordRow struct {
	Key, Person, State, Verdict, Day string
	EventID                          int64
	Words                            []string
	AboutWords                       bool
	Attempts                         int
}

func records(t *testing.T, store *Store) []recordRow {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `
		select text_key, coalesce(person, ''), state, coalesce(verdict, ''), day::text, coalesce(event_id, 0), words, about_words, attempts
		from word_questions order by id`)
	if err != nil {
		t.Fatalf("read the records: %v", err)
	}
	defer rows.Close()
	var out []recordRow
	for rows.Next() {
		var row recordRow
		if err := rows.Scan(&row.Key, &row.Person, &row.State, &row.Verdict, &row.Day, &row.EventID, &row.Words, &row.AboutWords, &row.Attempts); err != nil {
			t.Fatalf("read a record: %v", err)
		}
		out = append(out, row)
	}
	return out
}

func wordRecordsCursor(t *testing.T, store *Store) *int64 {
	t.Helper()
	var cursor *int64
	if err := store.Pool.QueryRow(context.Background(), `select event_id from word_records_cursor`).Scan(&cursor); err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	return cursor
}

// 20261119050003 creates the records and the cursor and refuses no row; the cursor starts null,
// a verdict the selection prompt does not give is refused, and a record whose verdict is about
// words says so.
func TestMigrationCreatesTheWordRecords(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, wordQuestionsVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", wordQuestionsVersion-1, err)
	}
	if got := censusCount(t, store, wordQuestionsVersion); got != 0 {
		t.Fatalf("census = %d, want 0", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, wordQuestionsVersion)); err != nil {
		t.Fatalf("migrate through %d: %v", wordQuestionsVersion, err)
	}
	if cursor := wordRecordsCursor(t, store); cursor != nil {
		t.Errorf("cursor = %d, want null before the first pass", *cursor)
	}
	insert := `insert into word_questions (source, text_key, event_id, written_at, day, kind, verdict, state)
		values ('event', $1, 1, now(), current_date, 'comment', $2, 'done')`
	if _, err := store.Pool.Exec(ctx, insert, "event:1", "asks_word_meaning"); err != nil {
		t.Fatalf("insert a record: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, insert, "event:2", "asks_about_weather"); err == nil {
		t.Error("a verdict the prompt does not give was stored")
	}
	if _, err := store.Pool.Exec(ctx, insert, "event:1", "none"); err == nil {
		t.Error("a second record of one text was stored")
	}
	if rows := records(t, store); len(rows) != 1 || !rows[0].AboutWords {
		t.Errorf("records = %+v, want one about words", rows)
	}
}

// The intake's first pass starts at the first event created at or after from, which is the
// backfill: an older text is never read. Each page moves the cursor, a page that is not full moves
// it to the safe horizon, and a text is one record however often it is read.
func TestTheRecordsIntakeStartsAtItsFromTime(t *testing.T) {
	ctx := context.Background()
	store := openWordRecordsStore(t)
	from := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	person := map[string]any{"kind": "user", "id": "alice"}
	agent := map[string]any{"kind": "session", "id": "s-agent"}
	before := insertLogEvent(t, store, "comment.created", person, map[string]any{"body": "What is a shard?"}, from.Add(-time.Hour))
	first := insertLogEvent(t, store, "comment.created", person, map[string]any{"body": "What is a lane?"}, from.Add(time.Hour))
	insertLogEvent(t, store, "comment.created", agent, map[string]any{"body": "The lane is the queue."}, from.Add(2*time.Hour))
	other := insertLogEvent(t, store, "issue.updated", person, map[string]any{"title": "A title"}, from.Add(3*time.Hour))
	last := insertLogEvent(t, store, "ask.answered", person, map[string]any{"answer": map[string]any{"text": "Yes."}}, from.Add(24*time.Hour))
	full, err := store.WordRecordsIntake(ctx, from, last, 2, usersTexts)
	if err != nil || !full {
		t.Fatalf("first page: full %t, %v; want a full page", full, err)
	}
	if cursor := wordRecordsCursor(t, store); cursor == nil || *cursor != first+1 {
		t.Fatalf("cursor after the first page = %v, want %d", cursor, first+1)
	}
	for full {
		if full, err = store.WordRecordsIntake(ctx, from, last, 2, usersTexts); err != nil {
			t.Fatalf("next page: %v", err)
		}
	}
	if cursor := wordRecordsCursor(t, store); cursor == nil || *cursor != last {
		t.Fatalf("cursor = %v, want the safe horizon %d", cursor, last)
	}
	rows := records(t, store)
	var events []int64
	for _, row := range rows {
		events = append(events, row.EventID)
		if row.State != WordQuestionPending || row.Person != "alice" {
			t.Errorf("record %+v, want alice's, pending", row)
		}
	}
	// The issue update is handed no payload (WordRecordsEventTypes), so it holds no text.
	if !slices.Equal(events, []int64{first, last}) || slices.Contains(events, before) || slices.Contains(events, other) {
		t.Errorf("records of events %v, want %d and %d", events, first, last)
	}
	if rows[0].Day != "2026-10-16" || rows[1].Day != "2026-10-17" {
		t.Errorf("days %s and %s, want the days written", rows[0].Day, rows[1].Day)
	}
	if _, err := store.WordRecordsIntake(ctx, from, last, 2, usersTexts); err != nil {
		t.Fatalf("a pass with nothing new: %v", err)
	}
	if got := len(records(t, store)); got != 2 {
		t.Errorf("%d records after a pass with nothing new, want 2", got)
	}
}

// A failed classification keeps the record due after its backoff; a verdict makes it done with its
// words. A Define use is a record of its own, once. The reader's words are each word once, by the
// case its person last wrote it in, before a time where one is given. The counts give each day,
// project and person their records, their answered asks and the reviewer's rows.
func TestTheRecordsVerdictsWordsAndCounts(t *testing.T) {
	ctx := context.Background()
	store := openWordRecordsStore(t)
	day := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	person := map[string]any{"kind": "user", "id": "alice"}
	question := insertLogEvent(t, store, "comment.created", person, map[string]any{"body": "What is a Store?"}, day)
	plain := insertLogEvent(t, store, "comment.created", person, map[string]any{"body": "Approved, thanks."}, day.Add(time.Minute))
	later := insertLogEvent(t, store, "comment.created", person, map[string]any{"body": "What is a store now?"}, day.Add(48*time.Hour))
	if _, err := store.WordRecordsIntake(ctx, day.Add(-time.Hour), later, 10, usersTexts); err != nil {
		t.Fatalf("intake: %v", err)
	}
	due, err := store.DueWordQuestions(ctx, 10)
	if err != nil || len(due) != 3 {
		t.Fatalf("due = %+v, %v; want the three texts", due, err)
	}
	ids := map[int64]int64{}
	for _, q := range due {
		ids[q.EventID] = q.ID
	}
	if err := store.RecordWordFailure(ctx, ids[question], "", "the model is down", time.Hour); err != nil {
		t.Fatalf("record a failure: %v", err)
	}
	if due, _ := store.DueWordQuestions(ctx, 10); len(due) != 2 {
		t.Errorf("due after a failure = %+v, want the failed text held back for its backoff", due)
	}
	if err := store.RecordWordFailure(ctx, ids[question], "ask:00000000-0000-0000-0000-000000000001", "the model is down", 0); err != nil {
		t.Fatalf("record a second failure: %v", err)
	}
	due, _ = store.DueWordQuestions(ctx, 10)
	if !slices.ContainsFunc(due, func(q WordQuestion) bool { return q.ID == ids[question] && q.Attempts == 2 }) {
		t.Errorf("due = %+v, want the failed text due again after two attempts", due)
	}
	for event, verdict := range map[int64]WordVerdict{
		question: {Verdict: "asks_word_meaning", Words: []string{"Store"}, Model: "stand-in"},
		plain:    {Verdict: "none", Model: "stand-in"},
		later:    {Verdict: "asks_word_meaning", Words: []string{"store", "lane"}, Model: "stand-in"},
	} {
		if err := store.RecordWordVerdict(ctx, ids[event], verdict); err != nil {
			t.Fatalf("record a verdict: %v", err)
		}
	}
	if err := store.RecordWordVerdict(ctx, ids[plain], WordVerdict{Verdict: "none"}); err == nil {
		t.Error("a second verdict of one record was taken")
	}
	define := DefineUse{DefinitionID: "def-1", Person: "alice", Project: "WCK", Words: []string{"cursor"}, Answers: "ask:00000000-0000-0000-0000-000000000002", At: day.Add(time.Hour)}
	for range 2 {
		if err := store.RecordDefineUse(ctx, define); err != nil {
			t.Fatalf("record a Define use: %v", err)
		}
	}
	words, err := store.ReaderWords(ctx, "alice", time.Time{})
	if err != nil {
		t.Fatalf("read alice's words: %v", err)
	}
	var got []string
	for _, word := range words {
		got = append(got, word.Word)
	}
	if !slices.Equal(got, []string{"lane", "store", "cursor"}) || words[1].Times != 2 {
		t.Errorf("words = %+v, want lane, store (twice, as last written) and cursor", words)
	}
	if before, _ := store.ReaderWords(ctx, "alice", day.Add(30*time.Minute)); len(before) != 1 || before[0].Word != "Store" {
		t.Errorf("words before the Define use = %+v, want the first question's alone", before)
	}
	if none, _ := store.ReaderWords(ctx, "bob", time.Time{}); len(none) != 0 {
		t.Errorf("bob's words = %+v, want none", none)
	}
	for _, event := range []WritingCheckEvent{
		{Route: "/api/v1/issues/{key}/comments", Method: "POST", ActorKind: "session", ActorID: "s-agent", Outcome: "held", Reader: "alice", Project: "WCK"},
		{Route: "/api/v1/issues/{key}/comments", Method: "POST", ActorKind: "session", ActorID: "s-agent", Outcome: "posted_as_written", Reader: "alice", Project: "WCK"},
		{Route: "/api/v1/issues/{key}/comments", Method: "POST", ActorKind: "session", ActorID: "s-agent", Outcome: "reviewed_after", Reader: "alice", Project: "WCK"},
		{Route: "/api/v1/asks/{id}/rewrites", Method: "POST", ActorKind: "user", ActorID: "alice", Outcome: "rewrite_requested", Reader: "alice", Project: "WCK"},
	} {
		if err := store.InsertWritingCheckEvent(ctx, event); err != nil {
			t.Fatalf("record %s: %v", event.Outcome, err)
		}
	}
	if _, err := store.Pool.Exec(ctx, `update writing_check_events set created_at = $1`, day); err != nil {
		t.Fatalf("date the reviewer's rows: %v", err)
	}
	counts, err := store.CountWritingCheck(ctx, day, day.Add(48*time.Hour), 50, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(counts.Days) != 3 || counts.Days[0].Total != 4 || counts.Days[0].Outcomes["held"] != 1 {
		t.Errorf("days = %+v, want three, the first with the four reviewer rows", counts.Days)
	}
	want := []WritingCheckRow{
		{Day: "2026-10-20", Project: "", Person: "alice", WordQuestions: 1, Texts: 2},
		{Day: "2026-10-20", Project: "WCK", Person: "alice", DefineUses: 1, RewriteUses: 1, ReviewerHeld: 1, PostedOverFinding: 1},
		{Day: "2026-10-22", Project: "", Person: "alice", WordQuestions: 1, Texts: 1},
	}
	if !slices.Equal(counts.Items, want) || counts.Total != len(want) || counts.Limit != 50 || counts.Offset != 0 {
		t.Errorf("rows = %+v total %d limit %d offset %d\nwant %+v of %d at limit 50 offset 0", counts.Items, counts.Total, counts.Limit, counts.Offset, want, len(want))
	}
	paged, err := store.CountWritingCheck(ctx, day, day.Add(48*time.Hour), 1, 1)
	if err != nil {
		t.Fatalf("count a page: %v", err)
	}
	if !slices.Equal(paged.Items, want[1:2]) || paged.Total != len(want) || len(paged.Days) != 3 {
		t.Errorf("the second page of one = %+v total %d days %d, want %+v of %d and the three days", paged.Items, paged.Total, len(paged.Days), want[1:2], len(want))
	}
	past, err := store.CountWritingCheck(ctx, day, day.Add(48*time.Hour), 50, len(want))
	if err != nil {
		t.Fatalf("count past the last row: %v", err)
	}
	if len(past.Items) != 0 || past.Total != len(want) {
		t.Errorf("a page past the last row = %+v total %d, want none of %d", past.Items, past.Total, len(want))
	}
	words, total, err := store.ReaderWordsPage(ctx, "alice", time.Time{}, 1, 1)
	if err != nil || total != 3 || len(words) != 1 || words[0].Word != "store" {
		t.Errorf("alice's second word of one = %+v total %d err %v, want store of 3", words, total, err)
	}
	if words, total, err := store.ReaderWordsPage(ctx, "alice", time.Time{}, 50, 3); err != nil || total != 3 || len(words) != 0 {
		t.Errorf("alice's words past the last = %+v total %d err %v, want none of 3", words, total, err)
	}
}
