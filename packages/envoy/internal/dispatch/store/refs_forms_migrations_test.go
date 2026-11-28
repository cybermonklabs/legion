package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// beforeRefsForms is the schema the seed's test stores its text at: a migration from before the
// forms queue (20261115182525_near_copy_checks), though not the last one, since
// 20261116163813_issue_role_holders, which creates a table that holds no text, applies after it
// too, before 20261117025900_refs_forms_owed through
// 20261117025910_ask_block_refs_refs_forms_delete_trigger.
// lastRefsFormsTrigger is the last of the source triggers (20261117025901-20261117025904) that
// the seed runs after.
const (
	beforeRefsForms      = 20261115182525
	lastRefsFormsTrigger = 20261117025904
)

// readOwedForms is every row of refs_forms_owed, with the enqueued_at it holds.
func readOwedForms(t *testing.T, store *Store) map[owedSource]time.Time {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `select kind, id::text, enqueued_at from refs_forms_owed`)
	if err != nil {
		t.Fatalf("read refs_forms_owed: %v", err)
	}
	defer rows.Close()
	owed := map[owedSource]time.Time{}
	for rows.Next() {
		var source owedSource
		var at time.Time
		if err := rows.Scan(&source.kind, &source.id, &at); err != nil {
			t.Fatalf("scan refs_forms_owed: %v", err)
		}
		owed[source] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs_forms_owed: %v", err)
	}
	return owed
}

func assertOwedForms(t *testing.T, store *Store, want ...owedSource) map[owedSource]time.Time {
	t.Helper()
	owed := readOwedForms(t, store)
	got := map[owedSource]bool{}
	for source := range owed {
		got[source] = true
	}
	wanted := map[owedSource]bool{}
	for _, source := range want {
		wanted[source] = true
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("refs_forms_owed holds %v, want %v", got, wanted)
	}
	return owed
}

// Text stored before this release was indexed by no build that reads an issue's tab addresses or
// a document's dashboard URLs, so the seed owes a re-index to every comment, every ask, every
// issue message and every document with a markdown version, with no edit and no command run. An
// image's version and a direct message, which no mention is indexed from, are owed nothing, and
// no `scope` row is written: these forms change no `#id`.
func TestTheRefsFormsSeedOwesTextStoredBeforeIt(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeRefsForms)
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	assertOwedForms(t, store,
		owedSource{"artifact", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
}

// The source triggers are live before the seed runs, so a write that lands between them is owed by
// its trigger and keeps the enqueued_at that write gave it: the seed adds the rows it finds missing
// and rewrites none.
func TestTheRefsFormsSeedKeepsWhatATriggerAlreadyOwed(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, lastRefsFormsTrigger)
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	enqueued, ok := readOwedForms(t, store)[owedSource{"comment", fixture.comment}]
	if !ok {
		t.Fatalf("the triggers owed %v, want the comment among them", readOwedForms(t, store))
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	after := assertOwedForms(t, store,
		owedSource{"artifact", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
	if at := after[owedSource{"comment", fixture.comment}]; !at.Equal(enqueued) {
		t.Fatalf("the seed moved the comment's enqueued_at to %v, want the trigger's %v", at, enqueued)
	}
}

// What a build that predates the forms writes during the roll is owed a re-index by the source
// triggers, on the writes refs_grammar_owed's triggers owe it for, whatever its text says: a
// comment, an ask and an issue message on insert and on an edit of the text or of the columns that
// say which document a `#id` in it resolves against, an ask on a write of its options or its
// answer too, which are its parts, and a document version its document. An
// image's version, a direct message, an update of a column the index does not read, an anchor
// refresh and an ask's question-block settlement owe nothing of their own - the last owes no
// `scope` row either, since these forms change no `#id` - beyond the ask itself, whose
// block_artifact_id is a column its index reads.
func TestTheRefsFormsTriggersOweWhatAnyBuildWrites(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	write := func(what, query string, args ...any) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	drain := func() { t.Helper(); write("drain the queue", `delete from refs_forms_owed`) }

	document := owedSource{"artifact", fixture.document}
	comment := owedSource{"comment", fixture.comment}
	ask := owedSource{"ask", fixture.ask}
	message := owedSource{"message", fixture.message}
	assertOwedForms(t, store, document, comment, ask, message)

	// A column the index does not read owes nothing; an edit of the text owes it again.
	drain()
	write("resolve the comment", `update comments set resolved = true where id = $1`, fixture.comment)
	write("raise the ask's urgency", `update asks set urgency = 'high' where id = $1`, fixture.ask)
	assertOwedForms(t, store)
	write("edit the comment", `update comments set body = body || ' See dispatch://CORE-1/plan.' where id = $1`, fixture.comment)
	write("edit the message", `update messages set body = body || ' See dispatch://CORE-1/plan.' where id = $1`, fixture.message)
	write("edit the direct message", `update messages set body = body || ' See dispatch://CORE-1/plan.' where id = $1`, fixture.direct)
	write("edit the ask's question", `update asks set question = question || ' See dispatch://CORE-1/plan.' where id = $1`, fixture.ask)
	assertOwedForms(t, store, comment, message, ask)

	// A later write of a row already owed moves its enqueued_at, so a drain that listed the
	// earlier time leaves it owed.
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	write("age the owed comment", `update refs_forms_owed set enqueued_at = $2 where kind = 'comment' and id = $1`, fixture.comment, past)
	write("edit the comment again", `update comments set body = body || ' Again.' where id = $1`, fixture.comment)
	if at := readOwedForms(t, store)[comment]; !at.After(past) {
		t.Fatalf("an edited owed comment kept enqueued_at %v, want it moved past %v", at, past)
	}

	// An ask's options and its answer are parts an older build re-indexes on their write - the
	// answer through the one row write its transition makes - so each owes the ask; the options and
	// the answer written back as they stand owe nothing.
	drain()
	write("edit the ask's options alone", `update asks set options = '[{"label":"dispatch://CORE-1/plan"}]' where id = $1`, fixture.ask)
	assertOwedForms(t, store, ask)
	drain()
	write("answer the ask as a transition does", `
		update asks set state = 'answered', answer = '{"user":"bob","selected":["dispatch://CORE-1/plan"],"at":"2026-01-01T00:00:00Z"}'
		where id = $1`, fixture.ask)
	assertOwedForms(t, store, ask)
	drain()
	write("write the options and the answer back as they stand", `update asks set options = options, answer = answer where id = $1`, fixture.ask)
	assertOwedForms(t, store)

	// The anchor's artifact_id is a column an older build's index reads, so moving it owes the
	// row; the anchor refreshes a settlement makes - a moved quote, an orphan mark, the block id -
	// re-index nothing and owe nothing.
	notes := insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text`)
	for _, item := range []struct {
		table string
		id    string
		owed  owedSource
	}{{"comments", fixture.comment, comment}, {"asks", fixture.ask, ask}} {
		drain()
		write("anchor the "+item.owed.kind, `update `+item.table+`
			set anchor = jsonb_build_object('artifact_id', $2::text, 'quote', 'Blocked')
			where id = $1`, item.id, fixture.document)
		assertOwedForms(t, store, item.owed)

		drain()
		write("move the "+item.owed.kind+"'s quote", `update `+item.table+`
			set anchor = jsonb_set(jsonb_set(anchor, '{quote}', to_jsonb('Moved'::text)), '{orphaned}', to_jsonb(true))
			where id = $1`, item.id)
		write("record the "+item.owed.kind+"'s anchor block", `update `+item.table+`
			set anchor = jsonb_set(anchor, '{block_id}', to_jsonb('b2'::text), true)
			where id = $1`, item.id)
		assertOwedForms(t, store)

		write("anchor the "+item.owed.kind+" to another document", `update `+item.table+`
			set anchor = jsonb_set(anchor, '{artifact_id}', to_jsonb($2::text))
			where id = $1`, item.id, notes)
		assertOwedForms(t, store, item.owed)
	}

	// A body written back as it stands cites what it cited; the comment a comment replies to is a
	// column an older build's index reads.
	drain()
	write("rewrite the comment's body as it stands", `update comments set body = body where id = $1`, fixture.comment)
	assertOwedForms(t, store)
	reply := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'A reply.')
		returning id::text`)
	drain()
	write("make the comment a reply", `update comments set reply_to = $2 where id = $1`, fixture.comment, reply)
	assertOwedForms(t, store, comment)

	// A version of a document owes the document and no scope; an image's version owes nothing.
	drain()
	write("write version 2", `
		insert into artifact_versions (artifact_id, number, markdown) values ($1, 2, 'See https://dispatch.example/issues/CORE-1/plan.')
	`, fixture.document)
	write("write the image's second version", `
		insert into artifact_versions (artifact_id, number, content, mime) values ($1, 2, '\x89504e47', 'image/png')
	`, fixture.image)
	assertOwedForms(t, store, document)

	// The settlement of a question block owes no scope here: the ask is owed for the
	// block_artifact_id it gains, a column its own index reads, and nothing else is.
	drain()
	write("settle the question block", `update asks set block_id = 'q1', block_artifact_id = $2 where id = $1`, fixture.ask, fixture.document)
	assertOwedForms(t, store, ask)
	drain()
	write("rename the question block", `update asks set block_id = 'q2' where id = $1`, fixture.ask)
	assertOwedForms(t, store)
}

// Every delete from refs owes the source whose edges it deleted, once per statement: an older
// build re-indexes text with no write of the source row - its scope drain, its block-index pass -
// and this is the one statement each of those re-indexes makes. Only the queue's four source kinds
// are owed, and only an id in the canonical spelling a source's id::text writes, so a synthetic
// source is passed over rather than refused; a delete that removes nothing owes nothing; and a row
// already owed keeps its enqueued_at.
func TestTheRefsFormsDeleteTriggerOwesEverySourceADeleteTouches(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedCoreWithADefaultAssignee(t, store)
	fixture := writeRefsGrammarFixture(t, store)
	write := func(what, query string, args ...any) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	upper := strings.ToUpper(fixture.message)
	write("write edges", `
		insert into refs (from_kind, from_id, to_kind, to_id) values
			('comment', $1, 'issue', 'CORE-1'), ('comment', $1, 'issue', 'CORE-2'),
			('artifact', $2, 'issue', 'CORE-1'),
			('ask', $3, 'issue', 'CORE-1'),
			('comment', 'not-a-uuid', 'issue', 'CORE-1'),
			('message', $4, 'issue', 'CORE-1'),
			('project', $1, 'issue', 'CORE-1')
	`, fixture.comment, fixture.document, fixture.ask, upper)
	write("drain the queue", `delete from refs_forms_owed`)

	write("delete nothing", `delete from refs where from_kind = 'comment' and from_id = $1 and to_id = 'CORE-9'`, fixture.comment)
	assertOwedForms(t, store)

	// One statement deleting both of the comment's edges and the document's owes each once.
	write("delete as an older build re-indexes", `
		delete from refs where (from_kind = 'comment' and from_id = $1) or (from_kind = 'artifact' and from_id = $2)
	`, fixture.comment, fixture.document)
	owed := assertOwedForms(t, store, owedSource{"comment", fixture.comment}, owedSource{"artifact", fixture.document})

	// A row already owed keeps the enqueued_at it holds.
	write("write the comment's edge again", `insert into refs (from_kind, from_id, to_kind, to_id) values ('comment', $1, 'issue', 'CORE-1')`, fixture.comment)
	write("delete it again", `delete from refs where from_kind = 'comment' and from_id = $1`, fixture.comment)
	if at := readOwedForms(t, store)[owedSource{"comment", fixture.comment}]; !at.Equal(owed[owedSource{"comment", fixture.comment}]) {
		t.Fatalf("a second delete moved the owed comment's enqueued_at to %v, want %v", at, owed[owedSource{"comment", fixture.comment}])
	}

	// A synthetic source, an id in another spelling and a kind that is no source owe nothing, and
	// the delete of each goes through.
	write("drain the queue", `delete from refs_forms_owed`)
	write("delete the edges of no source", `
		delete from refs where from_id in ('not-a-uuid', $1) or from_kind = 'project'
	`, upper)
	assertOwedForms(t, store)
	var left int
	if err := store.Pool.QueryRow(ctx, `select count(*) from refs where from_kind <> 'ask'`).Scan(&left); err != nil {
		t.Fatalf("count the edges left: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d edges of no source survived their delete, want none", left)
	}

	// The ask's delete is owed too: every source kind is.
	write("delete the ask's edge", `delete from refs where from_kind = 'ask'`)
	assertOwedForms(t, store, owedSource{"ask", fixture.ask})
}

// The drain's reads, exactly as refs.ListOwedAfter runs them on refs.RefsFormsOwed, go through the
// queue's own index in the order the index holds: no sort, so the limit cuts the backlog before it
// is read rather than after, which matters where the seed gives every row one enqueued_at. The read
// past a cursor bounds that index scan with the cursor (an index condition, not a filter), so a
// pass deep into the backlog starts where the last one ended rather than reading every row before.
func TestTheRefsFormsDrainReadsItsIndexInOrder(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const read = `select o.kind, o.id::text, o.enqueued_at from refs_forms_owed o order by o.enqueued_at, o.kind, o.id limit 200`
	plan := explainWithoutSeqScan(t, store, read)
	if !strings.Contains(plan, "refs_forms_owed_enqueued") || strings.Contains(plan, "Sort") {
		t.Errorf("the owed read's plan does not read refs_forms_owed_enqueued in order:\n%s", plan)
	}
	const past = `select o.kind, o.id::text, o.enqueued_at from refs_forms_owed o
		where (o.enqueued_at, o.kind, o.id) > ('2026-01-01T00:00:00Z'::timestamptz, 'comment', '00000000-0000-4000-8000-000000000000'::uuid)
		order by o.enqueued_at, o.kind, o.id limit 200`
	plan = explainWithoutSeqScan(t, store, past)
	if !strings.Contains(plan, "refs_forms_owed_enqueued") || strings.Contains(plan, "Sort") ||
		!strings.Contains(plan, "Index Cond") || strings.Contains(plan, "Filter") {
		t.Errorf("the read past a cursor does not start at the cursor in refs_forms_owed_enqueued:\n%s", plan)
	}
}
